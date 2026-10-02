// bench_search 对指定索引运行延迟基准,输出 p50/p95(规格 12)。
//
//	go run scripts/bench_search.go -index .lantern -qfile queries.txt [-n 200]
//
// queries.txt 每行一条查询;或用 -mode and|phrase|prefix|fuzzy 自动生成。
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strings"
	"time"

	"lantern/internal/fsx"
	"lantern/internal/index"
	"lantern/internal/query"
	"lantern/internal/rank"
)

func main() {
	idxDir := flag.String("index", ".lantern", "索引目录")
	qfile := flag.String("qfile", "", "查询文件(每行一条)")
	mode := flag.String("mode", "and", "无 -qfile 时的查询模式: and/phrase/prefix/fuzzy")
	num := flag.Int("n", 200, "执行次数")
	flag.Parse()

	ix, err := index.Open(*idxDir, fsx.OsFS())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer ix.Close()
	snap, err := ix.Snapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer snap.Close()
	s := query.NewSearcher(snap, rank.DefaultParams())
	s.SetRecency(0, 0, time.Now().Unix())

	var queries []string
	if *qfile != "" {
		f, err := os.Open(*qfile)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" && !strings.HasPrefix(line, "#") {
				queries = append(queries, line)
			}
		}
		f.Close()
	} else {
		queries = sampleQueries(snap, *mode)
	}
	if len(queries) == 0 {
		fmt.Fprintln(os.Stderr, "no queries")
		os.Exit(1)
	}
	rng := rand.New(rand.NewSource(1))
	var durs []time.Duration
	var totalHits uint64
	start := time.Now()
	for i := 0; i < *num; i++ {
		q := queries[rng.Intn(len(queries))]
		t0 := time.Now()
		res, err := s.Search(q, 10, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query %q: %v\n", q, err)
			os.Exit(1)
		}
		totalHits += res.Total
		durs = append(durs, time.Since(t0))
	}
	elapsed := time.Since(start)
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	pct := func(p float64) time.Duration {
		i := int(float64(len(durs)-1) * p)
		return durs[i]
	}
	fmt.Printf("mode=%s queries=%d runs=%d total_hits=%d\n", *mode, len(queries), *num, totalHits)
	fmt.Printf("p50=%v p95=%v mean=%.2fms throughput=%.1f qps\n",
		pct(0.50), pct(0.95), float64(elapsed.Microseconds())/float64(*num)/1000.0,
		float64(*num)/elapsed.Seconds())
}

// sampleQueries 从词典采样构造查询。
func sampleQueries(snap *index.Snapshot, mode string) []string {
	// 取 body 词典中间位置的词,保证有命中。
	var terms []string
	for _, seg := range snap.Segments() {
		_ = seg.Reader().Terms(2, func(term string, e index.TermEntry) bool {
			if e.DocFreq >= 2 {
				terms = append(terms, term)
			}
			return len(terms) < 400
		})
		if len(terms) >= 400 {
			break
		}
	}
	if len(terms) < 3 {
		terms = []string{"search", "engine", "the"}
	}
	pick := func(r *rand.Rand) string { return terms[r.Intn(len(terms))] }
	var qs []string
	r := rand.New(rand.NewSource(7))
	for i := 0; i < 20; i++ {
		switch mode {
		case "phrase":
			qs = append(qs, fmt.Sprintf("\"%s %s\"", pick(r), pick(r)))
		case "prefix":
			w := pick(r)
			if len(w) > 2 {
				w = w[:2]
			}
			qs = append(qs, w+"*")
		case "fuzzy":
			w := pick(r)
			if len(w) > 3 {
				w = w[:len(w)-1]
			}
			qs = append(qs, w+"~")
		default: // and:三词 AND
			qs = append(qs, fmt.Sprintf("%s %s %s", pick(r), pick(r), pick(r)))
		}
	}
	return qs
}
