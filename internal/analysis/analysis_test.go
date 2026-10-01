package analysis

import (
	"reflect"
	"testing"
	"unicode/utf8"
)

func TestAnalyzeTable(t *testing.T) {
	type want struct {
		text  string
		pos   int
		start int
		end   int
		kind  Kind
	}
	cases := []struct {
		name  string
		input string
		toks  []want
	}{
		{
			name:  "纯拉丁",
			input: "hello world",
			toks: []want{
				{"hello", 0, 0, 5, KindWord},
				{"world", 1, 6, 11, KindWord},
			},
		},
		{
			name:  "中英混排",
			input: "Go语言并发编程",
			toks: []want{
				{"go", 0, 0, 2, KindWord},
				{"语言", 1, 2, 8, KindCJK},
				{"言并", 2, 5, 11, KindCJK},
				{"并发", 3, 8, 14, KindCJK},
				{"发编", 4, 11, 17, KindCJK},
				{"编程", 5, 14, 20, KindCJK},
			},
		},
		{
			name:  "camelCase 标识符",
			input: "parseHTTPRequest",
			toks: []want{
				{"parsehttprequest", 0, 0, 16, KindWord},
				{"parse", 0, 0, 5, KindSub},
				{"http", 1, 5, 9, KindSub},
				{"request", 2, 9, 16, KindSub},
			},
		},
		{
			name:  "连续大写缩写",
			input: "HTTPRequest",
			toks: []want{
				{"httprequest", 0, 0, 11, KindWord},
				{"http", 0, 0, 4, KindSub},
				{"request", 1, 4, 11, KindSub},
			},
		},
		{
			name:  "snake_case 逐词切分",
			input: "snake_case_name",
			toks: []want{
				{"snake", 0, 0, 5, KindWord},
				{"case", 1, 6, 10, KindWord},
				{"name", 2, 11, 15, KindWord},
			},
		},
		{
			name:  "版本号",
			input: "v2.1.3",
			toks: []want{
				{"v2", 0, 0, 2, KindWord},
				{"1", 1, 3, 4, KindWord},
				{"3", 2, 5, 6, KindWord},
			},
		},
		{
			name:  "数字字母混合保持",
			input: "v2Max x86",
			toks: []want{
				{"v2max", 0, 0, 5, KindWord},
				{"v2", 0, 0, 2, KindSub},
				{"max", 1, 2, 5, KindSub},
				{"x86", 2, 6, 9, KindWord},
			},
		},
		{
			name:  "全角折叠(连续原子仍为一个词)",
			input: "Ｇｏｌａｎｇ１２３",
			toks: []want{
				{"golang123", 0, 0, 27, KindWord},
			},
		},
		{
			name:  "全角字母的标识符切分",
			input: "ＦｕｌｌＷｉｄｔｈ",
			toks: []want{
				{"fullwidth", 0, 0, 27, KindWord},
				{"full", 0, 0, 12, KindSub},
				{"width", 1, 12, 27, KindSub},
			},
		},
		{
			name:  "全角空格分隔",
			input: "ｘ　ｙ",
			toks: []want{
				{"x", 0, 0, 3, KindWord},
				{"y", 1, 6, 9, KindWord},
			},
		},
		{
			name:  "单个 CJK 字",
			input: "擎",
			toks: []want{
				{"擎", 0, 0, 3, KindCJK},
			},
		},
		{
			name:  "两个 CJK 字",
			input: "引擎",
			toks: []want{
				{"引擎", 0, 0, 6, KindCJK},
			},
		},
		{
			name:  "标点切分",
			input: "e-mail: foo@bar.com",
			toks: []want{
				{"e", 0, 0, 1, KindWord},
				{"mail", 1, 2, 6, KindWord},
				{"foo", 2, 8, 11, KindWord},
				{"bar", 3, 12, 15, KindWord},
				{"com", 4, 16, 19, KindWord},
			},
		},
		{
			name:  "非法 UTF-8 不 panic 且偏移合法",
			input: "\xff\xfeGO",
			toks: []want{
				{"go", 0, 2, 4, KindWord},
			},
		},
		{
			name:  "空输入",
			input: "",
			toks:  nil,
		},
		{
			name:  "纯标点",
			input: "!!! ... ,,,",
			toks:  nil,
		},
		{
			name:  "全大写缩写不切分",
			input: "ABC",
			toks: []want{
				{"abc", 0, 0, 3, KindWord},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Analyze(tc.input)
			if len(got) != len(tc.toks) {
				t.Fatalf("token count = %d, want %d\ngot: %+v", len(got), len(tc.toks), got)
			}
			for i, w := range tc.toks {
				g := got[i]
				if g.Text != w.text || g.Pos != w.pos || g.StartByte != w.start ||
					g.EndByte != w.end || g.Kind != w.kind {
					t.Fatalf("token[%d] = %+v, want %+v", i, g, w)
				}
				// Text 必须与原文切片对应位置(经折叠小写)语义一致:
				// 偏移必须是 rune 边界。
				if !utf8.RuneStart(tc.input[g.StartByte]) ||
					(g.EndByte < len(tc.input) && !utf8.RuneStart(tc.input[g.EndByte])) {
					t.Fatalf("token[%d] offsets %d:%d not rune-aligned", i, g.StartByte, g.EndByte)
				}
			}
		})
	}
}

func TestAnalyzeOffsetsMonotonic(t *testing.T) {
	inputs := []string{
		"Go语言并发编程 parseHTTPRequest snake_case_name v2.1.3 Ｆｕｌｌ引擎ABC x",
		"引擎擎擎擎 aBcD123 __init__",
	}
	for _, in := range inputs {
		toks := Analyze(in)
		prevStart, prevPos := -1, -1
		for i, tok := range toks {
			if tok.StartByte < prevStart {
				t.Fatalf("%q: token[%d] StartByte %d < prev %d", in, i, tok.StartByte, prevStart)
			}
			if tok.Pos < prevPos {
				t.Fatalf("%q: token[%d] Pos %d < prev %d", in, i, tok.Pos, prevPos)
			}
			prevStart, prevPos = tok.StartByte, tok.Pos
		}
	}
}

// 短语 "http request" 必须命中 parseHTTPRequest(4.4 的硬性约定)。
func TestPhraseHTTPRequests(t *testing.T) {
	toks := Analyze("call parseHTTPRequest here")
	var httpPos, reqPos int
	found := 0
	for _, tok := range toks {
		switch tok.Text {
		case "http":
			httpPos = tok.Pos
			found++
		case "request":
			reqPos = tok.Pos
			found++
		}
	}
	if found != 2 || reqPos != httpPos+1 {
		t.Fatalf("http@%d request@%d: 子词位置必须相邻", httpPos, reqPos)
	}
}

func TestKindString(t *testing.T) {
	if KindWord.String() != "word" || KindSub.String() != "sub" || KindCJK.String() != "cjk" {
		t.Fatal("unexpected Kind strings")
	}
	if Kind(99).String() != "unknown" {
		t.Fatal("unexpected unknown kind string")
	}
}

func TestIsCJK(t *testing.T) {
	cases := map[rune]bool{
		'擎': true, 'ひ': true, 'カ': true, '한': true, 'a': false, '1': false,
	}
	for r, want := range cases {
		if got := IsCJK(r); got != want {
			t.Fatalf("IsCJK(%q) = %v, want %v", r, got, want)
		}
	}
}

func TestAnalyzeDeterministic(t *testing.T) {
	in := "引擎 parseHTTPRequest Ｇｏ 语言"
	if !reflect.DeepEqual(Analyze(in), Analyze(in)) {
		t.Fatal("Analyze 必须是确定性的")
	}
}

func FuzzAnalyze(f *testing.F) {
	f.Add("Go语言并发编程")
	f.Add("parseHTTPRequest snake_case_name v2.1.3")
	f.Add("引擎 擎 ＦｕｌｌＷｉｄｔｈ OAuth2Token")
	f.Add("\xff\xfe\x00mixed 文字")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		toks := Analyze(s)
		prevStart, prevPos := 0, 0
		// rune 边界仅在合法 UTF-8 输入上有定义(4.6 针对用于高亮的原文)。
		valid := utf8.ValidString(s)
		for i, tok := range toks {
			if tok.StartByte < 0 || tok.EndByte > len(s) || tok.StartByte >= tok.EndByte {
				t.Fatalf("token[%d] bad offsets %d:%d len=%d", i, tok.StartByte, tok.EndByte, len(s))
			}
			if valid && (!utf8.RuneStart(s[tok.StartByte]) ||
				(tok.EndByte < len(s) && !utf8.RuneStart(s[tok.EndByte]))) {
				t.Fatalf("token[%d] offsets not rune-aligned", i)
			}
			if tok.Text == "" {
				t.Fatalf("token[%d] empty text", i)
			}
			if tok.StartByte < prevStart {
				t.Fatalf("token[%d] StartByte not monotonic", i)
			}
			if tok.Pos < prevPos {
				t.Fatalf("token[%d] Pos not monotonic", i)
			}
			prevStart, prevPos = tok.StartByte, tok.Pos
		}
	})
}
