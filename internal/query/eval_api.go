package query

// EvaluateClause 对单个文档求值一个查询(诊断用):
// 返回该文档是否命中与得分(与搜索打分同语义,不含 recency)。
func (s *Searcher) EvaluateClause(n Node, segID string, doc uint32) (bool, float64) {
	seg := s.segByID(segID)
	if seg == nil {
		return false, 0
	}
	trunc := false
	pl := newPlanner(s.segs, s.stats, s.params, &trunc)
	plan := pl.toPlan(n)
	m, score, _ := s.evalNode(plan, seg.Reader(), doc)
	return m, score
}

// RankOf 返回目标路径在查询全部结果中的排名(1-based,按得分降序、
// 路径升序)与总命中数。未命中时 found=false。
func (s *Searcher) RankOf(q string, targetPath string) (found bool, rank int, total uint64, score float64) {
	node, err := Parse(q)
	if err != nil {
		return false, 0, 0, 0
	}
	trunc := false
	pl := newPlanner(s.segs, s.stats, s.params, &trunc)
	plan := &Plan{root: pl.toPlan(node), terms: pl.termOrder, truncated: trunc, query: q}
	avg := s.avgLens()
	type cand struct {
		path  string
		score float64
	}
	var target *cand
	var all []cand
	for _, seg := range s.segs {
		se := &segEnv{r: seg.Reader(), params: s.params, avg: avg}
		it := plan.root.build(se)
		if it == nil {
			continue
		}
		for it.Next() {
			total++
			sc := s.scoreDoc(plan, seg, it.DocID())
			path := ""
			if sd, err := seg.Reader().StoredDoc(it.DocID()); err == nil {
				path = sd.Path
			}
			if path == targetPath {
				target = &cand{path: path, score: sc}
			}
			all = append(all, cand{path: path, score: sc})
		}
	}
	if target == nil {
		return false, 0, total, 0
	}
	// 排名 = 1 + 得分更高者数 + (同分中路径更小者数)。
	r := 1
	for _, c := range all {
		if c.path == targetPath {
			continue
		}
		if c.score > target.score || (c.score == target.score && c.path < target.path) {
			r++
		}
	}
	return true, r, total, target.score
}
