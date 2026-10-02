package cli

import (
	"strings"

	"lantern/internal/analysis"
	"lantern/internal/index"
	"lantern/internal/query"
	"lantern/internal/rank"
	"lantern/internal/snippet"
)

// newSearcherCLI 创建默认参数的搜索器。
func newSearcherCLI(snap *index.Snapshot) *query.Searcher {
	return query.NewSearcher(snap, rank.DefaultParams())
}

// parseDays 解析 "30d"/"12h"/"2w"/"365" 为天数。
func parseDays(s string) float64 {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 30
	}
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "d"):
		s = strings.TrimSuffix(s, "d")
	case strings.HasSuffix(s, "h"):
		mult, s = 1.0/24, strings.TrimSuffix(s, "h")
	case strings.HasSuffix(s, "w"):
		mult, s = 7, strings.TrimSuffix(s, "w")
	}
	var v float64
	if _, err := fmtSscan(s, &v); err != nil || v < 0 {
		return 30
	}
	return v * mult
}

// storedOf 取路径的存储文档(片段高亮用);找不到返回 nil。
func storedOf(ix *index.Index, path string) *index.StoredDoc {
	e, sr, ok := ix.Lookup(path)
	if !ok || sr == nil {
		return nil
	}
	sd, err := sr.Reader().StoredDoc(e.DocID)
	if err != nil {
		return nil
	}
	return sd
}

// queryMatcher 根据查询串构建片段命中判定:精确词项(含 CJK 二元组)
// 与前缀。模糊词项不做片段高亮(仅参与检索)。
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
