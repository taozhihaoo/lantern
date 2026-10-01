// Package index 实现分段倒排索引:memtable、不可变段读写、manifest、
// 原子提交与恢复、写锁、快照(MVCC)与合并。
//
// 并发模型:单写者(write.lock 保护)负责产生新段与合并;读者通过
// Snapshot 持有不可变段集合的引用计数,写者仅在引用归零后物理删除文件。
package index

import "lantern/internal/codec"

// FormatVersion 是当前段与 manifest 的格式版本。
const FormatVersion uint32 = 1

// Field 是可检索字段编号。
type Field uint8

const (
	// FieldPath 为相对路径字段。
	FieldPath Field = iota
	// FieldTitle 为标题字段。
	FieldTitle
	// FieldBody 为正文字段。
	FieldBody
	// NumFields 为可检索字段总数。
	NumFields
)

// FieldName 返回字段的规范名(查询语言中的字段限定符)。
func (f Field) Name() string {
	switch f {
	case FieldPath:
		return "path"
	case FieldTitle:
		return "title"
	case FieldBody:
		return "body"
	}
	return "?"
}

// FieldByName 按规范名解析字段;未知返回 false。
func FieldByName(name string) (Field, bool) {
	switch name {
	case "path":
		return FieldPath, true
	case "title":
		return FieldTitle, true
	case "body":
		return FieldBody, true
	}
	return 0, false
}

// 默认参数(GOAL.md 5.4/5.10/5.2)。
const (
	// DefaultFlushDocs 是 memtable 刷盘的文档数阈值。
	DefaultFlushDocs = 5000
	// DefaultFlushBytes 是 memtable 刷盘的内存估计阈值。
	DefaultFlushBytes = 64 << 20
	// DefaultBodyLimit 是存储正文的上限(字节)。
	DefaultBodyLimit = 1 << 20
	// DefaultCacheBytes 是 LRU 块缓存容量。
	DefaultCacheBytes = 64 << 20
	// PostingsBlockSize 是 postings 每块的文档数。
	PostingsBlockSize = 128
	// TermsBlockSize 是 terms.dat 每块的词项数。
	TermsBlockSize = 64
)

// Doc 是进入索引的一个文档(已由扫描层提取出纯文本)。
type Doc struct {
	// Path 为相对索引根、正斜杠规范的唯一路径。
	Path string
	// Title 为标题(Markdown 一级标题 / HTML title / 文件名)。
	Title string
	// Body 为正文纯文本。
	Body string
	// Ext 为小写扩展名(不含点),无扩展名为空。
	Ext string
	// Size 为原文件字节大小。
	Size int64
	// MTime 为修改时间(Unix 秒)。
	MTime int64
	// SHA256 为内容哈希。
	SHA256 [32]byte
	// StoreBody 为 true 时存储正文(受 BodyLimit 限制)。
	StoreBody bool
}

// DV 是 doc values(ext/size/mtime 过滤用)。
type DV struct {
	MTime int64
	Size  int64
	Ext   string
}

// StoredDoc 是从 docs.dat 读回的存储字段。
type StoredDoc struct {
	Path   string
	Title  string
	MTime  int64
	Size   int64
	SHA256 [32]byte
	Body   string // 未存储时为空
}

var _ = codec.FooterSize // 保持 codec 依赖(段文件均带 footer)
