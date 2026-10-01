package fsx

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOsFSRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fsys := OsFS()
	p := filepath.Join(dir, "a", "b.txt")
	if err := fsys.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := fsys.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("hello lantern")); err != nil {
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rf, err := fsys.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := rf.ReadAt(buf, 6); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "lante" {
		t.Fatalf("ReadAt got %q", buf)
	}
	rf.Close()

	if err := fsys.Rename(p, filepath.Join(dir, "a", "c.txt")); err != nil {
		t.Fatal(err)
	}
	ents, err := fsys.ReadDir(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "c.txt" {
		t.Fatalf("unexpected dir entries: %v", ents)
	}
	if err := fsys.SyncDir(filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	if err := fsys.Remove(filepath.Join(dir, "a", "c.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Stat(filepath.Join(dir, "a", "c.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected NotExist, got %v", err)
	}
}

func TestFaultFSFailAtNth(t *testing.T) {
	dir := t.TempDir()
	base := OsFS()
	ffs := NewFaultFS(base)
	p := filepath.Join(dir, "x.txt")

	// 第 2 次 create 失败(第 1 次成功)。
	ffs.Inject(Fault{Op: "create", Path: "x.txt", After: 2, Mode: ModeFail})
	f, err := ffs.Create(p)
	if err != nil {
		t.Fatalf("create #1: %v", err)
	}
	f.Close()
	if _, err := ffs.Create(p); !errors.Is(err, ErrInjected) {
		t.Fatalf("expected injected error on create #2, got %v", err)
	}
	if ffs.Ops("create") != 2 {
		t.Fatalf("create ops = %d, want 2", ffs.Ops("create"))
	}
}

func TestFaultFSDropRename(t *testing.T) {
	dir := t.TempDir()
	ffs := NewFaultFS(OsFS())
	a := filepath.Join(dir, "a.txt")
	b := filepath.Join(dir, "b.txt")
	f, err := ffs.Create(a)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	ffs.Inject(Fault{Op: "rename", Path: "a.txt", After: 1, Mode: ModeDrop})
	if err := ffs.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := ffs.Stat(a); err != nil {
		t.Fatalf("source should still exist after dropped rename: %v", err)
	}
	if _, err := ffs.Stat(b); !os.IsNotExist(err) {
		t.Fatalf("target should not exist after dropped rename: %v", err)
	}
}

func TestFaultFSTruncate(t *testing.T) {
	dir := t.TempDir()
	ffs := NewFaultFS(OsFS())
	p := filepath.Join(dir, "seg.bin")
	// 第 2 次写之后断电截断到 5 字节;断电后一切操作必须失败。
	ffs.Inject(Fault{Op: "write", Path: "seg.bin", After: 2, Mode: ModeTruncate, TruncSize: 5})
	f, err := ffs.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(bytes.Repeat([]byte("a"), 8)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte("b"), 8)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bytes.Repeat([]byte("c"), 8)); err == nil {
		t.Fatal("writes after power loss must fail")
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != 5 {
		t.Fatalf("size after truncate = %d, want 5", st.Size())
	}
	// 关闭重开后内容仍为 5 字节,且是截断点之前的内容。
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 5 || string(data) != "aaaaa" {
		t.Fatalf("reopened file = %q, want %q", data, "aaaaa")
	}
}
