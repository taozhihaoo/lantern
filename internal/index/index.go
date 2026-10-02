package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"lantern/internal/codec"
	"lantern/internal/fsx"
)

// Index 是指向一个索引目录的打开句柄。
//
// 并发所有权:
//   - 写路径(Writer)受 write.lock 串行化,进程内再由 Writer.cm 互斥;
//   - 读路径通过 Snapshot 持有段引用计数,manifest 切换不影响进行中的
//     搜索;被移出 manifest 的段在引用归零后物理删除;
//   - ix.mu 保护 segs/gen/state 等可变字段。
type Index struct {
	root  string
	fsys  fsx.FS
	cache *BlockCache

	mu        sync.RWMutex
	segs      []*SegmentRef
	gen       uint64
	stateLen  int64
	nextSegID uint64
	state     *stateTable
	closed    bool
}

// SegmentRef 是 manifest 中一个段的活引用。
// rec 创建后不可变;refs 与 liveGen 使用原子量(读路径仅持读锁)。
type SegmentRef struct {
	rec     segmentRecord
	r       *SegmentReader
	refs    atomic.Int32
	removed bool
	liveGen atomic.Uint32
}

// Reader 返回段读取器(生命周期由 Snapshot 保证)。
func (sr *SegmentRef) Reader() *SegmentReader { return sr.r }

// ID 返回段 ID。
func (sr *SegmentRef) ID() string { return sr.rec.ID }

// LiveGen 返回段的墓碑代。
func (sr *SegmentRef) LiveGen() uint32 { return sr.liveGen.Load() }

// Open 打开(或初始化)一个索引目录并执行启动恢复:
// 清理孤儿 tmp 与未被 manifest 引用的段目录,截断 state.jsonl 到
// manifest 记录的有效前缀。
func Open(root string, fsys fsx.FS) (*Index, error) {
	if err := fsys.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("index: mkdir root: %w", err)
	}
	if err := recoverDir(fsys, root); err != nil {
		return nil, err
	}
	ix := &Index{
		root:  root,
		fsys:  fsys,
		cache: NewBlockCache(0),
		state: newStateTable(),
	}
	if _, err := fsys.Stat(filepath.Join(root, manifestFile)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ix, nil // 全新索引
		}
		return nil, fmt.Errorf("index: stat manifest: %w", err)
	}
	m, err := readManifest(fsys, root)
	if err != nil {
		return nil, err
	}
	ix.gen = m.Generation
	ix.stateLen = m.StateLen
	ix.nextSegID = m.NextSegID
	for _, rec := range m.Segments {
		dir := filepath.Join(root, rec.ID)
		r, err := OpenSegment(fsys, dir, rec.ID, ix.cache)
		if err != nil {
			ix.closeAll()
			return nil, err
		}
		if rec.LiveGen > 0 {
			bits, err := readLiveBits(fsys, dir, rec.LiveGen)
			if err != nil {
				r.Close()
				ix.closeAll()
				return nil, err
			}
			r.SetLive(rec.LiveGen, bits)
		}
		ref := &SegmentRef{rec: rec, r: r}
		ref.refs.Store(1)
		ref.liveGen.Store(rec.LiveGen)
		ix.segs = append(ix.segs, ref)
	}
	if err := ix.loadState(); err != nil {
		ix.closeAll()
		return nil, err
	}
	return ix, nil
}

// recoverDir 执行目录级恢复:删除 manifest tmp、孤儿段与孤儿墓碑文件,
// 截断 state.jsonl。
func recoverDir(fsys fsx.FS, root string) error {
	ents, err := fsys.ReadDir(root)
	if err != nil {
		return fmt.Errorf("index: read index dir: %w", err)
	}
	var m *manifest
	if _, err := fsys.Stat(filepath.Join(root, manifestFile)); err == nil {
		m, err = readManifest(fsys, root)
		if err != nil {
			return fmt.Errorf("index: manifest unreadable, refusing recovery: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("index: stat manifest: %w", err)
	}

	known := map[string]uint32{}
	if m != nil {
		for _, rec := range m.Segments {
			known[rec.ID] = rec.LiveGen
		}
	}
	for _, e := range ents {
		name := e.Name()
		full := filepath.Join(root, name)
		switch {
		case strings.HasPrefix(name, "manifest.") && strings.HasSuffix(name, tmpSuffix):
			if err := fsys.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("index: remove orphan manifest tmp: %w", err)
			}
		case strings.HasPrefix(name, segPrefix) && strings.HasSuffix(name, tmpSuffix) && e.IsDir():
			if err := fsys.RemoveAll(full); err != nil {
				return fmt.Errorf("index: remove orphan tmp segment: %w", err)
			}
		case strings.HasPrefix(name, segPrefix) && e.IsDir():
			if _, isKnown := known[name]; !isKnown {
				if err := fsys.RemoveAll(full); err != nil {
					return fmt.Errorf("index: remove orphan segment: %w", err)
				}
			}
			// 已知段的旧代墓碑文件保留不删(文件极小;Windows 不允许
			// 删除打开中的文件,保留历史是最简单且崩溃安全的方案)。
		}
	}
	// state.jsonl 截断到有效前缀。
	statePath := filepath.Join(root, stateFile)
	if m == nil {
		if err := fsys.Remove(statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("index: remove state without manifest: %w", err)
		}
		return nil
	}
	st, err := fsys.Stat(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && m.StateLen == 0 {
			return nil
		}
		return fmt.Errorf("index: stat state.jsonl: %w", err)
	}
	if st.Size() < m.StateLen {
		return fmt.Errorf("index: state.jsonl (%d) shorter than manifest record (%d)",
			st.Size(), m.StateLen)
	}
	if st.Size() > m.StateLen {
		f, err := fsys.OpenFile(statePath, os.O_RDWR, 0o644)
		if err != nil {
			return fmt.Errorf("index: open state.jsonl for truncate: %w", err)
		}
		if err := f.Truncate(m.StateLen); err != nil {
			f.Close()
			return fmt.Errorf("index: truncate state.jsonl: %w", err)
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("index: sync state.jsonl: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("index: close state.jsonl: %w", err)
		}
	}
	return nil
}

func readLiveBits(fsys fsx.FS, dir string, gen uint32) (*codec.Bitset, error) {
	data, err := readWholeFile(fsys, filepath.Join(dir, liveName(gen)))
	if err != nil {
		return nil, fmt.Errorf("index: read live bits: %w", err)
	}
	payload, _, err := codec.ParseFooter(data)
	if err != nil {
		return nil, fmt.Errorf("index: live bits of gen %d: %w", gen, err)
	}
	return codec.DecodeBitset(payload)
}

// loadState 从 state.jsonl 的有效前缀构建内存状态表。
func (ix *Index) loadState() error {
	if ix.stateLen == 0 {
		return nil
	}
	data, err := readWholeFile(ix.fsys, filepath.Join(ix.root, stateFile))
	if err != nil {
		return fmt.Errorf("index: load state: %w", err)
	}
	if int64(len(data)) < ix.stateLen {
		return fmt.Errorf("index: state.jsonl shorter than manifest record")
	}
	var entries []stateEntry
	for _, line := range strings.Split(string(data[:ix.stateLen]), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e stateEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return fmt.Errorf("index: corrupt state entry: %w", err)
		}
		entries = append(entries, e)
	}
	ix.state.apply(entries)
	return nil
}

func (ix *Index) closeAll() {
	for _, sr := range ix.segs {
		sr.r.Close()
	}
	ix.segs = nil
}

// Close 释放全部资源(不删除数据)。并发进行中的 Snapshot 仍可安全读。
func (ix *Index) Close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.closed = true
	var firstErr error
	for _, sr := range ix.segs {
		if err := sr.r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Lookup 返回路径的状态条目与所在段。
func (ix *Index) Lookup(path string) (stateEntry, *SegmentRef, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	e, ok := ix.state.m[path]
	if !ok {
		return stateEntry{}, nil, false
	}
	for _, sr := range ix.segs {
		if sr.rec.ID == e.Seg {
			return e, sr, true
		}
	}
	return e, nil, false
}

// StateLen 返回状态表大小(存活文档数)。
func (ix *Index) StateLen() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.state.m)
}

// Snapshot 获取当前 manifest 对应的段集合快照并引用计数(规格 5.8)。
func (ix *Index) Snapshot() (*Snapshot, error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ix.closed {
		return nil, fmt.Errorf("index: index closed")
	}
	segs := make([]*SegmentRef, len(ix.segs))
	for i, sr := range ix.segs {
		sr.refs.Add(1)
		segs[i] = sr
	}
	return &Snapshot{ix: ix, segs: segs}, nil
}

// Snapshot 是一个不可变段集合视图。
type Snapshot struct {
	ix   *Index
	segs []*SegmentRef
}

// Segments 返回快照内的段(只读)。
func (s *Snapshot) Segments() []*SegmentRef { return s.segs }

// Close 释放引用;引用归零且段已移出 manifest 时物理删除段目录。
func (s *Snapshot) Close() {
	var gc []*SegmentRef
	s.ix.mu.Lock()
	for _, sr := range s.segs {
		if sr.refs.Add(-1) <= 0 && sr.removed {
			gc = append(gc, sr)
		}
	}
	s.ix.mu.Unlock()
	for _, sr := range gc {
		sr.r.Close()
		_ = s.ix.fsys.RemoveAll(filepath.Join(s.ix.root, sr.rec.ID))
	}
}

// commitPlan 描述一次原子提交的全部变更。
type commitPlan struct {
	newSegs      []*pendingSegment
	liveUpdates  map[string]uint32        // segID → 新墓碑代
	liveBits     map[string]*codec.Bitset // segID → 写盘的墓碑位集
	removeSegs   map[string]bool          // 合并移除
	stateEntries []stateEntry
}

type pendingSegment struct {
	mt        *MemTable
	storeBody bool
}

// assignSegmentID 分配下一个段 ID(调用方持锁)。
func (ix *Index) assignSegmentID() string {
	ix.nextSegID++
	return fmt.Sprintf("%s%06d", segPrefix, ix.nextSegID)
}

// commit 执行规格 5.6 的原子提交协议(调用方持有写锁):
//
//	写新段到 tmp → fsync 各文件 → fsync 目录 → rename 到最终段目录 →
//	追加 state(有 state_len 保护)→ 写 manifest.<gen>.tmp → fsync →
//	rename 为 manifest.json → fsync 索引目录
//
// 失败时的处理:重读磁盘上的 manifest——若新 manifest 已落盘(失败发生
// 在最后的目录 fsync 上),按成功切换内存状态;否则保留旧状态,新段目录
// 成为孤儿(下次 Open 恢复时清理)。
func (ix *Index) commit(plan commitPlan) (err error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	newGen := ix.gen + 1
	newRecords := make([]segmentRecord, 0, len(plan.newSegs))

	defer func() {
		if err == nil {
			return
		}
		// 失败收尾:判断提交点是否实际已越过。
		if dm, derr := readManifest(ix.fsys, ix.root); derr == nil && dm.Generation == newGen {
			// manifest 已提交:按成功完成内存切换(文件级收尾)。
			ix.finishCommit(plan, newRecords, newGen)
			err = nil
		}
	}()

	// 1. 写段(tmp → 文件 fsync(WriteSegment 内) → 目录 fsync → rename)。
	// put 状态条目在段 ID 确定后由此生成(seg/docID 才可知)。
	stateEntries := plan.stateEntries
	for _, ps := range plan.newSegs {
		id := ix.assignSegmentID()
		tmpDir := filepath.Join(ix.root, id+tmpSuffix)
		finalDir := filepath.Join(ix.root, id)
		if _, werr := WriteSegment(ix.fsys, tmpDir, ps.mt, ps.storeBody); werr != nil {
			return werr
		}
		if serr := ix.fsys.SyncDir(tmpDir); serr != nil {
			return fmt.Errorf("index: sync segment tmp dir: %w", serr)
		}
		if rerr := ix.fsys.Rename(tmpDir, finalDir); rerr != nil {
			return fmt.Errorf("index: rename segment %s: %w", id, rerr)
		}
		sums, cerr := checksumSegment(ix.fsys, finalDir)
		if cerr != nil {
			return cerr
		}
		newRecords = append(newRecords, segmentRecord{ID: id, LiveGen: 0, Files: sums})
		for i := range ps.mt.docs {
			d := &ps.mt.docs[i].doc
			stateEntries = append(stateEntries, stateEntry{
				Op:    "put",
				Path:  d.Path,
				Size:  d.Size,
				MTime: d.MTime,
				SHA:   fmt.Sprintf("%x", d.SHA256),
				Seg:   id,
				DocID: uint32(i),
			})
		}
	}

	// 2. 墓碑升级:为受影响段写 live_<gen+1>.bits(新增文件,不改旧文件)。
	// 回写 plan.stateEntries 为完整条目集(put+del),供提交成功后
	// 更新内存状态表使用。
	plan.stateEntries = stateEntries
	for _, sr := range ix.segs {
		if plan.removeSegs[sr.rec.ID] {
			continue
		}
		if gen, ok := plan.liveUpdates[sr.rec.ID]; ok {
			bits := plan.liveBits[sr.rec.ID]
			if bits == nil {
				bits = sr.r.CloneLive()
			}
			data := codec.AppendBitset(nil, bits)
			name := liveName(gen)
			dir := filepath.Join(ix.root, sr.rec.ID)
			if werr := writeFileSynced(ix.fsys, dir, name, codec.AppendFooter(data, FormatVersion)); werr != nil {
				return werr
			}
		}
	}

	// 3. 组装新 manifest。
	segs := make([]segmentRecord, 0, len(ix.segs)+len(newRecords))
	for _, sr := range ix.segs {
		if plan.removeSegs[sr.rec.ID] {
			continue
		}
		rec := sr.rec
		if _, ok := plan.liveUpdates[sr.rec.ID]; ok {
			rec.LiveGen = plan.liveUpdates[sr.rec.ID]
			sums, cerr := checksumSegment(ix.fsys, filepath.Join(ix.root, rec.ID))
			if cerr != nil {
				return cerr
			}
			rec.Files = sums
		}
		segs = append(segs, rec)
	}
	segs = append(segs, newRecords...)
	stateBytes := encodeStateEntries(stateEntries)
	stateLen := ix.stateLen + int64(len(stateBytes))
	m := &manifest{
		Format:     FormatVersion,
		Generation: newGen,
		StateLen:   stateLen,
		NextSegID:  ix.nextSegID,
		Segments:   segs,
	}

	// 4. 追加 state(位于 manifest rename 之前)。
	if aerr := appendState(ix.fsys, ix.root, stateBytes); aerr != nil {
		return aerr
	}

	// 5. 原子提交点:manifest rename + 目录 fsync。
	if werr := writeManifestAtomic(ix.fsys, ix.root, m); werr != nil {
		return werr
	}

	ix.finishCommit(plan, newRecords, newGen)
	return nil
}

// finishCommit 在提交点成功后切换内存状态并回收旧段。
// 调用方必须持有 ix.mu。
func (ix *Index) finishCommit(plan commitPlan, newRecords []segmentRecord, newGen uint64) {
	removed := make([]*SegmentRef, 0)
	kept := make([]*SegmentRef, 0, len(ix.segs)+len(newRecords))
	for _, sr := range ix.segs {
		if plan.removeSegs[sr.rec.ID] {
			sr.removed = true
			// 释放索引自身引用
			if sr.refs.Add(-1) <= 0 {
				removed = append(removed, sr)
			}
			continue
		}
		if g, ok := plan.liveUpdates[sr.rec.ID]; ok {
			sr.liveGen.Store(g)
		}
		kept = append(kept, sr)
	}
	for _, rec := range newRecords {
		r, err := OpenSegment(ix.fsys, filepath.Join(ix.root, rec.ID), rec.ID, ix.cache)
		if err != nil {
			// 刚写完即打不开属于严重错误;保留 panic 之外的兜底:跳过该段。
			// (commit 的调用方会在下次 check 时发现段缺失。)
			continue
		}
		nref := &SegmentRef{rec: rec, r: r}
		nref.refs.Store(1)
		nref.liveGen.Store(rec.LiveGen)
		kept = append(kept, nref)
	}
	ix.segs = kept
	ix.gen = newGen
	ix.stateLen += int64(len(encodeStateEntries(plan.stateEntries)))
	ix.state.apply(plan.stateEntries)
	for _, sr := range removed {
		sr.r.Close()
		_ = ix.fsys.RemoveAll(filepath.Join(ix.root, sr.rec.ID))
	}
}

// Root 返回索引根目录。
func (ix *Index) Root() string { return ix.root }

// SetFS 替换底层文件系统(仅测试使用:在打开后切换为故障注入 FS)。
func (ix *Index) SetFS(f fsx.FS) { ix.fsys = f }

// StatePaths 返回状态表中全部存活路径(排序)。
func (ix *Index) StatePaths() []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.state.paths()
}

// StateInfo 是路径状态的可读摘要(SHA 为十六进制)。
type StateInfo struct {
	Size  int64
	MTime int64
	SHA   string
}

// StateInfoOf 返回路径的状态摘要(增量扫描对比用)。
func (ix *Index) StateInfoOf(path string) (StateInfo, bool) {
	e, _, ok := ix.Lookup(path)
	if !ok {
		return StateInfo{}, false
	}
	return StateInfo{Size: e.Size, MTime: e.MTime, SHA: e.SHA}, true
}
