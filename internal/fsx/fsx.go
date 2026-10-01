// Package fsx 定义 Lantern 所有磁盘 IO 的抽象接口。
//
// 索引引擎不允许直接调用 os 包做文件操作,一律通过本包的 FS 接口,
// 以便在测试中注入故障(FaultFS)来验证崩溃安全。
package fsx

import (
	"errors"
	"io"
	"io/fs"
)

// File 是 FS 打开的文件句柄。读段文件使用 ReadAt(无偏移副作用),
// 写段文件按顺序 Write 并在完成后 Sync。
type File interface {
	io.Closer
	io.Reader
	io.ReaderAt
	io.Writer
	Name() string
	Stat() (fs.FileInfo, error)
	Sync() error
	Truncate(size int64) error
	WriteAt(p []byte, off int64) (int, error)
}

// FS 是 Lantern 的文件系统抽象。所有索引 IO 必须经由该接口。
type FS interface {
	// Create 以 O_CREATE|O_TRUNC|O_WRONLY 打开(或创建)文件。
	Create(name string) (File, error)
	// Open 以只读方式打开文件。
	Open(name string) (File, error)
	// OpenFile 按 flag(与 os.OpenFile 语义一致)打开文件,
	// 写锁等场景依赖 O_CREATE|O_EXCL。
	OpenFile(name string, flag int, perm fs.FileMode) (File, error)
	Rename(oldname, newname string) error
	Remove(name string) error
	RemoveAll(path string) error
	MkdirAll(path string, perm fs.FileMode) error
	Stat(name string) (fs.FileInfo, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	// SyncDir 刷目录项(POSIX fsync(dir))。在不支持目录 fsync 的
	// 平台(Windows)为尽力而为的 no-op,见 DECISIONS.md。
	SyncDir(name string) error
	// SyncFile 打开指定文件、刷盘并关闭。常规写入路径应直接调用
	// File.Sync;本方法供工具路径使用。
	SyncFile(name string) error
}

// ErrInjected 标识一次由 FaultFS 人为注入的故障。
var ErrInjected = errors.New("fsx: injected fault")

// OsFS 返回基于 os 包的真实文件系统实现。
func OsFS() FS { return osFS{} }
