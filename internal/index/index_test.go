package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lantern/internal/fsx"
)

func mkDoc(path, body string) Doc {
	return Doc{
		Path: path, Title: "T " + path, Body: body, Ext: "md",
		Size: int64(len(body)), MTime: 1700000000,
		SHA256: shaSum(body), StoreBody: true,
	}
}

// buildIndex 创建索引并写入 docs(每个 flushSegs 篇提交一次)。
// 返回的索引在测试结束时自动 Close(显式 Close 亦安全)。
func buildIndex(t *testing.T, dir string, docs []Doc, flushEvery int) *Index {
	t.Helper()
	ix, err := Open(dir, fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range docs {
		if err := w.AddDoc(d); err != nil {
			t.Fatal(err)
		}
		n++
		if n%flushEvery == 0 {
			if err := w.Flush(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return ix
}

func pathsOf(ix *Index) []string {
	out := make([]string, 0)
	ix.mu.RLock()
	for p := range ix.state.m {
		out = append(out, p)
	}
	ix.mu.RUnlock()
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		s := filepath.Join(src, e.Name())
		d := filepath.Join(dst, e.Name())
		if e.IsDir() {
			copyDir(t, s, d)
			continue
		}
		data, err := os.ReadFile(s)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(d, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCommitReopenLifecycle(t *testing.T) {
	dir := t.TempDir() + "/ix"
	ix := buildIndex(t, dir, []Doc{
		mkDoc("a.md", "alpha beta gamma"),
		mkDoc("b.md", "beta delta"),
		mkDoc("c.md", "gamma epsilon"),
	}, 2)
	defer ix.Close()

	if got := len(ix.snapshotSegs()); got != 2 {
		t.Fatalf("segments = %d, want 2", got)
	}
	if got := ix.StateLen(); got != 3 {
		t.Fatalf("state len = %d", got)
	}

	// 重开。
	ix2, err := Open(dir, fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	defer ix2.Close()
	got := pathsOf(ix2)
	want := []string{"a.md", "b.md", "c.md"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("paths after reopen = %v", got)
	}
	// 存储字段与内容一致性。
	e, sr, ok := ix2.Lookup("b.md")
	if !ok || sr == nil {
		t.Fatal("lookup b.md")
	}
	sd, err := sr.Reader().StoredDoc(e.DocID)
	if err != nil || sd.Body != "beta delta" {
		t.Fatalf("stored doc after reopen: %+v err=%v", sd, err)
	}
	// check 通过。
	rep, err := CheckIndex(fsx.OsFS(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() || rep.Docs != 3 {
		t.Fatalf("check report: %+v", rep)
	}
}

func TestUpdateAndDeleteDocs(t *testing.T) {
	dir := t.TempDir() + "/ix"
	ix := buildIndex(t, dir, []Doc{
		mkDoc("a.md", "first version"),
		mkDoc("b.md", "keep me"),
	}, 10)
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	// 更新 a.md。
	if err := w.AddDoc(mkDoc("a.md", "second version")); err != nil {
		t.Fatal(err)
	}
	// 删除 b.md。
	if err := w.DeleteDoc("b.md"); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := pathsOf(ix)
	if fmt.Sprint(got) != fmt.Sprint([]string{"a.md"}) {
		t.Fatalf("paths after update/delete = %v", got)
	}
	// 旧 a.md 文档必须被墓碑删除。
	e, sr, ok := ix.Lookup("a.md")
	if !ok {
		t.Fatal("a.md missing")
	}
	if e.DocID != 0 || !strings.HasPrefix(sr.rec.ID, "seg_") {
		t.Fatalf("a.md should point to new segment doc0: %+v %s", e, sr.rec.ID)
	}
	// check 仍通过(墓碑一致)。
	rep, err := CheckIndex(fsx.OsFS(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("check errors: %v", rep.Errors)
	}
	// 重开再验证。
	ix2, err := Open(dir, fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	defer ix2.Close()
	if got := pathsOf(ix2); fmt.Sprint(got) != fmt.Sprint([]string{"a.md"}) {
		t.Fatalf("paths after reopen = %v", got)
	}
	e2, sr2, _ := ix2.Lookup("a.md")
	sd, err := sr2.Reader().StoredDoc(e2.DocID)
	if err != nil || sd.Body != "second version" {
		t.Fatalf("updated body: %+v", sd)
	}
}

func TestCompactMergesAllSegments(t *testing.T) {
	dir := t.TempDir() + "/ix"
	var docs []Doc
	for i := 0; i < 10; i++ {
		docs = append(docs, mkDoc(fmt.Sprintf("d%02d.md", i), fmt.Sprintf("content number %d zebra", i)))
	}
	ix := buildIndex(t, dir, docs, 1)
	// 后台分层合并可能已把 10 个段合并到更少;仅要求仍有内容。
	if got := len(ix.snapshotSegs()); got < 1 {
		t.Fatalf("segments before compact = %d", got)
	}
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Compact(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := len(ix.snapshotSegs()); got != 1 {
		t.Fatalf("segments after compact = %d", got)
	}
	if got := ix.StateLen(); got != 10 {
		t.Fatalf("docs after compact = %d", got)
	}
	// 内容保留:读取每个文档。
	for i := 0; i < 10; i++ {
		p := fmt.Sprintf("d%02d.md", i)
		e, sr, ok := ix.Lookup(p)
		if !ok {
			t.Fatalf("%s missing after compact", p)
		}
		sd, err := sr.Reader().StoredDoc(e.DocID)
		if err != nil || sd.Body != fmt.Sprintf("content number %d zebra", i) {
			t.Fatalf("%s after compact: %+v err=%v", p, sd, err)
		}
	}
	// 磁盘上只有一个段目录。
	ents, _ := os.ReadDir(dir)
	segs := 0
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "seg_") {
			segs++
		}
	}
	if segs != 1 {
		t.Fatalf("segment dirs on disk = %d", segs)
	}
	rep, _ := CheckIndex(fsx.OsFS(), dir)
	if !rep.OK() {
		t.Fatalf("check after compact: %v", rep.Errors)
	}
}

func TestSnapshotIsolationDuringCommit(t *testing.T) {
	dir := t.TempDir() + "/ix"
	ix := buildIndex(t, dir, []Doc{mkDoc("old.md", "old content")}, 1)

	snap, err := ix.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()

	// 快照持有旧段:提交新段 + 删除旧文档。
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.AddDoc(mkDoc("new.md", "new content")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// 快照内旧段仍然可读。
	if len(snap.Segments()) == 0 {
		t.Fatal("snapshot lost segments")
	}
	r := snap.Segments()[0].Reader()
	sd, err := r.StoredDoc(0)
	if err != nil || sd.Path != "old.md" {
		t.Fatalf("snapshot read after commit: %+v err=%v", sd, err)
	}
	// 关闭快照不删除仍被 manifest 引用的段。
	snap.Close()
	if _, err := os.Stat(filepath.Join(dir, snap.Segments()[0].ID())); err != nil {
		t.Fatalf("referenced segment must survive: %v", err)
	}
}

func TestSnapshotGCDuringCompact(t *testing.T) {
	dir := t.TempDir() + "/ix"
	var docs []Doc
	for i := 0; i < 3; i++ {
		docs = append(docs, mkDoc(fmt.Sprintf("s%d.md", i), "x"))
	}
	ix := buildIndex(t, dir, docs, 1)
	snap, _ := ix.Snapshot()
	w, _ := ix.AcquireWriter(true)
	if err := w.Compact(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	// compact 后旧段 removed 但快照持有引用 → 目录还在。
	oldID := snap.Segments()[0].ID()
	if _, err := os.Stat(filepath.Join(dir, oldID)); err != nil {
		t.Fatalf("snapshot should pin old segment: %v", err)
	}
	snap.Close() // 引用归零 → 物理删除。
	if _, err := os.Stat(filepath.Join(dir, oldID)); !os.IsNotExist(err) {
		t.Fatalf("old segment should be removed after refs==0: %v", err)
	}
}

func TestWriteLockExclusiveAndStale(t *testing.T) {
	dir := t.TempDir() + "/ix"
	ix, err := Open(dir, fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	w1, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.AcquireWriter(true); err == nil {
		t.Fatal("second writer must fail while lock held")
	}
	if err := w1.Close(); err != nil {
		t.Fatal(err)
	}
	// 陈旧锁:手工写入过期心跳。
	old := lockContent{
		PID: 123456, Hostname: "ghost",
		Heartbeat: time.Now().Add(-LockStaleAfter - time.Minute),
	}
	lockPath := filepath.Join(dir, lockFile)
	data := make([]byte, lockSize)
	copy(data, fmt.Sprintf(`{"pid":123456,"hostname":"ghost","heartbeat":%q}`, old.Heartbeat.Format(time.RFC3339Nano)))
	if err := os.WriteFile(lockPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	w2, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatalf("stale lock must be taken over: %v", err)
	}
	w2.Close()
	// 新鲜锁:必须拒绝。
	if err := os.WriteFile(lockPath, mustJSON(t, lockContent{
		PID: 999, Hostname: "other", Heartbeat: time.Now(),
	}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.AcquireWriter(true); err == nil {
		t.Fatal("fresh foreign lock must be rejected")
	}
	os.Remove(lockPath)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := jsonMarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	// 补齐到 lockSize。
	out := make([]byte, lockSize)
	copy(out, b)
	return out
}

func TestRecoveryCleansOrphans(t *testing.T) {
	dir := t.TempDir() + "/ix"
	ix := buildIndex(t, dir, []Doc{mkDoc("a.md", "content")}, 1)
	ix.Close()
	// 制造孤儿:tmp 段、未引用段、manifest tmp、state 尾部。
	orphanSeg := filepath.Join(dir, "seg_999999")
	if err := os.MkdirAll(orphanSeg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanSeg, "meta.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seg_123.tmp", "x"), []byte("x"), 0o644); err != nil {
		_ = os.MkdirAll(filepath.Join(dir, "seg_123.tmp"), 0o755)
		os.WriteFile(filepath.Join(dir, "seg_123.tmp", "x"), []byte("x"), 0o644)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.99.tmp"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(filepath.Join(dir, stateFile), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"op":"put","path":"ghost.md"}` + "\n")
	f.Close()

	ix2, err := Open(dir, fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	defer ix2.Close()
	// 孤儿全部清理。
	for _, p := range []string{"seg_999999", "seg_123.tmp", "manifest.99.tmp"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s not cleaned", p)
		}
	}
	// ghost.md 不在状态表。
	if _, _, ok := ix2.Lookup("ghost.md"); ok {
		t.Fatal("uncommitted state tail must be ignored")
	}
	if got := pathsOf(ix2); fmt.Sprint(got) != fmt.Sprint([]string{"a.md"}) {
		t.Fatalf("paths = %v", got)
	}
}

func TestConcurrentSearchDuringMerge(t *testing.T) {
	dir := t.TempDir() + "/ix"
	var docs []Doc
	for i := 0; i < 12; i++ {
		docs = append(docs, mkDoc(fmt.Sprintf("m%02d.md", i), fmt.Sprintf("body %d", i)))
	}
	ix := buildIndex(t, dir, docs, 1)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, err := ix.Snapshot()
				if err != nil {
					errCh <- err
					return
				}
				for _, sr := range snap.Segments() {
					r := sr.Reader()
					for i := uint32(0); i < r.DocCount(); i++ {
						if !r.IsAlive(i) {
							continue
						}
						if _, err := r.StoredDoc(i); err != nil {
							errCh <- err
							return
						}
					}
				}
				snap.Close()
			}
		}()
	}
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 3; round++ {
		if err := w.Compact(); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	w.Close()
	select {
	case err := <-errCh:
		t.Fatalf("concurrent read failed: %v", err)
	default:
	}
	if got := len(ix.snapshotSegs()); got != 1 {
		t.Fatalf("segments = %d after compacts", got)
	}
}
