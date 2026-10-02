// Package whynot 实现 Why-not 诊断(规格 8):解释"某个文档为什么
// 没有命中某个查询",分三层:
//
//	文档层:被忽略(含规则来源)/超大/二进制/非 UTF-8/越界/未扫描/索引过期;
//	子句层:逐子句用存储正文重新分析,给出缺失词项的最近词与位置、
//	        短语/邻近的实际间隔与顺序、NOT 触发、过滤实际值;
//	建议层:移除哪个子句后可命中,并估计其在全部结果中的排名。
package whynot

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lantern/internal/analysis"
	"lantern/internal/fsx"
	"lantern/internal/index"
	"lantern/internal/query"
	"lantern/internal/scan"
)

// Config 是诊断参数。
type Config struct {
	// Base 为索引基目录(文档路径相对它)。
	Base string
	// MaxSize 为扫描时的大小上限(默认 2MB)。
	MaxSize int64
	Fsys    fsx.FS
}

// Report 是完整诊断报告(文本与 JSON 两种输出)。
type Report struct {
	Query   string `json:"query"`
	Path    string `json:"path"`
	InIndex bool   `json:"in_index"`
	// Match 为文档已命中查询(此时仅文档层的状态信息有意义)。
	Match bool `json:"match"`
	// Document 为文档层诊断(未入索引时必有)。
	Document *DocDiagnosis `json:"document,omitempty"`
	// Clauses 为子句层诊断。
	Clauses []ClauseDiagnosis `json:"clauses,omitempty"`
	// Suggestions 为建议层。
	Suggestions []Suggestion `json:"suggestions,omitempty"`
}

// DocDiagnosis 是文档层诊断。
type DocDiagnosis struct {
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	// Rule 为被忽略时的规则(来源与行号)。
	Rule *RuleInfo `json:"rule,omitempty"`
	// Indexed/Disk 为索引过期时的两侧记录。
	Indexed *StateSide `json:"indexed,omitempty"`
	Disk    *StateSide `json:"disk,omitempty"`
}

// RuleInfo 标识一条忽略规则。
type RuleInfo struct {
	Pattern string `json:"pattern"`
	Source  string `json:"source"`
	Line    int    `json:"line"`
}

// StateSide 是一端的 size/mtime/sha 记录。
type StateSide struct {
	Size  int64  `json:"size"`
	MTime int64  `json:"mtime"`
	SHA   string `json:"sha"`
}

// ClauseDiagnosis 是子句层诊断。
type ClauseDiagnosis struct {
	Clause string `json:"clause"`
	Kind   string `json:"kind"` // term/phrase/not/filter
	Status string `json:"status"`
	// ok / missing / field_mismatch / phrase_fail / not_triggered / filter_fail
	Detail    string     `json:"detail"`
	Positions []PosInfo  `json:"positions,omitempty"`
	Nearest   []NearTerm `json:"nearest,omitempty"`
	Actual    string     `json:"actual,omitempty"`
	Required  string     `json:"required,omitempty"`
}

// PosInfo 是某词项在某文档某字段的位置序列。
type PosInfo struct {
	Term  string  `json:"term"`
	Field string  `json:"field"`
	Pos   []int32 `json:"pos"`
}

// NearTerm 是文档中与缺失词项编辑距离 ≤2 的最近词。
type NearTerm struct {
	Term  string `json:"term"`
	Dist  int    `json:"dist"`
	Field string `json:"field"`
	Pos   int32  `json:"pos"`
}

// Suggestion 是建议层条目。
type Suggestion struct {
	// RemoveClause 为建议移除的子句原文。
	RemoveClause string `json:"remove_clause"`
	// NewQuery 为移除后的查询串。
	NewQuery string `json:"new_query"`
	// EstimatedRank 为移除后该文档在全部结果中的估计排名(1-based)。
	EstimatedRank int    `json:"estimated_rank"`
	Total         uint64 `json:"total"`
}

// Diagnose 执行三层诊断。
func Diagnose(ix *index.Index, s *query.Searcher, cfg Config, q, path string) (*Report, error) {
	cfg.applyDefaults()
	rep := &Report{Query: q, Path: path}

	node, err := query.Parse(q)
	if err != nil {
		return nil, err
	}

	e, sr, inIndex := ix.Lookup(path)
	rep.InIndex = inIndex

	// 命中判定。
	if inIndex && sr != nil {
		m, _ := s.EvaluateClause(node, sr.ID(), e.DocID)
		rep.Match = m
	}

	// 文档层。
	rep.Document = diagnoseDocument(ix, cfg, path, inIndex)
	if rep.Document != nil && rep.Document.Reason != "" {
		// 未入索引:无子句层。
		return rep, nil
	}
	if !inIndex {
		return rep, nil
	}

	// 子句层(已入索引但未命中)。
	if !rep.Match {
		sd, err := sr.Reader().StoredDoc(e.DocID)
		if err != nil {
			return rep, fmt.Errorf("whynot: 读取存储文档: %w", err)
		}
		dv, err := sr.Reader().DocValues(e.DocID)
		if err != nil {
			return rep, fmt.Errorf("whynot: 读取 doc values: %w", err)
		}
		doc := docIndex{
			path:  path,
			title: sd.Title,
			body:  sd.Body,
			segID: sr.ID(),
			docID: e.DocID,
			size:  dv.Size,
			mtime: dv.MTime,
			ext:   dv.Ext,
		}
		rep.Clauses = diagnoseClauses(s, node, doc)
		if len(rep.Clauses) == 0 {
			// 理论上不发生(未命中必有子句失败)。
			rep.Clauses = append(rep.Clauses, ClauseDiagnosis{
				Clause: q, Kind: "query", Status: "missing",
				Detail: "查询未命中,但未定位到失败子句",
			})
		}
	}

	// 建议层。
	rep.Suggestions = suggest(s, node, sr.ID(), e.DocID, path)
	return rep, nil
}

func (c *Config) applyDefaults() {
	if c.MaxSize <= 0 {
		c.MaxSize = 2 << 20
	}
	if c.Fsys == nil {
		c.Fsys = fsx.OsFS()
	}
}

// diagnoseDocument 文档层:路径不在状态表时判定具体原因;
// 已在状态表时检查过期。
func diagnoseDocument(ix *index.Index, cfg Config, path string, inIndex bool) *DocDiagnosis {
	// 越界检查。
	full := filepath.Join(cfg.Base, filepath.FromSlash(path))
	if !strings.HasPrefix(filepath.Clean(full), filepath.Clean(cfg.Base)+string(filepath.Separator)) {
		return &DocDiagnosis{Reason: "outside", Detail: "路径不在索引根目录之下"}
	}
	st, err := cfg.Fsys.Stat(full)
	if err != nil {
		return &DocDiagnosis{Reason: "missing_on_disk", Detail: "文件已从磁盘删除,但索引尚未更新"}
	}
	// 忽略规则(沿目录链装载)。
	chain := scan.NewIgnoreChain()
	dir := filepath.Dir(full)
	levels := 0
	for {
		chain.Push(scan.LoadDirRules(cfg.Fsys, dir, cfg.Base))
		levels++
		if dir == cfg.Base || levels > 64 {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	rel := filepath.ToSlash(path)
	if d := chain.Evaluate(rel, st.IsDir()); d.Ignored {
		diag := &DocDiagnosis{
			Reason: "ignored",
			Detail: "被忽略规则命中",
		}
		if d.Rule != nil {
			diag.Rule = &RuleInfo{Pattern: d.Rule.Pattern, Source: d.Rule.Source, Line: d.Rule.Line}
			diag.Detail = fmt.Sprintf("被规则 %q 命中(来源 %s:%d)", d.Rule.Pattern, d.Rule.Source, d.Rule.Line)
		}
		return diag
	}
	if st.Size() > cfg.MaxSize {
		return &DocDiagnosis{Reason: "too_large",
			Detail: fmt.Sprintf("文件 %d 字节超过上限 %d 字节", st.Size(), cfg.MaxSize)}
	}
	if data, err := readSmallFile(cfg, full); err == nil {
		sn := scan.Sniff(data)
		switch sn.Kind {
		case "binary":
			return &DocDiagnosis{Reason: "binary", Detail: "疑似二进制文件(含 NUL 或控制字符)"}
		case "nonutf8":
			return &DocDiagnosis{Reason: "non_utf8", Detail: "非 UTF-8 编码(如 GBK),不支持解码"}
		}
	}

	if !inIndex {
		return &DocDiagnosis{Reason: "not_scanned", Detail: "文件存在且可读,但尚未被索引(不在索引运行覆盖范围内或上次索引后未重新扫描)"}
	}
	// 已入索引:对比磁盘 mtime/hash 与索引记录(规格 8.1 索引过期)。
	info, ok := ix.StateInfoOf(path)
	if ok {
		changed := false
		disk := StateSide{Size: st.Size(), MTime: st.ModTime().Unix()}
		if disk.MTime != info.MTime {
			changed = true
		}
		if data, err := readSmallFile(cfg, full); err == nil {
			sum := sha256.Sum256(data)
			disk.SHA = fmt.Sprintf("%x", sum)
			if disk.SHA != info.SHA {
				changed = true
			}
		}
		if changed {
			return &DocDiagnosis{
				Reason:  "stale",
				Detail:  "磁盘文件与索引记录不一致,索引已过期;建议重新执行 lantern index",
				Indexed: &StateSide{Size: info.Size, MTime: info.MTime, SHA: info.SHA},
				Disk:    &disk,
			}
		}
	}
	return &DocDiagnosis{}
}

func readSmallFile(cfg Config, full string) ([]byte, error) {
	f, err := cfg.Fsys.Open(full)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > cfg.MaxSize {
		return nil, fmt.Errorf("too large")
	}
	buf := make([]byte, st.Size())
	if _, err := f.Read(buf); err != nil && len(buf) == 0 {
		return nil, err
	}
	return buf, nil
}

// docIndex 是子句层所需的文档内容。
type docIndex struct {
	path, title, body string
	segID             string
	docID             uint32
	size              int64
	mtime             int64
	ext               string
}

// tokens 返回某字段的重分析 token 流(规格 8.2:用存储正文重新分析)。
func (d *docIndex) tokens(f index.Field) []analysis.Token {
	var text string
	switch f {
	case index.FieldPath:
		text = d.path
	case index.FieldTitle:
		text = d.title
	default:
		text = d.body
	}
	return analysis.Analyze(text)
}

// diagnoseClauses 逐子句诊断。
func diagnoseClauses(s *query.Searcher, node query.Node, doc docIndex) []ClauseDiagnosis {
	var children []query.Node
	if and, ok := node.(*query.AndNode); ok {
		children = and.Children
	} else {
		children = []query.Node{node}
	}
	var out []ClauseDiagnosis
	for _, c := range children {
		// ok=false 表示该子句未命中(诊断其失败原因)。
		if d, ok := diagnoseClause(s, c, doc); !ok {
			out = append(out, d)
		}
	}
	return out
}

// diagnoseClause 单子句;ok 表示子句命中(不出现在报告中)。
func diagnoseClause(s *query.Searcher, n query.Node, doc docIndex) (ClauseDiagnosis, bool) {
	switch t := n.(type) {
	case *query.TermNode:
		return diagnoseTerm(t, doc)
	case *query.PhraseNode:
		return diagnosePhrase(t, doc)
	case *query.NotNode:
		// NOT 子句:文档命中其子树即被排除。
		child := t.Child
		if m, _ := s.EvaluateClause(child, doc.segID, doc.docID); m {
			d := ClauseDiagnosis{
				Clause: query.Render(child), Kind: "not", Status: "not_triggered",
				Detail: "NOT 子句命中该文档,导致整体被排除",
			}
			if tm, ok := child.(*query.TermNode); ok {
				d.Positions = termPositions(tm, doc)
			}
			return d, false
		}
		return ClauseDiagnosis{}, true
	case *query.FilterNode:
		return diagnoseFilter(t, doc)
	case *query.OrNode:
		// OR:全部分支都未命中才失败;报告第一个失败分支。
		for _, c := range t.Children {
			if m, _ := s.EvaluateClause(c, doc.segID, doc.docID); m {
				return ClauseDiagnosis{}, true
			}
		}
		return ClauseDiagnosis{
			Clause: query.Render(t), Kind: "or", Status: "missing",
			Detail: "OR 的全部分支都未命中该文档",
		}, false
	case *query.AndNode:
		for _, c := range t.Children {
			if m, _ := s.EvaluateClause(c, doc.segID, doc.docID); !m {
				return diagnoseClause(s, c, doc)
			}
		}
		return ClauseDiagnosis{}, true
	}
	return ClauseDiagnosis{}, true
}

// fieldsOf 返回词项的检索字段范围。
func fieldsOf(hasField bool, f index.Field) []index.Field {
	if hasField {
		return []index.Field{f}
	}
	return []index.Field{index.FieldTitle, index.FieldPath, index.FieldBody}
}

func termPositions(t *query.TermNode, doc docIndex) []PosInfo {
	var out []PosInfo
	for _, f := range fieldsOf(t.HasField, t.Field) {
		toks := doc.tokens(f)
		for _, tk := range toks {
			if tk.Text == t.Text {
				out = append(out, PosInfo{Term: t.Text, Field: f.Name(), Pos: []int32{int32(tk.Pos)}})
				break
			}
		}
	}
	return out
}

// diagnoseTerm 词项缺失/字段不匹配诊断。
func diagnoseTerm(t *query.TermNode, doc docIndex) (ClauseDiagnosis, bool) {
	fields := fieldsOf(t.HasField, t.Field)
	toks := analysis.Analyze(t.Text)
	if len(toks) == 0 {
		return ClauseDiagnosis{Clause: t.Text, Kind: "term", Status: "missing",
			Detail: "词项分析结果为空"}, false
	}
	// 多部分(混合词)按与引擎一致的方式拆分,任一部分失败即失败。
	parts := splitQ(toks)
	for _, p := range parts {
		switch {
		case p.singleCJK:
			if !docHasExpansion(p.terms[0], docContains(p.terms[0]), fields, doc) {
				d := ClauseDiagnosis{
					Clause: t.Text, Kind: "term", Status: "missing",
					Detail: fmt.Sprintf("文档中不含包含 %q 的词项(单字展开匹配失败)", p.terms[0]),
				}
				d.Nearest = nearestTerms(p.terms[0], fields, doc)
				return d, false
			}
		case p.isPhrase:
			if !groupMatchInDoc(p.terms, fields, doc, 0, false) {
				d := ClauseDiagnosis{
					Clause: t.Text, Kind: "term", Status: "phrase_fail",
					Detail: "连续中文词未按二元组序列相邻出现",
				}
				for _, tm := range p.terms {
					d.Positions = append(d.Positions, termTextPositions(tm, fields, doc)...)
				}
				return d, false
			}
		default:
			_, found := docHasWord(p.raw, fields, doc)
			if !found {
				d := ClauseDiagnosis{
					Clause: t.Text, Kind: "term", Status: "missing",
					Detail: fmt.Sprintf("词项 %q 不在该文档检索范围内", p.raw),
				}
				// 词只出现在未被检索的字段:指出字段(规格 8.2)。
				fl, ok2 := docHasWord(p.raw, allFields(), doc)
				if ok2 {
					d.Status = "field_mismatch"
					d.Detail = fmt.Sprintf("词项 %q 只出现在未被检索的字段 %s", p.raw, fl.Name())
					d.Positions = termTextPositions(p.raw, []index.Field{fl}, doc)
					return d, false
				}
				d.Nearest = nearestTerms(p.raw, fields, doc)
				if len(d.Nearest) > 0 {
					var ns []string
					for _, nt := range d.Nearest {
						ns = append(ns, fmt.Sprintf("%s(距离%d,%s@%d)", nt.Term, nt.Dist, nt.Field, nt.Pos))
					}
					d.Detail = fmt.Sprintf("词项 %q 缺失;文档中最近的词:%s", p.raw, strings.Join(ns, ", "))
				}
				return d, false
			}
		}
	}
	return ClauseDiagnosis{}, true
}

// diagnosePhrase 短语/邻近失败诊断(位置、间隔、顺序)。
func diagnosePhrase(t *query.PhraseNode, doc docIndex) (ClauseDiagnosis, bool) {
	fields := fieldsOf(t.HasField, t.Field)
	toks := analysis.Analyze(t.Raw)
	var groups [][]string
	var cur []string
	prevPos := -2
	for _, tk := range toks {
		if tk.Kind == analysis.KindSub {
			continue
		}
		if tk.Pos == prevPos+1 && len(cur) > 0 {
			cur = append(cur, tk.Text)
		} else {
			if len(cur) > 0 {
				groups = append(groups, cur)
			}
			cur = []string{tk.Text}
		}
		prevPos = tk.Pos
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	slop := 0
	if t.HasSlop {
		slop = t.Slop
	}
	for _, f := range fields {
		ok := true
		for _, g := range groups {
			if !groupMatchInDoc(g, []index.Field{f}, doc, slop, slop > 0) {
				ok = false
				break
			}
		}
		if ok {
			return ClauseDiagnosis{}, true
		}
	}
	// 失败:取第一组在第一个字段的位置细节。
	d := ClauseDiagnosis{
		Clause: `"` + t.Raw + `"`, Kind: "phrase", Status: "phrase_fail",
	}
	if t.HasSlop {
		d.Detail = fmt.Sprintf("邻近查询(最大间隔 %d)未满足", slop)
	} else {
		d.Detail = "短语未按顺序相邻出现"
	}
	g := groups[0]
	for _, f := range fields {
		for _, tm := range g {
			d.Positions = append(d.Positions, termTextPositions(tm, []index.Field{f}, doc)...)
		}
		if span, reversed, have := spanInfo(g, f, doc); have {
			d.Actual = fmt.Sprintf("实际最小间隔 %d", span)
			d.Required = fmt.Sprintf("要求最小间隔 %d", slop+len(g)-1)
			if reversed {
				d.Detail += ";检测到词项顺序颠倒"
			} else {
				d.Detail += ";顺序正确但间隔过大"
			}
			break
		}
	}
	return d, false
}

// spanInfo 计算组内各词项的实际最小窗口与是否顺序颠倒。
func spanInfo(terms []string, f index.Field, doc docIndex) (span int, reversed bool, have bool) {
	if len(terms) < 2 {
		return 0, false, false
	}
	posOf := func(tm string) []int32 {
		for _, tk := range doc.tokens(f) {
			if tk.Text == tm {
				return []int32{int32(tk.Pos)}
			}
		}
		return nil
	}
	first := posOf(terms[0])
	last := posOf(terms[len(terms)-1])
	if len(first) == 0 || len(last) == 0 {
		return 0, false, false
	}
	// 顺序正确时首词位置应小于尾词位置。
	if first[0] > last[0] {
		rev := true
		for i := 0; i+1 < len(terms); i++ {
			a := posOf(terms[i])
			b := posOf(terms[i+1])
			if len(a) == 0 || len(b) == 0 || a[0] <= b[0] {
				rev = false
				break
			}
		}
		return int(first[0] - last[0]), rev, true
	}
	// 最小窗口:每词取与首词最近的位置。
	lo, hi := first[0], first[0]
	missing := false
	for _, tm := range terms[1:] {
		ps := posOf(tm)
		if len(ps) == 0 {
			missing = true
			break
		}
		for _, p := range ps {
			if p < lo {
				lo = p
			}
			if p > hi {
				hi = p
			}
		}
	}
	if missing {
		return 0, false, false
	}
	return int(hi - lo), false, true
}

func diagnoseFilter(t *query.FilterNode, doc docIndex) (ClauseDiagnosis, bool) {
	d := ClauseDiagnosis{Clause: t.Field + ":" + t.Op + t.Val, Kind: "filter", Status: "filter_fail"}
	switch t.Field {
	case "ext":
		d.Actual = doc.ext
		d.Required = strings.TrimPrefix(t.Val, ".")
		if d.Actual == d.Required {
			return ClauseDiagnosis{}, true
		}
		d.Detail = "扩展名不匹配"
	case "size":
		d.Actual = fmt.Sprintf("%d 字节", doc.size)
		d.Required = t.Op + " " + t.Val
		if cmpSize(doc.size, t.Op, t.Val) {
			return ClauseDiagnosis{}, true
		}
		d.Detail = "文件大小不满足条件"
	case "mtime":
		d.Actual = time.Unix(doc.mtime, 0).UTC().Format("2006-01-02")
		d.Required = t.Op + " " + t.Val
		if cmpTime(doc.mtime, t.Op, t.Val) {
			return ClauseDiagnosis{}, true
		}
		d.Detail = "修改时间不满足条件"
	}
	return d, false
}

// nearestTerms 文档内编辑距离 ≤2 的最近词(规格 8.2)。
func nearestTerms(term string, fields []index.Field, doc docIndex) []NearTerm {
	pat := []byte(term)
	var out []NearTerm
	seen := map[string]bool{}
	for _, f := range fields {
		for _, tk := range doc.tokens(f) {
			if seen[tk.Text+"/"+f.Name()] {
				continue
			}
			d := levBytes(pat, []byte(tk.Text))
			if d > 0 && d <= 2 {
				seen[tk.Text+"/"+f.Name()] = true
				out = append(out, NearTerm{Term: tk.Text, Dist: d, Field: f.Name(), Pos: int32(tk.Pos)})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dist < out[j].Dist })
	if len(out) > 5 {
		out = out[:5]
	}
	return out
}

func levBytes(a, b []byte) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			m := prev[j] + 1
			if cur[j-1]+1 < m {
				m = cur[j-1] + 1
			}
			if prev[j-1]+cost < m {
				m = prev[j-1] + cost
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// qPart / splitQ:与引擎查询分析一致的拆分(诊断侧)。
type qPart struct {
	raw       string
	terms     []string
	singleCJK bool
	isPhrase  bool
}

func splitQ(toks []analysis.Token) []qPart {
	var parts []qPart
	var cur []string
	flush := func() {
		if len(cur) == 1 {
			parts = append(parts, qPart{raw: cur[0], terms: cur, singleCJK: true})
		} else if len(cur) > 1 {
			parts = append(parts, qPart{raw: strings.Join(cur, ""), terms: cur, isPhrase: true})
		}
		cur = nil
	}
	prevPos := -2
	for _, tk := range toks {
		switch tk.Kind {
		case analysis.KindCJK:
			if tk.Pos == prevPos+1 {
				cur = append(cur, tk.Text)
			} else {
				flush()
				cur = []string{tk.Text}
			}
			prevPos = tk.Pos
		case analysis.KindWord:
			flush()
			prevPos = -2
			parts = append(parts, qPart{raw: tk.Text, terms: []string{tk.Text}})
		case analysis.KindSub:
			prevPos = -2
		}
	}
	flush()
	return parts
}

func allFields() []index.Field {
	return []index.Field{index.FieldTitle, index.FieldPath, index.FieldBody}
}

func docHasWord(word string, fields []index.Field, doc docIndex) (index.Field, bool) {
	for _, f := range fields {
		for _, tk := range doc.tokens(f) {
			if tk.Text == word {
				return f, true
			}
		}
	}
	return 0, false
}

func termTextPositions(term string, fields []index.Field, doc docIndex) []PosInfo {
	var out []PosInfo
	for _, f := range fields {
		var ps []int32
		for _, tk := range doc.tokens(f) {
			if tk.Text == term {
				ps = append(ps, int32(tk.Pos))
			}
		}
		if len(ps) > 0 {
			out = append(out, PosInfo{Term: term, Field: f.Name(), Pos: ps})
		}
	}
	return out
}

func docContains(ch string) func(tk analysis.Token) bool {
	return func(tk analysis.Token) bool {
		return len(tk.Text) == 6 && strings.Contains(tk.Text, ch)
	}
}

func docHasExpansion(ch string, pred func(analysis.Token) bool, fields []index.Field, doc docIndex) bool {
	for _, f := range fields {
		for _, tk := range doc.tokens(f) {
			if pred(tk) {
				return true
			}
		}
	}
	return false
}

// groupMatchInDoc 组匹配(与引擎/打分一致的窗口语义)。
func groupMatchInDoc(terms []string, fields []index.Field, doc docIndex, slop int, proximity bool) bool {
	for _, f := range fields {
		posLists := make([][]int32, len(terms))
		ok := true
		for i, tm := range terms {
			var ps []int32
			for _, tk := range doc.tokens(f) {
				if tk.Text == tm {
					ps = append(ps, int32(tk.Pos))
				}
			}
			if len(ps) == 0 {
				ok = false
				break
			}
			posLists[i] = ps
		}
		if !ok {
			continue
		}
		if !proximity {
			for _, anchor := range posLists[0] {
				good := true
				for i := 1; i < len(posLists); i++ {
					found := false
					for _, p := range posLists[i] {
						if p == anchor+int32(i) {
							found = true
							break
						}
					}
					if !found {
						good = false
						break
					}
				}
				if good {
					return true
				}
			}
			continue
		}
		limit := int32(slop + len(terms) - 1)
		type pt struct {
			pos  int32
			term int
		}
		var all []pt
		for i, ps := range posLists {
			for _, p := range ps {
				all = append(all, pt{p, i})
			}
		}
		sort.Slice(all, func(i, j int) bool { return all[i].pos < all[j].pos })
		need := make([]int, len(terms))
		missing := len(terms)
		best := int32(1<<31 - 1)
		lo := 0
		for hi := 0; hi < len(all); hi++ {
			need[all[hi].term]++
			if need[all[hi].term] == 1 {
				missing--
			}
			for missing == 0 {
				span := all[hi].pos - all[lo].pos
				if span < best {
					best = span
				}
				need[all[lo].term]--
				if need[all[lo].term] == 0 {
					missing++
				}
				lo++
			}
		}
		if best <= limit {
			return true
		}
	}
	return false
}

func cmpSize(v int64, op, val string) bool {
	s := strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(val), "B"))
	s = strings.TrimSuffix(s, "B")
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "KB"):
		mult, s = 1<<10, strings.TrimSuffix(s, "KB")
	case strings.HasSuffix(s, "MB"):
		mult, s = 1<<20, strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "GB"):
		mult, s = 1<<30, strings.TrimSuffix(s, "GB")
	}
	var n int64
	fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	n *= mult
	switch op {
	case ">":
		return v > n
	case ">=":
		return v >= n
	case "<":
		return v < n
	case "<=":
		return v <= n
	}
	return v == n
}

func cmpTime(v int64, op, val string) bool {
	st, en, err := parseTimeRange(val)
	if err != nil {
		return false
	}
	if op == "=" || op == "" {
		return v >= st && v < en
	}
	th := st
	if op == "<" || op == "<=" {
		th = en - 1
	}
	switch op {
	case ">":
		return v > th
	case ">=":
		return v >= th
	case "<":
		return v < th
	case "<=":
		return v <= th
	}
	return false
}

func parseTimeRange(s string) (int64, int64, error) {
	s = strings.TrimSpace(s)
	loc := time.UTC
	if len(s) == 4 {
		var y int
		fmt.Sscanf(s, "%d", &y)
		t := time.Date(y, 1, 1, 0, 0, 0, 0, loc)
		return t.Unix(), t.AddDate(1, 0, 0).Unix(), nil
	}
	if len(s) == 7 {
		var y, m int
		fmt.Sscanf(s[:4], "%d", &y)
		fmt.Sscanf(s[5:7], "%d", &m)
		t := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, loc)
		return t.Unix(), t.AddDate(0, 1, 0).Unix(), nil
	}
	if len(s) == 10 {
		var y, m, d int
		fmt.Sscanf(s[:4], "%d", &y)
		fmt.Sscanf(s[5:7], "%d", &m)
		fmt.Sscanf(s[8:10], "%d", &d)
		t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
		return t.Unix(), t.AddDate(0, 0, 1).Unix(), nil
	}
	return 0, 0, fmt.Errorf("bad time")
}

// suggest 建议层:移除单个子句后可命中 + 排名估计(规格 8.3)。
func suggest(s *query.Searcher, node query.Node, segID string, docID uint32, path string) []Suggestion {
	var children []query.Node
	if and, ok := node.(*query.AndNode); ok {
		children = and.Children
	} else if _, ok := node.(*query.OrNode); ok {
		// OR 语义:至少一个分支命中才算命中;移除子句只会缩小结果集。
		return []Suggestion{{
			RemoveClause:  "(OR 查询的子句)",
			NewQuery:      query.Render(node),
			EstimatedRank: 0,
			Total:         0,
		}}
	} else {
		return nil // 单子句:移除后为空查询,无意义
	}
	var out []Suggestion
	for i, c := range children {
		var rest []query.Node
		rest = append(rest, children[:i]...)
		rest = append(rest, children[i+1:]...)
		var newNode query.Node
		switch len(rest) {
		case 0:
			continue
		case 1:
			newNode = rest[0]
		default:
			newNode = &query.AndNode{Children: rest}
		}
		q2 := query.Render(newNode)
		if m, _ := s.EvaluateClause(newNode, segID, docID); m {
			_, rank, total, _ := s.RankOf(q2, path)
			out = append(out, Suggestion{
				RemoveClause:  query.Render(c),
				NewQuery:      q2,
				EstimatedRank: rank,
				Total:         total,
			})
		}
	}
	return out
}
