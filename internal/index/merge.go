package index

import (
	"fmt"
	"sort"
)

// mergeTierBytes 返回段的总字节大小(由 manifest 校验和累计)。
func mergeTierBytes(sr *SegmentRef) int64 {
	var n int64
	for _, fc := range sr.rec.Files {
		n += fc.Size
	}
	return n
}

// pickMergeTier 实现分层合并策略(规格 5.9):按 8 为底的数量级分层
// (tier = floor(log8(bytes))),某层内段数 ≥ 8 时合并该层全部段;
// 多层同时满足时优先合并最小层。不满足时返回 nil。
func pickMergeTier(segs []*SegmentRef) []*SegmentRef {
	if len(segs) < 8 {
		return nil
	}
	tiers := map[int][]*SegmentRef{}
	for _, sr := range segs {
		b := mergeTierBytes(sr)
		tier := 0
		for b >= 8*1024 {
			b /= 8
			tier++
		}
		tiers[tier] = append(tiers[tier], sr)
	}
	var tierIDs []int
	for t := range tiers {
		tierIDs = append(tierIDs, t)
	}
	sort.Ints(tierIDs)
	for _, t := range tierIDs {
		if len(tiers[t]) >= 8 {
			return tiers[t]
		}
	}
	// 无单层满足但段总量持续增长:合并最小的 8 个段防止无限增长。
	all := make([]*SegmentRef, len(segs))
	copy(all, segs)
	sort.Slice(all, func(i, j int) bool {
		return mergeTierBytes(all[i]) < mergeTierBytes(all[j])
	})
	return all[:8]
}

// mergeLocked 把 victims 合并为一个新段(丢弃墓碑文档,规格 5.9)。
// 调用方持有 w.cm。文档按路径排序后重放分析,保证 docID 分配确定。
func (w *Writer) mergeLocked(victims []*SegmentRef) error {
	if len(victims) < 2 {
		return nil
	}
	type liveDoc struct {
		path string
		doc  Doc
	}
	var docs []liveDoc
	storeBody := false
	victimIDs := map[string]bool{}
	for _, sr := range victims {
		victimIDs[sr.rec.ID] = true
	}
	// 任一来源段存正文,则合并段也存(逐文档仍受 1MB 上限约束)。
	for _, sr := range victims {
		if sr.Reader().StoreAll() {
			storeBody = true
			break
		}
	}
	for _, sr := range victims {
		r := sr.Reader()
		for i := uint32(0); i < r.DocCount(); i++ {
			if !r.IsAlive(i) {
				continue
			}
			sd, err := r.StoredDoc(i)
			if err != nil {
				return fmt.Errorf("index: merge read doc: %w", err)
			}
			dv, err := r.DocValues(i)
			if err != nil {
				return fmt.Errorf("index: merge read dv: %w", err)
			}
			docs = append(docs, liveDoc{
				path: sd.Path,
				doc: Doc{
					Path:      sd.Path,
					Title:     sd.Title,
					Body:      sd.Body,
					Ext:       dv.Ext,
					Size:      dv.Size,
					MTime:     dv.MTime,
					SHA256:    sd.SHA256,
					StoreBody: storeBody,
				},
			})
		}
	}
	if len(docs) == 0 {
		// 全部为墓碑:直接移除这些段,不产生新段。
		plan := commitPlan{removeSegs: victimIDs}
		return w.ix.commit(plan)
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].path < docs[j].path })
	mt := NewMemTable()
	for _, d := range docs {
		mt.Add(d.doc)
	}
	plan := commitPlan{
		newSegs:    []*pendingSegment{{mt: mt, storeBody: storeBody}},
		removeSegs: victimIDs,
	}
	return w.ix.commit(plan)
}
