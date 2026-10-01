package index

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"lantern/internal/fsx"
)

// 写锁参数(规格 5.7):心跳 5s,超过 30s 无心跳视为陈旧。
const (
	LockHeartbeatInterval = 5 * time.Second
	LockStaleAfter        = 30 * time.Second
)

// lockContent 是 write.lock 的内容。
type lockContent struct {
	PID       int       `json:"pid"`
	Hostname  string    `json:"hostname"`
	StartedAt time.Time `json:"started_at"`
	Heartbeat time.Time `json:"heartbeat"`
}

// lockSize 是锁文件的固定写入宽度(心跳原位覆写)。
const lockSize = 256

// WriteLock 是单写者锁。持有期间后台 goroutine 每 5s 刷新心跳。
type WriteLock struct {
	f     fsx.File
	path  string
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
	Owner lockContent
}

// acquireWriteLock 尝试取得写锁。若锁文件存在且心跳新鲜则失败;
// 陈旧锁打印警告后接管。
func acquireWriteLock(fsys fsx.FS, root string) (*WriteLock, error) {
	path := filepath.Join(root, lockFile)
	content := lockContent{
		PID:       os.Getpid(),
		StartedAt: time.Now(),
		Heartbeat: time.Now(),
	}
	content.Hostname, _ = os.Hostname()

	f, err := fsys.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		// 锁已存在:检查陈旧。
		stale, owner, serr := lockIsStale(fsys, path)
		if serr != nil {
			return nil, fmt.Errorf("index: inspect write.lock: %w", serr)
		}
		if !stale {
			return nil, fmt.Errorf("index: index is locked by pid %d on %s (heartbeat %s)",
				owner.PID, owner.Hostname, owner.Heartbeat.Format(time.RFC3339))
		}
		fmt.Printf("lantern: 警告:接管陈旧写锁(pid %d on %s, 心跳 %s)\n",
			owner.PID, owner.Hostname, owner.Heartbeat.Format(time.RFC3339))
		if err := fsys.Remove(path); err != nil {
			return nil, fmt.Errorf("index: remove stale write.lock: %w", err)
		}
		f, err = fsys.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("index: reacquire write.lock: %w", err)
		}
	}
	l := &WriteLock{f: f, path: path, stop: make(chan struct{}), done: make(chan struct{}), Owner: content}
	if err := l.writeHeartbeat(); err != nil {
		f.Close()
		return nil, err
	}
	go l.heartbeatLoop()
	return l, nil
}

// writeHeartbeat 原位覆写锁内容(定长 JSON,补空格对齐)。
func (l *WriteLock) writeHeartbeat() error {
	l.Owner.Heartbeat = time.Now()
	b, err := json.Marshal(&l.Owner)
	if err != nil {
		return fmt.Errorf("index: marshal lock content: %w", err)
	}
	if len(b) > lockSize {
		return fmt.Errorf("index: lock content too large")
	}
	buf := make([]byte, lockSize)
	copy(buf, b)
	if _, err := l.f.WriteAt(buf, 0); err != nil {
		return fmt.Errorf("index: write lock heartbeat: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("index: sync lock heartbeat: %w", err)
	}
	return nil
}

func (l *WriteLock) heartbeatLoop() {
	defer close(l.done)
	t := time.NewTicker(LockHeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-t.C:
			// 心跳失败不致命(进程仍在,Close 时会释放文件)。
			_ = l.writeHeartbeat()
		}
	}
}

// Release 停止心跳并删除锁文件。
func (l *WriteLock) Release(fsys fsx.FS) error {
	l.once.Do(func() { close(l.stop) })
	<-l.done
	err := l.f.Close()
	if rerr := fsys.Remove(l.path); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// lockIsStale 读取锁文件并判断心跳是否超过 30s。
func lockIsStale(fsys fsx.FS, path string) (bool, lockContent, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return false, lockContent{}, err
	}
	buf := make([]byte, lockSize)
	n, _ := f.Read(buf)
	f.Close()
	var c lockContent
	if err := json.Unmarshal(trimSpaceBytes(buf[:n]), &c); err != nil {
		// 内容损坏:视为陈旧。
		return true, lockContent{}, nil
	}
	if c.Heartbeat.IsZero() {
		return true, c, nil
	}
	return time.Since(c.Heartbeat) > LockStaleAfter, c, nil
}

func trimSpaceBytes(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == 0) {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == 0 || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}
