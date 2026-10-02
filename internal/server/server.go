// Package server 实现只读 HTTP 服务与内嵌网页界面(规格 10):
// /api/search、/api/why-not、/api/doc、/api/stats、/healthz 与 /。
// 安全:只读索引;/api/doc 仅返回索引内已有路径;设置 CSP 与
// X-Content-Type-Options。
package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

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

// Server 是只读索引的 HTTP 服务。
type Server struct {
	ix      *index.Index
	base    string
	maxSize int64
}

// New 创建服务(只读;不获取写锁)。
func New(ix *index.Index, base string) *Server {
	return &Server{ix: ix, base: base, maxSize: 2 << 20}
}

// Handler 返回带安全头的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/api/search", s.handleSearch)
	mux.HandleFunc("/api/why-not", s.handleWhyNot)
	mux.HandleFunc("/api/doc", s.handleDoc)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/healthz", s.handleHealth)
	return securityHeaders(mux)
}

// ListenAndServe 监听地址(默认应使用 127.0.0.1)。
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Handler())
}

// securityHeaders 统一设置安全响应头。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; connect-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
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
	_, _ = w.Write(data)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// snapshotFor 为单次请求取 MVCC 快照(规格 5.8)。
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

type searchHitJSON struct {
	Path       string              `json:"path"`
	Title      string              `json:"title,omitempty"`
	Score      float64             `json:"score"`
	Snippet    string              `json:"snippet,omitempty"`
	Highlights []snippet.Highlight `json:"highlights,omitempty"`
	Explain    *rank.Explain       `json:"explain,omitempty"`
}

type searchRespJSON struct {
	Query     string          `json:"query"`
	Total     uint64          `json:"total"`
	Truncated bool            `json:"truncated"`
	Hits      []searchHitJSON `json:"hits"`
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing q"})
		return
	}
	n := 10
	if v, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && v > 0 && v <= 100 {
		n = v
	}
	wantExplain := r.URL.Query().Get("explain") == "1"

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
		Hits: make([]searchHitJSON, 0, len(res.Hits))}
	for _, h := range res.Hits {
		hj := searchHitJSON{Path: h.Path, Title: h.Title, Score: h.Score, Explain: h.Explain}
		if sd := s.storedOf(h.Path); sd != nil && sd.Body != "" {
			sn := snippet.Build(sd.Body, 0, m)
			hj.Snippet = sn.Text
			hj.Highlights = sn.Highlights
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
	// 只允许返回索引内已有路径(不读磁盘,无穿越面)。
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

// storedOf 取路径的存储文档。
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

// queryMatcher 由查询串构建片段命中判定(词项 + CJK 二元组 + 前缀)。
func queryMatcher(q string) snippet.Matcher {
	exact := map[string]bool{}
	var prefixes []string
	if node, err := query.Parse(q); err == nil {
		var walk func(n query.Node)
		walk = func(n query.Node) {
			if n == nil {
				return
			}
			switch t := n.(type) {
			case *query.TermNode:
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
				for _, tok := range analysis.Analyze(t.Raw) {
					if tok.Kind != analysis.KindSub {
						exact[tok.Text] = true
					}
				}
			case *query.AndNode:
				for _, c := range t.Children {
					walk(c)
				}
			case *query.OrNode:
				for _, c := range t.Children {
					walk(c)
				}
			case *query.NotNode:
				walk(t.Child)
			}
		}
		walk(node)
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

var _ = fmt.Sprintf
