package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"lantern/internal/fsx"
	"lantern/internal/index"
	"lantern/internal/scan"
)

func setupServer(t *testing.T) *Server {
	t.Helper()
	base := t.TempDir()
	files := map[string]string{
		"a.md":  "# Alpha\n全文搜索引擎 alphaDocument",
		"b.txt": "beta release notes",
	}
	for rel, content := range files {
		if err := osWrite(base+"/"+rel, content); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := index.Open(base+"/.lantern", fsx.OsFS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.Close() })
	w, err := ix.AcquireWriter(true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	outs, _, _, err := scan.Run(scan.Options{
		Base: base, Targets: []string{"."}, Workers: 1,
		State:     func(string) (scan.StateEntry, bool) { return scan.StateEntry{}, false },
		StateList: func() []string { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range outs {
		if err := w.AddDoc(d.Doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	return New(ix, base)
}

func TestSearchAPI(t *testing.T) {
	s := setupServer(t)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/search?q=%E6%90%9C%E7%B4%A2&n=5&explain=1", nil)
	srv.Config.Handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp searchRespJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total < 1 || len(resp.Hits) == 0 || resp.Hits[0].Path != "a.md" {
		t.Fatalf("search result: %+v", resp)
	}
	if resp.Hits[0].Explain == nil {
		t.Fatal("explain missing")
	}
	if !strings.Contains(resp.Hits[0].Snippet, "搜索") {
		t.Fatalf("snippet: %q", resp.Hits[0].Snippet)
	}
}

func TestSearchAPISyntaxError(t *testing.T) {
	s := setupServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/search?q=%28unclosed", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestWhyNotAPI(t *testing.T) {
	s := setupServer(t)
	handler := s.Handler()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/why-not?q=alpha&path=b.txt", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var rep map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if v, ok := rep["in_index"].(bool); !ok || !v {
		t.Fatalf("in_index: %v", rep)
	}
}

func TestDocAPIOnlyIndexed(t *testing.T) {
	s := setupServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/doc?path=a.md", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["path"] != "a.md" {
		t.Fatalf("doc=%v", doc)
	}
	// 索引外路径必须 404(穿越防护)。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/api/doc?path=../../etc/passwd", nil)
	s.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != 404 {
		t.Fatalf("traversal code=%d", rec2.Code)
	}
}

func TestSecurityHeadersAndHealth(t *testing.T) {
	s := setupServer(t)
	handler := s.Handler()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("ui code=%d", rec.Code)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); csp == "" ||
		!strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP: %q", csp)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("nosniff missing")
	}
	if !strings.Contains(rec.Body.String(), "Lantern") {
		t.Fatal("ui content")
	}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/healthz", nil)
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != 200 || !strings.Contains(rec2.Body.String(), "ok") {
		t.Fatalf("healthz: %d %s", rec2.Code, rec2.Body.String())
	}
}
