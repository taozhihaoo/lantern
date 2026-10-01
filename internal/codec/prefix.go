package codec

import (
	"encoding/binary"
	"fmt"
)

// PrefixBlock 是前缀压缩的有序字符串块(terms.dat 每 64 词项一块)。
//
// 布局:n_entries(u16 LE) + 每项 [shared_len(varint) suffix_len(varint) suffix(bytes)]。
// shared_len 为与前一项的公共前缀长度;首项 shared_len=0。
// 输入必须已按字典序排序且无重复,否则解码结果不保证(写侧负责)。
type PrefixBlock struct {
	Terms []string
}

// EncodePrefixBlock 编码一组有序字符串。
func EncodePrefixBlock(terms []string) []byte {
	out := make([]byte, 2, 64)
	binary.LittleEndian.PutUint16(out, uint16(len(terms)))
	var prev string
	for _, t := range terms {
		shared := 0
		max := len(prev)
		if len(t) < max {
			max = len(t)
		}
		for shared < max && prev[shared] == t[shared] {
			shared++
		}
		out = binary.AppendUvarint(out, uint64(shared))
		out = binary.AppendUvarint(out, uint64(len(t)-shared))
		out = append(out, t[shared:]...)
		prev = t
	}
	return out
}

// DecodePrefixBlock 解码一个前缀压缩块。
func DecodePrefixBlock(data []byte) (*PrefixBlock, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("codec: prefix block truncated header")
	}
	n := int(binary.LittleEndian.Uint16(data))
	data = data[2:]
	pb := &PrefixBlock{Terms: make([]string, 0, n)}
	var prev []byte
	for i := 0; i < n; i++ {
		shared, sz := binary.Uvarint(data)
		if sz <= 0 {
			return nil, fmt.Errorf("codec: prefix block entry %d: bad shared len", i)
		}
		data = data[sz:]
		sufLen, sz := binary.Uvarint(data)
		if sz <= 0 || uint64(len(data)-sz) < sufLen {
			return nil, fmt.Errorf("codec: prefix block entry %d: bad suffix len", i)
		}
		data = data[sz:]
		if int(shared) > len(prev) {
			return nil, fmt.Errorf("codec: prefix block entry %d: shared %d > prev %d", i, shared, len(prev))
		}
		term := make([]byte, 0, int(shared)+int(sufLen))
		term = append(term, prev[:shared]...)
		term = append(term, data[:sufLen]...)
		data = data[sufLen:]
		prev = term
		pb.Terms = append(pb.Terms, string(term))
	}
	return pb, nil
}
