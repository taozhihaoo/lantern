// Package query 实现 Lantern 查询语言:lexer、parser、AST、planner
// 与迭代器执行引擎。
//
// 语法(优先级 NOT > AND > OR;词之间隐式 AND):
//
//	foo bar              两词都需出现
//	foo OR bar           任一出现(AND/OR/NOT 必须大写)
//	-foo / NOT foo       排除
//	"exact phrase"       短语(基于位置);"a b"~3 最大间隔 3
//	title:foo path:src   字段限定(省略则在 title/path/body 联合检索)
//	pre*                 前缀(展开上限 1024 词项,超限标记 truncated)
//	foo~ / foo~1         模糊(无数字时按词长自动定级)
//	ext:go size:>1mb size:<=10kb mtime:>2025-01-01 mtime:2025-06   过滤
package query

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// 词法单元类型。
type tokKind int

const (
	tokEOF tokKind = iota
	tokLParen
	tokRParen
	tokAnd
	tokOr
	tokNot
	tokMinus
	tokTerm
	tokPhrase
)

// token 是词法单元。Term 的修饰信息直接解析进结构:
// Field 限定、前缀 *、模糊 ~N。
type token struct {
	kind tokKind
	text string // 原始文本(term 的含字段前缀原文;phrase 的引号内内容)
	// 以下仅 tokTerm / tokPhrase 有效:
	field    string // "" 或 title/path/body/ext/size/mtime
	isPrefix bool
	fuzzy    int  // -1 = 无;0..2 = 编辑距离
	slop     int  // phrase 的 ~N;0 = 严格短语
	hasFuzzy bool // 区分 foo~(自动)与无修饰
	raw      string
	col      int // rune 列(1-based)
}

// SyntaxError 是查询语法错误,带列号与指示符。
type SyntaxError struct {
	Msg   string
	Line  int
	Col   int
	Query string
}

// Error 按 "query:行:列: 消息 + 原文 + ^ 指示符" 格式输出。
func (e *SyntaxError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "query:%d:%d: %s\n", e.Line, e.Col, e.Msg)
	sb.WriteString("  " + e.Query + "\n  ")
	// caret 按 rune 计数对齐列。
	r := []rune(e.Query)
	for i := 0; i < e.Col-1 && i < len(r); i++ {
		if r[i] == '\t' {
			sb.WriteByte('\t')
		} else {
			sb.WriteByte(' ')
		}
	}
	sb.WriteString("^")
	return sb.String()
}

func syntaxErr(q string, line, col int, format string, args ...any) *SyntaxError {
	return &SyntaxError{Msg: fmt.Sprintf(format, args...), Line: line, Col: col, Query: q}
}

// 已知字段限定符。
var knownFields = map[string]bool{
	"title": true, "path": true, "body": true,
	"ext": true, "size": true, "mtime": true,
}

// Lexer 把查询串切成 token 流。
type Lexer struct {
	query string
	runes []rune
	pos   int // rune 下标
	line  int
}

// NewLexer 创建词法器(单行查询;Line 恒为 1)。
func NewLexer(q string) *Lexer {
	return &Lexer{query: q, runes: []rune(q), line: 1}
}

func (lx *Lexer) cur() (rune, bool) {
	if lx.pos >= len(lx.runes) {
		return 0, false
	}
	return lx.runes[lx.pos], true
}

func (lx *Lexer) col() int { return lx.pos + 1 }

func (lx *Lexer) peekAt(off int) (rune, bool) {
	if lx.pos+off >= len(lx.runes) {
		return 0, false
	}
	return lx.runes[lx.pos+off], true
}

func isTermChar(r rune) bool {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	switch r {
	case '_', '-', '.', '#', '@', '/', '\\':
		return true
	}
	return false
}

func isFieldIdent(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// Lex 全部切分。
func (lx *Lexer) Lex() ([]token, error) {
	var out []token
	for {
		r, ok := lx.cur()
		if !ok {
			out = append(out, token{kind: tokEOF, col: lx.col()})
			return out, nil
		}
		switch {
		case r == ' ' || r == '\t' || r == '\n':
			lx.pos++
		case r == '(':
			out = append(out, token{kind: tokLParen, col: lx.col()})
			lx.pos++
		case r == ')':
			out = append(out, token{kind: tokRParen, col: lx.col()})
			lx.pos++
		case r == '"':
			tok, err := lx.lexPhrase()
			if err != nil {
				return nil, err
			}
			out = append(out, tok)
		case r == '-':
			// 仅当后跟词字符时为排除符。
			if n, ok := lx.peekAt(1); ok && (isTermChar(n) || n == '"') {
				out = append(out, token{kind: tokMinus, col: lx.col()})
				lx.pos++
			} else {
				out = append(out, lx.lexTerm())
			}
		default:
			if isFieldIdent(r) {
				// 大写操作符。
				if w, n := lx.matchWord(); n > 0 {
					if w == "AND" {
						lx.pos += n
						out = append(out, token{kind: tokAnd, col: lx.col()})
						continue
					}
					if w == "OR" {
						lx.pos += n
						out = append(out, token{kind: tokOr, col: lx.col()})
						continue
					}
					if w == "NOT" {
						lx.pos += n
						out = append(out, token{kind: tokNot, col: lx.col()})
						continue
					}
				}
				out = append(out, lx.lexTerm())
			} else {
				return nil, syntaxErr(lx.query, 1, lx.col(), "意外字符 %q", string(r))
			}
		}
	}
}

// matchWord 尝试匹配连续大写字母单词,返回 (词, 长度)。
func (lx *Lexer) matchWord() (string, int) {
	n := 0
	for {
		r, ok := lx.peekAt(n)
		if !ok || !unicode.IsUpper(r) {
			break
		}
		n++
	}
	if n == 0 {
		return "", 0
	}
	return string(lx.runes[lx.pos : lx.pos+n]), n
}

// splitField 检查当前位置是否为 "knownfield:" 前缀,是则消费并返回 true。
func (lx *Lexer) splitField() (string, bool) {
	n := 0
	for {
		r, ok := lx.peekAt(n)
		if !ok || !isFieldIdent(r) {
			break
		}
		n++
	}
	if n == 0 {
		return "", false
	}
	if r, ok := lx.peekAt(n); !ok || r != ':' {
		return "", false
	}
	name := string(lx.runes[lx.pos : lx.pos+n])
	if !knownFields[name] {
		return "", false
	}
	lx.pos += n + 1
	return name, true
}

// lexTerm 解析一个词项(含字段前缀、前缀 *、模糊 ~)。
func (lx *Lexer) lexTerm() token {
	startCol := lx.col()
	tok := token{kind: tokTerm, col: startCol}
	if field, ok := lx.splitField(); ok {
		tok.field = field
	}
	// 值部分:过滤字段读过滤值;普通字段读词项。
	if tok.field == "ext" || tok.field == "size" || tok.field == "mtime" {
		start := lx.pos
		for {
			r, ok := lx.cur()
			if !ok || r == ' ' || r == '\t' || r == '(' || r == ')' {
				break
			}
			lx.pos++
		}
		tok.text = string(lx.runes[start:lx.pos])
		tok.raw = tok.text
		return tok
	}
	start := lx.pos
	for {
		r, ok := lx.cur()
		if !ok || !isTermChar(r) {
			break
		}
		lx.pos++
	}
	text := string(lx.runes[start:lx.pos])
	if text == "" {
		tok.text = ""
		tok.raw = ""
		return tok
	}
	// 后缀修饰:前缀 * 与模糊 ~N。
	if strings.HasSuffix(text, "*") {
		text = strings.TrimRight(text, "*")
		tok.isPrefix = true
	}
	if idx := strings.LastIndex(text, "~"); idx >= 0 {
		suffix := text[idx+1:]
		if suffix == "" {
			tok.hasFuzzy = true
			tok.fuzzy = -1 // 自动按词长定级
			text = text[:idx]
		} else if allDigits(suffix) {
			tok.hasFuzzy = true
			tok.fuzzy = atoi(suffix)
			text = text[:idx]
		}
	}
	tok.text = text
	tok.raw = string(lx.runes[start:lx.pos])
	return tok
}

// lexPhrase 解析 "..."(含 ~N slop)。
func (lx *Lexer) lexPhrase() (token, error) {
	startCol := lx.col()
	tok := token{kind: tokPhrase, col: startCol}
	if field, ok := lx.splitField(); ok {
		tok.field = field
	}
	if r, ok := lx.cur(); !ok || r != '"' {
		return tok, syntaxErr(lx.query, 1, startCol, "缺少引号")
	}
	quoteCol := lx.col()
	lx.pos++
	var sb strings.Builder
	for {
		r, ok := lx.cur()
		if !ok {
			return tok, syntaxErr(lx.query, 1, quoteCol, "缺少右引号")
		}
		lx.pos++
		if r == '"' {
			break
		}
		if r == '\\' {
			nr, ok2 := lx.cur()
			if !ok2 {
				return tok, syntaxErr(lx.query, 1, quoteCol, "缺少右引号")
			}
			lx.pos++
			sb.WriteRune(nr)
			continue
		}
		sb.WriteRune(r)
	}
	tok.text = sb.String()
	// slop。
	if r, ok := lx.cur(); ok && r == '~' {
		lx.pos++
		start := lx.pos
		for {
			r2, ok2 := lx.cur()
			if !ok2 || !unicode.IsDigit(r2) {
				break
			}
			lx.pos++
		}
		if lx.pos > start {
			tok.slop = atoi(string(lx.runes[start:lx.pos]))
		} else {
			tok.slop = 0
		}
	}
	tok.raw = string(lx.runes[startCol-1 : lx.pos])
	return tok, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return utf8.RuneCountInString(s) < 10
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 1 << 30
		}
	}
	return n
}
