package index

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"lantern/internal/codec"
	"lantern/internal/fsx"
)

// 段内文件名。
const (
	fMeta     = "meta.json"
	fTerms    = "terms.dat"
	fTermsIdx = "terms.idx"
	fPostings = "postings.dat"
	fNorms    = "norms.dat"
	fDV       = "dv.dat"
	fDocs     = "docs.dat"
	fDocsIdx  = "docs.idx"
)

// segTermKey 标识段内一个词项(字段, 词项文本)。
type segTermKey struct {
	field Field
	term  string
}

// segTermMeta 是词项在 postings.dat 中的位置与统计。
type segTermMeta struct {
	docFreq uint32
	totalTF uint64
	off     uint64
	ln      uint64
}

// FieldStat 是单字段的段级统计。
type FieldStat struct {
	// DocCount 为该字段至少有一个 token 的文档数。
	DocCount uint32
	// TotalLen 为该字段 token 总数。
	TotalLen uint64
}

// SegmentStats 是写段完成后的段级统计(进入 meta.json)。
type SegmentStats struct {
	Docs    uint32
	Fields  [NumFields]FieldStat
	ExtDict []string
}

// WriteSegment 把 memtable 写成一个段目录(dir 由提交协议放在 tmp 目录
// 再 rename)。所有二进制文件带 LNTN footer 并逐个 fsync;meta.json 为
// 纯 JSON,完整性由 manifest 的文件校验和覆盖。
func WriteSegment(fsys fsx.FS, dir string, mt *MemTable, storeBody bool) (*SegmentStats, error) {
	if err := fsys.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("index: mkdir segment: %w", err)
	}
	if mt.Len() == 0 {
		return nil, fmt.Errorf("index: refuse to write empty segment")
	}
	st := &SegmentStats{Docs: uint32(mt.Len())}

	// 词项按 (field, term) 排序,保证确定性。
	keys := make([]segTermKey, 0, 1024)
	for f := Field(0); f < NumFields; f++ {
		for t := range mt.terms[f] {
			keys = append(keys, segTermKey{f, t})
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].field != keys[j].field {
			return keys[i].field < keys[j].field
		}
		return keys[i].term < keys[j].term
	})

	// postings.dat:按词项顺序写条目。
	var postings bytes.Buffer
	metas := make(map[segTermKey]segTermMeta, len(keys))
	for _, k := range keys {
		p := mt.terms[k.field][k.term]
		off := uint64(postings.Len())
		entry := encodePostingsEntry(p, off)
		postings.Write(entry)
		var totalTF uint64
		for _, pp := range p.pos {
			totalTF += uint64(len(pp))
		}
		metas[k] = segTermMeta{
			docFreq: uint32(len(p.docs)),
			totalTF: totalTF,
			off:     off,
			ln:      uint64(postings.Len()) - off,
		}
	}

	// 字段统计(以 token 数为准;DocCount 为含该字段 token 的文档数)。
	for f := Field(0); f < NumFields; f++ {
		var tl uint64
		var dc uint32
		for i := range mt.docs {
			tl += uint64(mt.docs[i].fieldLen[f])
			if mt.docs[i].fieldLen[f] > 0 {
				dc++
			}
		}
		st.Fields[f] = FieldStat{DocCount: dc, TotalLen: tl}
	}

	// terms.dat + terms.idx:每 TermsBlockSize 项一块。
	var terms bytes.Buffer
	var idxRecs []byte
	nBlocks := 0
	for start := 0; start < len(keys); start += TermsBlockSize {
		end := start + TermsBlockSize
		if end > len(keys) {
			end = len(keys)
		}
		block := keys[start:end]
		payload := encodeTermsBlock(block, metas)
		var frame [4]byte
		binary.LittleEndian.PutUint32(frame[:], uint32(len(payload)))
		terms.Write(frame[:])
		terms.Write(payload)
		off := uint64(terms.Len()) - uint64(len(payload)) - 4
		idxRecs = append(idxRecs, byte(block[0].field))
		idxRecs = binary.AppendUvarint(idxRecs, uint64(len(block[0].term)))
		idxRecs = append(idxRecs, block[0].term...)
		var offb [8]byte
		binary.LittleEndian.PutUint64(offb[:], off)
		idxRecs = append(idxRecs, offb[:]...)
		nBlocks++
	}
	var termsIdx bytes.Buffer
	var idxHead [4]byte
	binary.LittleEndian.PutUint32(idxHead[:], uint32(nBlocks))
	termsIdx.Write(idxHead[:])
	termsIdx.Write(idxRecs)

	// norms.dat:nDocs × NumFields × u32 LE。
	var norms bytes.Buffer
	var nb [4]byte
	for i := range mt.docs {
		for f := Field(0); f < NumFields; f++ {
			binary.LittleEndian.PutUint32(nb[:], mt.docs[i].fieldLen[f])
			norms.Write(nb[:])
		}
	}

	// dv.dat:先建 ext 字典。
	exts := sortedKeysSet(mt)
	extID := make(map[string]uint32, len(exts))
	for i, e := range exts {
		extID[e] = uint32(i)
	}
	var dv bytes.Buffer
	var dvb [20]byte
	for i := range mt.docs {
		d := &mt.docs[i].doc
		binary.LittleEndian.PutUint64(dvb[0:8], uint64(d.MTime))
		binary.LittleEndian.PutUint64(dvb[8:16], uint64(d.Size))
		binary.LittleEndian.PutUint32(dvb[16:20], extID[d.Ext])
		dv.Write(dvb[:])
	}
	st.ExtDict = exts

	// docs.dat / docs.idx。
	var docsDat bytes.Buffer
	offsets := make([]uint64, 0, mt.Len()+1)
	offsets = append(offsets, 0)
	for i := range mt.docs {
		rec, err := encodeStoredDoc(&mt.docs[i].doc, storeBody, DefaultBodyLimit)
		if err != nil {
			return nil, fmt.Errorf("index: encode doc %q: %w", mt.docs[i].doc.Path, err)
		}
		docsDat.Write(rec)
		offsets = append(offsets, uint64(docsDat.Len()))
	}
	var docsIdx bytes.Buffer
	var ib [4]byte
	binary.LittleEndian.PutUint32(ib[:], uint32(mt.Len()))
	docsIdx.Write(ib[:])
	var ob [8]byte
	for _, o := range offsets {
		binary.LittleEndian.PutUint64(ob[:], o)
		docsIdx.Write(ob[:])
	}

	// live_0.bits(空墓碑)。
	live := codec.AppendBitset(nil, codec.NewBitset(mt.Len()))

	// 逐文件写入并 fsync(均带 footer)。
	files := []struct {
		name string
		data []byte
	}{
		{fTerms, terms.Bytes()},
		{fTermsIdx, termsIdx.Bytes()},
		{fPostings, postings.Bytes()},
		{fNorms, norms.Bytes()},
		{fDV, dv.Bytes()},
		{fDocs, docsDat.Bytes()},
		{fDocsIdx, docsIdx.Bytes()},
		{liveName(0), live},
	}
	for _, fi := range files {
		if err := writeFileSynced(fsys, dir, fi.name, codec.AppendFooter(fi.data, FormatVersion)); err != nil {
			return nil, err
		}
	}

	// meta.json。
	meta := metaJSON{
		Format:   FormatVersion,
		Docs:     st.Docs,
		Created:  time.Now().UTC().Format(time.RFC3339),
		Fields:   make(map[string]fieldMetaJSON, NumFields),
		ExtDict:  exts,
		StoreAll: storeBody,
	}
	for f := Field(0); f < NumFields; f++ {
		meta.Fields[f.Name()] = fieldMetaJSON{
			DocCount: st.Fields[f].DocCount,
			TotalLen: st.Fields[f].TotalLen,
		}
	}
	mb, err := json.MarshalIndent(&meta, "", " ")
	if err != nil {
		return nil, fmt.Errorf("index: marshal meta: %w", err)
	}
	if err := writeFileSynced(fsys, dir, fMeta, mb); err != nil {
		return nil, err
	}
	return st, nil
}

// sortedKeysSet 收集 memtable 内全部非空 ext,排序返回。
func sortedKeysSet(mt *MemTable) []string {
	seen := map[string]bool{}
	for i := range mt.docs {
		if mt.docs[i].doc.Ext != "" {
			seen[mt.docs[i].doc.Ext] = true
		}
	}
	out := make([]string, 0, len(seen))
	for e := range seen {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// encodePostingsEntry:文档块(每块 ≤ PostingsBlockSize)+ 尾部
// skip 表 + u16 表长。块内:docID delta+varint、tf varint、
// 位置首值以 0 为基准其后 delta。base 为本条目在 postings.dat 中的
// 文件偏移,skip 表记录文件绝对偏移。
func encodePostingsEntry(p *memPostings, base uint64) []byte {
	var entry bytes.Buffer
	type skipEnt struct {
		maxDoc uint32
		off    uint64
		ln     uint64
	}
	var skips []skipEnt
	var prevDoc uint32
	for start := 0; start < len(p.docs); start += PostingsBlockSize {
		end := start + PostingsBlockSize
		if end > len(p.docs) {
			end = len(p.docs)
		}
		ids := p.docs[start:end]
		n := len(ids)
		var b []byte
		b = binary.AppendUvarint(b, uint64(n))
		for _, id := range ids {
			b = binary.AppendUvarint(b, uint64(id-prevDoc))
			prevDoc = id
		}
		for i := 0; i < n; i++ {
			b = binary.AppendUvarint(b, uint64(len(p.pos[start+i])))
		}
		for i := 0; i < n; i++ {
			prev := 0
			for _, pos := range p.pos[start+i] {
				b = binary.AppendUvarint(b, uint64(uint32(int(pos)-prev)))
				prev = int(pos)
			}
		}
		var frame [4]byte
		binary.LittleEndian.PutUint32(frame[:], uint32(len(b)))
		off := uint64(entry.Len()) + base
		entry.Write(frame[:])
		entry.Write(b)
		skips = append(skips, skipEnt{maxDoc: ids[n-1], off: off, ln: uint64(len(b)) + 4})
	}
	trailer := binary.AppendUvarint(nil, uint64(len(skips)))
	for _, s := range skips {
		trailer = binary.AppendUvarint(trailer, uint64(s.maxDoc))
		trailer = binary.AppendUvarint(trailer, s.off)
		trailer = binary.AppendUvarint(trailer, s.ln)
	}
	var tl [2]byte
	binary.LittleEndian.PutUint16(tl[:], uint16(len(trailer)))
	entry.Write(trailer)
	entry.Write(tl[:])
	return entry.Bytes()
}

// encodeTermsBlock:u16 n + 每项 [field u8][shared varint][suffixLen varint]
// [suffix][docFreq varint][totalTF varint][off varint][len varint]。
// shared 仅在同字段时相对前项的公共前缀长度。
func encodeTermsBlock(block []segTermKey, metas map[segTermKey]segTermMeta) []byte {
	var b []byte
	var h [2]byte
	binary.LittleEndian.PutUint16(h[:], uint16(len(block)))
	b = append(b, h[:]...)
	var prevTerm string
	var prevField Field = 0xFF
	for _, k := range block {
		m := metas[k]
		b = append(b, byte(k.field))
		shared := 0
		if k.field == prevField {
			max := len(prevTerm)
			if len(k.term) < max {
				max = len(k.term)
			}
			for shared < max && prevTerm[shared] == k.term[shared] {
				shared++
			}
		}
		b = binary.AppendUvarint(b, uint64(shared))
		suffix := k.term[shared:]
		b = binary.AppendUvarint(b, uint64(len(suffix)))
		b = append(b, suffix...)
		b = binary.AppendUvarint(b, uint64(m.docFreq))
		b = binary.AppendUvarint(b, m.totalTF)
		b = binary.AppendUvarint(b, m.off)
		b = binary.AppendUvarint(b, m.ln)
		prevTerm, prevField = k.term, k.field
	}
	return b
}

// encodeStoredDoc:[pathLen varint][path][titleLen varint][title]
// [mtime i64 LE][size i64 LE][sha 32B][flags u8][bodyLen varint][body]。
// flags bit0=存正文;正文 flate 压缩且不超过 bodyLimit。
func encodeStoredDoc(d *Doc, storeBody bool, bodyLimit int) ([]byte, error) {
	var b []byte
	b = binary.AppendUvarint(b, uint64(len(d.Path)))
	b = append(b, d.Path...)
	b = binary.AppendUvarint(b, uint64(len(d.Title)))
	b = append(b, d.Title...)
	var f8 [8]byte
	binary.LittleEndian.PutUint64(f8[:], uint64(d.MTime))
	b = append(b, f8[:]...)
	binary.LittleEndian.PutUint64(f8[:], uint64(d.Size))
	b = append(b, f8[:]...)
	b = append(b, d.SHA256[:]...)
	var flags byte
	if storeBody && len(d.Body) <= bodyLimit {
		flags |= 1
	}
	b = append(b, flags)
	if flags&1 != 0 {
		var z bytes.Buffer
		zw, err := flate.NewWriter(&z, flate.DefaultCompression)
		if err != nil {
			return nil, err
		}
		if _, err := zw.Write([]byte(d.Body)); err != nil {
			return nil, err
		}
		if err := zw.Close(); err != nil {
			return nil, err
		}
		b = binary.AppendUvarint(b, uint64(z.Len()))
		b = append(b, z.Bytes()...)
	} else {
		b = binary.AppendUvarint(b, 0)
	}
	return b, nil
}

// writeFileSynced 创建文件、写入、fsync、关闭。
func writeFileSynced(fsys fsx.FS, dir, name string, data []byte) error {
	f, err := fsys.Create(filepath.Join(dir, name))
	if err != nil {
		return fmt.Errorf("index: create %s: %w", name, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("index: write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("index: sync %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("index: close %s: %w", name, err)
	}
	return nil
}

// liveName 返回第 gen 代墓碑位集文件名。
func liveName(gen uint32) string { return fmt.Sprintf("live_%d.bits", gen) }

type metaJSON struct {
	Format   uint32                   `json:"format"`
	Docs     uint32                   `json:"docs"`
	Created  string                   `json:"created"`
	Fields   map[string]fieldMetaJSON `json:"fields"`
	ExtDict  []string                 `json:"ext_dict"`
	StoreAll bool                     `json:"store_body"`
}

type fieldMetaJSON struct {
	DocCount uint32 `json:"doc_count"`
	TotalLen uint64 `json:"total_len"`
}
