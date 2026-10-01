package codec

import (
	"encoding/binary"
	"fmt"
)

// Bitset 是固定容量的紧凑位集,用于段的墓碑(live 位集)。
type Bitset struct {
	words []uint64
	n     uint32
}

// NewBitset 创建可容纳 n 位的位集。
func NewBitset(n int) *Bitset {
	if n < 0 {
		n = 0
	}
	return &Bitset{words: make([]uint64, (n+63)/64), n: uint32(n)}
}

// Len 返回位集容量。
func (b *Bitset) Len() uint32 { return b.n }

// Set 置位第 i 位(越界忽略,不 panic)。
func (b *Bitset) Set(i uint32) {
	if i >= b.n {
		return
	}
	b.words[i/64] |= 1 << (i % 64)
}

// Clear 清零第 i 位。
func (b *Bitset) Clear(i uint32) {
	if i >= b.n {
		return
	}
	b.words[i/64] &^= 1 << (i % 64)
}

// Get 返回第 i 位是否置位。
func (b *Bitset) Get(i uint32) bool {
	if i >= b.n {
		return false
	}
	return b.words[i/64]&(1<<(i%64)) != 0
}

// Count 返回置位总数(popcount)。
func (b *Bitset) Count() int {
	c := 0
	for _, w := range b.words {
		c += popcount(w)
	}
	return c
}

// Rank1 返回 [0,i] 区间内(含 i)置位个数。
func (b *Bitset) Rank1(i uint32) int {
	if i >= b.n {
		i = b.n - 1
	}
	w := i / 64
	c := 0
	for x := uint32(0); x < w; x++ {
		c += popcount(b.words[x])
	}
	if w < uint32(len(b.words)) {
		c += popcount(b.words[w] & ((1 << (i%64 + 1)) - 1))
	}
	return c
}

// Iterate 依次回调所有置位下标;fn 返回 false 时提前停止。
func (b *Bitset) Iterate(fn func(uint32) bool) {
	for w, word := range b.words {
		if word == 0 {
			continue
		}
		base := uint32(w * 64)
		for bit := uint32(0); bit < 64; bit++ {
			if word&(1<<bit) != 0 {
				idx := base + bit
				if idx >= b.n {
					return
				}
				if !fn(idx) {
					return
				}
			}
		}
	}
}

func popcount(w uint64) int {
	c := 0
	for w != 0 {
		w &= w - 1
		c++
	}
	return c
}

// AppendBitset 序列化:n(u32 LE) + words([]u64 LE)。
func AppendBitset(dst []byte, b *Bitset) []byte {
	var h [4]byte
	binary.LittleEndian.PutUint32(h[:], b.n)
	dst = append(dst, h[:]...)
	var buf [8]byte
	for _, w := range b.words {
		binary.LittleEndian.PutUint64(buf[:], w)
		dst = append(dst, buf[:]...)
	}
	return dst
}

// DecodeBitset 反序列化 AppendBitset 的输出。
func DecodeBitset(data []byte) (*Bitset, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("codec: bitset truncated header")
	}
	n := binary.LittleEndian.Uint32(data)
	needWords := (int(n) + 63) / 64
	if len(data) < 4+needWords*8 {
		return nil, fmt.Errorf("codec: bitset truncated body: need %d words", needWords)
	}
	b := NewBitset(int(n))
	for i := 0; i < needWords; i++ {
		b.words[i] = binary.LittleEndian.Uint64(data[4+i*8:])
	}
	return b, nil
}
