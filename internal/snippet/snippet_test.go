package snippet

import (
	"strings"
	"testing"

	"lantern/internal/analysis"
)

func matchText(terms ...string) Matcher {
	set := map[string]bool{}
	for _, t := range terms {
		set[t] = true
	}
	return func(tok analysis.Token) bool { return set[tok.Text] }
}

func TestSnippetBasic(t *testing.T) {
	body := "The quick brown fox jumps over the lazy dog. " +
		"Search engines index documents for fast retrieval. " +
		"Another sentence about indexing and search quality follows here."
	res := Build(body, 80, matchText("search", "engines"))
	if !strings.Contains(res.Text, "Search engines") {
		t.Fatalf("snippet missing hit: %q", res.Text)
	}
	if len(res.Highlights) == 0 {
		t.Fatal("no highlights")
	}
	for _, h := range res.Highlights {
		if h.Start < 0 || h.End > len(res.Text) || h.Start >= h.End {
			t.Fatalf("bad highlight %+v in len %d", h, len(res.Text))
		}
		word := res.Text[h.Start:h.End]
		lw := strings.ToLower(word)
		if lw != "search" && lw != "engines" {
			t.Fatalf("highlight on %q", word)
		}
	}
}

func TestSnippetPicksDensestWindow(t *testing.T) {
	body := "alpha one two three " +
		strings.Repeat("filler ", 40) +
		"alpha beta alpha gamma alpha delta"
	res := Build(body, 60, matchText("alpha"))
	// 最密的窗口在尾部(4 个 alpha)。
	if !strings.Contains(res.Text, "beta") || !strings.Contains(res.Text, "delta") {
		t.Fatalf("snippet should center on dense tail: %q", res.Text)
	}
	if got := strings.Count(res.Text, "alpha"); got < 2 {
		t.Fatalf("expected >=2 alphas in window, got %d: %q", got, res.Text)
	}
}

func TestSnippetCJKAndIdentifiers(t *testing.T) {
	body := "前半部分没有相关内容,这里是关于全文搜索引擎的详细说明,包含索引与查询。"
	res := Build(body, 40, matchText("搜索", "引擎"))
	if !strings.Contains(res.Text, "搜索") || !strings.Contains(res.Text, "引擎") {
		t.Fatalf("CJK snippet missing hits: %q", res.Text)
	}
	// 高亮区间必须落在 rune 边界且对应命中词。
	for _, h := range res.Highlights {
		if !strings.ContainsAny(res.Text[h.Start:h.End], "搜索引") {
			t.Fatalf("highlight %q not a hit", res.Text[h.Start:h.End])
		}
	}

	body2 := "the parser calls parseHTTPRequest and snake_case_name helpers"
	res2 := Build(body2, 80, func(tok analysis.Token) bool {
		return tok.Kind == analysis.KindSub && (tok.Text == "http" || tok.Text == "request")
	})
	if !strings.Contains(res2.Text, "parseHTTPRequest") {
		t.Fatalf("identifier snippet missing: %q", res2.Text)
	}
}

func TestSnippetNoHits(t *testing.T) {
	body := strings.Repeat("nothing relevant here ", 20)
	res := Build(body, 30, matchText("absent"))
	if len(res.Highlights) != 0 {
		t.Fatal("no-hit snippet must have no highlights")
	}
	if res.Text == "" || strings.Contains(res.Text, "absent") {
		t.Fatalf("fallback snippet wrong: %q", res.Text)
	}
	if len([]rune(res.Text)) > 31 {
		t.Fatalf("fallback too long: %d runes", len([]rune(res.Text)))
	}
}
