package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"lantern/internal/codec"
	"lantern/internal/fsx"
)

// Stats 是 lantern stats 的数据。
type Stats struct {
	Docs     int                // 状态表中的存活文档数
	Terms    int                // 全部段的去重 (字段,词项) 数
	Segments int                // manifest 中的段数
	Bytes    int64              // 索引目录磁盘占用
	AvgLen   [NumFields]float64 // 各字段平均长度(按含该字段的文档)
}

// Stats 汇总索引统计。
func (ix *Index) Stats() (Stats, error) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	st := Stats{Docs: len(ix.state.m), Segments: len(ix.segs)}
	seen := map[string]bool{}
	var docCount [NumFields]uint64
	var totalLen [NumFields]uint64
	for _, sr := range ix.segs {
		fs := sr.r.FieldStats()
		for f := Field(0); f < NumFields; f++ {
			docCount[f] += uint64(fs[f].DocCount)
			totalLen[f] += fs[f].TotalLen
		}
		for f := Field(0); f < NumFields; f++ {
			err := sr.r.Terms(f, func(term string, _ TermEntry) bool {
				seen[f.Name()+"\x00"+term] = true
				return true
			})
			if err != nil {
				return st, fmt.Errorf("index: stats terms: %w", err)
			}
		}
	}
	st.Terms = len(seen)
	for f := Field(0); f < NumFields; f++ {
		if docCount[f] > 0 {
			st.AvgLen[f] = float64(totalLen[f]) / float64(docCount[f])
		}
	}
	var err error
	st.Bytes, err = dirSize(ix.fsys, ix.root)
	if err != nil {
		return st, err
	}
	return st, nil
}

func dirSize(fsys fsx.FS, root string) (int64, error) {
	var total int64
	ents, err := fsys.ReadDir(root)
	if err != nil {
		return 0, fmt.Errorf("index: dirsize: %w", err)
	}
	for _, e := range ents {
		full := filepath.Join(root, e.Name())
		if e.IsDir() {
			n, err := dirSize(fsys, full)
			if err != nil {
				return 0, err
			}
			total += n
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

// CheckReport 是 CheckIndex 的结果。
type CheckReport struct {
	Generation uint64
	Segments   int
	Docs       int
	Errors     []string
	Warnings   []string
}

// OK 报告是否通过全部检查。
func (r *CheckReport) OK() bool { return len(r.Errors) == 0 }

// CheckIndex 完整校验:manifest 可解析、各段文件 CRC 与 meta 一致、
// 段文件校验和与 manifest 一致、无孤儿文件、状态表指向的文档存活。
func CheckIndex(fsys fsx.FS, root string) (*CheckReport, error) {
	rep := &CheckReport{}
	m, err := readManifest(fsys, root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			rep.Warnings = append(rep.Warnings, "索引为空(无 manifest)")
			return rep, nil
		}
		return nil, err
	}
	rep.Generation = m.Generation
	rep.Segments = len(m.Segments)
	known := map[string]bool{}
	for _, rec := range m.Segments {
		known[rec.ID] = true
		dir := filepath.Join(root, rec.ID)
		if _, err := fsys.Stat(dir); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("段 %s 目录缺失", rec.ID))
			continue
		}
		if err := CheckSegment(fsys, dir, rec.ID, rec.LiveGen); err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			continue
		}
		sums, err := checksumSegment(fsys, dir)
		if err != nil {
			rep.Errors = append(rep.Errors, err.Error())
			continue
		}
		if len(sums) != len(rec.Files) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("段 %s 文件数与 manifest 不一致: %d != %d",
				rec.ID, len(sums), len(rec.Files)))
		}
		for name, fc := range rec.Files {
			got, ok := sums[name]
			if !ok {
				rep.Errors = append(rep.Errors, fmt.Sprintf("段 %s 缺少文件 %s", rec.ID, name))
				continue
			}
			if got != fc {
				rep.Errors = append(rep.Errors, fmt.Sprintf("段 %s 文件 %s 校验和不一致", rec.ID, name))
			}
		}
	}
	// 孤儿文件检查。
	ents, err := fsys.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, "manifest.") && strings.HasSuffix(name, tmpSuffix):
			rep.Errors = append(rep.Errors, fmt.Sprintf("孤儿文件 %s(应由恢复清理)", name))
		case strings.HasPrefix(name, segPrefix) && strings.HasSuffix(name, tmpSuffix):
			rep.Errors = append(rep.Errors, fmt.Sprintf("孤儿临时段 %s(应由恢复清理)", name))
		case strings.HasPrefix(name, segPrefix) && e.IsDir() && !known[name]:
			rep.Errors = append(rep.Errors, fmt.Sprintf("孤儿段目录 %s(应由恢复清理)", name))
		}
	}
	// state.jsonl 前缀检查。
	if st, err := fsys.Stat(filepath.Join(root, stateFile)); err == nil {
		if st.Size() > m.StateLen {
			rep.Warnings = append(rep.Warnings,
				fmt.Sprintf("state.jsonl 有 %d 字节未提交尾部(可由恢复清理)", st.Size()-m.StateLen))
		}
	}
	rep.Docs = countStateDocs(fsys, root, m, rep)
	return rep, nil
}

// countStateDocs 解析状态表有效前缀,校验每条 put 的 (段, docID) 存活。
func countStateDocs(fsys fsx.FS, root string, m *manifest, rep *CheckReport) int {
	data, err := readWholeFile(fsys, filepath.Join(root, stateFile))
	if err != nil {
		if m.StateLen > 0 {
			rep.Errors = append(rep.Errors, "state.jsonl 缺失但 manifest 记录非空")
		}
		return 0
	}
	if int64(len(data)) < m.StateLen {
		rep.Errors = append(rep.Errors, "state.jsonl 比 manifest 记录短")
		return 0
	}
	segByID := map[string]segmentRecord{}
	for _, rec := range m.Segments {
		segByID[rec.ID] = rec
	}
	// 先按序应用全部条目,得到每路径的最终状态;再校验最终状态
	// (旧 put 条目指向已被合并删除的段是正常历史)。
	final := map[string]stateEntry{}
	order := []string{}
	for _, line := range strings.Split(string(data[:m.StateLen]), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e stateEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("状态条目损坏: %v", err))
			continue
		}
		if e.Op == "del" {
			if _, seen := final[e.Path]; !seen {
				order = append(order, e.Path)
			}
			delete(final, e.Path)
			continue
		}
		if _, seen := final[e.Path]; !seen {
			order = append(order, e.Path)
		}
		final[e.Path] = e
	}
	liveBitsCache := map[string]*codec.Bitset{}
	for _, p := range order {
		e, ok := final[p]
		if !ok {
			continue // 已删除
		}
		if e.Seg == "" {
			continue
		}
		rec, ok := segByID[e.Seg]
		if !ok {
			rep.Errors = append(rep.Errors, fmt.Sprintf("路径 %s 指向不存在的段 %s", p, e.Seg))
			continue
		}
		bits, ok := liveBitsCache[e.Seg]
		if !ok {
			var err error
			bits, err = readLiveBits(fsys, filepath.Join(root, e.Seg), rec.LiveGen)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("读取段 %s 墓碑失败: %v", e.Seg, err))
				liveBitsCache[e.Seg] = nil
				continue
			}
			liveBitsCache[e.Seg] = bits
		}
		if bits == nil {
			continue
		}
		if e.DocID >= uint32(bits.Len()) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("路径 %s docID 越界", p))
			continue
		}
		if bits.Get(e.DocID) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("路径 %s 指向已删除文档 %s#%d", p, e.Seg, e.DocID))
		}
	}
	return len(final)
}
