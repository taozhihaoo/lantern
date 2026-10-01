package query_test

import (
	"testing"

	"lantern/internal/index"
	"lantern/internal/query"
	"lantern/internal/rank"
	"lantern/internal/testutil"
)

// buildCriteriaIndex 构建第 14 节标准 4/5 的验证语料。
func buildCriteriaIndex(t *testing.T) *criteriaEnv {
	docs := []testutil.Doc{
		{Path: "a.md", Title: "A", Body: "全文搜索引擎设计与实现", Ext: "md", Size: 30, MTime: 1700000000},
		{Path: "b.md", Title: "B", Body: "Go语言并发编程实战", Ext: "md", Size: 28, MTime: 1700000001},
		{Path: "c.md", Title: "C", Body: "we call parseHTTPRequest here", Ext: "md", Size: 29, MTime: 1700000002},
		{Path: "d.md", Title: "D", Body: "snake_case_name style guide", Ext: "md", Size: 27, MTime: 1700000003},
	}
	dir := t.TempDir()
	ix := buildDiffIndex(t, dir, docs, 100, false)
	snap, err := ix.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	s := query.NewSearcher(snap, rank.DefaultParams())
	s.SetRecency(0, 0, 1800000000)
	_ = snap
	return &criteriaEnv{s: s}
}

type criteriaEnv struct {
	s    *query.Searcher
	snap *index.Snapshot
}

func TestCriteriaCJKAndIdentifiers(t *testing.T) {
	env := buildCriteriaIndex(t)
	cases := []struct {
		q     string
		want  string // 必须命中的路径
		empty bool   // 必须无命中
	}{
		{q: "搜索", want: "a.md"},
		{q: "引擎", want: "a.md"},
		{q: "擎", want: "a.md"},      // 单字前缀(包含)匹配二元组词典
		{q: "Go语言并发", want: "b.md"}, // 中英混排
		{q: "\"http request\"", want: "c.md"},
		{q: "case", want: "d.md"},             // snake 子词单独检索
		{q: "snake_case_name", want: "d.md"},  // 多原子 AND
		{q: "parsehttprequest", want: "c.md"}, // 标识符整体(小写化)
		{q: "title:A", want: "a.md"},
		{q: "搜索引擎", want: "a.md"}, // CJK 多字查询转二元组短语
		{q: "编程实战", want: "b.md"},
		{q: "不存在的词", empty: true},
	}
	for _, tc := range cases {
		res, err := env.s.Search(tc.q, 10, false)
		if err != nil {
			t.Fatalf("%q: %v", tc.q, err)
		}
		if tc.empty {
			if res.Total != 0 {
				t.Fatalf("%q: want empty, got %d hits", tc.q, res.Total)
			}
			continue
		}
		found := false
		for _, h := range res.Hits {
			if h.Path == tc.want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%q: missing %s (got %v)", tc.q, tc.want, hitPaths(res))
		}
	}
	_ = env
}
