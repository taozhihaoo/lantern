package testutil

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
)

// RandomQuery 生成一条随机查询串(规格 11.2:布尔/短语/前缀/字段/过滤)。
// 词项取自语料高频词,保证多数查询有命中。
func RandomQuery(rng *rand.Rand, docs []Doc) string {
	// 语料高频词采样。
	pickWord := func() string {
		d := docs[rng.Intn(len(docs))]
		switch rng.Intn(3) {
		case 0:
			// 标题词。
			ws := strings.Fields(d.Title)
			if len(ws) > 0 {
				return ws[rng.Intn(len(ws))]
			}
		case 1:
			ws := strings.Fields(d.Body)
			if len(ws) > 0 {
				return ws[rng.Intn(len(ws))]
			}
		}
		return randomWord(rng)
	}
	pickTerm := func() string {
		switch rng.Intn(12) {
		case 0: // 字段限定
			w := pickWord()
			f := []string{"title:", "body:", "path:", "title:", "body:"}[rng.Intn(5)]
			return f + w
		case 1: // 短语
			d := docs[rng.Intn(len(docs))]
			ws := strings.Fields(d.Body)
			if len(ws) >= 2 {
				i := rng.Intn(len(ws) - 1)
				n := 2
				if len(ws)-i > 2 && rng.Intn(2) == 0 {
					n = 3
				}
				q := "\"" + strings.Join(ws[i:i+n], " ") + "\""
				if rng.Intn(4) == 0 {
					q += "~" + strconv.Itoa(1+rng.Intn(4))
				}
				return q
			}
			return pickWord()
		case 2: // 前缀(按 rune 切,避免截断 UTF-8)
			w := pickWord()
			rs := []rune(w)
			if len(rs) > 2 {
				return string(rs[:1+rng.Intn(2)]) + "*"
			}
			return w
		case 3: // 模糊
			w := pickWord()
			if len([]rune(w)) >= 3 {
				return w + "~"
			}
			return w
		case 4: // ext 过滤
			return "ext:" + []string{"md", "txt", "go", "json"}[rng.Intn(4)]
		case 5: // size 过滤
			op := []string{">", ">=", "<", "<="}[rng.Intn(4)]
			return "size:" + op + strconv.Itoa(50+rng.Intn(300)) + "b"
		case 6: // mtime 过滤
			op := []string{">", ">=", "<", "<="}[rng.Intn(4)]
			year := 2024 + rng.Intn(2)
			month := 1 + rng.Intn(12)
			return "mtime:" + op + fmt.Sprintf("%d-%02d", year, month)
		default:
			w := pickWord()
			if rng.Intn(8) == 0 {
				return "-" + w
			}
			return w
		}
	}
	n := 1 + rng.Intn(3)
	var toks []string
	for i := 0; i < n; i++ {
		toks = append(toks, pickTerm())
		if i < n-1 && rng.Intn(4) == 0 {
			toks = append(toks, "OR")
		}
	}
	return strings.Join(toks, " ")
}
