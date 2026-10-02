package index

import (
	"fmt"
	"testing"

	"lantern/internal/fsx"
)

func TestDebugTermPrefix(t *testing.T) {
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
		_ = w.AddDoc(Doc{Path: fmt.Sprintf("d%d.md", i), Title: "T", Body: body,
			SHA256: [32]byte{byte(i)}, Size: int64(len(body)), MTime: 1700000000})
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	snap, _ := ix.Snapshot()
	r := snap.Segments()[0].Reader()
	for _, p := range []string{"co", "a", "con", "z", "gamma"} {
		n := 0
		first := ""
		err := r.TermPrefix(2, p, func(term string, e TermEntry) bool {
			if first == "" {
				first = term
			}
			n++
			return true
		})
		fmt.Printf("prefix %q: n=%d first=%q err=%v\n", p, n, first, err)
	}
	// 词典抽样。
	var all []string
	_ = r.Terms(2, func(term string, _ TermEntry) bool { all = append(all, term); return true })
	fmt.Printf("dict sample: %v\n", all[:min(8, len(all))])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
