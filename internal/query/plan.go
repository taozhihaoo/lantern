package query

import (
	"sort"
	"strings"

	"lantern/internal/analysis"
	"lantern/internal/index"
	"lantern/internal/rank"
)

// ExpansionLimit 是前缀/模糊/单字展开的词项上限(规格 6)。
const ExpansionLimit = 1024

// Plan 是规划产物:可对快照中每个段实例化执行,并支持 Explain。
type Plan struct {
	root      planNode
	terms     []*scoreTerm // 打分词项(Explain 顺序)
	truncated bool
	query     string
}

type planNode interface {
	build(se *segEnv) DocIterator
}

// emptyPlan 恒不匹配。
type emptyPlan struct{}

func (emptyPlan) build(*segEnv) DocIterator { return &emptyIter{} }

type emptyIter struct{}

func (e *emptyIter) Next() bool          { return false }
func (e *emptyIter) Advance(uint32) bool { return false }
func (e *emptyIter) DocID() uint32       { return 0 }
func (e *emptyIter) Score() float64      { return 0 }

type allPlan struct{}

func (allPlan) build(se *segEnv) DocIterator { return &allIter{se: se} }

type termPlan struct {
	sc     *scoreTerm
	fields []index.Field
}

func (p *termPlan) build(se *segEnv) DocIterator { return newTermIter(se, p.sc, p.fields) }

type orPlan struct{ kids []planNode }

func (p *orPlan) build(se *segEnv) DocIterator {
	its := make([]DocIterator, 0, len(p.kids))
	for _, k := range p.kids {
		its = append(its, k.build(se))
	}
	return newOrIter(its)
}

type andPlan struct {
	pos     []planNode
	negs    []planNode
	filters []*filterCond
}

func (p *andPlan) build(se *segEnv) DocIterator {
	pos := make([]DocIterator, 0, len(p.pos))
	for _, k := range p.pos {
		it := k.build(se)
		if it == nil {
			return &emptyIter{}
		}
		pos = append(pos, it)
	}
	negs := make([]DocIterator, 0, len(p.negs))
	for _, k := range p.negs {
		negs = append(negs, k.build(se))
	}
	return newAndIter(pos, negs, p.filters, se)
}

type notPlan struct{ child planNode }

func (p *notPlan) build(se *segEnv) DocIterator {
	return newAndIter([]DocIterator{&allIter{se: se}}, []DocIterator{p.child.build(se)}, nil, se)
}

// phrasePlan 是短语/邻近:多个组按 AND 组合,组内词项在单字段内
// 顺序相邻(Slop=0)或窗口邻近(Slop>0);字段范围取 OR。
type phrasePlan struct {
	fields  []index.Field
	groups  [][]string
	slop    int
	hasSlop bool
}

func (p *phrasePlan) build(se *segEnv) DocIterator {
	var fieldIters []DocIterator
	for _, f := range p.fields {
		var groupIters []DocIterator
		ok := true
		for _, g := range p.groups {
			its := make([]*index.PostingIterator, 0, len(g))
			for _, t := range g {
				it, err := se.r.Postings(f, t)
				if err != nil || it == nil {
					ok = false
					break
				}
				its = append(its, it)
			}
			if !ok {
				break
			}
			groupIters = append(groupIters, newPhraseIter(its, p.slopFor()))
		}
		if !ok || len(groupIters) == 0 {
			continue
		}
		var fi DocIterator
		if len(groupIters) == 1 {
			fi = groupIters[0]
		} else {
			fi = newAndIter(groupIters, nil, nil, se)
		}
		fieldIters = append(fieldIters, fi)
	}
	return newOrIter(fieldIters)
}

// slopFor:严格短语 slop=0;邻近窗口按规格 "~S 最大间隔 S"。
func (p *phrasePlan) slopFor() int { return p.slop }

// planner 把 AST 编译为 Plan。
type planner struct {
	segs      []*index.SegmentRef
	stats     rank.Stats
	params    rank.Params
	dfCache   map[string]*scoreTerm
	dictMu    map[string][]string // (segID|field) → 有序词典缓存
	budget    int
	trunc     *bool
	termOrder []*scoreTerm // 打分词项顺序(Explain 用;NOT 子树不计分)
	inNeg     bool         // 当前是否处于 NOT 子树
}

func newPlanner(segs []*index.SegmentRef, st rank.Stats, params rank.Params, trunc *bool) *planner {
	return &planner{
		segs:    segs,
		stats:   st,
		params:  params,
		dfCache: map[string]*scoreTerm{},
		dictMu:  map[string][]string{},
		budget:  ExpansionLimit,
		trunc:   trunc,
	}
}

// Compile 把 AST 编译为 Plan。
func Compile(n Node, segs []*index.SegmentRef, st rank.Stats, params rank.Params) (*Plan, error) {
	trunc := false
	pl := newPlanner(segs, st, params, &trunc)
	root := pl.toPlan(n)
	return &Plan{root: root, terms: pl.termOrder, truncated: trunc, query: ""}, nil
}

// termOrder 记录词项创建顺序(Compile 中填充)。
// (放在 planner 上,Compile 结束时拷出。)

func (pl *planner) markTrunc() {
	if pl.trunc != nil {
		*pl.trunc = true
	}
}

func fieldsFor(hasField bool, f index.Field) []index.Field {
	if hasField {
		return []index.Field{f}
	}
	out := make([]index.Field, 0, index.NumFields)
	for i := index.Field(0); i < index.NumFields; i++ {
		out = append(out, i)
	}
	return out
}

// toPlan 把 AST 子树转换为 planNode。
func (pl *planner) toPlan(n Node) planNode {
	switch t := n.(type) {
	case *MatchAllNode:
		return allPlan{}
	case *TermNode:
		return pl.planTerm(t)
	case *PhraseNode:
		return pl.planPhrase(t)
	case *FilterNode:
		fc, err := compileFilter(t)
		if err != nil {
			return emptyPlan{}
		}
		return &andPlan{pos: []planNode{allPlan{}}, filters: []*filterCond{fc}}
	case *NotNode:
		prev := pl.inNeg
		pl.inNeg = true
		child := pl.toPlan(t.Child)
		pl.inNeg = prev
		return &notPlan{child: child}
	case *AndNode:
		ap := &andPlan{}
		for _, c := range t.Children {
			switch cn := c.(type) {
			case *NotNode:
				ap.negs = append(ap.negs, pl.toPlan(cn.Child))
			case *FilterNode:
				fc, err := compileFilter(cn)
				if err != nil {
					return emptyPlan{}
				}
				ap.filters = append(ap.filters, fc)
			default:
				ap.pos = append(ap.pos, pl.toPlan(c))
			}
		}
		if len(ap.pos) == 0 {
			ap.pos = append(ap.pos, allPlan{})
		}
		return ap
	case *OrNode:
		op := &orPlan{}
		for _, c := range t.Children {
			op.kids = append(op.kids, pl.toPlan(c))
		}
		return op
	}
	return emptyPlan{}
}

// appendTerm 登记打分词项(NOT 子树内不计分,与引擎得分语义一致)。
func (pl *planner) appendTerm(sc *scoreTerm) {
	if !pl.inNeg {
		pl.termOrder = append(pl.termOrder, sc)
	}
}

// planTerm 处理词项节点:多 token 拆分、前缀/模糊/单字 CJK 展开。
func (pl *planner) planTerm(t *TermNode) planNode {
	fields := fieldsFor(t.HasField, t.Field)
	// 查询侧分析。
	toks := analysis.Analyze(t.Text)
	if len(toks) == 0 {
		return emptyPlan{}
	}
	// 多原子(如 v2.1.3、Go语言并发)→ AND 各部分。
	// CJK 连续串部分直接以其 bigram 序列构建短语;若用拼接 raw 重新
	// 分析会引入错误的相邻二元组(如 语言言并并发 → 语言,言言,…)。
	parts := splitQueryTokens(toks)
	if len(parts) > 1 {
		ap := &andPlan{}
		for _, part := range parts {
			if part.cjkRun && len(part.terms) > 1 && !t.IsPrefix && !t.HasFuzzy {
				pp := &phrasePlan{fields: fields}
				pp.groups = [][]string{part.terms}
				ap.pos = append(ap.pos, pp)
				continue
			}
			sub := &TermNode{Field: t.Field, HasField: t.HasField, Text: part.raw,
				IsPrefix: t.IsPrefix, HasFuzzy: t.HasFuzzy, Fuzzy: t.Fuzzy}
			ap.pos = append(ap.pos, pl.planTerm(sub))
		}
		return ap
	}
	part := parts[0]
	switch {
	case part.singleCJK && !t.IsPrefix && !t.HasFuzzy:
		return pl.planSingleCJK(part.terms[0], fields)
	case t.IsPrefix:
		return pl.planPrefix(part.raw, fields)
	case t.HasFuzzy:
		return pl.planFuzzy(part.raw, t.Fuzzy, fields)
	case part.cjkRun && len(part.terms) > 1:
		// 多字 CJK 查询按二元组序列转短语(位置连续,规格 4.5)。
		pp := &phrasePlan{fields: fields}
		pp.groups = [][]string{part.terms}
		return pp
	default:
		sc := pl.termDf(part.raw, fields)
		pl.appendTerm(sc)
		return &termPlan{sc: sc, fields: fields}
	}
}

// qPart 是查询词项分析出的部分。
type qPart struct {
	raw       string
	terms     []string // singleCJK: 单字;多 CJK bigram 组;普通: 单词
	singleCJK bool
	cjkRun    bool
}

// splitQueryTokens 把分析结果拆成多个独立查询部分:
// 连续 CJK bigram 合为一组(多字 CJK 查询按二元组序列转短语);
// 标识符整体取首 token;多拉丁原子拆分。
func splitQueryTokens(toks []analysis.Token) []qPart {
	var parts []qPart
	var curCJK []string
	flushCJK := func() {
		if len(curCJK) == 0 {
			return
		}
		if len(curCJK) == 1 {
			parts = append(parts, qPart{raw: curCJK[0], terms: []string{curCJK[0]}, singleCJK: true, cjkRun: true})
		} else {
			parts = append(parts, qPart{raw: strings.Join(curCJK, ""), terms: curCJK, cjkRun: true})
		}
		curCJK = nil
	}
	prevPos := -2
	for i := range toks {
		tk := &toks[i]
		switch tk.Kind {
		case analysis.KindCJK:
			if tk.Pos == prevPos+1 {
				curCJK = append(curCJK, tk.Text)
			} else {
				flushCJK()
				curCJK = []string{tk.Text}
			}
			prevPos = tk.Pos
		case analysis.KindWord:
			flushCJK()
			prevPos = -2
			parts = append(parts, qPart{raw: tk.Text, terms: []string{tk.Text}})
		case analysis.KindSub:
			// 跳过子词(整体 token 已在 KindWord)。
			prevPos = -2
		}
	}
	flushCJK()
	return parts
}

// termDf 跨段汇总某词项的 df(各字段求和取最大)并缓存。
func (pl *planner) termDf(text string, fields []index.Field) *scoreTerm {
	key := text + "\x00" + fieldsKey(fields)
	if sc, ok := pl.dfCache[key]; ok {
		return sc
	}
	sc := &scoreTerm{text: text, weight: 1, fields: fields}
	for _, seg := range pl.segs {
		for _, f := range fields {
			te, ok, err := seg.Reader().Term(f, text)
			if err == nil && ok {
				sc.dfByField[f] += uint64(te.DocFreq)
			}
		}
	}
	for f := 0; f < int(index.NumFields); f++ {
		if sc.dfByField[f] > sc.df {
			sc.df = sc.dfByField[f]
		}
	}
	sc.idf = rank.Idf(pl.stats.N, sc.df)
	pl.dfCache[key] = sc
	return sc
}

func fieldsKey(fields []index.Field) string {
	var sb strings.Builder
	for _, f := range fields {
		sb.WriteByte('0' + byte(f))
	}
	return sb.String()
}

// consumeBudget 消耗一个展开名额;超额返回 false 并标记截断。
func (pl *planner) consumeBudget() bool {
	if pl.budget <= 0 {
		pl.markTrunc()
		return false
	}
	pl.budget--
	return true
}

// planPrefix 前缀展开:扫描范围内全部词典,收集前缀匹配词项。
func (pl *planner) planPrefix(prefix string, fields []index.Field) planNode {
	agg := map[string]*scoreTerm{}
	for _, seg := range pl.segs {
		for _, f := range fields {
			_ = seg.Reader().Terms(f, func(term string, te index.TermEntry) bool {
				if !strings.HasPrefix(term, prefix) {
					return true
				}
				sc := agg[term]
				if sc == nil {
					if !pl.consumeBudget() {
						return false
					}
					sc = &scoreTerm{text: term, weight: 1, fields: fields}
					agg[term] = sc
				}
				sc.dfByField[f] += uint64(te.DocFreq)
				return true
			})
		}
	}
	return pl.buildExpansion(prefix, agg, fields)
}

// planFuzzy 模糊展开:按词长自动定级(<=2:0,3–5:1,>=6:2),
// 有序词典 + 共享 DP 行前缀剪枝。
func (pl *planner) planFuzzy(term string, dist int, fields []index.Field) planNode {
	if dist < 0 {
		n := len([]rune(term))
		switch {
		case n <= 2:
			dist = 0
		case n <= 5:
			dist = 1
		default:
			dist = 2
		}
	}
	if dist == 0 {
		sc := pl.termDf(term, fields)
		pl.appendTerm(sc)
		return &termPlan{sc: sc, fields: fields}
	}
	agg := map[string]*scoreTerm{}
	pat := []byte(term)
	patLen := len(pat)
	for _, seg := range pl.segs {
		for _, f := range fields {
			dict := pl.dictionary(seg, f)
			fuzzyCollect(dict, pat, dist, func(word string, d int) {
				if _, ok := agg[word]; ok {
					return
				}
				if !pl.consumeBudget() {
					return
				}
				w := 1.0 - float64(d)/float64(patLen)
				if w < 0.05 {
					w = 0.05
				}
				agg[word] = &scoreTerm{text: word, weight: w, fields: fields}
			})
		}
	}
	// 聚合 df。
	for word := range agg {
		sc := agg[word]
		for _, seg := range pl.segs {
			for _, f := range fields {
				if te, ok, err := seg.Reader().Term(f, word); err == nil && ok {
					sc.dfByField[f] += uint64(te.DocFreq)
				}
			}
		}
	}
	return pl.buildExpansion(term, agg, fields)
}

// planSingleCJK 单字 CJK:展开为词典中所有包含该字的二元组
// (DECISIONS.md D5)。
func (pl *planner) planSingleCJK(ch string, fields []index.Field) planNode {
	agg := map[string]*scoreTerm{}
	for _, seg := range pl.segs {
		for _, f := range fields {
			_ = seg.Reader().Terms(f, func(term string, te index.TermEntry) bool {
				if len(term) != 6 { // 仅二元组(UTF-8 3B×2)
					return true
				}
				if !strings.Contains(term, ch) {
					return true
				}
				sc := agg[term]
				if sc == nil {
					if !pl.consumeBudget() {
						return false
					}
					sc = &scoreTerm{text: term, weight: 1, fields: fields}
					agg[term] = sc
				}
				sc.dfByField[f] += uint64(te.DocFreq)
				return true
			})
		}
	}
	return pl.buildExpansion(ch, agg, fields)
}

// buildExpansion 把聚合的展开词项组装为 OR;空展开 → emptyPlan。
func (pl *planner) buildExpansion(orig string, agg map[string]*scoreTerm, fields []index.Field) planNode {
	if len(agg) == 0 {
		return emptyPlan{}
	}
	terms := make([]*scoreTerm, 0, len(agg))
	for _, sc := range agg {
		for f := 0; f < int(index.NumFields); f++ {
			if sc.dfByField[f] > sc.df {
				sc.df = sc.dfByField[f]
			}
		}
		sc.idf = rank.Idf(pl.stats.N, sc.df)
		terms = append(terms, sc)
	}
	sort.Slice(terms, func(i, j int) bool { return terms[i].text < terms[j].text })
	for _, sc := range terms {
		pl.appendTerm(sc)
	}
	op := &orPlan{}
	for _, sc := range terms {
		op.kids = append(op.kids, &termPlan{sc: sc, fields: fields})
	}
	return op
}

// planPhrase 短语:分析原文 → 位置连续组;单字 CJK 组退化为词项。
func (pl *planner) planPhrase(t *PhraseNode) planNode {
	fields := fieldsFor(t.HasField, t.Field)
	toks := analysis.Analyze(t.Raw)
	if len(toks) == 0 {
		return emptyPlan{}
	}
	// 组:按 Pos 连续切分(仅 KindWord/KindCJK)。
	var groups [][]string
	var cur []string
	prevPos := -2
	for i := range toks {
		tk := &toks[i]
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
	if len(groups) == 0 {
		return emptyPlan{}
	}
	pp := &phrasePlan{fields: fields, slop: t.Slop, hasSlop: t.HasSlop}
	for _, g := range groups {
		pp.groups = append(pp.groups, g)
	}
	return pp
}

// dictionary 取(段,字段)的有序词典,带缓存。
func (pl *planner) dictionary(seg *index.SegmentRef, f index.Field) []string {
	key := seg.ID() + "|" + f.Name()
	if d, ok := pl.dictMu[key]; ok {
		return d
	}
	var d []string
	_ = seg.Reader().Terms(f, func(term string, _ index.TermEntry) bool {
		d = append(d, term)
		return true
	})
	pl.dictMu[key] = d
	return d
}

// fuzzyCollect 在有序词典中收集与 pattern 编辑距离 <= k 的词项。
// 共享 DP 行 + 前缀剪枝:某行最小值 > k 时整棵前缀子树剪枝
// (规格 6)。距离按 UTF-8 字节计算(DECISIONS.md D14)。
func fuzzyCollect(dict []string, pattern []byte, k int, fn func(word string, dist int)) {
	base := make([]int, len(pattern)+1)
	for j := range base {
		base[j] = j
	}
	var rec func(lo, hi, depth int, row []int)
	rec = func(lo, hi, depth int, row []int) {
		if lo >= hi || rowMin(row) > k {
			return
		}
		i := lo
		for i < hi {
			w := dict[i]
			if depth >= len(w) {
				// 词项在当前深度前结束:距离 = row[len(pattern)]。
				if len(w) >= depth { // 理论上 depth==len(w)
					if d := row[len(pattern)]; d <= k {
						fn(w, d)
					}
				}
				i++
				continue
			}
			b := w[depth]
			j := i
			for j < hi && depth < len(dict[j]) && dict[j][depth] == b {
				j++
			}
			nrow := dpRow(row, b, pattern)
			if rowMin(nrow) <= k {
				// 恰好在该深度结束的词项。
				for x := i; x < j; x++ {
					if len(dict[x]) == depth+1 {
						if d := nrow[len(pattern)]; d <= k {
							fn(dict[x], d)
						}
					}
				}
				rec(i, j, depth+1, nrow)
			}
			i = j
		}
	}
	rec(0, len(dict), 0, base)
}

func dpRow(prev []int, b byte, pattern []byte) []int {
	next := make([]int, len(prev))
	next[0] = prev[0] + 1
	for j := 0; j < len(pattern); j++ {
		cost := 1
		if pattern[j] == b {
			cost = 0
		}
		m := prev[j+1] + 1
		if v := next[j] + 1; v < m {
			m = v
		}
		if v := prev[j] + cost; v < m {
			m = v
		}
		next[j+1] = m
	}
	return next
}

func rowMin(row []int) int {
	m := row[0]
	for _, v := range row[1:] {
		if v < m {
			m = v
		}
	}
	return m
}
