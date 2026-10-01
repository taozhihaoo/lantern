package index

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lantern/internal/fsx"
)

// manifestFile / stateFile / lockFile / tmpSuffix。
const (
	manifestFile = "manifest.json"
	stateFile    = "state.jsonl"
	lockFile     = "write.lock"
	tmpSuffix    = ".tmp"
	segPrefix    = "seg_"
)

// fileChecksum 记录段文件的大小与 CRC32(对含 footer 的完整文件字节)。
type fileChecksum struct {
	Size  int64  `json:"size"`
	CRC32 uint32 `json:"crc32"`
}

// segmentRecord 是 manifest 中的一个段。
type segmentRecord struct {
	ID      string                  `json:"id"`
	LiveGen uint32                  `json:"live_gen"`
	Files   map[string]fileChecksum `json:"files"`
}

// manifest 是原子提交的元数据文件内容。
type manifest struct {
	Format     uint32          `json:"format"`
	Generation uint64          `json:"generation"`
	StateLen   int64           `json:"state_len"`
	NextSegID  uint64          `json:"next_seg_id"`
	Segments   []segmentRecord `json:"segments"`
}

// readManifest 读取并解析 manifest.json。
func readManifest(fsys fsx.FS, root string) (*manifest, error) {
	data, err := readWholeFile(fsys, filepath.Join(root, manifestFile))
	if err != nil {
		return nil, fmt.Errorf("index: read manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("index: parse manifest: %w", err)
	}
	if m.Format != FormatVersion {
		return nil, fmt.Errorf("index: manifest format %d != %d", m.Format, FormatVersion)
	}
	return &m, nil
}

// writeManifestAtomic 按提交协议写入 manifest:
// 写 manifest.<gen>.tmp → fsync → rename 为 manifest.json → fsync 索引目录。
func writeManifestAtomic(fsys fsx.FS, root string, m *manifest) error {
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return fmt.Errorf("index: marshal manifest: %w", err)
	}
	tmp := filepath.Join(root, fmt.Sprintf("manifest.%d%s", m.Generation, tmpSuffix))
	f, err := fsys.Create(tmp)
	if err != nil {
		return fmt.Errorf("index: create manifest tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("index: write manifest tmp: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("index: sync manifest tmp: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("index: close manifest tmp: %w", err)
	}
	if err := fsys.Rename(tmp, filepath.Join(root, manifestFile)); err != nil {
		return fmt.Errorf("index: rename manifest: %w", err)
	}
	if err := fsys.SyncDir(root); err != nil {
		return fmt.Errorf("index: sync index dir: %w", err)
	}
	return nil
}

// checksumSegment 枚举段目录内全部文件并计算校验和。
func checksumSegment(fsys fsx.FS, dir string) (map[string]fileChecksum, error) {
	ents, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("index: checksum read dir: %w", err)
	}
	out := make(map[string]fileChecksum, len(ents))
	for _, e := range ents {
		if e.IsDir() {
			return nil, fmt.Errorf("index: checksum: unexpected subdir in %s", dir)
		}
		data, err := readWholeFile(fsys, filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("index: checksum %s: %w", e.Name(), err)
		}
		out[e.Name()] = fileChecksum{Size: int64(len(data)), CRC32: crc32.ChecksumIEEE(data)}
	}
	return out, nil
}

// stateEntry 是 state.jsonl 的一条记录。
type stateEntry struct {
	Op    string `json:"op"` // "put" | "del"
	Path  string `json:"path"`
	Size  int64  `json:"size,omitempty"`
	MTime int64  `json:"mtime,omitempty"`
	SHA   string `json:"sha,omitempty"`
	Seg   string `json:"seg,omitempty"`
	DocID uint32 `json:"doc,omitempty"`
}

// stateTable 是内存中的文件状态表:path → 条目(put 生效)。
type stateTable struct {
	m map[string]stateEntry
}

func newStateTable() *stateTable { return &stateTable{m: map[string]stateEntry{}} }

// apply 把一批条目按顺序应用到内存表。
func (t *stateTable) apply(entries []stateEntry) {
	for _, e := range entries {
		if e.Op == "del" {
			delete(t.m, e.Path)
			continue
		}
		t.m[e.Path] = e
	}
}

// paths 返回排序后的存活路径。
func (t *stateTable) paths() []string {
	out := make([]string, 0, len(t.m))
	for p := range t.m {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// encodeStateEntries 编码一批 state 条目为 jsonl 字节。
func encodeStateEntries(entries []stateEntry) []byte {
	var buf strings.Builder
	for _, e := range entries {
		b, err := json.Marshal(&e)
		if err != nil {
			continue // 结构固定,不会发生
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return []byte(buf.String())
}

// appendState 追加状态条目并刷盘(提交协议中位于 manifest rename 之前,
// 由 manifest 的 state_len 标记有效前缀)。
func appendState(fsys fsx.FS, root string, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	f, err := fsys.OpenFile(filepath.Join(root, stateFile),
		os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("index: open state.jsonl: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("index: append state.jsonl: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("index: sync state.jsonl: %w", err)
	}
	return f.Close()
}
