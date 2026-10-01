package query

import (
	"container/heap"
	"sort"

	"lantern/internal/index"
	"lantern/internal/rank"
)

// Hit 是一条搜索结果。
type Hit struct {
	SegID   string        `json:"-"`
	Doc     uint32        `json:"-"`
	Path    string        `json:"path"`
	Title   string        `json:"title"`
	Score   float64       `json:"score"`
	Explain *rank.Explain `json:"explain,omitempty"`
}

// Result 是一次搜索的完整结果。
type Result struct {
	Hits      []Hit  `json:"hits"`
	Total     uint64 `json:"total"`     // 全部匹配文档数(含未进入 Top-K)
	Truncated bool   `json:"truncated"` // 展开(前缀/单字)超限
	Query     string `json:"query"`
}

// topEntry 是 Top-K 堆元素。
type topEntry struct {
	path  string
	score float64
	seg   string
	doc   uint32
	title string
}

// topK 是大小为 K 的小根堆(根 = 当前最差结果);
// 并列得分时路径字典序大者视为更差,保证输出确定(规格 7.4)。
type topK struct {
	k    int
	data []topEntry
}

func (h *topK) Len() int { return len(h.data) }
func (h *topK) Less(i, j int) bool {
	if h.data[i].score != h.data[j].score {
		return h.data[i].score < h.data[j].score // 小根:最差在根
	}
	return h.data[i].path > h.data[j].path // 同分:路径大者更差
}
func (h *topK) Swap(i, j int) { h.data[i], h.data[j] = h.data[j], h.data[i] }
func (h *topK) Push(x any)    { h.data = append(h.data, x.(topEntry)) }
func (h *topK) Pop() any {
	old := h.data
	n := len(old)
	x := old[n-1]
	h.data = old[:n-1]
	return x
}

func entryBetter(a, worst topEntry) bool {
	if a.score != worst.score {
		return a.score > worst.score
	}
	return a.path < worst.path
}

func (h *topK) offer(e topEntry) {
	if h.k <= 0 {
		return
	}
	if len(h.data) < h.k {
		heap.Push(h, e)
		return
	}
	if h.Len() > 0 && entryBetter(e, h.data[0]) {
		heap.Pop(h)
		heap.Push(h, e)
	}
}

// Searcher 在一个快照上执行查询。
type Searcher struct {
	snap   *index.Snapshot
	segs   []*index.SegmentRef
	stats  rank.Stats
	params rank.Params
	// recency 加成(默认关闭):score *= 1 + r·0.5^(ageDays/halfLifeDays)。
	recencyR        float64
	recencyHalfLife float64
	recencyNow      int64 // unix 秒
}

// NewSearcher 创建搜索器并跨段汇总统计(规格 7.1)。
// N 与字段统计包含已删除但未合并的文档。
func NewSearcher(snap *index.Snapshot, params rank.Params) *Searcher {
	s := &Searcher{snap: snap, segs: snap.Segments(), params: params}
	var n uint64
	var totalLen [index.NumFields]float64
	var docCnt [index.NumFields]float64
	for _, seg := range s.segs {
		r := seg.Reader()
		n += uint64(r.DocCount())
		fs := r.FieldStats()
		for f := 0; f < int(index.NumFields); f++ {
			totalLen[f] += float64(fs[f].TotalLen)
			docCnt[f] += float64(fs[f].DocCount)
		}
	}
	s.stats.N = n
	for f := 0; f < int(index.NumFields); f++ {
		s.stats.Fields[f] = rank.FieldStats{TotalLen: totalLen[f], DocCount: docCnt[f]}
	}
	return s
}

// SetRecency 启用 recency 加成(r=0 关闭)。
func (s *Searcher) SetRecency(r, halfLifeDays float64, nowUnix int64) {
	s.recencyR = r
	s.recencyHalfLife = halfLifeDays
	s.recencyNow = nowUnix
}

// avgLens 返回全局平均字段长度。
func (s *Searcher) avgLens() [index.NumFields]float64 {
	var out [index.NumFields]float64
	for f := 0; f < int(index.NumFields); f++ {
		out[f] = s.stats.Fields[f].AvgLen()
	}
	return out
}

// Search 执行查询:Top-K + 总命中数。
//
// 匹配由迭代器树完成(leapfrog/归并/位置交集);打分与匹配分离:
// 每个匹配文档的得分 = Σ 打分词项贡献 × recency 加成(NOT 子树、
// 短语与过滤不贡献得分,与规格 7.2 一致)。
func (s *Searcher) Search(q string, k int, wantExplain bool) (*Result, error) {
	node, err := Parse(q)
	if err != nil {
		return nil, err
	}
	trunc := false
	pl := newPlanner(s.segs, s.stats, s.params, &trunc)
	root := pl.toPlan(node)
	plan := &Plan{root: root, terms: pl.termOrder, truncated: trunc, query: q}

	avg := s.avgLens()
	tk := &topK{k: k}
	var total uint64
	for _, seg := range s.segs {
		se := &segEnv{r: seg.Reader(), params: s.params, avg: avg}
		it := plan.root.build(se)
		if it == nil {
			continue
		}
		for it.Next() {
			total++
			score := s.scoreDoc(plan, seg, it.DocID())
			path, title := "", ""
			if sd, err := seg.Reader().StoredDoc(it.DocID()); err == nil {
				path = sd.Path
				title = sd.Title
			}
			tk.offer(topEntry{path: path, score: score, seg: seg.ID(), doc: it.DocID(), title: title})
		}
	}
	final := make([]topEntry, len(tk.data))
	copy(final, tk.data)
	sort.SliceStable(final, func(i, j int) bool {
		if final[i].score != final[j].score {
			return final[i].score > final[j].score
		}
		return final[i].path < final[j].path
	})
	res := &Result{Total: total, Truncated: trunc, Query: q}
	for i := range final {
		h := Hit{SegID: final[i].seg, Doc: final[i].doc, Path: final[i].path,
			Title: final[i].title, Score: final[i].score}
		if wantExplain {
			_, h.Explain = s.scoreDocExplain(plan, s.segByID(final[i].seg), final[i].doc)
		}
		res.Hits = append(res.Hits, h)
	}
	return res, nil
}

func (s *Searcher) segByID(id string) *index.SegmentRef {
	for _, seg := range s.segs {
		if seg.ID() == id {
			return seg
		}
	}
	return nil
}

// termContrib 是递归求值产出的单词条贡献。
type termContrib struct {
	sc  *scoreTerm
	val float64
}

// evalNode 对 plan 树在单文档上递归求值,语义与迭代器树一致:
// AND = 全部命中且 Σ 子树分;OR = 命中子树分之和;NOT/短语/过滤 = 0 分;
// 词项分 = weight·idf·tfw/(k1+tfw)(tfw 先跨字段求和)。
// 只有真正命中的子树才产出贡献,保证 Explain 求和与总分严格一致。
func (s *Searcher) evalNode(p planNode, r *index.SegmentReader, doc uint32) (bool, float64, []termContrib) {
	switch t := p.(type) {
	case emptyPlan:
		return false, 0, nil
	case allPlan:
		return true, 0, nil
	case *termPlan:
		var tf, ln [index.NumFields]float64
		any := false
		for _, f := range t.sc.fields {
			it, err := r.Postings(f, t.sc.text)
			if err != nil || it == nil {
				continue
			}
			if !it.Advance(doc) || it.DocID() != doc {
				continue
			}
			ln2 := uint32(0)
			if n, nerr := r.Norm(doc, f); nerr == nil {
				ln2 = n
			}
			tf[f] = float64(it.TF())
			ln[f] = float64(ln2)
			any = true
		}
		if !any {
			return false, 0, nil
		}
		tfw := rank.TFW(s.params, tf, ln, s.avgLens())
		if tfw <= 0 {
			return false, 0, nil
		}
		score := rank.TermScore(s.params, t.sc.idf, tfw, t.sc.weight)
		return true, score, []termContrib{{sc: t.sc, val: score}}
	case *orPlan:
		any := false
		total := 0.0
		var contribs []termContrib
		for _, k := range t.kids {
			m, sc2, c := s.evalNode(k, r, doc)
			if m {
				any = true
				total += sc2
				contribs = append(contribs, c...)
			}
		}
		return any, total, contribs
	case *andPlan:
		total := 0.0
		var contribs []termContrib
		for _, k := range t.pos {
			m, sc2, c := s.evalNode(k, r, doc)
			if !m {
				return false, 0, nil
			}
			total += sc2
			contribs = append(contribs, c...)
		}
		for _, k := range t.negs {
			if m, _, _ := s.evalNode(k, r, doc); m {
				return false, 0, nil
			}
		}
		for _, fc := range t.filters {
			if !fc.match(r, doc) {
				return false, 0, nil
			}
		}
		return true, total, contribs
	case *notPlan:
		if m, _, _ := s.evalNode(t.child, r, doc); m {
			return false, 0, nil
		}
		return true, 0, nil
	case *phrasePlan:
		return s.matchPhrasePlan(t, r, doc), 0, nil
	}
	return false, 0, nil
}

// matchPhrasePlan 短语/邻近的单文档判定(组间 AND,字段间 OR)。
func (s *Searcher) matchPhrasePlan(p *phrasePlan, r *index.SegmentReader, doc uint32) bool {
	for _, f := range p.fields {
		allGroups := true
		for _, g := range p.groups {
			var posLists [][]uint32
			ok := true
			for _, term := range g {
				it, err := r.Postings(f, term)
				if err != nil || it == nil {
					ok = false
					break
				}
				if !it.Advance(doc) || it.DocID() != doc {
					ok = false
					break
				}
				posLists = append(posLists, append([]uint32(nil), it.Positions()...))
			}
			if !ok || !groupMatches(posLists, p.slopFor()) {
				allGroups = false
				break
			}
		}
		if allGroups {
			return true
		}
	}
	return false
}

// groupMatches 判定一组词项的位置是否满足短语(Slop=0 顺序相邻)
// 或邻近(Slop>0 任意顺序窗口)。
func groupMatches(posLists [][]uint32, slop int) bool {
	if len(posLists) <= 1 {
		return len(posLists) == 1 && len(posLists[0]) > 0
	}
	if slop <= 0 {
		for _, anchor := range posLists[0] {
			ok := true
			for i := 1; i < len(posLists); i++ {
				if !posContains(posLists[i], anchor+uint32(i)) {
					ok = false
					break
				}
			}
			if ok {
				return true
			}
		}
		return false
	}
	limit := uint32(slop) + uint32(len(posLists)) - 1
	type pt struct {
		pos  uint32
		term int
	}
	var all []pt
	for i, pl := range posLists {
		for _, pos := range pl {
			all = append(all, pt{pos, i})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].pos < all[j].pos })
	need := make([]int, len(posLists))
	missing := len(posLists)
	best := uint32(1<<31 - 1)
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

// scoreDoc 计算单个文档最终得分(匹配树已确认命中)。
func (s *Searcher) scoreDoc(plan *Plan, seg *index.SegmentRef, doc uint32) float64 {
	r := seg.Reader()
	_, score, _ := s.evalNode(plan.root, r, doc)
	boost := s.boostOf(r, doc)
	return score * boost
}

// scoreDocExplain 计算得分并输出拆解:贡献 = raw·boost,
// 按 plan.terms 顺序求和,Sum 与 FinalScore 严格一致。
func (s *Searcher) scoreDocExplain(plan *Plan, seg *index.SegmentRef, doc uint32) (float64, *rank.Explain) {
	r := seg.Reader()
	matched, _, contribs := s.evalNode(plan.root, r, doc)
	if !matched {
		return 0, nil
	}
	boost := s.boostOf(r, doc)
	byTerm := map[*scoreTerm]float64{}
	sum := 0.0
	for _, c := range contribs {
		v := c.val * boost
		byTerm[c.sc] += v
		sum += v
	}
	ex := &rank.Explain{N: s.stats.N, Boost: boost, Truncated: plan.truncated}
	for _, sc := range plan.terms {
		te := rank.TermExplain{
			Text: sc.text, Weight: sc.weight, DF: sc.df, IDF: sc.idf, DFIsMax: true,
			Contribution: byTerm[sc],
		}
		ex.Terms = append(ex.Terms, te)
	}
	ex.FinalScore = sum
	return sum, ex
}

func (s *Searcher) boostOf(r *index.SegmentReader, doc uint32) float64 {
	if dv, err := r.DocValues(doc); err == nil {
		return rank.RecencyBoost(s.recencyR, s.recencyHalfLife,
			float64(s.recencyNow-dv.MTime)/86400.0)
	}
	return 1
}
