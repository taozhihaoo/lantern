package query_test

import (
	"fmt"
	"testing"

	"lantern/internal/index"
	"lantern/internal/testutil"
)

func TestDebugPositions(t *testing.T) {
	docs := []testutil.Doc{
		{Path: "b.md", Title: "B", Body: "Go语言并发编程实战", Ext: "md", Size: 28, MTime: 1700000001},
	}
	dir := t.TempDir()
	ix := buildDiffIndex(t, dir, docs, 100, false)
	snap, _ := ix.Snapshot()
	r := snap.Segments()[0].Reader()
	for _, tm := range []string{"go", "语言", "言并", "并发", "发编", "编程"} {
		it, err := r.Postings(index.FieldBody, tm)
		if err != nil || it == nil {
			fmt.Printf("%s: nil (%v)\n", tm, err)
			continue
		}
		for it.Next() {
			fmt.Printf("%s: doc=%d tf=%d pos=%v\n", tm, it.DocID(), it.TF(), it.Positions())
		}
	}
}
