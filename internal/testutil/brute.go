package testutil

import (
	"sort"
	"strings"

	"lantern/internal/analysis"
	"lantern/internal/index"
	"lantern/internal/query"
	"lantern/internal/rank"
)

// BruteResult 是暴力搜索的一条结果。
type BruteResult struct {
	Path  string
	Score float64
}

// Brute 是暴力扫描 oracle:直接对语料逐篇求值,与引擎使用同一分析器
// 与同一 BM25F 公式(独立实现)。
type Brute struct {
	Docs   []Doc
	toks   []docTokens
	dict   [3]map[string][]int // 字段 → 词项 → 文档下标列表
	params rank.Params
	stats  rank.Stats
}

// NewBrute 构建语料的全部统计。
func NewBrute(docs []Doc) *Brute {
	b := &Brute{Docs: docs, params: rank.DefaultParams()}
	for f := range b.dict {
		b.dict[f] = map[string][]int{}
	}
	b.toks = make([]docTokens, len(docs))
	var totalLen, docCnt [index.NumFields]float64
	for i := range docs {
		b.toks[i] = AnalyzeDoc(docs[i])
		for f := 0; f < int(index.NumFields); f++ {
			seen := map[string]bool{}
			for _, tk := range b.toks[i].fields[f] {
				if !seen[tk.Text] {
					seen[tk.Text] = true
					b.dict[f][tk.Text] = append(b.dict[f][tk.Text], i)
				}
				totalLen[f]++
			}
			if len(b.toks[i].fields[f]) > 0 {
				docCnt[f]++
			}
		}
	}
	b.stats.N = uint64(len(docs))
	for f := 0; f < int(index.NumFields); f++ {
		b.stats.Fields[f] = rank.FieldStats{TotalLen: totalLen[f], DocCount: docCnt[f]}
	}
	return b
}

func (b *Brute) avgLens() [index.NumFields]float64 {
	var out [index.NumFields]float64
	for f := 0; f < int(index.NumFields); f++ {
		out[f] = b.stats.Fields[f].AvgLen()
	}
	return out
}

func scope(hasField bool, f index.Field) []index.Field {
	if hasField {
		return []index.Field{f}
	}
	out := make([]index.Field, 0, 3)
	for i := index.Field(0); i < index.NumFields; i++ {
		out = append(out, i)
	}
	return out
}

// Search 返回 Top-K 与总命中数。结果按 (score desc, path asc)。
func (b *Brute) Search(q string, k int, _ ...bool) ([]BruteResult, int, error) {
	node, err := query.Parse(q)
	if err != nil {
		return nil, 0, err
	}
	avg := b.avgLens()
	var hits []BruteResult
	for i := range b.Docs {
		if m, sc := b.eval(node, i, avg); m {
			hits = append(hits, BruteResult{Path: b.Docs[i].Path, Score: sc})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Path < hits[j].Path
	})
	total := len(hits)
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, total, nil
}

// eval 对单文档求值:返回 (是否匹配, 得分)。
func (b *Brute) eval(n query.Node, doc int, avg [index.NumFields]float64) (bool, float64) {
	switch t := n.(type) {
	case *query.MatchAllNode:
		return true, 0
	case *query.AndNode:
		score := 0.0
		for _, c := range t.Children {
			m, s := b.eval(c, doc, avg)
			if !m {
				return false, 0
			}
			score += s
		}
		return true, score
	case *query.OrNode:
		score := 0.0
		any := false
		for _, c := range t.Children {
			m, s := b.eval(c, doc, avg)
			if m {
				any = true
				score += s
			}
		}
		return any, score
	case *query.NotNode:
		m, _ := b.eval(t.Child, doc, avg)
		return !m, 0
	case *query.TermNode:
		return b.evalTerm(t, doc, avg)
	case *query.PhraseNode:
		return b.evalPhrase(t, doc), 0
	case *query.FilterNode:
		return b.evalFilter(t, doc), 0
	}
	return false, 0
}

// evalTerm 词项匹配与打分(含前缀/模糊/单字 CJK 展开)。
func (b *Brute) evalTerm(t *query.TermNode, doc int, avg [index.NumFields]float64) (bool, float64) {
	fields := scope(t.HasField, t.Field)
	toks := analysis.Analyze(t.Text)
	if len(toks) == 0 {
		return false, 0
	}
	parts := splitParts(toks)
	score := 0.0
	for _, p := range parts {
		var m bool
		var s float64
		switch {
		case p.singleCJK && !t.IsPrefix && !t.HasFuzzy:
			m, s = b.matchExpansion(p.terms[0], b.expandSingleCJK(p.terms[0], fields), fields, doc, avg)
		case t.IsPrefix:
			m, s = b.matchExpansion(p.raw, b.expandPrefix(p.raw, fields), fields, doc, avg)
		case t.HasFuzzy:
			k := t.Fuzzy
			if k < 0 {
				n := len([]rune(p.raw))
				switch {
				case n <= 2:
					k = 0
				case n <= 5:
					k = 1
				default:
					k = 2
				}
			}
			if k == 0 {
				m, s = b.matchExpansion(p.raw, map[string]int{p.raw: 0}, fields, doc, avg)
			} else {
				m, s = b.matchExpansion(p.raw, b.expandFuzzy(p.raw, k, fields), fields, doc, avg)
			}
		case p.isPhrase:
			m = b.matchGroup(p.terms, fields, doc, 0, false)
			s = 0
		default:
			m, s = b.matchExpansion(p.raw, map[string]int{p.raw: 0}, fields, doc, avg)
		}
		if !m {
			return false, 0
		}
		score += s
	}
	return true, score
}

// part 是查询词项的一个原子部分。
type part struct {
	raw       string
	terms     []string
	singleCJK bool
	isPrefix  bool
	isPhrase  bool
	fuzzyK    int // -1 = 非;>=0 = 距离
}

// splitParts 与引擎 planTerm 的查询分析语义一致(独立实现)。
func splitParts(toks []analysis.Token) []part {
	var parts []part
	var curCJK []string
	flush := func() {
		if len(curCJK) == 1 {
			parts = append(parts, part{raw: curCJK[0], terms: curCJK, singleCJK: true})
		} else if len(curCJK) > 1 {
			parts = append(parts, part{raw: strings.Join(curCJK, ""), terms: curCJK, isPhrase: true})
		}
		curCJK = nil
	}
	prevPos := -2
	for i := range toks {
		tk := toks[i]
		switch tk.Kind {
		case analysis.KindCJK:
			if tk.Pos == prevPos+1 {
				curCJK = append(curCJK, tk.Text)
			} else {
				flush()
				curCJK = []string{tk.Text}
			}
			prevPos = tk.Pos
		case analysis.KindWord:
			flush()
			prevPos = -2
			parts = append(parts, part{raw: tk.Text, terms: []string{tk.Text}})
		case analysis.KindSub:
			prevPos = -2
		}
	}
	flush()
	return parts
}

// matchExpansion 对一组等价词项(展开结果)求值:
// 文档命中任一词项即在任一范围内字段出现;得分为逐 (词项, 字段) 贡献和。
// expansion 值为编辑距离(0 表示精确/前缀/单字,权重 1)。
func (b *Brute) matchExpansion(raw string, expansion map[string]int, fields []index.Field,
	doc int, avg [index.NumFields]float64) (bool, float64) {
	score := 0.0
	matched := false
	// 词项按字典序(与引擎展开顺序一致,求和顺序相同)。
	terms := make([]string, 0, len(expansion))
	for tm := range expansion {
		terms = append(terms, tm)
	}
	sort.Strings(terms)
	for _, tm := range terms {
		dist := expansion[tm]
		weight := 1.0
		if dist > 0 {
			weight = 1.0 - float64(dist)/float64(len(raw))
			if weight < 0.05 {
				weight = 0.05
			}
		}
		// df = 范围内各字段 df 之和的最大值(整库统计)。
		df := uint64(0)
		for _, f := range fields {
			if n := len(b.dict[f][tm]); uint64(n) > df {
				df = uint64(n)
			}
		}
		if df == 0 {
			continue
		}
		idf := rank.Idf(b.stats.N, df)
		var tf, ln [index.NumFields]float64
		for _, f := range fields {
			tf[f] = float64(b.TfInField(tm, f, doc))
			ln[f] = float64(len(b.toks[doc].fields[f]))
		}
		tfw := rank.TFW(b.params, tf, ln, avg)
		s := rank.TermScore(b.params, idf, tfw, weight)
		if tfw > 0 {
			matched = true
			score += s
		}
	}
	return matched, score
}

// tfInField 返回词项在某文档某字段的词频。
func (b *Brute) TfInField(term string, f index.Field, doc int) int {
	n := 0
	for _, tk := range b.toks[doc].fields[f] {
		if tk.Text == term {
			n++
		}
	}
	return n
}

// positionsInField 返回词项在某文档某字段的全部位置。
func (b *Brute) positionsInField(term string, f index.Field, doc int) []int {
	var out []int
	for _, tk := range b.toks[doc].fields[f] {
		if tk.Text == term {
			out = append(out, tk.Pos)
		}
	}
	return out
}

// expandPrefix 前缀展开(整库词表)。
func (b *Brute) expandPrefix(prefix string, fields []index.Field) map[string]int {
	out := map[string]int{}
	for _, f := range fields {
		for tm := range b.dict[f] {
			if strings.HasPrefix(tm, prefix) {
				if _, ok := out[tm]; !ok {
					out[tm] = 0
				}
			}
		}
	}
	return out
}

// expandFuzzy 模糊展开。
func (b *Brute) expandFuzzy(term string, k int, fields []index.Field) map[string]int {
	out := map[string]int{}
	pat := []byte(term)
	for _, f := range fields {
		for tm := range b.dict[f] {
			d := Levenshtein(pat, []byte(tm))
			if d <= k {
				if old, ok := out[tm]; !ok || d < old {
					out[tm] = d
				}
			}
		}
	}
	return out
}

// expandSingleCJK 单字展开:包含该字的二元组。
func (b *Brute) expandSingleCJK(ch string, fields []index.Field) map[string]int {
	out := map[string]int{}
	for _, f := range fields {
		for tm := range b.dict[f] {
			if len(tm) == 6 && strings.Contains(tm, ch) {
				out[tm] = 0
			}
		}
	}
	return out
}

// evalPhrase 短语/邻近:组间 AND,字段间 OR;得分为 0。
func (b *Brute) evalPhrase(t *query.PhraseNode, doc int) bool {
	fields := scope(t.HasField, t.Field)
	toks := analysis.Analyze(t.Raw)
	if len(toks) == 0 {
		return false
	}
	// 组:按 Pos 连续切分。
	var groups [][]string
	var cur []string
	prevPos := -2
	for i := range toks {
		tk := toks[i]
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
		all := true
		for _, g := range groups {
			if !b.matchGroup(g, []index.Field{f}, doc, slop, slop > 0) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// matchGroup 组内匹配:严格 = 顺序相邻;邻近 = 任意顺序窗口。
func (b *Brute) matchGroup(terms []string, fields []index.Field, doc int, slop int, proximity bool) bool {
	for _, f := range fields {
		if proximity {
			if b.matchWindow(terms, f, doc, slop+len(terms)-1) {
				return true
			}
			continue
		}
		okAll := true
		for _, start := range b.positionsInField(terms[0], f, doc) {
			good := true
			for j := 1; j < len(terms); j++ {
				found := false
				for _, p := range b.positionsInField(terms[j], f, doc) {
					if int(p) == start+j {
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
				okAll = true
				return true
			}
			_ = okAll
		}
	}
	return false
}

// matchWindow 邻近:存在一组位置(每词一个,任意顺序)使 max-min <= limit。
func (b *Brute) matchWindow(terms []string, f index.Field, doc int, limit int) bool {
	type pt struct {
		pos  int
		term int
	}
	var all []pt
	for i, tm := range terms {
		for _, p := range b.positionsInField(tm, f, doc) {
			all = append(all, pt{p, i})
		}
	}
	if len(all) < len(terms) {
		return false
	}
	sort.Slice(all, func(i, j int) bool { return all[i].pos < all[j].pos })
	need := make([]int, len(terms))
	missing := len(terms)
	best := 1 << 30
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
	return best <= limit
}

// evalFilter 过滤条件。
func (b *Brute) evalFilter(t *query.FilterNode, doc int) bool {
	d := b.Docs[doc]
	val := t.Val
	switch t.Field {
	case "ext":
		return strings.TrimPrefix(strings.ToLower(d.Ext), ".") == strings.TrimPrefix(strings.ToLower(val), ".")
	case "size":
		n, err := parseSizeB(val)
		if err != nil {
			return false
		}
		return cmpInt64(d.Size, t.Op, n)
	case "mtime":
		st, en, err := parseTimeB(val)
		if err != nil {
			return false
		}
		if t.Op == "=" {
			return d.MTime >= st && d.MTime < en
		}
		th := st
		if t.Op == "<" || t.Op == "<=" {
			th = en - 1
		}
		return cmpInt64(d.MTime, t.Op, th)
	}
	return false
}

func cmpInt64(v int64, op string, t int64) bool {
	switch op {
	case ">":
		return v > t
	case ">=":
		return v >= t
	case "<":
		return v < t
	case "<=":
		return v <= t
	}
	return v == t
}
