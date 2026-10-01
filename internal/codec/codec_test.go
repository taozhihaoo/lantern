package codec

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

func TestFooterRoundtrip(t *testing.T) {
	payload := []byte("hello lantern 全文搜索")
	data := AppendFooter(payload, 1)
	got, ver, err := ParseFooter(data)
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 || !bytes.Equal(got, payload) {
		t.Fatalf("roundtrip mismatch: ver=%d payload=%q", ver, got)
	}
}

func TestFooterCorruption(t *testing.T) {
	data := AppendFooter([]byte("payload payload payload"), 7)
	// 翻转 payload 中间一个字节。
	bad := append([]byte(nil), data...)
	bad[3] ^= 0xFF
	if _, _, err := ParseFooter(bad); !errors.Is(err, ErrBadFooter) {
		t.Fatalf("expected ErrBadFooter on flip, got %v", err)
	}
	// 截断。
	for _, n := range []int{0, 5, FooterSize - 1} {
		if _, _, err := ParseFooter(data[:n]); !errors.Is(err, ErrBadFooter) {
			t.Fatalf("expected ErrBadFooter on trunc to %d, got %v", n, err)
		}
	}
	// 破坏 CRC 字节。
	bad2 := append([]byte(nil), data...)
	bad2[len(bad2)-1] ^= 0x01
	if _, _, err := ParseFooter(bad2); !errors.Is(err, ErrBadFooter) {
		t.Fatalf("expected ErrBadFooter on crc flip, got %v", err)
	}
}

func TestDeltaRoundtrip(t *testing.T) {
	ids := []uint32{3, 4, 9, 100, 101, 1 << 20, 0xFFFFFFFF - 1, 0xFFFFFFFF}
	enc := AppendDelta(nil, ids)
	dec, err := DeltaDecode(enc, len(ids))
	if err != nil {
		t.Fatal(err)
	}
	if !equalU32(dec, ids) {
		t.Fatalf("delta mismatch: %v", dec)
	}
}

func TestDeltaDecodeTruncated(t *testing.T) {
	enc := AppendDelta(nil, []uint32{1, 5, 9})
	if _, err := DeltaDecode(enc[:2], 3); err == nil {
		t.Fatal("expected error on truncated data")
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBitsetRoundtrip(t *testing.T) {
	b := NewBitset(1000)
	for _, i := range []uint32{0, 1, 63, 64, 65, 127, 500, 999} {
		b.Set(i)
	}
	if got := b.Count(); got != 8 {
		t.Fatalf("count = %d, want 8", got)
	}
	if b.Rank1(64) != 4 {
		t.Fatalf("rank1(64) = %d, want 4", b.Rank1(64))
	}
	enc := AppendBitset(nil, b)
	dec, err := DecodeBitset(enc)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Count() != 8 {
		t.Fatalf("decoded count = %d", dec.Count())
	}
	for i := uint32(0); i < 1000; i++ {
		if dec.Get(i) != b.Get(i) {
			t.Fatalf("bit %d mismatch", i)
		}
	}
	// Iterate 顺序与提前停止。
	var seen []uint32
	b.Iterate(func(i uint32) bool { seen = append(seen, i); return len(seen) < 3 })
	if len(seen) != 3 || seen[0] != 0 || seen[1] != 1 || seen[2] != 63 {
		t.Fatalf("iterate = %v", seen)
	}
}

func TestBitsetTruncated(t *testing.T) {
	b := NewBitset(200)
	b.Set(150)
	enc := AppendBitset(nil, b)
	if _, err := DecodeBitset(enc[:len(enc)-8]); err == nil {
		t.Fatal("expected error on truncated bitset")
	}
	if _, err := DecodeBitset(nil); err == nil {
		t.Fatal("expected error on empty bitset")
	}
}

func TestPrefixBlockRoundtrip(t *testing.T) {
	terms := []string{"body:a", "body:aa", "body:ab", "title:z", "a", "ab", "abc"}
	enc := EncodePrefixBlock(terms)
	dec, err := DecodePrefixBlock(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(dec.Terms) != len(terms) {
		t.Fatalf("count = %d, want %d", len(dec.Terms), len(terms))
	}
	for i := range terms {
		if dec.Terms[i] != terms[i] {
			t.Fatalf("term[%d] = %q, want %q", i, dec.Terms[i], terms[i])
		}
	}
}

func TestPrefixBlockTruncated(t *testing.T) {
	enc := EncodePrefixBlock([]string{"aaa", "aab", "aac"})
	for _, n := range []int{0, 1, 3, len(enc) - 1} {
		if _, err := DecodePrefixBlock(enc[:n]); err == nil {
			t.Fatalf("expected error at trunc %d", n)
		}
	}
	// shared 长度非法。
	bad := EncodePrefixBlock([]string{"aaa"})
	bad = append([]byte{1, 0}, 200)
	bad = append(bad, 0)
	if _, err := DecodePrefixBlock(bad); err == nil {
		t.Fatal("expected error on bad shared len")
	}
}

func TestVarintBasics(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 1 << 32, 1<<64 - 1} {
		enc := AppendUvarint(nil, v)
		got, n := Uvarint(enc)
		if got != v || n != len(enc) {
			t.Fatalf("varint %d: got %d consumed %d/%d", v, got, n, len(enc))
		}
	}
}

func randomSorted(rng *rand.Rand, n, max int) []uint32 {
	out := make([]uint32, 0, n)
	var cur uint32
	for len(out) < n {
		cur += uint32(1 + rng.Intn(max))
		out = append(out, cur)
	}
	return out
}

func TestDeltaRandomRoundtrip(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for iter := 0; iter < 100; iter++ {
		ids := randomSorted(rng, 1+rng.Intn(500), 1+rng.Intn(1000))
		enc := AppendDelta(nil, ids)
		dec, err := DeltaDecode(enc, len(ids))
		if err != nil {
			t.Fatal(err)
		}
		if !equalU32(dec, ids) {
			t.Fatalf("iter %d mismatch", iter)
		}
	}
}

// FuzzCodecRoundtrip 一侧验证任意字节输入下所有解码路径不 panic 且
// 返回错误而非崩溃;另一侧验证编码-解码往返一致性。
func FuzzCodecRoundtrip(f *testing.F) {
	f.Add(AppendFooter([]byte("seed"), 1))
	f.Add(AppendDelta(nil, []uint32{1, 7, 9, 1000}))
	f.Add(AppendBitset(nil, func() *Bitset {
		b := NewBitset(130)
		b.Set(5)
		b.Set(128)
		return b
	}()))
	f.Add(EncodePrefixBlock([]string{"abc", "abd", "abde"}))
	f.Add([]byte{0x01, 0x02, 0x03, 0x04})

	f.Fuzz(func(t *testing.T, data []byte) {
		// 任意字节:解码不 panic。
		ParseFooter(data)
		DeltaDecode(data, 64)
		DecodeBitset(data)
		DecodePrefixBlock(data)

		// 往返:合法编码必须精确还原。
		if payload, ver, err := ParseFooter(AppendFooter(data, 3)); err != nil ||
			ver != 3 || !bytes.Equal(payload, data) {
			t.Fatalf("footer roundtrip failed")
		}
		if b, err := DecodeBitset(AppendBitset(nil, NewBitset(len(data)+64))); err != nil || b.Len() != uint32(len(data)+64) {
			t.Fatalf("bitset roundtrip failed")
		}
		if pb, err := DecodePrefixBlock(EncodePrefixBlock([]string{string(data)})); err != nil ||
			len(pb.Terms) != 1 || pb.Terms[0] != string(data) {
			t.Fatalf("prefix roundtrip failed")
		}
	})
}
