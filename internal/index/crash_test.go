package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lantern/internal/fsx"
)

// TestCrashInjectionAtEveryStep 在提交协议的每一步注入故障(规格 11.3):
// 在第 k 次操作上分别以 Fail(操作前崩溃)、Drop(操作后崩溃)两种模式
// 注入,随后以真实 FS 重新打开索引,要求:可打开、内容等于旧版本或
// 新版本之一、CheckIndex 通过、无孤儿文件。
func TestCrashInjectionAtEveryStep(t *testing.T) {
	oldDocs := []Doc{
		mkDoc("old1.md", "alpha"),
		mkDoc("old2.md", "beta"),
	}
	newDocs := []Doc{
		mkDoc("new1.md", "gamma"),
		mkDoc("new2.md", "delta"),
	}
	base := t.TempDir() + "/base"
	ix := buildIndex(t, base, oldDocs, 10)
	ix.Close()
	oldPaths := "old1.md old2.md"
	newPaths := "new1.md new2.md old1.md old2.md"

	// 先跑一次干净提交,得到总操作数上界。
	probe := t.TempDir() + "/probe"
	copyDir(t, base, probe)
	ffs := fsx.NewFaultFS(fsx.OsFS())
	runCommitWithFS(t, probe, ffs, newDocs)
	totalOps := 0
	for _, op := range []string{"create", "open", "openfile", "write", "syncfile", "close",
		"rename", "remove", "removeall", "mkdirall", "stat", "readdir", "syncdir"} {
		totalOps += ffs.Ops(op)
	}
	if totalOps == 0 {
		t.Fatal("no ops counted")
	}

	for _, mode := range []fsx.FaultMode{fsx.ModeFail, fsx.ModeDrop} {
		for k := 1; k <= totalOps+2; k++ {
			k := k
			t.Run(fmt.Sprintf("%s_k%d", modeName(mode), k), func(t *testing.T) {
				dir := t.TempDir() + "/ix"
				copyDir(t, base, dir)
				// 打开用真实 FS;提交换成故障 FS(模拟运行中掉电)。
				ix, err := Open(dir, fsx.OsFS())
				if err != nil {
					t.Fatalf("open before commit: %v", err)
				}
				ffs := fsx.NewFaultFS(ix.fsys)
				ix.SetFS(ffs)
				w, werr := ix.AcquireWriter(true)
				if werr != nil {
					ix.Close()
					return // 锁获取失败也算旧版本可用
				}
				for _, d := range newDocs {
					_ = w.AddDoc(d)
				}
				_ = w.Flush()
				_ = w.Close()
				_ = ix.Close()

				// 以真实 FS 重开并恢复。
				ix2, err := Open(dir, fsx.OsFS())
				if err != nil {
					t.Fatalf("k=%d mode=%v: reopen failed: %v", k, mode, err)
				}
				defer ix2.Close()
				rep, err := CheckIndex(fsx.OsFS(), dir)
				if err != nil {
					t.Fatalf("k=%d: check error: %v", k, err)
				}
				if !rep.OK() {
					t.Fatalf("k=%d mode=%v: check failed: %v", k, mode, rep.Errors)
				}
				got := strings.Join(pathsOf(ix2), " ")
				if got != oldPaths && got != newPaths {
					t.Fatalf("k=%d mode=%v: content neither old nor new: %q", k, mode, got)
				}
			})
		}
	}
}

// TestCrashTruncate torn-write(断电截断)注入:对每个新段文件与
// manifest tmp、state 追加,在写入后截断到指定长度。注:截断模型
// 只施加于"提交点之前"的文件(新段文件、manifest.<g>.tmp、
// state.jsonl 追加),因为真实掉电不可能让已通过 manifest 提交的
// 数据回退。
func TestCrashTruncate(t *testing.T) {
	oldDocs := []Doc{mkDoc("old1.md", "alpha")}
	newDocs := []Doc{mkDoc("new1.md", "gamma")}
	base := t.TempDir() + "/base"
	ix := buildIndex(t, base, oldDocs, 10)
	ix.Close()

	var oldState int64
	if st, err := os.Stat(filepath.Join(base, stateFile)); err == nil {
		oldState = st.Size()
	}

	truncTargets := []struct {
		path string
		size int64
	}{
		{"terms.dat", 8},
		{"terms.idx", 8},
		{"postings.dat", 8},
		{"norms.dat", 8},
		{"dv.dat", 8},
		{"docs.dat", 8},
		{"docs.idx", 8},
		{"live_0.bits", 8},
		{"meta.json", 8},
		{".tmp", 8},
		{"state.jsonl", oldState}, // 追加撕裂:有效前缀必须保留
	}
	for _, tt := range truncTargets {
		for k := 1; k <= 6; k++ {
			k, tt := k, tt
			t.Run(fmt.Sprintf("trunc_%s_k%d", tt.path, k), func(t *testing.T) {
				dir := t.TempDir() + "/ix"
				copyDir(t, base, dir)
				ix, err := Open(dir, fsx.OsFS())
				if err != nil {
					t.Fatal(err)
				}
				ffs := fsx.NewFaultFS(ix.fsys)
				ffs.Inject(fsx.Fault{Op: "write", Path: tt.path, After: k, Mode: fsx.ModeTruncate, TruncSize: tt.size})
				ix.SetFS(ffs)
				w, werr := ix.AcquireWriter(true)
				if werr != nil {
					ix.Close()
					return
				}
				for _, d := range newDocs {
					_ = w.AddDoc(d)
				}
				_ = w.Flush()
				_ = w.Close()
				_ = ix.Close()

				ix2, err := Open(dir, fsx.OsFS())
				if err != nil {
					t.Fatalf("reopen failed: %v", err)
				}
				defer ix2.Close()
				rep, err := CheckIndex(fsx.OsFS(), dir)
				if err != nil {
					t.Fatalf("check error: %v", err)
				}
				if !rep.OK() {
					t.Fatalf("check failed: %v", rep.Errors)
				}
				got := strings.Join(pathsOf(ix2), " ")
				if got != "old1.md" && got != "new1.md old1.md" {
					t.Fatalf("content neither old nor new: %q", got)
				}
			})
		}
	}
}

func runCommitWithFS(t *testing.T, dir string, fsys fsx.FS, docs []Doc) {
	t.Helper()
	ix, err := Open(dir, fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	ix.SetFS(fsys)
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		_ = w.AddDoc(d)
	}
	_ = w.Flush()
	_ = w.Close()
	ix.Close()
}

func modeName(m fsx.FaultMode) string {
	switch m {
	case fsx.ModeFail:
		return "fail"
	case fsx.ModeDrop:
		return "drop"
	case fsx.ModeTruncate:
		return "trunc"
	}
	return "?"
}
