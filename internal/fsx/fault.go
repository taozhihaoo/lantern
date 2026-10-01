package fsx

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
)

// FaultMode 描述故障注入的方式。
type FaultMode int

const (
	// ModeFail:第 After 次匹配操作直接返回错误(模型:崩溃发生在该操作之前)。
	ModeFail FaultMode = iota
	// ModeDrop:第 After 次匹配操作被静默跳过并返回成功
	// (模型:崩溃发生在该操作之后,效果丢失,如丢失的 rename/fsync)。
	ModeDrop
	// ModeTruncate:仅对 write 操作有效。第 After 次写完成后把文件截断到
	// TruncSize 字节,并且该句柄后续写全部丢弃(模型:断电导致尾部撕裂)。
	ModeTruncate
)

// Fault 描述一条注入规则。Op 为操作类型之一:
// create/open/openfile/write/syncfile/close/rename/remove/removeall/
// mkdirall/stat/readdir/syncdir。Path 为路径后缀匹配(以 / 归一化),
// 为空匹配全部;After 从 1 计数。
type Fault struct {
	Op        string
	Path      string
	After     int
	Times     int
	Mode      FaultMode
	TruncSize int64
}

// FaultFS 在任意 base FS 之上注入故障,用于崩溃安全测试。
// Truncate 注入模拟断电撕裂:截断发生后进程视为死亡,FaultFS 的所有
// 后续操作立即失败(ErrInjected),提交协议因此无法越过提交点。
// FaultFS 并发安全。
type FaultFS struct {
	base FS

	mu        sync.Mutex
	faults    []*faultRule
	opsCount  map[string]int
	frozen    map[*faultFile]bool
	powerLost bool
}

type faultRule struct {
	Fault
	hits int
}

// NewFaultFS 包装 base 并返回可注入故障的 FS。
func NewFaultFS(base FS) *FaultFS {
	return &FaultFS{
		base:     base,
		opsCount: map[string]int{},
		frozen:   map[*faultFile]bool{},
	}
}

// Inject 追加一条故障规则。Op 为 "*" 时匹配全部操作;
// 对不支持静默跳过的操作(create/open/openfile/write/stat/readdir),
// Drop 模式自动降级为 Fail(跳过它们会破坏句柄语义)。
func (f *FaultFS) Inject(ft Fault) {
	if ft.Times < 1 {
		ft.Times = 1
	}
	if ft.Mode == ModeDrop && !dropable(ft.Op) {
		ft.Mode = ModeFail
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, &faultRule{Fault: ft})
}

func dropable(op string) bool {
	switch op {
	case "rename", "remove", "removeall", "mkdirall", "syncdir", "syncfile", "close":
		return true
	}
	return false
}

// Ops 返回某类操作(含被 Drop 的)累计发生次数。
func (f *FaultFS) Ops(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opsCount[op]
}

// Reset 清除全部规则与冻结标记(保留计数)。
func (f *FaultFS) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = nil
	f.frozen = map[*faultFile]bool{}
}

// injected 返回规则命中时应采取的动作;第二个返回值表示是否命中。
func (f *FaultFS) injected(op, name string) (*faultRule, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opsCount[op]++
	if f.powerLost {
		return &faultRule{Fault: Fault{Mode: ModeFail}}, true
	}
	for _, r := range f.faults {
		if r.Op != "*" && r.Op != op {
			continue
		}
		if !matchFaultPath(r.Path, name) {
			continue
		}
		r.hits++
		if r.hits == r.After {
			return r, true
		}
		if r.hits > r.After && r.hits < r.After+r.Times {
			return r, true
		}
	}
	return nil, false
}

func (f *FaultFS) isFrozen(w *faultFile) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.frozen[w]
}

func (f *FaultFS) isPowerLost() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.powerLost
}

func (f *FaultFS) freeze(w *faultFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frozen[w] = true
}

func (f *FaultFS) thaw(w *faultFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.frozen, w)
}

func matchFaultPath(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	name = filepath.ToSlash(name)
	pattern = filepath.ToSlash(pattern)
	return strings.HasSuffix(name, pattern)
}

func (f *FaultFS) Create(name string) (File, error) {
	if r, ok := f.injected("create", name); ok && r.Mode == ModeFail {
		return nil, fmt.Errorf("%w: create %s", ErrInjected, name)
	}
	fl, err := f.base.Create(name)
	if err != nil {
		return nil, err
	}
	return f.wrap(fl), nil
}

func (f *FaultFS) Open(name string) (File, error) {
	if r, ok := f.injected("open", name); ok && r.Mode == ModeFail {
		return nil, fmt.Errorf("%w: open %s", ErrInjected, name)
	}
	fl, err := f.base.Open(name)
	if err != nil {
		return nil, err
	}
	return f.wrap(fl), nil
}

func (f *FaultFS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	if r, ok := f.injected("openfile", name); ok && r.Mode == ModeFail {
		return nil, fmt.Errorf("%w: openfile %s", ErrInjected, name)
	}
	fl, err := f.base.OpenFile(name, int(flag), perm)
	if err != nil {
		return nil, err
	}
	return f.wrap(fl), nil
}

func (f *FaultFS) Rename(oldname, newname string) error {
	if r, ok := f.injected("rename", oldname); ok {
		switch r.Mode {
		case ModeFail:
			return fmt.Errorf("%w: rename %s", ErrInjected, oldname)
		case ModeDrop:
			return nil
		}
	}
	return f.base.Rename(oldname, newname)
}

func (f *FaultFS) Remove(name string) error {
	if r, ok := f.injected("remove", name); ok {
		switch r.Mode {
		case ModeFail:
			return fmt.Errorf("%w: remove %s", ErrInjected, name)
		case ModeDrop:
			return nil
		}
	}
	return f.base.Remove(name)
}

func (f *FaultFS) RemoveAll(path string) error {
	if r, ok := f.injected("removeall", path); ok {
		switch r.Mode {
		case ModeFail:
			return fmt.Errorf("%w: removeall %s", ErrInjected, path)
		case ModeDrop:
			return nil
		}
	}
	return f.base.RemoveAll(path)
}

func (f *FaultFS) MkdirAll(path string, perm fs.FileMode) error {
	if r, ok := f.injected("mkdirall", path); ok {
		switch r.Mode {
		case ModeFail:
			return fmt.Errorf("%w: mkdirall %s", ErrInjected, path)
		case ModeDrop:
			return nil
		}
	}
	return f.base.MkdirAll(path, perm)
}

func (f *FaultFS) Stat(name string) (fs.FileInfo, error) {
	if r, ok := f.injected("stat", name); ok && r.Mode == ModeFail {
		return nil, fmt.Errorf("%w: stat %s", ErrInjected, name)
	}
	return f.base.Stat(name)
}

func (f *FaultFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if r, ok := f.injected("readdir", name); ok && r.Mode == ModeFail {
		return nil, fmt.Errorf("%w: readdir %s", ErrInjected, name)
	}
	return f.base.ReadDir(name)
}

func (f *FaultFS) SyncDir(name string) error {
	if r, ok := f.injected("syncdir", name); ok {
		switch r.Mode {
		case ModeFail:
			return fmt.Errorf("%w: syncdir %s", ErrInjected, name)
		case ModeDrop:
			return nil
		}
	}
	return f.base.SyncDir(name)
}

func (f *FaultFS) SyncFile(name string) error {
	if r, ok := f.injected("syncfile", name); ok {
		switch r.Mode {
		case ModeFail:
			return fmt.Errorf("%w: syncfile %s", ErrInjected, name)
		case ModeDrop:
			return nil
		}
	}
	return f.base.SyncFile(name)
}

func (f *FaultFS) wrap(fl File) *faultFile {
	w := &faultFile{fs: f, File: fl}
	f.mu.Lock()
	f.frozen[w] = false
	f.mu.Unlock()
	return w
}

type faultFile struct {
	fs *FaultFS
	File
}

func (w *faultFile) Write(p []byte) (int, error) {
	name := w.File.Name()
	r, ok := w.fs.injected("write", name)
	if ok && r.Mode == ModeFail {
		return 0, fmt.Errorf("%w: write %s", ErrInjected, name)
	}
	if w.fs.isFrozen(w) {
		// 断电后进程已死亡:一切操作失败。
		return 0, fmt.Errorf("%w: power lost after truncate on %s", ErrInjected, name)
	}
	n, err := w.File.Write(p)
	if err != nil {
		return n, err
	}
	if ok && r.Mode == ModeTruncate {
		if terr := w.File.Truncate(r.TruncSize); terr == nil {
			w.fs.mu.Lock()
			w.fs.frozen[w] = true
			w.fs.powerLost = true
			w.fs.mu.Unlock()
		}
	}
	return n, nil
}

func (w *faultFile) Sync() error {
	name := w.File.Name()
	r, ok := w.fs.injected("syncfile", name)
	if ok {
		switch r.Mode {
		case ModeFail:
			return fmt.Errorf("%w: syncfile %s", ErrInjected, name)
		case ModeDrop:
			return nil
		}
	}
	return w.File.Sync()
}

func (w *faultFile) Close() error {
	if w.fs.isPowerLost() {
		// 断电后进程死亡:操作系统关闭全部句柄,Close 必须真实释放,
		// 否则测试环境的文件删除会被阻塞。
		err := w.File.Close()
		w.fs.thaw(w)
		return err
	}
	name := w.File.Name()
	r, ok := w.fs.injected("close", name)
	if ok {
		switch r.Mode {
		case ModeFail:
			w.fs.thaw(w)
			return fmt.Errorf("%w: close %s", ErrInjected, name)
		case ModeDrop:
			w.fs.thaw(w)
			return nil
		}
	}
	err := w.File.Close()
	w.fs.thaw(w)
	return err
}
