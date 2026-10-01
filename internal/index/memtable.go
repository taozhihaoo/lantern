package index

import "lantern/internal/analysis"

// memDoc 是 memtable 内的一个文档。
type memDoc struct {
	doc      Doc
	fieldLen [NumFields]uint32
}

// memPostings 是一个词项在 memtable 内的倒排:docs 与 pos 平行,
// docs 严格递增(文档按写入顺序编号)。
type memPostings struct {
	docs []uint32
	pos  [][]uint32
}

// MemTable 在内存中累积待索引文档,达到阈值后整体刷成新段。
// 非并发安全:仅单写者 goroutine 使用。
type MemTable struct {
	docs     []memDoc
	terms    [NumFields]map[string]*memPostings
	totalLen [NumFields]uint64
	bytes    int
}

// NewMemTable 创建空 memtable。
func NewMemTable() *MemTable {
	m := &MemTable{docs: make([]memDoc, 0, 1024)}
	for f := range m.terms {
		m.terms[f] = make(map[string]*memPostings)
	}
	return m
}

// Add 分析并加入一个文档,返回其段内 docID。
// 同一 memtable 内不允许重复 Path(由上层状态表保证)。
func (m *MemTable) Add(d Doc) uint32 {
	id := uint32(len(m.docs))
	md := memDoc{doc: d}
	fields := [NumFields]string{d.Path, d.Title, d.Body}
	for f, text := range fields {
		toks := analysis.Analyze(text)
		md.fieldLen[f] = uint32(len(toks))
		m.totalLen[f] += uint64(len(toks))
		for _, tok := range toks {
			p := m.terms[f][tok.Text]
			if p == nil {
				p = &memPostings{}
				m.terms[f][tok.Text] = p
			}
			if n := len(p.docs); n > 0 && p.docs[n-1] == id {
				p.pos[n-1] = append(p.pos[n-1], uint32(tok.Pos))
			} else {
				p.docs = append(p.docs, id)
				p.pos = append(p.pos, []uint32{uint32(tok.Pos)})
			}
		}
	}
	m.docs = append(m.docs, md)
	m.bytes += len(d.Path) + len(d.Title) + len(d.Body) + 96
	return id
}

// Len 返回累积文档数。
func (m *MemTable) Len() int { return len(m.docs) }

// Bytes 返回内存占用的粗略估计。
func (m *MemTable) Bytes() int { return m.bytes }

// ShouldFlush 报告是否达到刷盘阈值(默认 5000 篇或 64MB)。
func (m *MemTable) ShouldFlush() bool {
	return m.Len() >= DefaultFlushDocs || m.Bytes() >= DefaultFlushBytes
}

// Reset 清空以复用。
func (m *MemTable) Reset() {
	m.docs = m.docs[:0]
	m.bytes = 0
	for f := range m.terms {
		for k := range m.terms[f] {
			delete(m.terms[f], k)
		}
		m.totalLen[f] = 0
	}
}
