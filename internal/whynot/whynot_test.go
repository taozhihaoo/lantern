package whynot

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lantern/internal/fsx"
	"lantern/internal/index"
	"lantern/internal/query"
	"lantern/internal/rank"
	"lantern/internal/scan"
)

var updateGolden = flag.Bool("update", false, "重写 golden 文件")

// setupFixture 构建固定夹具:索引目录 + 磁盘文件 + 已索引文档。
// 所有文件 mtime 固定,保证 golden 确定性。
func setupFixture(t *testing.T) (*index.Index, *query.Searcher, Config) {
	t.Helper()
	base := t.TempDir()
	fixTime := func(p string) {
		st := time.Unix(1700000000, 0)
		if err := os.Chtimes(p, st, st); err != nil {
			t.Fatal(err)
		}
	}
	fix := func(rel, content string) {
		p := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		fixTime(p)
	}

	fix(".gitignore", "*.log\n")
	fix("normal.md", "release notes for 全文搜索引擎设计 alpha tail")
	fix("skip.log", "ignored content keyword")
	fix("big.md", "this file is deliberately much longer than the configured size limit for the test fixture, well over one hundred bytes")
	fix("bin.dat", "text\x00with nul byte")
	fix("gbk.txt", string([]byte{0xC4, 0xE3, 0xBA, 0xC3}))
	fix("late.md", "stale content original")

	ix, err := index.Open(filepath.Join(base, ".lantern"), fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// 扫描(与 CLI 相同流程);MaxSize=50 使 big.md 被跳过。
	outs, _, sum, err := scan.Run(scan.Options{
		Base: base, Targets: []string{"."}, Workers: 1, MaxSize: 100,
		State:     func(string) (scan.StateEntry, bool) { return scan.StateEntry{}, false },
		StateList: func() []string { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range outs {
		if err := w.AddDoc(d.Doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if sum.Added != 3 { // normal.md + late.md + .gitignore(big.md 超限被跳过)
		t.Fatalf("fixture indexed %d docs, want 3 (skipped=%v)", sum.Added, sum.Skipped)
	}

	// 索引后:notindexed.md 出现在磁盘;late.md 内容变化(仍固定 mtime)。
	fix("notindexed.md", "never scanned word")
	fix("late.md", "stale content CHANGED after indexing")

	snap, err := ix.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snap.Close() })
	s := query.NewSearcher(snap, rank.DefaultParams())
	s.SetRecency(0, 0, 1800000000)
	cfg := Config{Base: base, MaxSize: 100, Fsys: fsx.OsFS()}
	return ix, s, cfg
}

// TestGolden 覆盖第 8 节每种原因(规格 8/11.5)。
func TestGolden(t *testing.T) {
	ix, s, cfg := setupFixture(t)
	cases := []struct {
		name  string
		query string
		path  string
	}{
		{"ignored", "搜索", "skip.log"},
		{"too_large", "搜索", "big.md"},
		{"binary", "搜索", "bin.dat"},
		{"non_utf8", "搜索", "gbk.txt"},
		{"not_scanned", "搜索", "notindexed.md"},
		{"outside", "搜索", "../outside.md"},
		{"stale", "CHANGED", "late.md"},
		{"term_missing_nearest", "releasx", "normal.md"},
		{"field_mismatch", "title:release", "normal.md"},
		{"phrase_reversed", "\"notes release\"", "normal.md"},
		{"phrase_gap", "\"release alpha\"", "normal.md"},
		{"not_triggered", "-release alpha", "normal.md"},
		{"filter_fail", "release ext:go", "normal.md"},
		{"suggest_remove", "release missingword", "normal.md"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rep, err := Diagnose(ix, s, cfg, tc.query, tc.path)
			if err != nil {
				t.Fatalf("Diagnose(%q,%q): %v", tc.query, tc.path, err)
			}
			text := rep.RenderText()
			if *updateGolden {
				gp := filepath.Join("testdata", "golden", tc.name+".txt")
				if err := os.MkdirAll(filepath.Dir(gp), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(gp, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(filepath.Join("testdata", "golden", tc.name+".txt"))
			if err != nil {
				t.Fatalf("read golden(先运行 go test -update 生成): %v", err)
			}
			if string(want) != text {
				t.Fatalf("golden mismatch for %s:\n--- want ---\n%s\n--- got ---\n%s",
					tc.name, want, text)
			}
			// JSON 输出必须合法。
			j, err := rep.RenderJSON()
			if err != nil || !jsonValid(j) {
				t.Fatalf("invalid JSON: %v", err)
			}
		})
	}
}

func jsonValid(s string) bool {
	var v interface{}
	return json.Unmarshal([]byte(s), &v) == nil
}
