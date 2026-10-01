// Package codec 实现索引文件的底层编码:varint、delta、位集、
// 前缀压缩字符串块,以及带 CRC32 footer 的文件封装。
//
// 所有数据文件的 footer 布局(ARCHITECTURE.md 第 2 节):
//
//	payload || "LNTN"(4B) || format_version(u32 LE) || crc32(u32 LE)
//
// 其中 crc32 覆盖 payload+magic+version 的全部字节。
package codec

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// Magic 是所有 Lantern 数据文件的魔数。
var Magic = [4]byte{'L', 'N', 'T', 'N'}

// FooterSize 是 footer 的字节数:magic(4)+version(4)+crc32(4)。
const FooterSize = 12

// ErrBadFooter 表示 footer 缺失、magic 不符、版本不支持或 CRC 不匹配。
var ErrBadFooter = errors.New("codec: bad footer")

// AppendFooter 追加 footer 并返回新切片。
func AppendFooter(payload []byte, version uint32) []byte {
	out := append(payload, Magic[:]...)
	var ver [4]byte
	binary.LittleEndian.PutUint32(ver[:], version)
	out = append(out, ver[:]...)
	sum := crc32.ChecksumIEEE(out)
	var crc [4]byte
	binary.LittleEndian.PutUint32(crc[:], sum)
	return append(out, crc[:]...)
}

// ParseFooter 校验 data 的 footer,返回 payload 与 format version。
// data 长度不足、magic 不符或 CRC 不匹配时返回 ErrBadFooter。
func ParseFooter(data []byte) (payload []byte, version uint32, err error) {
	if len(data) < FooterSize {
		return nil, 0, fmt.Errorf("%w: file too short (%d bytes)", ErrBadFooter, len(data))
	}
	tail := data[len(data)-FooterSize:]
	if tail[0] != Magic[0] || tail[1] != Magic[1] || tail[2] != Magic[2] || tail[3] != Magic[3] {
		return nil, 0, fmt.Errorf("%w: bad magic", ErrBadFooter)
	}
	version = binary.LittleEndian.Uint32(tail[4:8])
	want := binary.LittleEndian.Uint32(tail[8:12])
	// CRC 覆盖除 crc 字段本身外的全部字节(payload+magic+version)。
	got := crc32.ChecksumIEEE(data[:len(data)-4])
	if got != want {
		return nil, 0, fmt.Errorf("%w: crc mismatch got=%08x want=%08x", ErrBadFooter, got, want)
	}
	return data[:len(data)-FooterSize], version, nil
}

// AppendUvarint 追加一个 varint。
func AppendUvarint(dst []byte, v uint64) []byte {
	return binary.AppendUvarint(dst, v)
}

// Uvarint 从 b 起始解析一个 varint,返回值与消耗的字节数。
func Uvarint(b []byte) (uint64, int) {
	return binary.Uvarint(b)
}

// AppendDelta 编码严格递增的 docID 序列:首值原样,其余为与前值之差,
// 全部 varint 编码后追加到 dst。
func AppendDelta(dst []byte, ids []uint32) []byte {
	var prev uint32
	for i, id := range ids {
		if i > 0 && id <= prev {
			// 输入约定为递增;非递增按 0 差值容错(不 panic)。
			dst = binary.AppendUvarint(dst, 0)
			continue
		}
		d := uint64(id)
		if i > 0 {
			d = uint64(id - prev)
		}
		dst = binary.AppendUvarint(dst, d)
		prev = id
	}
	return dst
}

// DeltaDecode 解码 AppendDelta 的输出,返回 n 个 docID。
func DeltaDecode(data []byte, n int) ([]uint32, error) {
	out := make([]uint32, 0, n)
	var prev uint64
	for len(out) < n {
		v, sz := binary.Uvarint(data)
		if sz <= 0 {
			return nil, fmt.Errorf("codec: delta decode failed at %d: truncated varint", len(out))
		}
		data = data[sz:]
		prev += v
		if prev > 0xFFFFFFFF {
			return nil, fmt.Errorf("codec: delta decode overflow at %d", len(out))
		}
		out = append(out, uint32(prev))
	}
	return out, nil
}
