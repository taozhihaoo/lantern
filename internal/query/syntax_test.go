package query

import (
	"strings"
	"testing"
)

func TestParseValid(t *testing.T) {
	cases := []string{
		"foo bar",
		"foo OR bar",
		"foo AND bar OR baz",
		"-foo",
		"NOT foo AND bar",
		"(a OR b) AND -c",
		"\"exact phrase\"",
		"\"a b\"~3",
		"title:foo path:src body:bar",
		"pre*",
		"foo~",
		"foo~1",
		"foo~2",
		"ext:go",
		"size:>1mb",
		"size:<=10kb",
		"mtime:>2025-01-01",
		"mtime:2025-06",
		"title:\"hello world\"~2",
	}
	for _, q := range cases {
		if _, err := Parse(q); err != nil {
			t.Fatalf("Parse(%q) unexpected error: %v", q, err)
		}
	}
}

func TestSyntaxErrorColumns(t *testing.T) {
	cases := []struct {
		q    string
		want string // 错误消息子串
		col  int
	}{
		{"(foo AND bar", "缺少右括号", 1},
		{"foo)", "多余的右括号", 4},
		{"\"unclosed", "缺少右引号", 1},
		{"AND foo", "缺少操作数", 1},
		{"foo OR", "查询意外结束", 7},
		{"a @ b", "意外字符", 3},
	}
	for _, tc := range cases {
		_, err := Parse(tc.q)
		if err == nil {
			t.Fatalf("Parse(%q): expected error", tc.q)
		}
		se, ok := err.(*SyntaxError)
		if !ok {
			t.Fatalf("Parse(%q): not SyntaxError: %v", tc.q, err)
		}
		if !strings.Contains(se.Msg, tc.want) {
			t.Fatalf("Parse(%q): msg %q want contains %q", tc.q, se.Msg, tc.want)
		}
		if se.Col != tc.col {
			t.Fatalf("Parse(%q): col=%d want %d", tc.q, se.Col, tc.col)
		}
		msg := se.Error()
		lines := strings.Split(msg, "\n")
		if len(lines) != 3 {
			t.Fatalf("error output must have 3 lines, got %d:\n%s", len(lines), msg)
		}
		if !strings.HasPrefix(lines[0], "query:1:") {
			t.Fatalf("first line: %q", lines[0])
		}
		caret := strings.Index(lines[2], "^")
		if caret != tc.col { // "  " 前缀 2 字符 + col-1 空格 → index = col+1... 见下
			// 行首有两个前导空格,caret 的 rune 下标 = 2 + col - 1 = col+1。
			if caret != tc.col+1 {
				t.Fatalf("caret col: got %d want %d\n%s", caret, tc.col+1, msg)
			}
		}
	}
}

func TestOperatorPrecedence(t *testing.T) {
	// a OR b c ⇒ a OR (b AND c):OR 的第二个操作数是 AND 节点。
	n, err := Parse("a OR b c")
	if err != nil {
		t.Fatal(err)
	}
	or, ok := n.(*OrNode)
	if !ok || len(or.Children) != 2 {
		t.Fatalf("want OrNode with 2 children, got %T", n)
	}
	if _, ok := or.Children[1].(*AndNode); !ok {
		t.Fatalf("second operand must be AndNode (NOT>AND>OR), got %T", or.Children[1])
	}
	// -a b ⇒ AND(NOT a, b)。
	n, err = Parse("-a b")
	if err != nil {
		t.Fatal(err)
	}
	and, ok := n.(*AndNode)
	if !ok || len(and.Children) != 2 {
		t.Fatalf("want AndNode, got %T", n)
	}
	if _, ok := and.Children[0].(*NotNode); !ok {
		t.Fatalf("first child must be NotNode, got %T", and.Children[0])
	}
}
