package index

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"sync"

	"lantern/internal/codec"
	"lantern/internal/fsx"
)

// TermEntry 是词典中一个词项的统计与 postings 位置。
type TermEntry struct {
	// DocFreq 为含该词项的文档数(含已删除未合并的,见规格 7.1)。
	DocFreq uint32
	// TotalTF 为该词项位置总数。
	TotalTF uint64
	Off     uint64
	Len     uint64
}

type termIdxEntry struct {
	field Field
	term  string
	off   uint64
}

type skipEntry struct {
	maxDoc uint32
	off    uint64
	ln     uint64
}

// segmentFiles 是段的全部带 footer 文件(meta.json 除外)。
func segmentFiles() []string {
	return []string{fTerms, fTermsIdx, fPostings, fNorms, fDV, fDocs, fDocsIdx, liveName(0)}
}

// SegmentReader 打开并读取一个不可变段。并发安全:所有读方法可被
// 多个搜索 goroutine 同时调用;内部仅对 skip 表缓存加锁。
// 关闭前必须保证没有进行中的读取(由 Snapshot 引用计数保证)。
type SegmentReader struct {
	ID    string
	Dir   string
	fsys  fsx.FS
	cache *BlockCache

	files map[string]fsx.File

	nDocs    uint32
	fields   [NumFields]FieldStat
	extDict  []string
	termsIdx []termIdxEntry

	mu        sync.Mutex
	skipCache map[segTermKey][]skipEntry

	live    *codec.Bitset
	liveGen uint32
}

// OpenSegment 打开段目录并对每个文件做 footer 尾部的快速校验
// (magic+version)。完整 CRC 校验由 CheckSegment(lantern check)执行。
func OpenSegment(fsys fsx.FS, dir, id string, cache *BlockCache) (*SegmentReader, error) {
	if cache == nil {
		cache = NewBlockCache(0)
	}
	r := &SegmentReader{
		ID:        id,
		Dir:       dir,
		fsys:      fsys,
		cache:     cache,
		files:     map[string]fsx.File{},
		skipCache: map[segTermKey][]skipEntry{},
	}
	fail := func(err error) (*SegmentReader, error) {
		r.Close()
		return nil, err
	}
	for _, name := range segmentFiles() {
		f, err := fsys.Open(filepath.Join(dir, name))
		if err != nil {
			return fail(fmt.Errorf("index: open segment %s/%s: %w", id, name, err))
		}
		r.files[name] = f
		st, err := f.Stat()
		if err != nil {
			return fail(fmt.Errorf("index: stat %s/%s: %w", id, name, err))
		}
		if st.Size() < codec.FooterSize {
			return fail(fmt.Errorf("index: %s/%s too short", id, name))
		}
		tail := make([]byte, codec.FooterSize)
		if _, err := f.ReadAt(tail, st.Size()-codec.FooterSize); err != nil {
			return fail(fmt.Errorf("index: read footer %s/%s: %w", id, name, err))
		}
		if !bytes.Equal(tail[:4], codec.Magic[:]) {
			return fail(fmt.Errorf("index: %s/%s bad magic", id, name))
		}
	}

	// meta.json。
	mb, err := readWholeFile(fsys, filepath.Join(dir, fMeta))
	if err != nil {
		return fail(err)
	}
	var meta metaJSON
	if err := json.Unmarshal(mb, &meta); err != nil {
		return fail(fmt.Errorf("index: parse meta.json of %s: %w", id, err))
	}
	if meta.Format != FormatVersion {
		return fail(fmt.Errorf("index: segment %s format %d != %d", id, meta.Format, FormatVersion))
	}
	r.nDocs = meta.Docs
	r.extDict = meta.ExtDict
	for f := Field(0); f < NumFields; f++ {
		fm, ok := meta.Fields[f.Name()]
		if !ok {
			return fail(fmt.Errorf("index: meta.json of %s missing field %s", id, f.Name()))
		}
		r.fields[f] = FieldStat{DocCount: fm.DocCount, TotalLen: fm.TotalLen}
	}

	// terms.idx 全量载入(规格 5.2:打开时载入内存,二分定位块)。
	idxData, err := r.readFilePayload(fTermsIdx)
	if err != nil {
		return fail(err)
	}
	if err := r.parseTermsIdx(idxData); err != nil {
		return fail(err)
	}

	// live_0.bits。
	liveData, err := r.readFilePayload(liveName(0))
	if err != nil {
		return fail(err)
	}
	live, err := codec.DecodeBitset(liveData)
	if err != nil {
		return fail(fmt.Errorf("index: decode live bits of %s: %w", id, err))
	}
	if uint32(live.Len()) != r.nDocs && live.Len() > 0 {
		return fail(fmt.Errorf("index: live bits len %d != docs %d in %s", live.Len(), r.nDocs, id))
	}
	r.live = live
	r.liveGen = 0
	return r, nil
}

func (r *SegmentReader) parseTermsIdx(data []byte) error {
	if len(data) < 4 {
		return fmt.Errorf("index: terms.idx of %s truncated", r.ID)
	}
	nBlocks := int(binary.LittleEndian.Uint32(data))
	pos := 4
	r.termsIdx = make([]termIdxEntry, 0, nBlocks)
	for i := 0; i < nBlocks; i++ {
		if pos >= len(data) {
			return fmt.Errorf("index: terms.idx of %s truncated at block %d", r.ID, i)
		}
		field := Field(data[pos])
		pos++
		tl, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			return fmt.Errorf("index: terms.idx of %s corrupt at block %d", r.ID, i)
		}
		pos += n
		if pos+int(tl)+8 > len(data) {
			return fmt.Errorf("index: terms.idx of %s corrupt at block %d", r.ID, i)
		}
		term := string(data[pos : pos+int(tl)])
		pos += int(tl)
		off := binary.LittleEndian.Uint64(data[pos:])
		pos += 8
		r.termsIdx = append(r.termsIdx, termIdxEntry{field: field, term: term, off: off})
	}
	return nil
}

// Close 关闭全部文件句柄。
func (r *SegmentReader) Close() error {
	var firstErr error
	for _, f := range r.files {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	r.files = map[string]fsx.File{}
	return firstErr
}

// readWholeFile 读取整个文件。
func readWholeFile(fsys fsx.FS, path string) ([]byte, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, fmt.Errorf("index: open %s: %w", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("index: read %s: %w", path, err)
	}
	return data, nil
}

// readFilePayload 读取段内一个带 footer 的文件并校验 CRC,
// 返回去 footer 后的载荷。
func (r *SegmentReader) readFilePayload(name string) ([]byte, error) {
	f := r.files[name]
	if f == nil {
		return nil, fmt.Errorf("index: file %s not open", name)
	}
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("index: stat %s: %w", name, err)
	}
	buf := make([]byte, st.Size())
	if _, err := f.ReadAt(buf, 0); err != nil && err != io.EOF {
		return nil, fmt.Errorf("index: read %s: %w", name, err)
	}
	payload, _, err := codec.ParseFooter(buf)
	if err != nil {
		return nil, fmt.Errorf("index: %s of %s: %w", name, r.ID, err)
	}
	return payload, nil
}

// readAt 经 LRU 缓存读取段内文件的 [off,off+ln) 字节。返回切片为
// 缓存共享,只读。
func (r *SegmentReader) readAt(name string, off int64, ln int) ([]byte, error) {
	if ln < 0 {
		return nil, fmt.Errorf("index: negative read length on %s", name)
	}
	key := cacheKey{seg: r.ID, file: name, off: off, ln: ln}
	if b, ok := r.cache.Get(key); ok {
		return b, nil
	}
	f := r.files[name]
	if f == nil {
		return nil, fmt.Errorf("index: file %s not open", name)
	}
	buf := make([]byte, ln)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, fmt.Errorf("index: readAt %s@%d: %w", name, off, err)
	}
	r.cache.Put(key, buf)
	return buf, nil
}

// DocCount 返回段内文档数(含已删除未合并)。
func (r *SegmentReader) DocCount() uint32 { return r.nDocs }

// FieldStats 返回段级字段统计。
func (r *SegmentReader) FieldStats() [NumFields]FieldStat { return r.fields }

// IsAlive 报告 docID 是否存活(未被墓碑删除)。
func (r *SegmentReader) IsAlive(docID uint32) bool { return !r.live.Get(docID) }

// LiveGen 返回当前墓碑代。
func (r *SegmentReader) LiveGen() uint32 { return r.liveGen }

// SetLive 更新墓碑位集与代号(仅由提交/合并路径调用)。
func (r *SegmentReader) SetLive(gen uint32, bits *codec.Bitset) {
	r.live = bits
	r.liveGen = gen
}

// termsEntryFull 是解码出的完整词典条目。
type termsEntryFull struct {
	field Field
	term  string
	entry TermEntry
}

// decodeTermsEntries 解码 terms.dat 一个块的全部条目。
func decodeTermsEntries(payload []byte) ([]termsEntryFull, error) {
	if len(payload) < 2 {
		return nil, fmt.Errorf("index: terms block truncated")
	}
	n := int(binary.LittleEndian.Uint16(payload))
	pos := 2
	out := make([]termsEntryFull, 0, n)
	var prevTerm []byte
	var prevField Field = 0xFF
	for i := 0; i < n; i++ {
		if pos >= len(payload) {
			return nil, fmt.Errorf("index: terms block truncated entry %d", i)
		}
		f := Field(payload[pos])
		pos++
		shared, sz := binary.Uvarint(payload[pos:])
		if sz <= 0 {
			return nil, fmt.Errorf("index: terms block bad shared")
		}
		pos += sz
		sufLen, sz := binary.Uvarint(payload[pos:])
		if sz <= 0 || pos+sz+int(sufLen) > len(payload) {
			return nil, fmt.Errorf("index: terms block bad suffix")
		}
		pos += sz
		var cur []byte
		if f == prevField && int(shared) <= len(prevTerm) {
			cur = append(prevTerm[:shared:int(shared)], payload[pos:pos+int(sufLen)]...)
		} else {
			cur = append([]byte(nil), payload[pos:pos+int(sufLen)]...)
		}
		pos += int(sufLen)
		var vals [4]uint64
		for vi := 0; vi < 4; vi++ {
			v, s := binary.Uvarint(payload[pos:])
			if s <= 0 {
				return nil, fmt.Errorf("index: terms block bad varint %d at entry %d", vi, i)
			}
			pos += s
			vals[vi] = v
		}
		prevTerm, prevField = cur, f
		out = append(out, termsEntryFull{
			field: f,
			term:  string(cur),
			entry: TermEntry{
				DocFreq: uint32(vals[0]), TotalTF: vals[1], Off: vals[2], Len: vals[3],
			},
		})
	}
	return out, nil
}

func (r *SegmentReader) readTermsBlock(off uint64) ([]termsEntryFull, error) {
	lenBuf, err := r.readAt(fTerms, int64(off), 4)
	if err != nil {
		return nil, err
	}
	plen := binary.LittleEndian.Uint32(lenBuf)
	payload, err := r.readAt(fTerms, int64(off)+4, int(plen))
	if err != nil {
		return nil, err
	}
	return decodeTermsEntries(payload)
}

// Term 查找词项,返回其统计;不存在时 ok=false。
func (r *SegmentReader) Term(field Field, term string) (TermEntry, bool, error) {
	// 在稀疏索引上二分:定位最后一个首词项 <= (field,term) 的块。
	// 词条全局有序,目标若存在必在该块的区间内。
	lo, hi := 0, len(r.termsIdx)-1
	found := -1
	for lo <= hi {
		mid := int(uint(lo+hi) >> 1)
		e := r.termsIdx[mid]
		if e.field < field || (e.field == field && e.term <= term) {
			found = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if found < 0 {
		return TermEntry{}, false, nil
	}
	ents, err := r.readTermsBlock(r.termsIdx[found].off)
	if err != nil {
		return TermEntry{}, false, err
	}
	for _, en := range ents {
		if en.field == field && en.term == term {
			return en.entry, true, nil
		}
	}
	return TermEntry{}, false, nil
}

// Terms 按字典序遍历字段 field 的全部词项;fn 返回 false 停止。
func (r *SegmentReader) Terms(field Field, fn func(term string, e TermEntry) bool) error {
	for _, blk := range r.termsIdx {
		ents, err := r.readTermsBlock(blk.off)
		if err != nil {
			return err
		}
		for _, en := range ents {
			if en.field != field {
				continue
			}
			if !fn(en.term, en.entry) {
				return nil
			}
		}
	}
	return nil
}

// Postings 返回词项的倒排迭代器(自动跳过墓碑文档);
// 词项不存在时返回 nil。
func (r *SegmentReader) Postings(field Field, term string) (*PostingIterator, error) {
	te, ok, err := r.Term(field, term)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &PostingIterator{r: r, field: field, term: term, entry: te}, nil
}

// PostingIterator 是一个词项内的文档游标。
// 约定:先调用 Next 或 Advance 定位,再读取 DocID/TF/Positions。
// 每次成功定位后读方法的结果在下一次 Next/Advance 前有效。
type PostingIterator struct {
	r      *SegmentReader
	field  Field
	term   string
	entry  TermEntry
	skips  []skipEntry
	loaded bool
	blockI int
	docs   []uint32
	tfs    []uint32
	pos    [][]uint32
	cur    int
	done   bool
}

// DocFreq 返回词项文档频率(含已删除未合并)。
func (it *PostingIterator) DocFreq() uint32 { return it.entry.DocFreq }

// TotalTF 返回词项位置总数。
func (it *PostingIterator) TotalTF() uint64 { return it.entry.TotalTF }

// DocID 返回当前文档 ID(仅在 Next/Advance 返回 true 后有效)。
func (it *PostingIterator) DocID() uint32 { return it.docs[it.cur] }

// TF 返回当前文档的词频。
func (it *PostingIterator) TF() uint32 { return it.tfs[it.cur] }

// Positions 返回当前文档的位置序列(共享只读)。
func (it *PostingIterator) Positions() []uint32 { return it.pos[it.cur] }

func (it *PostingIterator) ensureSkips() error {
	if it.skips != nil {
		return nil
	}
	tlBuf, err := it.r.readAt(fPostings, int64(it.entry.Off+it.entry.Len)-2, 2)
	if err != nil {
		return err
	}
	tl := binary.LittleEndian.Uint16(tlBuf)
	trailer, err := it.r.readAt(fPostings, int64(it.entry.Off+it.entry.Len)-2-int64(tl), int(tl))
	if err != nil {
		return err
	}
	n, sz := binary.Uvarint(trailer)
	if sz <= 0 {
		return fmt.Errorf("index: bad skip count for %q", it.term)
	}
	pos := sz
	skips := make([]skipEntry, 0, n)
	for i := 0; i < int(n); i++ {
		var v [3]uint64
		for vi := 0; vi < 3; vi++ {
			x, s := binary.Uvarint(trailer[pos:])
			if s <= 0 {
				return fmt.Errorf("index: bad skip entry %d for %q", i, it.term)
			}
			pos += s
			v[vi] = x
		}
		skips = append(skips, skipEntry{maxDoc: uint32(v[0]), off: v[1], ln: v[2]})
	}
	it.skips = skips
	return nil
}

// blockLastDoc 返回第 i 块最后一个 docID;i<0 返回 0(首块基准)。
func (it *PostingIterator) blockLastDoc(i int) uint32 {
	if i < 0 {
		return 0
	}
	return it.skips[i].maxDoc
}

func (it *PostingIterator) loadBlock(i int) error {
	s := it.skips[i]
	payload, err := it.r.readAt(fPostings, int64(s.off)+4, int(s.ln)-4)
	if err != nil {
		return err
	}
	base := uint32(0)
	first := i == 0
	if !first {
		base = it.skips[i-1].maxDoc
	}
	docs, tfs, pos, err := decodePostingsBlock(payload, first, base)
	if err != nil {
		return err
	}
	it.blockI = i
	it.docs = docs
	it.tfs = tfs
	it.pos = pos
	it.cur = -1
	it.loaded = true
	return nil
}

func decodePostingsBlock(payload []byte, first bool, baseDoc uint32) ([]uint32, []uint32, [][]uint32, error) {
	n, sz := binary.Uvarint(payload)
	if sz <= 0 || n > 1<<20 {
		return nil, nil, nil, fmt.Errorf("index: postings block bad count")
	}
	pos := sz
	docs := make([]uint32, n)
	prev := uint64(0)
	if !first {
		prev = uint64(baseDoc)
	}
	for i := 0; i < int(n); i++ {
		d, s := binary.Uvarint(payload[pos:])
		if s <= 0 {
			return nil, nil, nil, fmt.Errorf("index: postings block bad docID delta")
		}
		pos += s
		prev += d
		if prev > 0xFFFFFFFF {
			return nil, nil, nil, fmt.Errorf("index: postings docID overflow")
		}
		docs[i] = uint32(prev)
	}
	tfs := make([]uint32, n)
	for i := 0; i < int(n); i++ {
		tf, s := binary.Uvarint(payload[pos:])
		if s <= 0 || tf > 1<<20 {
			return nil, nil, nil, fmt.Errorf("index: postings block bad tf")
		}
		pos += s
		tfs[i] = uint32(tf)
	}
	posArr := make([][]uint32, n)
	for i := 0; i < int(n); i++ {
		arr := make([]uint32, 0, tfs[i])
		prevP := 0
		for j := uint32(0); j < tfs[i]; j++ {
			d, s := binary.Uvarint(payload[pos:])
			if s <= 0 {
				return nil, nil, nil, fmt.Errorf("index: postings block bad pos delta")
			}
			pos += s
			prevP += int(d)
			arr = append(arr, uint32(prevP))
		}
		posArr[i] = arr
	}
	return docs, tfs, posArr, nil
}

// nextDoc 前进到下一个存活文档(内部游标推进)。
func (it *PostingIterator) nextDoc() bool {
	for {
		it.cur++
		if it.cur >= len(it.docs) {
			next := it.blockI + 1
			if !it.loaded {
				next = 0
			}
			if it.skips == nil {
				if err := it.ensureSkips(); err != nil {
					it.done = true
					return false
				}
			}
			if next >= len(it.skips) {
				it.done = true
				return false
			}
			if err := it.loadBlock(next); err != nil {
				it.done = true
				return false
			}
			continue
		}
		if it.r.IsAlive(it.docs[it.cur]) {
			return true
		}
	}
}

// Next 前进到下一个存活文档;无更多文档时返回 false。
func (it *PostingIterator) Next() bool {
	if it.done {
		return false
	}
	return it.nextDoc()
}

// Advance 定位到第一个 >= target 的存活文档并返回 true;
// 不存在时返回 false 且迭代器耗尽。
func (it *PostingIterator) Advance(target uint32) bool {
	if it.done {
		return false
	}
	if err := it.ensureSkips(); err != nil {
		it.done = true
		return false
	}
	if len(it.skips) == 0 {
		it.done = true
		return false
	}
	// 已定位且满足条件。
	if it.loaded && it.cur >= 0 && it.cur < len(it.docs) && it.docs[it.cur] >= target {
		return true
	}
	// 判断是否需要在当前块内线性前进(目标在本块范围内)。
	jump := -1
	if it.loaded && target <= it.skips[it.blockI].maxDoc {
		jump = it.blockI
	} else {
		lo, hi := 0, len(it.skips)-1
		for lo <= hi {
			mid := int(uint(lo+hi) >> 1)
			if it.skips[mid].maxDoc >= target {
				jump = mid
				hi = mid - 1
			} else {
				lo = mid + 1
			}
		}
		if jump < 0 {
			it.done = true
			return false
		}
	}
	if jump != it.blockI || !it.loaded {
		if err := it.loadBlock(jump); err != nil {
			it.done = true
			return false
		}
	}
	for it.nextDoc() {
		if it.DocID() >= target {
			return true
		}
	}
	return false
}

// Norm 读取某文档某字段的长度。
func (r *SegmentReader) Norm(docID uint32, field Field) (uint32, error) {
	if docID >= r.nDocs {
		return 0, fmt.Errorf("index: docID %d out of range in %s", docID, r.ID)
	}
	buf, err := r.readAt(fNorms, int64(docID)*int64(NumFields)*4+int64(field)*4, 4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf), nil
}

// DocValues 读取某文档的 ext/size/mtime。
func (r *SegmentReader) DocValues(docID uint32) (DV, error) {
	if docID >= r.nDocs {
		return DV{}, fmt.Errorf("index: docID %d out of range in %s", docID, r.ID)
	}
	buf, err := r.readAt(fDV, int64(docID)*20, 20)
	if err != nil {
		return DV{}, err
	}
	extID := binary.LittleEndian.Uint32(buf[16:20])
	ext := ""
	if int(extID) < len(r.extDict) {
		ext = r.extDict[extID]
	}
	return DV{
		MTime: int64(binary.LittleEndian.Uint64(buf[0:8])),
		Size:  int64(binary.LittleEndian.Uint64(buf[8:16])),
		Ext:   ext,
	}, nil
}

// StoredDoc 读取某文档的存储字段。
func (r *SegmentReader) StoredDoc(docID uint32) (*StoredDoc, error) {
	if docID >= r.nDocs {
		return nil, fmt.Errorf("index: docID %d out of range in %s", docID, r.ID)
	}
	offBuf, err := r.readAt(fDocsIdx, 4+int64(docID)*8, 16)
	if err != nil {
		return nil, err
	}
	start := binary.LittleEndian.Uint64(offBuf[0:8])
	end := binary.LittleEndian.Uint64(offBuf[8:16])
	if end < start {
		return nil, fmt.Errorf("index: docs.idx inverted offsets in %s", r.ID)
	}
	rec, err := r.readAt(fDocs, int64(start), int(end-start))
	if err != nil {
		return nil, err
	}
	return decodeStoredDoc(rec)
}

func decodeStoredDoc(rec []byte) (*StoredDoc, error) {
	sd := &StoredDoc{}
	pos := 0
	plen, sz := binary.Uvarint(rec[pos:])
	if sz <= 0 || pos+sz+int(plen) > len(rec) {
		return nil, fmt.Errorf("index: stored doc bad path len")
	}
	pos += sz
	sd.Path = string(rec[pos : pos+int(plen)])
	pos += int(plen)
	tlen, sz := binary.Uvarint(rec[pos:])
	if sz <= 0 || pos+sz+int(tlen) > len(rec) {
		return nil, fmt.Errorf("index: stored doc bad title len")
	}
	pos += sz
	sd.Title = string(rec[pos : pos+int(tlen)])
	pos += int(tlen)
	if pos+8+8+32+1 > len(rec) {
		return nil, fmt.Errorf("index: stored doc truncated header")
	}
	sd.MTime = int64(binary.LittleEndian.Uint64(rec[pos:]))
	pos += 8
	sd.Size = int64(binary.LittleEndian.Uint64(rec[pos:]))
	pos += 8
	copy(sd.SHA256[:], rec[pos:pos+32])
	pos += 32
	flags := rec[pos]
	pos++
	blen, sz := binary.Uvarint(rec[pos:])
	if sz <= 0 || pos+sz+int(blen) > len(rec) {
		return nil, fmt.Errorf("index: stored doc bad body len")
	}
	pos += sz
	if flags&1 != 0 {
		body := make([]byte, blen)
		fr := flate.NewReader(bytes.NewReader(rec[pos : pos+int(blen)]))
		n, err := io.ReadFull(fr, body)
		if err != nil && err != io.ErrUnexpectedEOF {
			fr.Close()
			return nil, fmt.Errorf("index: stored doc body inflate: %w", err)
		}
		fr.Close()
		sd.Body = string(body[:n])
	}
	return sd, nil
}

// CheckSegment 完整校验段内所有带 footer 文件的 CRC 与 meta 一致性。
func CheckSegment(fsys fsx.FS, dir, id string) error {
	for _, name := range segmentFiles() {
		data, err := readWholeFile(fsys, filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("index: check %s/%s: %w", id, name, err)
		}
		if _, ver, err := codec.ParseFooter(data); err != nil {
			return fmt.Errorf("index: check %s/%s: %w", id, name, err)
		} else if ver != FormatVersion {
			return fmt.Errorf("index: check %s/%s: format %d", id, name, ver)
		}
	}
	mb, err := readWholeFile(fsys, filepath.Join(dir, fMeta))
	if err != nil {
		return fmt.Errorf("index: check %s/meta.json: %w", id, err)
	}
	var meta metaJSON
	if err := json.Unmarshal(mb, &meta); err != nil {
		return fmt.Errorf("index: check %s/meta.json: %w", id, err)
	}
	if meta.Format != FormatVersion {
		return fmt.Errorf("index: check %s: format %d != %d", id, meta.Format, FormatVersion)
	}
	return nil
}

// TermsSorted 返回字段全部词项(调试与测试用)。
func (r *SegmentReader) TermsSorted(field Field) ([]string, error) {
	var out []string
	err := r.Terms(field, func(term string, _ TermEntry) bool {
		out = append(out, term)
		return true
	})
	sort.Strings(out)
	return out, err
}
