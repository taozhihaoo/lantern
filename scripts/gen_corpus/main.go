// gen_corpus 生成基准语料(规格 12:10 万篇、约 500MB 文本)。
//
//	go run scripts/gen_corpus.go -dir corpus -n 100000 -target-mb 500
//
// 词表为齐夫分布,混合拉丁词、标识符与中文;每篇文档随机追加/截断,
// 使总体积接近目标值。
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var latin = []string{
	"search", "engine", "index", "query", "parse", "http", "request", "lantern",
	"data", "concurrent", "rank", "term", "posting", "segment", "merge", "cache",
	"token", "field", "score", "alpha", "beta", "gamma", "delta", "lambda",
	"text", "match", "prefix", "fuzzy", "phrase", "filter", "boost", "explain",
	"doc", "store", "commit", "recover", "snapshot", "segment", "manifest",
}

var idents = []string{
	"parseHTTPRequest", "snake_case_name", "v2_1_3", "HTTPRequest", "my_func",
	"doWork", "XMLParser", "ioReader", "config_loader", "OAuth2Token",
}

var cjk = []rune("全文搜索引擎索引查询排序分词倒排中文测试数据并发编程语言存储段合并提交恢复快照")

func word(rng *rand.Rand) string {
	switch rng.Intn(10) {
	case 0:
		return idents[rng.Intn(len(idents))]
	case 1, 2:
		n := 2 + rng.Intn(3)
		b := make([]rune, n)
		for i := range b {
			b[i] = cjk[rng.Intn(len(cjk))]
		}
		return string(b)
	default:
		idx := 0
		for idx < len(latin)-1 && rng.Intn(3) != 0 {
			idx++
		}
		return latin[idx]
	}
}

func main() {
	dir := flag.String("dir", "corpus", "输出目录")
	n := flag.Int("n", 100000, "文档数")
	targetMB := flag.Int("target-mb", 500, "目标总体积(MB)")
	seed := flag.Int64("seed", 42, "随机种子")
	flag.Parse()

	rng := rand.New(rand.NewSource(*seed))
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	targetBytes := int64(*targetMB) << 20
	perDoc := targetBytes / int64(*n)
	start := time.Now()
	var total int64
	for i := 0; i < *n; i++ {
		var sb strings.Builder
		sb.WriteString("# Document ")
		sb.WriteString(fmt.Sprintf("%d\n\n", i))
		// 目标词数 ≈ perDoc/7(平均词长+空格)。
		words := int(perDoc / 7)
		for j := 0; j < words; j++ {
			sb.WriteString(word(rng))
			sb.WriteByte(' ')
			if j%16 == 15 {
				sb.WriteByte('\n')
			}
		}
		sb.WriteByte('\n')
		data := sb.String()
		total += int64(len(data))
		sub := fmt.Sprintf("d%02d", i%25)
		if err := os.MkdirAll(filepath.Join(*dir, sub), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		name := filepath.Join(*dir, sub, fmt.Sprintf("doc%06d.md", i))
		if err := os.WriteFile(name, []byte(data), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if i%10000 == 0 && i > 0 {
			fmt.Printf("... %d docs, %.1f MB\n", i, float64(total)/1024/1024)
		}
	}
	fmt.Printf("done: %d docs, %.1f MB, %.1fs (%.1f MB/s)\n",
		*n, float64(total)/1024/1024, time.Since(start).Seconds(),
		float64(total)/1024/1024/time.Since(start).Seconds())
}
