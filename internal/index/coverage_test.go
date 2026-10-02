package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lantern/internal/codec"
	"lantern/internal/fsx"
)

// TestAPIAndUtilityCoverage 覆盖查询层/CLI 使用的公开 API 与工具路径。
func TestAPIAndUtilityCoverage(t *testing.T) {
	dir := t.TempDir()
	ix, err := Open(dir, fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if ix.Root() != dir {
		t.Fatalf("Root = %q", ix.Root())
	}
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for i := 0; i < 10; i++ {
		body := fmt.Sprintf("coverage doc %d shared alpha", i)
		if err := w.AddDoc(Doc{
			Path: fmt.Sprintf("d%02d.md", i), Title: fmt.Sprintf("T%d", i), Body: body,
			Ext: "md", Size: int64(len(body)), MTime: 1700000000 + int64(i),
			SHA256: shaSum(body), StoreBody: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	// FieldByName / Field.Name 往返。
	for _, name := range []string{"path", "title", "body"} {
		f, ok := FieldByName(name)
		if !ok || f.Name() != name {
			t.Fatalf("FieldByName(%q) = %v", name, ok)
		}
	}
	if _, ok := FieldByName("nope"); ok {
		t.Fatal("unknown field must fail")
	}

	// StatePaths / StateInfoOf。
	paths := ix.StatePaths()
	if len(paths) != 10 {
		t.Fatalf("StatePaths = %d", len(paths))
	}
	info, ok := ix.StateInfoOf("d00.md")
	if !ok || info.Size == 0 || info.SHA == "" {
		t.Fatalf("StateInfoOf = %+v", info)
	}
	if _, ok := ix.StateInfoOf("ghost.md"); ok {
		t.Fatal("ghost lookup must fail")
	}

	// 状态表 paths 排序。
	st := newStateTable()
	st.apply([]stateEntry{{Op: "put", Path: "b"}, {Op: "put", Path: "a"}, {Op: "del", Path: "b"}})
	got := st.paths()
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("stateTable.paths = %v", got)
	}

	snap, err := ix.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	r := snap.Segments()[0].Reader()

	// PostingsNoPos / TF / TotalTF / LiveGen / blockLastDoc。
	it, err := r.PostingsNoPos(FieldBody, "shared")
	if err != nil || it == nil {
		t.Fatalf("PostingsNoPos: %v", err)
	}
	if it.TotalTF() == 0 {
		t.Fatal("TotalTF missing")
	}
	cnt := 0
	for it.Next() {
		if it.TF() == 0 {
			t.Fatal("TF missing")
		}
		cnt++
	}
	if cnt != 10 {
		t.Fatalf("NoPos iterations = %d", cnt)
	}
	if r.LiveGen() != 0 {
		t.Fatalf("LiveGen = %d", r.LiveGen())
	}

	// StoredDocLite。
	path, title, err := r.StoredDocLite(3)
	if err != nil || path != "d03.md" || title != "T3" {
		t.Fatalf("StoredDocLite = %q %q %v", path, title, err)
	}
	if _, _, err := r.StoredDocLite(9999); err == nil {
		t.Fatal("out-of-range must fail")
	}
	if _, _, err := decodeStoredDocLite([]byte{0}); err == nil {
		t.Fatal("corrupt lite record must fail")
	}

	// 缓存逐出路径(Put 超容量)。
	c := NewBlockCache(256)
	for i := 0; i < 64; i++ {
		c.Put(cacheKey{seg: "s", file: "f", off: int64(i), ln: 64}, make([]byte, 64))
	}
	if c.size > c.cap {
		t.Fatalf("cache eviction failed: %d > %d", c.size, c.cap)
	}

	// pickMergeTier 边界:不足 8 段返回 nil;多小段合并选择。
	if pickMergeTier(nil) != nil {
		t.Fatal("empty tier")
	}
	var refs []*SegmentRef
	for i := 0; i < 9; i++ {
		refs = append(refs, &SegmentRef{rec: segmentRecord{ID: fmt.Sprintf("seg_%06d", i),
			Files: map[string]fileChecksum{"a": {Size: 1024}}}})
	}
	if v := pickMergeTier(refs); len(v) != 9 {
		t.Fatalf("tier pick = %d", len(v))
	}

	// readLiveBits:损坏输入报错。
	if _, err := readLiveBits(fsx.OsFS(), dir, 7); err == nil {
		t.Fatal("missing live bits must fail")
	}

	// loadState:损坏条目报错。
	ix2, err := Open(dir, fsx.OsFS())
	if err2 := ix2.Close(); err2 != nil {
		t.Fatal(err2)
	}
	sp := filepath.Join(dir, stateFile)
	data, _ := os.ReadFile(sp)
	if err := os.WriteFile(sp, append(data[:len(data)-10], []byte("{corrupt")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), corruptManifest(t, filepath.Join(dir, manifestFile), int64(len(data)-10)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, fsx.OsFS()); err == nil {
		t.Fatal("corrupt state entry must fail open")
	}

	// closeAll:Open 失败路径(缺文件)内部触发。
	bad := t.TempDir() + "/seg_bad"
	if err := fsx.OsFS().MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSegment(fsx.OsFS(), bad, "seg_bad", nil); err == nil {
		t.Fatal("empty segment dir must fail open")
	}
}

// corruptManifest 将 manifest 的 state_len 改小,制造"状态表尾部无效"。
func corruptManifest(t *testing.T, path string, stateLen int64) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	marker := "\"state_len\": "
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatal("state_len not found")
	}
	j := strings.IndexByte(text[i:], ',')
	out := text[:i+len(marker)] + fmt.Sprintf("%d", stateLen) + text[i+j:]
	return []byte(out)
}

var _ = codec.FooterSize
