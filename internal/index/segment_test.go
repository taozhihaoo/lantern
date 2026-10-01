package index

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"testing"

	"lantern/internal/codec"
	"lantern/internal/fsx"
)

func testDoc(path, title, body string, store bool) Doc {
	return Doc{
		Path: path, Title: title, Body: body,
		Ext: "md", Size: int64(len(body)), MTime: 1700000000,
		SHA256: sha256.Sum256([]byte(body)), StoreBody: store,
	}
}

// buildSegment 用给定文档写出一个段并重新打开。
func buildSegment(t *testing.T, docs []Doc, store bool) (*SegmentReader, *BlockCache) {
	t.Helper()
	fsys := fsx.OsFS()
	dir := t.TempDir() + "/seg_test"
	mt := NewMemTable()
	for _, d := range docs {
		mt.Add(d)
	}
	if _, err := WriteSegment(fsys, dir, mt, store); err != nil {
		t.Fatal(err)
	}
	r, err := OpenSegment(fsys, dir, "seg_test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r, r.cache
}

func TestSegmentRoundtripBasics(t *testing.T) {
	docs := []Doc{
		testDoc("a.md", "Alpha", "hello world hello lantern", true),
		testDoc("b/c.md", "Beta", "world of Go语言并发编程 parseHTTPRequest", true),
		testDoc("d.md", "", "v2.1.3 snake_case_name", true),
	}
	r, _ := buildSegment(t, docs, true)
	if r.DocCount() != 3 {
		t.Fatalf("docCount = %d", r.DocCount())
	}

	// 词项查找与统计。
	te, ok, err := r.Term(FieldBody, "hello")
	if err != nil || !ok {
		t.Fatalf("term hello: ok=%v err=%v", ok, err)
	}
	if te.DocFreq != 1 || te.TotalTF != 2 {
		t.Fatalf("hello entry = %+v", te)
	}
	if _, ok, _ := r.Term(FieldBody, "missing"); ok {
		t.Fatal("missing term should not be found")
	}

	// posting 迭代与位置。
	it, err := r.Postings(FieldBody, "world")
	if err != nil || it == nil {
		t.Fatalf("postings world: %v", err)
	}
	var ids []uint32
	var allPos [][]uint32
	for it.Next() {
		ids = append(ids, it.DocID())
		allPos = append(allPos, append([]uint32(nil), it.Positions()...))
	}
	if len(ids) != 2 || ids[0] != 0 || ids[1] != 1 {
		t.Fatalf("world docs = %v", ids)
	}
	if len(allPos[0]) != 1 || allPos[0][0] != 1 {
		t.Fatalf("world pos in doc0 = %v", allPos[0])
	}

	// 标识符子词。
	if te, ok, _ := r.Term(FieldBody, "http"); !ok || te.DocFreq != 1 {
		t.Fatalf("sub token http missing: ok=%v", ok)
	}
	// CJK bigram。
	if te, ok, _ := r.Term(FieldBody, "并发"); !ok || te.DocFreq != 1 {
		t.Fatal("cjk bigram 并发 missing")
	}

	// norms。
	n, err := r.Norm(0, FieldBody)
	if err != nil || n != 4 {
		t.Fatalf("norm doc0 body = %d err=%v (want 4: hello world hello lantern)", n, err)
	}

	// doc values。
	dv, err := r.DocValues(2)
	if err != nil || dv.Ext != "md" || dv.Size != int64(len("v2.1.3 snake_case_name")) {
		t.Fatalf("dv = %+v err=%v", dv, err)
	}

	// 存储字段。
	sd, err := r.StoredDoc(1)
	if err != nil {
		t.Fatal(err)
	}
	if sd.Path != "b/c.md" || sd.Title != "Beta" {
		t.Fatalf("stored = %+v", sd)
	}
	if sd.Body != "world of Go语言并发编程 parseHTTPRequest" {
		t.Fatalf("stored body = %q", sd.Body)
	}
}

func TestSegmentNoBody(t *testing.T) {
	docs := []Doc{testDoc("a.md", "T", "body text here", false)}
	r, _ := buildSegment(t, docs, false)
	sd, err := r.StoredDoc(0)
	if err != nil {
		t.Fatal(err)
	}
	if sd.Body != "" {
		t.Fatalf("body should not be stored, got %q", sd.Body)
	}
}

func TestSegmentTombstones(t *testing.T) {
	docs := []Doc{
		testDoc("a.md", "A", "apple banana", true),
		testDoc("b.md", "B", "apple cherry", true),
		testDoc("c.md", "C", "apple durian", true),
	}
	r, _ := buildSegment(t, docs, true)
	bits := codec.NewBitset(3)
	bits.Set(1)
	r.SetLive(1, bits)
	if r.IsAlive(1) {
		t.Fatal("doc1 should be deleted")
	}
	it, _ := r.Postings(FieldBody, "apple")
	var ids []uint32
	for it.Next() {
		ids = append(ids, it.DocID())
	}
	if len(ids) != 2 || ids[0] != 0 || ids[1] != 2 {
		t.Fatalf("after tombstone: %v", ids)
	}
	// Advance 跳过墓碑(用全新迭代器)。
	it2, _ := r.Postings(FieldBody, "apple")
	if !it2.Advance(1) || it2.DocID() != 2 {
		t.Fatalf("advance to 1 should land on 2")
	}
	// DocFreq 仍统计已删除文档(Lucene 语义)。
	te, ok, _ := r.Term(FieldBody, "apple")
	if !ok || te.DocFreq != 3 {
		t.Fatalf("df should include deleted: %+v", te)
	}
}

func TestSegmentAdvanceAcrossBlocks(t *testing.T) {
	// 300 篇文档 → 每个 posting 多块,验证 Advance 跳块。
	var docs []Doc
	for i := 0; i < 300; i++ {
		docs = append(docs, testDoc(
			fmt.Sprintf("d%03d.md", i), fmt.Sprintf("T%d", i),
			fmt.Sprintf("common doc%d zebra", i), true))
	}
	r, _ := buildSegment(t, docs, true)
	it, err := r.Postings(FieldBody, "common")
	if err != nil || it == nil {
		t.Fatal("postings common")
	}
	if it.DocFreq() != 300 {
		t.Fatalf("df = %d", it.DocFreq())
	}
	// 顺序迭代计数。
	cnt := 0
	last := uint32(0)
	for it.Next() {
		if it.DocID() <= last && cnt > 0 {
			t.Fatalf("docID not increasing: %d after %d", it.DocID(), last)
		}
		last = it.DocID()
		cnt++
	}
	if cnt != 300 {
		t.Fatalf("iterated %d", cnt)
	}
	// Advance 到块边界(128/256)与结尾。
	for _, target := range []uint32{0, 1, 127, 128, 200, 299, 300} {
		it2, _ := r.Postings(FieldBody, "common")
		if target < 300 {
			if !it2.Advance(target) {
				t.Fatalf("advance %d failed", target)
			}
			if it2.DocID() != target {
				t.Fatalf("advance %d landed on %d", target, it2.DocID())
			}
		} else {
			if it2.Advance(300) {
				t.Fatalf("advance 300 should fail")
			}
		}
	}
	// Advance 语义:向后推进保持单调。
	it3, _ := r.Postings(FieldBody, "zebra")
	if !it3.Advance(5) || it3.DocID() != 5 {
		t.Fatal("advance 5")
	}
	if !it3.Advance(5) || it3.DocID() != 5 {
		t.Fatal("advance to same doc stays")
	}
	if !it3.Advance(250) || it3.DocID() != 250 {
		t.Fatal("advance 250")
	}
}

func TestSegmentTermsIteration(t *testing.T) {
	docs := []Doc{
		testDoc("a.md", "Title One", "alpha beta", true),
		testDoc("b.md", "Title Two", "beta gamma", true),
	}
	r, _ := buildSegment(t, docs, true)
	bodyTerms, err := r.TermsSorted(FieldBody)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if len(bodyTerms) != len(want) {
		t.Fatalf("terms = %v", bodyTerms)
	}
	for i := range want {
		if bodyTerms[i] != want[i] {
			t.Fatalf("terms[%d] = %q, want %q", i, bodyTerms[i], want[i])
		}
	}
	titleTerms, _ := r.TermsSorted(FieldTitle)
	// one,title,two(title 在两篇中重复,去重后 3 个)。
	if len(titleTerms) != 3 {
		t.Fatalf("title terms = %v", titleTerms)
	}
}

func TestSegmentCorruption(t *testing.T) {
	fsys := fsx.OsFS()
	dir := t.TempDir() + "/seg_corrupt"
	mt := NewMemTable()
	mt.Add(testDoc("a.md", "A", "some text content", true))
	if _, err := WriteSegment(fsys, dir, mt, true); err != nil {
		t.Fatal(err)
	}
	// 完整校验先通过。
	if err := CheckSegment(fsys, dir, "x", 0); err != nil {
		t.Fatalf("clean check failed: %v", err)
	}
	// 翻转 terms.dat 中间字节 → CheckSegment 必须报错。
	data := readTestFile(t, dir+"/terms.dat")
	data[len(data)/2] ^= 0xFF
	writeTestFile(t, dir+"/terms.dat", data)
	if err := CheckSegment(fsys, dir, "x", 0); err == nil {
		t.Fatal("corrupted terms.dat must fail check")
	}
	// 截断 postings.dat → 打开段必须报错(尾部 magic 缺失)。
	pdata := readTestFile(t, dir+"/postings.dat")
	writeTestFile(t, dir+"/postings.dat", pdata[:len(pdata)-5])
	if _, err := OpenSegment(fsys, dir, "x", nil); err == nil {
		t.Fatal("truncated postings.dat must fail open")
	}
}

func TestSegmentEmptyRefused(t *testing.T) {
	fsys := fsx.OsFS()
	dir := t.TempDir() + "/seg_empty"
	if _, err := WriteSegment(fsys, dir, NewMemTable(), true); err == nil {
		t.Fatal("empty memtable must be refused")
	}
}

func TestMemTableFlushThreshold(t *testing.T) {
	mt := NewMemTable()
	for i := 0; i < DefaultFlushDocs-1; i++ {
		mt.Add(testDoc(fmt.Sprintf("f%d.md", i), "", "x", false))
	}
	if mt.ShouldFlush() {
		t.Fatal("below threshold should not flush")
	}
	mt.Add(testDoc("f9999.md", "", "x", false))
	if !mt.ShouldFlush() {
		t.Fatal("at threshold should flush")
	}
}

func TestSegmentFieldStats(t *testing.T) {
	docs := []Doc{
		testDoc("a.md", "one two", "three", true),
		testDoc("b.md", "four", "", true),
	}
	r, _ := buildSegment(t, docs, true)
	fs := r.FieldStats()
	// title: a=2, b=1 → total 3; body: a=1, b=0 → total 1, docCount 1。
	if fs[FieldTitle].TotalLen != 3 || fs[FieldTitle].DocCount != 2 {
		t.Fatalf("title stats = %+v", fs[FieldTitle])
	}
	if fs[FieldBody].TotalLen != 1 || fs[FieldBody].DocCount != 1 {
		t.Fatalf("body stats = %+v", fs[FieldBody])
	}
	// path 字段:a.md → a, md。
	pathTerms, _ := r.TermsSorted(FieldPath)
	if !sort.StringsAreSorted(pathTerms) {
		t.Fatal("path terms not sorted")
	}
}
