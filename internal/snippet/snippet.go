// Package snippet 实现搜索结果的片段选择与高亮:
// 在文档正文中选取包含命中的窗口,并给出命中词项的字节高亮区间。
package snippet

import (
	"lantern/internal/analysis"
)

// Highlight 是片段文本内的一个高亮字节区间 [Start, End)。
type Highlight struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// Result 是一个片段。
type Result struct {
	// Text 为片段文本(正文的子串)。
	Text string `json:"text"`
	// Highlights 为 Text 内的高亮区间(字节偏移,相对 Text)。
	Highlights []Highlight `json:"highlights"`
	// Start 为片段在原正文中的字节偏移。
	Start int `json:"start"`
}

// Matcher 报告一个正文 token 是否为查询命中(词项相等、前缀、模糊等
// 由调用方决定)。
type Matcher func(tok analysis.Token) bool

// DefaultWidth 是默认片段目标宽度(rune 数)。
const DefaultWidth = 160

// Build 从正文中选取片段:分析正文,找出命中 token,以命中密度最高
// 的位置为中心裁出约 width 个 rune 的窗口(窗口边界对齐 token),
// 输出片段与相对高亮。无命中时返回正文开头截断。
func Build(body string, width int, m Matcher) Result {
	if width <= 0 {
		width = DefaultWidth
	}
	toks := analysis.Analyze(body)
	var hits []analysis.Token
	for _, tk := range toks {
		if m(tk) {
			hits = append(hits, tk)
		}
	}
	if len(hits) == 0 {
		return truncRune(body, 0, width)
	}
	// 以每个命中为中心,统计窗口内的命中数,取密度最高者。
	bestCenter := hits[0]
	bestCount := -1
	for _, h := range hits {
		lo := h.StartByte
		hi := h.EndByte
		// 扩展到约 width rune。
		lo = expandLeft(body, lo, width/3)
		hi = expandRight(body, hi, width)
		cnt := 0
		for _, tk := range hits {
			if tk.StartByte >= lo && tk.EndByte <= hi {
				cnt++
			}
		}
		if cnt > bestCount {
			bestCount = cnt
			bestCenter = h
		}
	}
	start := expandLeft(body, bestCenter.StartByte, width/3)
	end := expandRight(body, bestCenter.EndByte, width)
	// 窗口至少包含首个命中。
	if start > hits[0].StartByte {
		start = expandLeft(body, hits[0].StartByte, width/4)
	}
	var res Result
	res.Start = start
	res.Text = body[start:end]
	for _, h := range hits {
		if h.StartByte >= start && h.EndByte <= end {
			res.Highlights = append(res.Highlights, Highlight{Start: h.StartByte - start, End: h.EndByte - start})
		}
	}
	return res
}

func truncRune(s string, start, width int) Result {
	end := expandRight(s, start, width)
	return Result{Text: s[start:end], Start: start}
}

// expandLeft 从 off 向左扩展约 n 个 rune,对齐到 rune 边界。
func expandLeft(s string, off, n int) int {
	cnt := 0
	for off > 0 && cnt < n {
		off--
		for off > 0 && !isRuneStart(s[off]) {
			off--
		}
		cnt++
	}
	return off
}

// expandRight 从 off 向右扩展约 n 个 rune(不超过文长,rune 对齐)。
func expandRight(s string, off, n int) int {
	total := len(s)
	cnt := 0
	for off < total && cnt < n {
		off++
		cnt++
		for off < total && !isRuneStart(s[off]) {
			off++
		}
	}
	return off
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
