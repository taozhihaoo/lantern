// Package analysis 实现查询与索引共用的规范化与分词。
//
// 规则(GOAL.md 第 4 节):
//   - 全角 ASCII 折叠为半角(最小折叠表,见 DECISIONS.md D3);
//   - 转小写;
//   - 拉丁/数字按非字母数字切分,保留字母数字混合体(v2);
//   - 标识符感知:camelCase/PascalCase/连续大写缩写切出子词;
//   - CJK 连续串输出重叠二元组,长度 1 输出单字。
//
// 输出 Token 的 StartByte/EndByte 是原文有效字节偏移且落在 rune 边界,
// 全部 token 按 StartByte 单调不减排列,Pos 非递减。
package analysis

import (
	"unicode"
	"unicode/utf8"
)

// Kind 区分 token 的来源类型。
type Kind uint8

const (
	// KindWord 为普通词(拉丁/数字原子,或标识符整体)。
	KindWord Kind = iota
	// KindSub 为标识符切分出的子词(camelCase 子串)。
	KindSub
	// KindCJK 为 CJK 二元组或单字 token。
	KindCJK
)

// String 实现 fmt.Stringer,便于测试输出与 Explain 渲染。
func (k Kind) String() string {
	switch k {
	case KindWord:
		return "word"
	case KindSub:
		return "sub"
	case KindCJK:
		return "cjk"
	}
	return "unknown"
}

// Token 是分析输出的最小单元。
type Token struct {
	// Text 为规范化(折叠+小写)后的词项文本。
	Text string
	// Pos 为词序位置:普通 token 依次递增;标识符整体与首个子词共享
	// 同一 Pos,后续子词依次递增;CJK 二元组每个占一个位置。
	Pos       int
	StartByte int
	// EndByte 为原文中该 token 字节区间的开区间端点。
	EndByte int
	Kind    Kind
}

// fold 把全角 ASCII 与全角空格折叠为半角;其他字符原样返回。
func fold(r rune) rune {
	switch {
	case r >= 0xFF01 && r <= 0xFF5E:
		return r - 0xFEE0
	case r == 0x3000:
		return ' '
	}
	return r
}

// IsCJK 报告 r 是否属于 Han/平假名/片假名/谚文。
func IsCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// atomRune 是原子内的一个 rune:折叠后的字符与其在原文中的字节区间。
type atomRune struct {
	fr    rune
	start int
	end   int
}

// identCuts 在(已小写的)rune 序列上找标识符切分点(camelCase/
// PascalCase/连续大写缩写)。返回 rune 下标切分点(不含 0 与 n);
// 无切分点时返回 nil。
func identCuts(rs []rune) []int {
	var cuts []int
	for i := 1; i < len(rs); i++ {
		cur, prev := rs[i], rs[i-1]
		if !unicode.IsUpper(cur) {
			continue
		}
		if unicode.IsLower(prev) || unicode.IsDigit(prev) {
			cuts = append(cuts, i)
			continue
		}
		if unicode.IsUpper(prev) && i+1 < len(rs) && unicode.IsLower(rs[i+1]) {
			cuts = append(cuts, i)
		}
	}
	return cuts
}

// Analyze 对 text 分词,返回全部 token(顺序与原文一致)。
// 对任意输入(含非法 UTF-8)都不 panic。
func Analyze(text string) []Token {
	toks := make([]Token, 0, 16)
	atom := make([]atomRune, 0, 16)
	pos := 0
	i := 0
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		fr := fold(r)
		if !isAlnum(fr) {
			i += size
			continue
		}
		// 收集一个原子:极大连续字母数字段。
		atom := atom[:0]
		j := i
		for j < len(text) {
			r2, s2 := utf8.DecodeRuneInString(text[j:])
			f2 := fold(r2)
			if !isAlnum(f2) {
				break
			}
			atom = append(atom, atomRune{fr: f2, start: j, end: j + s2})
			j += s2
		}
		toks, pos = emitAtom(toks, atom, pos)
		i = j
	}
	return toks
}

// emitAtom 按原文顺序输出一个原子内的 CJK 二元组与拉丁词/子词,
// 返回推进后的位置计数。
func emitAtom(toks []Token, atom []atomRune, pos int) ([]Token, int) {
	k := 0
	for k < len(atom) {
		if IsCJK(atom[k].fr) {
			m := k
			for m < len(atom) && IsCJK(atom[m].fr) {
				m++
			}
			if m-k == 1 {
				toks = append(toks, Token{
					Text: string(atom[k].fr), Pos: pos,
					StartByte: atom[k].start, EndByte: atom[k].end, Kind: KindCJK,
				})
				pos++
			} else {
				for x := k; x+1 < m; x++ {
					toks = append(toks, Token{
						Text:      string(atom[x].fr) + string(atom[x+1].fr),
						Pos:       pos,
						StartByte: atom[x].start,
						EndByte:   atom[x+1].end,
						Kind:      KindCJK,
					})
					pos++
				}
			}
			k = m
			continue
		}
		m := k
		for m < len(atom) && !IsCJK(atom[m].fr) {
			m++
		}
		toks, pos = emitLatinRun(toks, atom[k:m], pos)
		k = m
	}
	return toks, pos
}

// emitLatinRun 输出一个拉丁/数字子段:无切分点时为单个词;
// 有切分点时输出整体 token 与子词(整体与首子词共享 Pos)。
// 切分点在保留大小写的折叠序列上计算,输出时再逐 rune 小写化。
func emitLatinRun(toks []Token, run []atomRune, pos int) ([]Token, int) {
	folded := make([]rune, len(run))
	for i, ar := range run {
		folded[i] = ar.fr
	}
	cuts := identCuts(folded)
	if len(cuts) == 0 {
		toks = append(toks, Token{
			Text: lowerRun(folded), Pos: pos,
			StartByte: run[0].start, EndByte: run[len(run)-1].end, Kind: KindWord,
		})
		return toks, pos + 1
	}
	toks = append(toks, Token{
		Text: lowerRun(folded), Pos: pos,
		StartByte: run[0].start, EndByte: run[len(run)-1].end, Kind: KindWord,
	})
	bounds := append([]int{0}, cuts...)
	bounds = append(bounds, len(folded))
	for b := 0; b+1 < len(bounds); b++ {
		s, e := bounds[b], bounds[b+1]
		toks = append(toks, Token{
			Text:      lowerRun(folded[s:e]),
			Pos:       pos + b,
			StartByte: run[s].start,
			EndByte:   run[e-1].end,
			Kind:      KindSub,
		})
	}
	return toks, pos + len(bounds) - 1
}

// lowerRun 逐 rune 小写(1:1 映射,不改变 rune 数)。
func lowerRun(rs []rune) string {
	buf := make([]rune, len(rs))
	for i, r := range rs {
		buf[i] = unicode.ToLower(r)
	}
	return string(buf)
}
