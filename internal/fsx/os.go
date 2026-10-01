package fsx

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// osFS 是 FS 的真实实现,直接包装 os 包。
type osFS struct{}

func (osFS) Create(name string) (File, error) {
	f, err := os.Create(name)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

func (osFS) Open(name string) (File, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

func (osFS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	return osFile{f}, nil
}

func (osFS) Rename(oldname, newname string) error { return os.Rename(oldname, newname) }
func (osFS) Remove(name string) error             { return os.Remove(name) }
func (osFS) RemoveAll(path string) error          { return os.RemoveAll(path) }
func (osFS) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}
func (osFS) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }
func (osFS) ReadDir(name string) ([]fs.DirEntry, error) {
	return os.ReadDir(name)
}

// SyncDir 对目录执行 fsync。Windows 无法对目录句柄 FlushFileBuffers,
// 此时退化为 no-op(NTFS 元数据日志已保证目录项持久性)。
func (osFS) SyncDir(name string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(name)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

func (osFS) SyncFile(name string) error {
	f, err := os.OpenFile(name, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// osFile 将 *os.File 适配为 fsx.File。
type osFile struct{ f *os.File }

func (o osFile) Close() error               { return o.f.Close() }
func (o osFile) Read(p []byte) (int, error) { return o.f.Read(p) }
func (o osFile) ReadAt(p []byte, off int64) (int, error) {
	return o.f.ReadAt(p, off)
}
func (o osFile) Write(p []byte) (int, error) { return o.f.Write(p) }
func (o osFile) Name() string                { return o.f.Name() }
func (o osFile) Stat() (fs.FileInfo, error)  { return o.f.Stat() }
func (o osFile) Sync() error                 { return o.f.Sync() }
func (o osFile) Truncate(size int64) error   { return o.f.Truncate(size) }

var _ File = osFile{}

// RealPath 返回适用于 osFS 的绝对路径。仅 CLI 顶层使用。
func RealPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
