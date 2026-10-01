package query_test

import (
	"testing"

	"lantern/internal/query"
	"lantern/internal/rank"
	"lantern/internal/testutil"
)

func recencyDocs() []testutil.Doc {
	return []testutil.Doc{
		{Path: "old.md", Title: "release", Body: "release notes content here", Ext: "md",
			Size: 26, MTime: 1704067200}, // 2024-01-01
		{Path: "new.md", Title: "release", Body: "release notes content here", Ext: "md",
			Size: 26, MTime: 1800000000}, // 更新
	}
}

// 7.2:recency 加成 score *= 1 + r·0.5^(age/halfLife)。
func TestRecencyBoostReorders(t *testing.T) {
	docs := recencyDocs()
	dir := t.TempDir()
	ix := buildDiffIndex(t, dir, docs, 100, false)
	snap, _ := ix.Snapshot()

	// 关闭:同分,按路径排序(old < new)。
	s1 := query.NewSearcher(snap, rank.DefaultParams())
	s1.SetRecency(0, 0, 1800000000)
	r1, _ := s1.Search("release", 10, false)
	if len(r1.Hits) != 2 || r1.Hits[0].Path != "new.md" || r1.Hits[1].Path != "old.md" {
		t.Fatalf("no-recency order: %v", hitPaths(r1))
	}
	if r1.Hits[0].Score != r1.Hits[1].Score {
		t.Fatal("identical docs must score equally without recency")
	}

	// 开启:新文档排前且分数更高。
	s2 := query.NewSearcher(snap, rank.DefaultParams())
	s2.SetRecency(0.5, 30, 1800000100)
	r2, _ := s2.Search("release", 10, true)
	if len(r2.Hits) != 2 || r2.Hits[0].Path != "new.md" {
		t.Fatalf("recency order: %v", hitPaths(r2))
	}
	if r2.Hits[0].Score <= r2.Hits[1].Score {
		t.Fatalf("newer doc must score higher: %v vs %v", r2.Hits[0].Score, r2.Hits[1].Score)
	}
	// 7.3:Explain 的 boost 因子与求和一致。
	ex := r2.Hits[0].Explain
	if ex == nil || ex.Boost <= 1 {
		t.Fatalf("explain boost missing: %+v", ex)
	}
}

// 7.3(硬性):各项贡献之和与最终得分之差 < 1e-9。
func TestExplainSumEqualsFinal(t *testing.T) {
	docs := testutil.RandomCorpus(42, 120)
	dir := t.TempDir()
	ix := buildDiffIndex(t, dir, docs, 9, false)
	snap, _ := ix.Snapshot()
	s := query.NewSearcher(snap, rank.DefaultParams())
	s.SetRecency(0.6, 45, 1800000000) // 开启加成也必须一致
	queries := []string{
		"search engine",
		"search OR index",
		"title:search AND body:engine",
		"\"search engine\"~3",
		"sear*",
		"search~1",
		"ext:md AND search",
		"-missing search",
	}
	for _, q := range queries {
		res, err := s.Search(q, 5, true)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		for i, h := range res.Hits {
			if h.Explain == nil {
				t.Fatalf("%q hit %d: no explain", q, i)
			}
			diff := h.Explain.Sum() - h.Explain.FinalScore
			if diff < -1e-9 || diff > 1e-9 {
				t.Fatalf("%q hit %d: Σ贡献 %.18f != Final %.18f (diff %g)",
					q, i, h.Explain.Sum(), h.Explain.FinalScore, diff)
			}
			d2 := h.Explain.FinalScore - h.Score
			if d2 < -1e-9 || d2 > 1e-9 {
				t.Fatalf("%q hit %d: explain final %.18f != hit score %.18f",
					q, i, h.Explain.FinalScore, h.Score)
			}
		}
	}
}
