package query_test

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"testing"

	"lantern/internal/fsx"
	"lantern/internal/index"
	"lantern/internal/query"
	"lantern/internal/rank"
	"lantern/internal/testutil"
)

func shaOf(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// buildDiffIndex 用语料构建索引(可控制分段与合并)。
func buildDiffIndex(t *testing.T, dir string, docs []testutil.Doc, flushEvery int, compact bool) *index.Index {
	t.Helper()
	ix, err := index.Open(dir, fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	for i := range docs {
		d := docs[i]
		if err := w.AddDoc(index.Doc{
			Path: d.Path, Title: d.Title, Body: d.Body, Ext: d.Ext,
			Size: d.Size, MTime: d.MTime,
			SHA256:    shaOf(d.Body),
			StoreBody: true,
		}); err != nil {
			t.Fatal(err)
		}
		if (i+1)%flushEvery == 0 {
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if compact {
		if err := w.Compact(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return ix
}

// TestDifferential 布尔/短语/前缀/模糊/字段/过滤查询:
// 引擎结果集(路径序列)与暴力扫描必须完全一致,Total 一致(规格 11.2)。
func TestDifferential(t *testing.T) {
	const numQueries = 40
	for seed := 1; seed <= 5; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
			docs := testutil.RandomCorpus(seed, 200)
			brute := testutil.NewBrute(docs)
			for _, cfg := range []struct {
				flush   int
				compact bool
				label   string
			}{
				{200, false, "single-seg"},
				{7, false, "multi-seg"},
				{7, true, "merged"},
			} {
				t.Run(cfg.label, func(t *testing.T) {
					dir := t.TempDir()
					ix := buildDiffIndex(t, dir, docs, cfg.flush, cfg.compact)
					snap, err := ix.Snapshot()
					if err != nil {
						t.Fatal(err)
					}
					defer snap.Close()
					s := query.NewSearcher(snap, rank.DefaultParams())
					s.SetRecency(0, 0, 1800000000) // 显式关闭加成
					rng := rand.New(rand.NewSource(int64(seed * 1000)))
					for qn := 0; qn < numQueries; qn++ {
						q := testutil.RandomQuery(rng, docs)
						res, err := s.Search(q, 10, false)
						if err != nil {
							t.Fatalf("query %q: engine error: %v", q, err)
						}
						want, wantTotal, err := brute.Search(q, 10)
						if err != nil {
							t.Fatalf("query %q: brute error: %v", q, err)
						}
						if res.Truncated {
							t.Fatalf("query %q: unexpected truncation at this corpus size", q)
						}
						if res.Total != uint64(wantTotal) {
							t.Fatalf("query %q: total = %d, want %d", q, res.Total, wantTotal)
						}
						if len(res.Hits) != len(want) {
							t.Fatalf("query %q: hits = %d, want %d\nengine: %v\nbrute: %v",
								q, len(res.Hits), len(want), hitPaths(res), brutePaths(want))
						}
						for i := range res.Hits {
							if res.Hits[i].Path != want[i].Path {
								t.Fatalf("query %q: hit[%d] = %s, want %s\nengine: %v\nbrute: %v",
									q, i, res.Hits[i].Path, want[i].Path, hitPaths(res), brutePaths(want))
							}
						}
					}
				})
			}
		})
	}
}

func hitPaths(r *query.Result) []string {
	out := make([]string, len(r.Hits))
	for i := range r.Hits {
		out[i] = r.Hits[i].Path
	}
	return out
}

func brutePaths(r []testutil.BruteResult) []string {
	out := make([]string, len(r))
	for i := range r {
		out[i] = r[i].Path
	}
	return out
}

// TestDifferentialScoring Top-K 分数与暴力 BM25F 比对,误差 < 1e-9。
func TestDifferentialScoring(t *testing.T) {
	for seed := 1; seed <= 3; seed++ {
		docs := testutil.RandomCorpus(seed+100, 150)
		brute := testutil.NewBrute(docs)
		dir := t.TempDir()
		ix := buildDiffIndex(t, dir, docs, 13, false)
		snap, _ := ix.Snapshot()
		s := query.NewSearcher(snap, rank.DefaultParams())
		s.SetRecency(0, 0, 1800000000)
		rng := rand.New(rand.NewSource(int64(seed * 7)))
		for qn := 0; qn < 30; qn++ {
			q := testutil.RandomQuery(rng, docs)
			res, err := s.Search(q, 10, false)
			if err != nil {
				t.Fatalf("query %q: %v", q, err)
			}
			want, _, err := brute.Search(q, 10)
			if err != nil {
				t.Fatalf("query %q: brute: %v", q, err)
			}
			if len(res.Hits) != len(want) {
				t.Fatalf("query %q: hit count mismatch", q)
			}
			for i := range res.Hits {
				if res.Hits[i].Path != want[i].Path {
					t.Fatalf("query %q: hit[%d] path %s != %s", q, i, res.Hits[i].Path, want[i].Path)
				}
				if diff := res.Hits[i].Score - want[i].Score; diff < -1e-9 || diff > 1e-9 {
					t.Fatalf("query %q: hit[%d] score %.15f != %.15f (diff %g)",
						q, i, res.Hits[i].Score, want[i].Score, diff)
				}
			}
		}
		snap.Close()
	}
}

// FuzzLexer 解析任意输入不 panic。
func FuzzLexer(f *testing.F) {
	f.Add("foo AND bar")
	f.Add("(a OR b) AND -c")
	f.Add("\"phrase one\"~2 title:x* ext:go size:>1mb mtime:2025-06")
	f.Add("((unclosed")
	f.Add("a:b:c ~ ~~ **")
	f.Fuzz(func(t *testing.T, s string) {
		node, err := query.Parse(s)
		if err != nil {
			if _, ok := err.(*query.SyntaxError); !ok {
				t.Fatalf("non-syntax error: %v", err)
			}
			return
		}
		if node == nil {
			t.Fatal("nil node without error")
		}
	})
}

func testutilCorpus305() []testutil.Doc { return testutil.RandomCorpus(305, 150) }

func rankDefault() rank.Params { return rank.DefaultParams() }

func newSearcherOf(snap *index.Snapshot) *query.Searcher {
	s := query.NewSearcher(snap, rank.DefaultParams())
	s.SetRecency(0, 0, 1800000000)
	return s
}

type indexTermEntry = index.TermEntry
