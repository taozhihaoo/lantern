// Package server implements the read-only HTTP service and the embedded
// single-file web UI: /api/search, /api/why-not, /api/doc, /api/stats,
// /healthz and /.
//
// Security: the index is read-only; /api/doc only serves paths that are
// already present in the index; every response carries CSP / nosniff /
// X-Frame-Options / Referrer-Policy headers; API endpoints are GET/HEAD only.
package server

import (
	"embed"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lantern/internal/analysis"
	"lantern/internal/fsx"
	"lantern/internal/index"
	"lantern/internal/query"
	"lantern/internal/rank"
	"lantern/internal/snippet"
	"lantern/internal/whynot"
)

//go:embed ui/index.html
var uiFS embed.FS

// Server is a read-only HTTP service over an index.
type Server struct {
	ix      *index.Index
	base    string
	maxSize int64
}

// New creates a service (read-only; no write lock is acquired).
func New(ix *index.Index, base string) *Server {
	return &Server{ix: ix, base: base, maxSize: 2 << 20}
}

// Handler returns the router wrapped with security headers.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", getOnly(s.handleRoot))
	mux.HandleFunc("/api/search", getOnly(s.handleSearch))
	mux.HandleFunc("/api/why-not", getOnly(s.handleWhyNot))
	mux.HandleFunc("/api/doc", getOnly(s.handleDoc))
	mux.HandleFunc("/api/stats", getOnly(s.handleStats))
	mux.HandleFunc("/healthz", getOnly(s.handleHealth))
	return securityHeaders(mux)
}

// getOnly restricts an endpoint to GET/HEAD. A search service must never
// accept POST/PUT/DELETE, even though the handlers themselves are read-only.
func getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSON(w, http.StatusMethodNotAllowed,
				map[string]string{"error": "method not allowed; use GET or HEAD"})
			return
		}
		next(w, r)
	}
}

// HTTPServer returns a fully configured *http.Server so callers can control
// graceful shutdown while sharing one timeout policy.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// ListenAndServe listens on addr (callers should default to 127.0.0.1).
func (s *Server) ListenAndServe(addr string) error {
	return s.HTTPServer(addr).ListenAndServe()
}

// securityHeaders applies the same headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "geolocation=(), microphone=(), camera=()")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := uiFS.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, "ui missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// snapshotFor takes an MVCC snapshot for a single request.
func (s *Server) snapshotFor(w http.ResponseWriter) (*index.Snapshot, *query.Searcher, bool) {
	snap, err := s.ix.Snapshot()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return nil, nil, false
	}
	srch := query.NewSearcher(snap, rank.DefaultParams())
	srch.SetRecency(0, 0, time.Now().Unix())
	return snap, srch, true
}

// webHighlight is a snippet highlight expressed in UTF-16 code units, which
// is the index unit of JavaScript String.prototype.slice.
//
// snippet.Highlight uses UTF-8 byte offsets (correct for the Go CLI and for
// the /api JSON contract). For a web client those offsets are wrong: a CJK
// character is 3 bytes but 1 UTF-16 unit, and an emoji is 4 bytes but 2
// units. Converting here keeps the Go-side contract intact while making the
// embedded UI able to slice the string directly.
type webHighlight struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// webHighlights converts byte offsets into UTF-16 code unit offsets.
// Offsets that do not land on a UTF-8 rune boundary are dropped instead of
// producing a broken <mark> span.
func webHighlights(text string, hs []snippet.Highlight) []webHighlight {
	if len(hs) == 0 {
		return nil
	}
	boundaries := make(map[int]int, len(text)+1)
	byteOff, unitOff := 0, 0
	for byteOff < len(text) {
		boundaries[byteOff] = unitOff
		r, size := utf8.DecodeRuneInString(text[byteOff:])
		// Keep this Go 1.22 compatible: unicode/utf16.RuneLen was added in
		// Go 1.23. A supplementary-plane rune occupies a surrogate pair.
		units := 1
		if r > 0xFFFF && r <= 0x10FFFF {
			units = 2
		}
		byteOff += size
		unitOff += units
	}
	boundaries[len(text)] = unitOff

	out := make([]webHighlight, 0, len(hs))
	for _, h := range hs {
		start, okS := boundaries[h.Start]
		end, okE := boundaries[h.End]
		if !okS || !okE || start >= end {
			continue
		}
		out = append(out, webHighlight{Start: start, End: end})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

type searchHitJSON struct {
	Path       string         `json:"path"`
	Title      string         `json:"title,omitempty"`
	Score      float64        `json:"score"`
	Snippet    string         `json:"snippet,omitempty"`
	Highlights []webHighlight `json:"highlights,omitempty"`
	Explain    *rank.Explain  `json:"explain,omitempty"`
}

type searchRespJSON struct {
	Query     string          `json:"query"`
	Total     uint64          `json:"total"`
	Truncated bool            `json:"truncated"`
	TookMs    float64         `json:"took_ms"`
	Hits      []searchHitJSON `json:"hits"`
}

const maxPageSize = 100

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing q"})
		return
	}
	n := 10
	if raw := r.URL.Query().Get("n"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 || v > maxPageSize {
			writeJSON(w, http.StatusBadRequest,
				map[string]string{"error": "invalid n: expected an integer in 1..100"})
			return
		}
		n = v
	}
	ex := r.URL.Query().Get("explain")
	wantExplain := ex == "1" || strings.EqualFold(ex, "true")

	snap, srch, ok := s.snapshotFor(w)
	if !ok {
		return
	}
	defer snap.Close()
	res, err := srch.Search(q, n, wantExplain)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	m := queryMatcher(q)
	resp := searchRespJSON{Query: res.Query, Total: res.Total, Truncated: res.Truncated,
		TookMs: float64(time.Since(started).Microseconds()) / 1000,
		Hits:   make([]searchHitJSON, 0, len(res.Hits))}
	for _, h := range res.Hits {
		hj := searchHitJSON{Path: h.Path, Title: h.Title, Score: h.Score, Explain: h.Explain}
		if sd := s.storedOf(h.Path); sd != nil && sd.Body != "" {
			sn := snippet.Build(sd.Body, 0, m)
			hj.Snippet = sn.Text
			hj.Highlights = webHighlights(sn.Text, sn.Highlights)
		}
		resp.Hits = append(resp.Hits, hj)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleWhyNot(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if q == "" || path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing q or path"})
		return
	}
	snap, srch, ok := s.snapshotFor(w)
	if !ok {
		return
	}
	defer snap.Close()
	rep, err := whynot.Diagnose(s.ix, srch, whynot.Config{Base: s.base, MaxSize: s.maxSize, Fsys: fsx.OsFS()}, q, path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) handleDoc(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing path"})
		return
	}
	// Only paths already present in the index are served (no disk reads, no
	// traversal surface).
	sd := s.storedOf(path)
	if sd == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not in index"})
		return
	}
	dv := index.DV{}
	if e, sr, ok := s.ix.Lookup(path); ok && sr != nil {
		if v, err := sr.Reader().DocValues(e.DocID); err == nil {
			dv = v
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path": sd.Path, "title": sd.Title, "body": sd.Body,
		"size": sd.Size, "mtime": sd.MTime, "ext": dv.Ext,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.ix.Stats()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"docs": st.Docs, "terms": st.Terms, "segments": st.Segments,
		"bytes": st.Bytes,
		"avg_len": map[string]float64{
			"title": st.AvgLen[1], "path": st.AvgLen[0], "body": st.AvgLen[2],
		},
	})
}

// storedOf returns the stored document for a path.
func (s *Server) storedOf(path string) *index.StoredDoc {
	e, sr, ok := s.ix.Lookup(path)
	if !ok || sr == nil {
		return nil
	}
	sd, err := sr.Reader().StoredDoc(e.DocID)
	if err != nil {
		return nil
	}
	return sd
}

// queryMatcher builds the snippet match predicate from the query AST.
// Negated branches are deliberately skipped: a term excluded by NOT must
// never be highlighted as if it were a hit.
func queryMatcher(q string) snippet.Matcher {
	exact := map[string]bool{}
	var prefixes []string
	if node, err := query.Parse(q); err == nil {
		var walk func(n query.Node, positive bool)
		walk = func(n query.Node, positive bool) {
			if n == nil {
				return
			}
			switch t := n.(type) {
			case *query.TermNode:
				if !positive {
					return
				}
				toks := analysis.Analyze(t.Text)
				for _, tok := range toks {
					if tok.Kind == analysis.KindSub {
						continue
					}
					exact[tok.Text] = true
				}
				if t.IsPrefix && len(toks) > 0 {
					prefixes = append(prefixes, toks[0].Text)
				}
			case *query.PhraseNode:
				if !positive {
					return
				}
				for _, tok := range analysis.Analyze(t.Raw) {
					if tok.Kind != analysis.KindSub {
						exact[tok.Text] = true
					}
				}
			case *query.AndNode:
				for _, c := range t.Children {
					walk(c, positive)
				}
			case *query.OrNode:
				for _, c := range t.Children {
					walk(c, positive)
				}
			case *query.NotNode:
				walk(t.Child, false)
			}
		}
		walk(node, true)
	}
	return func(tok analysis.Token) bool {
		if exact[tok.Text] {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(tok.Text, p) {
				return true
			}
		}
		return false
	}
}
