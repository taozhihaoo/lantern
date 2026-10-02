package index

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"lantern/internal/fsx"
)

func benchDoc(i int) Doc {
	body := fmt.Sprintf("bench document number %d about search engines and index structures. "+
		"parseHTTPRequest sample snake_case_name 全文搜索引擎测试数据 第%d段。", i, i%97)
	return Doc{
		Path:      fmt.Sprintf("d%02d/doc%05d.md", i%16, i),
		Title:     fmt.Sprintf("Doc %d", i),
		Body:      body,
		Ext:       "md",
		Size:      int64(len(body)),
		MTime:     1700000000,
		SHA256:    sha256.Sum256([]byte(body)),
		StoreBody: true,
	}
}

// BenchmarkIndexDocs 衡量索引吞吐(每 op = 一篇文档,含建段)。
func BenchmarkIndexDocs(b *testing.B) {
	dir := b.TempDir()
	ix, err := Open(dir, fsx.OsFS())
	if err != nil {
		b.Fatal(err)
	}
	defer ix.Close()
	w, err := ix.AcquireWriter(true)
	if err != nil {
		b.Fatal(err)
	}
	defer w.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := w.AddDoc(benchDoc(i)); err != nil {
			b.Fatal(err)
		}
		if w.mt.Len() >= 2000 {
			if err := w.Flush(); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := w.Flush(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkSegmentOpen 衡量段打开成本。
func BenchmarkSegmentOpen(b *testing.B) {
	dir := b.TempDir()
	ix, err := Open(dir, fsx.OsFS())
	if err != nil {
		b.Fatal(err)
	}
	defer ix.Close()
	w, err := ix.AcquireWriter(true)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 5000; i++ {
		_ = w.AddDoc(benchDoc(i))
	}
	if err := w.Flush(); err != nil {
		b.Fatal(err)
	}
	w.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, err := ix.Snapshot()
		if err != nil {
			b.Fatal(err)
		}
		snap.Close()
	}
}
