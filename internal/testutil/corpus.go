// Package testutil 提供固定种子的随机语料生成器与暴力(oracle)实现,
// 仅供各包的 _test.go 导入(生产代码禁止导入,见 DECISIONS.md D12)。
package testutil

import (
	"fmt"
	"math/rand"

	"lantern/internal/analysis"
)

// Doc 是语料文档。
type Doc struct {
	Path  string
	Title string
	Body  string
	Ext   string
	Size  int64
	MTime int64
}

// 拉丁词表(前一半高频,模拟齐夫分布)。
var latinWords = []string{
	"search", "engine", "index", "query", "parse", "http", "request",
	"lantern", "data", "concurrent", "go", "rank", "term", "posting",
	"segment", "merge", "cache", "token", "field", "score", "alpha",
	"beta", "gamma", "delta", "lambda", "omega", "text", "match",
	"prefix", "fuzzy", "phrase", "filter", "boost", "explain",
}

// 标识符(测试标识符感知分词)。
var idents = []string{
	"parseHTTPRequest", "snake_case_name", "v2_1_3", "HTTPRequest",
	"my_func", "doWork", "XMLParser", "ioReader", "kebab-case-id",
	"config_loader", "AbstractFactory", "OAuth2Token",
}

// 中文单字池(组合成 bigram)。
var cjkChars = []rune("全文搜索引擎索引查询排序分词倒排中文测试数据并发编程语言")

// randomWord 按 Zipf 风格选择:前一半词表概率更高。
func randomWord(rng *rand.Rand) string {
	switch rng.Intn(10) {
	case 0:
		return idents[rng.Intn(len(idents))]
	case 1, 2:
		// 中文 2-4 字。
		n := 2 + rng.Intn(3)
		out := make([]rune, n)
		for i := range out {
			out[i] = cjkChars[rng.Intn(len(cjkChars))]
		}
		return string(out)
	default:
		// 齐夫:指数偏向词表头部。
		idx := 0
		for idx < len(latinWords)-1 && rng.Intn(3) != 0 {
			idx++
		}
		return latinWords[idx]
	}
}

// RandomCorpus 生成 n 篇固定种子的随机文档。
func RandomCorpus(seed, n int) []Doc {
	rng := rand.New(rand.NewSource(int64(seed)))
	docs := make([]Doc, 0, n)
	for i := 0; i < n; i++ {
		var bodyWords []string
		m := 20 + rng.Intn(60)
		for j := 0; j < m; j++ {
			bodyWords = append(bodyWords, randomWord(rng))
		}
		body := ""
		for j, w := range bodyWords {
			if j > 0 {
				body += " "
			}
			body += w
		}
		var titleWords []string
		for j := 0; j < 1+rng.Intn(3); j++ {
			titleWords = append(titleWords, randomWord(rng))
		}
		title := ""
		for j, w := range titleWords {
			if j > 0 {
				title += " "
			}
			title += w
		}
		ext := []string{"md", "txt", "go", "json"}[rng.Intn(4)]
		path := fmt.Sprintf("dir%d/sub%d/doc%04d.%s", rng.Intn(3), rng.Intn(4), i, ext)
		docs = append(docs, Doc{
			Path:  path,
			Title: title,
			Body:  body,
			Ext:   ext,
			Size:  int64(len(body)),
			MTime: 1704067200 + int64(rng.Intn(730))*86400, // 2024 起两年内
		})
	}
	return docs
}

// fieldTokens 是单字段的 token 流。
type fieldTokens []analysis.Token

// docTokens 是单文档各字段 token 流。
type docTokens struct {
	fields [3]fieldTokens // path/title/body
}

// Field 返回指定字段的 token 流(0=path 1=title 2=body;测试用)。
func (dt docTokens) Field(f int) fieldTokens { return dt.fields[f] }

// AnalyzeDoc 分析文档三个可检索字段。
func AnalyzeDoc(d Doc) docTokens {
	var dt docTokens
	dt.fields[0] = analysis.Analyze(d.Path)
	dt.fields[1] = analysis.Analyze(d.Title)
	dt.fields[2] = analysis.Analyze(d.Body)
	return dt
}

// Levenshtein 计算字节串编辑距离(与引擎 D14 语义一致)。
func Levenshtein(a, b []byte) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			m := prev[j] + 1
			if cur[j-1]+1 < m {
				m = cur[j-1] + 1
			}
			if prev[j-1]+cost < m {
				m = prev[j-1] + cost
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
