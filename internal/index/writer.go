package index

import (
	"fmt"
	"path/filepath"
	"sync"

	"lantern/internal/codec"
)

// Writer 是单写者会话。同一索引同时只有一个 Writer(跨进程由
// write.lock 保证)。
//
// 所有权:一个 Writer 及其 memtable 只被创建它的 goroutine 使用;
// 后台合并 goroutine 由 Writer 启动并在 Close 时停止,与 Flush 通过
// cm 互斥。
type Writer struct {
	ix        *Index
	lock      *WriteLock
	storeBody bool

	cm sync.Mutex // 串行化 Flush、Compact、Close 与后台合并

	mt *MemTable
	// pendingDel 是待删除的 (段, docID) 集合(去重)。
	pendingDel map[delKey]bool
	// pendingDelPaths 是显式 DeleteDoc 的路径(del 状态条目)。
	pendingDelPaths []string
	// stagedBits 暂存本次提交写盘的墓碑位集,提交成功后换入读取器。
	// 仅在持有 cm 时使用。
	stagedBits map[string]*codec.Bitset

	mergeCh   chan struct{}
	stopMerge chan struct{}
	mergeDone chan struct{}
}

type delKey struct {
	seg string
	doc uint32
}

// AcquireWriter 获取写锁并创建 Writer。若锁被他人新鲜持有则报错;
// 陈旧锁(>30s 无心跳)打印警告后接管。
func (ix *Index) AcquireWriter(storeBody bool) (*Writer, error) {
	lock, err := acquireWriteLock(ix.fsys, ix.root)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		ix:         ix,
		lock:       lock,
		storeBody:  storeBody,
		mt:         NewMemTable(),
		pendingDel: map[delKey]bool{},
		stagedBits: map[string]*codec.Bitset{},
		mergeCh:    make(chan struct{}, 1),
		stopMerge:  make(chan struct{}),
		mergeDone:  make(chan struct{}),
	}
	go w.mergeLoop()
	return w, nil
}

// AddDoc 加入或更新一个文档。若该路径已存在于索引,旧文档会被标记
// 删除;put 状态条目在提交时由新段自动生成。
func (w *Writer) AddDoc(d Doc) error {
	if d.Path == "" {
		return fmt.Errorf("index: doc path must not be empty")
	}
	if e, _, ok := w.ix.Lookup(d.Path); ok {
		w.pendingDel[delKey{e.Seg, e.DocID}] = true
	}
	w.mt.Add(d)
	return nil
}

// DeleteDoc 删除一个文档(写墓碑 + del 状态条目)。路径不存在时为 no-op。
func (w *Writer) DeleteDoc(path string) error {
	e, sr, ok := w.ix.Lookup(path)
	if !ok || sr == nil {
		return nil
	}
	w.pendingDel[delKey{e.Seg, e.DocID}] = true
	w.pendingDelPaths = append(w.pendingDelPaths, path)
	return nil
}

// Dirty 报告是否有未提交变更。
func (w *Writer) Dirty() bool {
	return w.mt.Len() > 0 || len(w.pendingDel) > 0
}

// Flush 提交全部未落库变更:memtable 刷成新段 + 墓碑升级 + 状态追加,
// 一次原子提交。空变更时为 no-op。
func (w *Writer) Flush() error {
	w.cm.Lock()
	defer w.cm.Unlock()
	return w.flushLocked()
}

func (w *Writer) flushLocked() error {
	if w.mt.Len() == 0 && len(w.pendingDel) == 0 {
		return nil
	}
	mtFinal := dedupeByPath(w.mt)
	plan := commitPlan{}
	if mtFinal.Len() > 0 {
		plan.newSegs = append(plan.newSegs, &pendingSegment{mt: mtFinal, storeBody: w.storeBody})
	}
	if len(w.pendingDel) > 0 {
		bySeg := map[string][]uint32{}
		for k := range w.pendingDel {
			bySeg[k.seg] = append(bySeg[k.seg], k.doc)
		}
		w.stagedBits = map[string]*codec.Bitset{}
		liveUpdates := make(map[string]uint32, len(bySeg))
		w.ix.mu.RLock()
		for _, sr := range w.ix.segs {
			docs, need := bySeg[sr.rec.ID]
			if !need {
				continue
			}
			bits := sr.r.CloneLiveWith(docs)
			w.stagedBits[sr.rec.ID] = bits
			liveUpdates[sr.rec.ID] = sr.LiveGen() + 1
		}
		w.ix.mu.RUnlock()
		plan.liveUpdates = liveUpdates
		plan.liveBits = w.stagedBits
	}
	for _, p := range w.pendingDelPaths {
		plan.stateEntries = append(plan.stateEntries, stateEntry{Op: "del", Path: p})
	}
	if err := w.ix.commit(plan); err != nil {
		w.stagedBits = map[string]*codec.Bitset{}
		return err
	}
	w.mt.Reset()
	w.pendingDel = map[delKey]bool{}
	w.pendingDelPaths = nil
	// 把新代墓碑换入内存读取器(内容与提交写入磁盘的一致)。
	w.ix.mu.Lock()
	for _, sr := range w.ix.segs {
		if gen, ok := plan.liveUpdates[sr.rec.ID]; ok {
			if bits := w.stagedBits[sr.rec.ID]; bits != nil {
				sr.r.SetLive(gen, bits)
			}
		}
	}
	w.ix.mu.Unlock()
	w.stagedBits = map[string]*codec.Bitset{}
	select {
	case w.mergeCh <- struct{}{}:
	default:
	}
	return nil
}

// Close 释放写锁并停止合并 goroutine。不自动 Flush。
func (w *Writer) Close() error {
	w.cm.Lock()
	close(w.stopMerge)
	w.cm.Unlock()
	<-w.mergeDone
	return w.lock.Release(w.ix.fsys)
}

// Compact 强制全量合并为单个段。
func (w *Writer) Compact() error {
	w.cm.Lock()
	defer w.cm.Unlock()
	if err := w.flushLocked(); err != nil {
		return err
	}
	return w.mergeLocked(w.ix.snapshotSegs())
}

// mergeLoop 后台合并 goroutine(规格 5.9):每次 Flush 后检查分层策略。
func (w *Writer) mergeLoop() {
	defer close(w.mergeDone)
	for {
		select {
		case <-w.stopMerge:
			return
		case <-w.mergeCh:
			w.cm.Lock()
			victims := pickMergeTier(w.ix.snapshotSegs())
			if len(victims) >= 2 {
				// 后台合并失败不致命:下次提交后重试。
				_ = w.mergeLocked(victims)
			}
			w.cm.Unlock()
		}
	}
}

// snapshotSegs 在读锁下复制当前段引用列表。
func (ix *Index) snapshotSegs() []*SegmentRef {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]*SegmentRef, len(ix.segs))
	copy(out, ix.segs)
	return out
}

// dedupeByPath 返回一个 memtable:同路径仅保留最后一次 AddDoc。
func dedupeByPath(mt *MemTable) *MemTable {
	last := map[string]int{}
	for i := range mt.docs {
		last[mt.docs[i].doc.Path] = i
	}
	if len(last) == len(mt.docs) {
		return mt
	}
	out := NewMemTable()
	for i := range mt.docs {
		if last[mt.docs[i].doc.Path] == i {
			out.Add(mt.docs[i].doc)
		}
	}
	return out
}

// segDir 返回段目录。
func (ix *Index) segDir(id string) string { return filepath.Join(ix.root, id) }
