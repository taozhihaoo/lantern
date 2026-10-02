package index

import (
	"fmt"
	"testing"

	"lantern/internal/fsx"
)

// TermPrefix 走稀疏索引的范围扫描:覆盖起始块回退、跨字段边界与
// 不存在前缀。
func TestTermPrefixRanges(t *testing.T) {
	dir := t.TempDir()
	ix, err := Open(dir, fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		body := fmt.Sprintf("alpha concurrent common%d beta gamma", i)
		if err := w.AddDoc(Doc{Path: fmt.Sprintf("d%d.md", i), Title: "T", Body: body,
			SHA256: [32]byte{byte(i)}, Size: int64(len(body)), MTime: 1700000000}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	snap, err := ix.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	r := snap.Segments()[0].Reader()
	cases := []struct {
		prefix string
		want   int
		first  string
	}{
		{"co", 51, "common0"}, // common0-49 + concurrent
		{"con", 1, "concurrent"},
		{"a", 1, "alpha"},
		{"gamma", 1, "gamma"},
		{"zzz", 0, ""},
	}
	for _, tc := range cases {
		n := 0
		first := ""
		err := r.TermPrefix(FieldBody, tc.prefix, func(term string, _ TermEntry) bool {
			if n == 0 {
				first = term
			}
			n++
			return true
		})
		if err != nil {
			t.Fatalf("prefix %q: %v", tc.prefix, err)
		}
		if n != tc.want || first != tc.first {
			t.Fatalf("prefix %q: n=%d first=%q, want %d/%q", tc.prefix, n, first, tc.want, tc.first)
		}
	}
}
