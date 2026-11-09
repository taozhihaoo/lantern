package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"

	"lantern/internal/analysis"
	"lantern/internal/snippet"
)

// Regression: snippet.Highlight uses UTF-8 byte offsets, while the browser
// slices with UTF-16 code units. Chinese text (3 bytes / 1 unit) and emoji
// (4 bytes / 2 units) used to produce completely misplaced <mark> spans.
func TestWebHighlightsUTF16(t *testing.T) {
	cases := []string{
		"全文搜索引擎 design 说明",
		"emoji 🚀 launch 中文 mixed",
		"plain ascii words only",
		"🚀🚀 prefix 搜索引擎 tail",
	}
	for _, body := range cases {
		res := snippet.Build(body, 0, func(analysis.Token) bool { return true })
		wh := webHighlights(res.Text, res.Highlights)
		if len(wh) != len(res.Highlights) {
			t.Fatalf("%q: got %d web highlights, want %d", body, len(wh), len(res.Highlights))
		}
		units := utf16.Encode([]rune(res.Text))
		for i, h := range wh {
			if h.Start < 0 || h.End > len(units) || h.Start >= h.End {
				t.Fatalf("%q: bad UTF-16 range %+v (total %d units)", body, h, len(units))
			}
			got := string(utf16.Decode(units[h.Start:h.End]))
			raw := res.Highlights[i]
			want := res.Text[raw.Start:raw.End]
			if got != want {
				t.Fatalf("%q: highlight %d = %q, want %q", body, i, got, want)
			}
		}
	}
}

// Regression: every endpoint used to answer POST/PUT/DELETE with 200.
func TestReadOnlyMethods(t *testing.T) {
	s := setupServer(t)
	h := s.Handler()
	paths := []string{"/", "/api/search?q=alpha", "/api/why-not?q=alpha&path=a.md",
		"/api/doc?path=a.md", "/api/stats", "/healthz"}
	for _, path := range paths {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s -> %d, want 405", method, path, rec.Code)
			}
			if rec.Header().Get("Allow") != "GET, HEAD" {
				t.Fatalf("%s %s missing Allow header", method, path)
			}
		}
	}
}

// Regression: an invalid `n` used to be silently replaced by the default.
func TestPageSizeValidation(t *testing.T) {
	s := setupServer(t)
	h := s.Handler()
	for _, n := range []string{"0", "-3", "abc", "101", "999999"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/search?q=alpha&n="+n, nil))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid n") {
			t.Fatalf("n=%s -> %d %s", n, rec.Code, rec.Body.String())
		}
	}
	for _, n := range []string{"1", "20", "100"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/search?q=alpha&n="+n, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("n=%s -> %d %s", n, rec.Code, rec.Body.String())
		}
	}
}

// explain=true was undocumented; the UI uses explain=1, make both explicit.
func TestSearchExplainAliasAndTiming(t *testing.T) {
	s := setupServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/api/search?q=alpha&explain=true", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d %s", rec.Code, rec.Body.String())
	}
	var resp searchRespJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.TookMs < 0 || resp.TookMs > 60000 {
		t.Fatalf("took_ms=%v", resp.TookMs)
	}
	found := false
	for _, hit := range resp.Hits {
		if hit.Explain != nil {
			found = true
		}
	}
	if !found {
		t.Fatal("explain=true did not produce explanations")
	}
}

// Regression: terms inside a NOT branch were highlighted as if they matched.
func TestQueryMatcherSkipsNegatedTerms(t *testing.T) {
	m := queryMatcher("alpha NOT beta")
	cases := map[string]bool{
		"alpha": true,
		"beta":  false,
		"other": false,
	}
	for text, want := range cases {
		if got := m(analysis.Token{Text: text}); got != want {
			t.Fatalf("queryMatcher(%q) = %v, want %v", text, got, want)
		}
	}
	if !queryMatcher("pre*")(analysis.Token{Text: "prefix"}) {
		t.Fatal("prefix match broken")
	}
}
