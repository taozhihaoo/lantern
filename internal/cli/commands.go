package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"lantern/internal/fsx"
	"lantern/internal/index"
	"lantern/internal/scan"
	"lantern/internal/snippet"
)

// scanAndApply 执行一次扫描并把变更写入 writer(增量索引公用流程)。
// 返回扫描汇总与删除路径数。
func scanAndApply(w *index.Writer, ix *index.Index, base string, targets []string,
	workers int, maxSize int64, noStoreBody bool) (*scan.Summary, int, error) {
	outs, deletes, sum, err := scan.Run(scan.Options{
		Base:        base,
		Targets:     targets,
		Workers:     workers,
		MaxSize:     maxSize,
		NoStoreBody: noStoreBody,
		State: func(p string) (scan.StateEntry, bool) {
			info, ok := ix.StateInfoOf(p)
			if !ok {
				return scan.StateEntry{}, false
			}
			return scan.StateEntry{Size: info.Size, MTime: info.MTime, SHA: info.SHA}, true
		},
		StateList: ix.StatePaths,
	})
	if err != nil {
		return sum, len(deletes), err
	}
	for _, d := range outs {
		if err := w.AddDoc(d.Doc); err != nil {
			return sum, len(deletes), fmt.Errorf("添加文档 %s: %w", d.Path, err)
		}
	}
	for _, p := range deletes {
		if err := w.DeleteDoc(p); err != nil {
			return sum, len(deletes), fmt.Errorf("删除文档 %s: %w", p, err)
		}
	}
	if err := w.Flush(); err != nil {
		return sum, len(deletes), err
	}
	sum.Deleted = len(deletes)
	return sum, len(deletes), nil
}

func printSummary(sum *scan.Summary) {
	elapsed := sum.Elapsed.Seconds()
	tp := 0.0
	if elapsed > 0 {
		tp = float64(sum.Bytes) / 1024 / 1024 / elapsed
	}
	fmt.Printf("索引完成:新增 %d,更新 %d,删除 %d,mtime 更新 %d,未变 %d\n",
		sum.Added, sum.Updated, sum.Deleted, sum.MTimeOnly, sum.Unchanged)
	if len(sum.Skipped) > 0 {
		keys := make([]scan.SkipReason, 0, len(sum.Skipped))
		for k := range sum.Skipped {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", string(k), sum.Skipped[k]))
		}
		fmt.Printf("跳过:%s\n", strings.Join(parts, ", "))
	}
	fmt.Printf("耗时 %.2fs,读取 %.1f MB(%.2f MB/s),文件 %d\n",
		elapsed, float64(sum.Bytes)/1024/1024, tp, sum.FilesSeen)
}

func scanOptsFor(ix *index.Index, base string, targets []string, workers int, noStore bool) scan.Options {
	return scan.Options{
		Base: base, Targets: targets, Workers: workers, NoStoreBody: noStore,
		State: func(p string) (scan.StateEntry, bool) {
			info, ok := ix.StateInfoOf(p)
			if !ok {
				return scan.StateEntry{}, false
			}
			return scan.StateEntry{Size: info.Size, MTime: info.MTime, SHA: info.SHA}, true
		},
		StateList: ix.StatePaths,
	}
}

// cmdIndex:lantern index <dir...>。
func cmdIndex(args []string) int {
	args, idxDir := parseGlobal(args)
	pv, targets, uerr := parseKnown(args, indexFlags)
	if uerr {
		fmt.Fprintln(os.Stderr, "lantern index: 参数错误")
		return ExitUsage
	}
	workers := pv.int("workers", runtime.GOMAXPROCS(0))
	maxSize := pv.str("max-size", "2MB")
	noStore := pv.boolean("no-store-body")
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "lantern index: 需要至少一个目录")
		return ExitUsage
	}
	limit, err := parseSizeFlag(maxSize)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern index: %v\n", err)
		return ExitUsage
	}
	idxAbs, base := resolvePaths(idxDir)
	ix, err := openIndex(idxAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern index: %v\n", err)
		return ExitErr
	}
	defer ix.Close()
	w, err := ix.AcquireWriter(!noStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern index: %v\n", err)
		return ExitErr
	}
	defer w.Close()
	sum, _, err := scanAndApply(w, ix, base, targets, workers, limit, noStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern index: %v\n", err)
		return ExitErr
	}
	printSummary(sum)
	return ExitOK
}

func parseSizeFlag(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "kb"):
		mult, s = 1<<10, s[:len(s)-2]
	case strings.HasSuffix(s, "mb"):
		mult, s = 1<<20, s[:len(s)-2]
	case strings.HasSuffix(s, "gb"):
		mult, s = 1<<30, s[:len(s)-2]
	case strings.HasSuffix(s, "b"):
		mult, s = 1, s[:len(s)-1]
	}
	var n int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("非法大小 %q", s)
	}
	return n * mult, nil
}

// cmdWatch:lantern watch <dir...>,轮询 + 去抖 + 优雅退出(规格 9.6)。
func cmdWatch(args []string) int {
	args, idxDir := parseGlobal(args)
	pv, targets, uerr := parseKnown(args, watchFlags)
	if uerr {
		fmt.Fprintln(os.Stderr, "lantern watch: 参数错误")
		return ExitUsage
	}
	interval := time.Duration(pv.float("interval", 2) * float64(time.Second))
	workers := pv.int("workers", runtime.GOMAXPROCS(0))
	noStore := pv.boolean("no-store-body")
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "lantern watch: 需要至少一个目录")
		return ExitUsage
	}
	idxAbs, base := resolvePaths(idxDir)
	ix, err := openIndex(idxAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern watch: %v\n", err)
		return ExitErr
	}
	defer ix.Close()
	w, err := ix.AcquireWriter(!noStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern watch: %v\n", err)
		return ExitErr
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	fmt.Printf("watching %s (interval %s),Ctrl-C 退出\n", strings.Join(targets, ", "), interval)

	for {
		outs, deletes, sum, err := scan.Run(scanOptsFor(ix, base, targets, workers, noStore))
		if err != nil {
			fmt.Fprintf(os.Stderr, "lantern watch: %v\n", err)
		}
		if len(outs) == 0 && len(deletes) == 0 {
			select {
			case <-sig:
				w.Close()
				fmt.Println("watch 退出,已提交数据一致")
				return ExitOK
			case <-time.After(interval):
				continue
			}
		}
		// 去抖:一个间隔内仍有新变化则继续合并,安静后才提交。
	interruptible:
		for {
			select {
			case <-sig:
				break interruptible
			case <-time.After(interval):
				more, moreDel, _, err2 := scan.Run(scanOptsFor(ix, base, targets, workers, noStore))
				if err2 != nil || (len(more) == 0 && len(moreDel) == 0) {
					break interruptible
				}
				outs = append(outs, more...)
				deletes = append(deletes, moreDel...)
			}
		}
		for _, d := range outs {
			_ = w.AddDoc(d.Doc)
		}
		for _, p := range deletes {
			_ = w.DeleteDoc(p)
		}
		sum.Deleted = len(deletes)
		if err := w.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "lantern watch: 提交失败: %v\n", err)
			w.Close()
			return ExitErr
		}
		printSummary(sum)
		select {
		case <-sig:
			w.Close()
			fmt.Println("watch 退出,已提交数据一致")
			return ExitOK
		default:
		}
	}
}

// cmdSearch:lantern search "<query>"。
func cmdSearch(args []string) int {
	args, idxDir := parseGlobal(args)
	pv, qargs, uerr := parseKnown(args, searchFlags)
	if uerr {
		fmt.Fprintln(os.Stderr, "lantern search: 参数错误")
		return ExitUsage
	}
	n := pv.int("n", 10)
	jsonOut := pv.boolean("json")
	explain := pv.boolean("explain")
	noColor := pv.boolean("no-color")
	recency := pv.float("recency", 0)
	halfLife := pv.str("half-life", "30d")
	q := strings.Join(qargs, " ")
	idxAbs, _ := resolvePaths(idxDir)
	ix, err := openIndex(idxAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern search: %v\n", err)
		return ExitErr
	}
	defer ix.Close()
	snap, err := ix.Snapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern search: %v\n", err)
		return ExitErr
	}
	defer snap.Close()
	s := newSearcherCLI(snap)
	hl := parseDays(halfLife)
	s.SetRecency(recency, hl, time.Now().Unix())
	if os.Getenv("LANTERN_DEBUG") != "" {
		fmt.Printf("debug: N=%d segs=%d q=%q\n", s.Stats().N, len(snap.Segments()), q)
	}
	res, err := s.Search(q, n, explain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err) // 含列号与指示符(规格 2)
		return ExitUsage
	}
	color := colorEnabled(noColor)
	if jsonOut {
		out := searchJSON{
			Query: res.Query, Total: res.Total, Truncated: res.Truncated,
			Hits: make([]hitJSON, 0, len(res.Hits)),
		}
		for _, h := range res.Hits {
			hj := hitJSON{Path: h.Path, Title: h.Title, Score: h.Score, Explain: h.Explain}
			if sd := storedOf(ix, h.Path); sd != nil && sd.Body != "" {
				sn := snippet.Build(sd.Body, 0, queryMatcher(res.Query))
				hj.Snippet = sn.Text
				hj.Highlights = sn.Highlights
			}
			out.Hits = append(out.Hits, hj)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(&out); err != nil {
			return ExitErr
		}
		return ExitOK
	}
	if len(res.Hits) == 0 {
		fmt.Println("无结果")
		return ExitOK
	}
	for i, h := range res.Hits {
		fmt.Printf("%2d. %s  %s\n", i+1,
			hiPath(color, h.Path), hiText(color, fmt.Sprintf("%.4f", h.Score)))
		if h.Title != "" {
			fmt.Printf("    %s\n", h.Title)
		}
		if sd := storedOf(ix, h.Path); sd != nil && sd.Body != "" {
			sn := snippet.Build(sd.Body, 0, queryMatcher(res.Query))
			if line := renderSnippet(sn, color); line != "" {
				fmt.Printf("    %s\n", line)
			}
		}
		if h.Explain != nil {
			fmt.Print(h.Explain.RenderText())
		}
	}
	fmt.Printf("共 %d 条命中(显示 %d)\n", res.Total, len(res.Hits))
	return ExitOK
}

type searchJSON struct {
	Query     string    `json:"query"`
	Total     uint64    `json:"total"`
	Truncated bool      `json:"truncated"`
	Hits      []hitJSON `json:"hits"`
}

type hitJSON struct {
	Path       string              `json:"path"`
	Title      string              `json:"title,omitempty"`
	Score      float64             `json:"score"`
	Snippet    string              `json:"snippet,omitempty"`
	Highlights []snippet.Highlight `json:"highlights,omitempty"`
	Explain    interface{}         `json:"explain,omitempty"`
}

func renderSnippet(sn snippet.Result, color bool) string {
	if sn.Text == "" {
		return ""
	}
	var sb strings.Builder
	last := 0
	for _, h := range sn.Highlights {
		if h.Start < last || h.End > len(sn.Text) {
			continue
		}
		sb.WriteString(sn.Text[last:h.Start])
		sb.WriteString(hiText(color, sn.Text[h.Start:h.End]))
		last = h.End
	}
	sb.WriteString(sn.Text[last:])
	out := strings.ReplaceAll(sb.String(), "\n", " ")
	if len([]rune(out)) > 300 {
		r := []rune(out)
		out = string(r[:300])
	}
	return out
}

// cmdStats:lantern stats。
func cmdStats(args []string) int {
	_, idxDir := parseGlobal(args)
	idxAbs, _ := resolvePaths(idxDir)
	ix, err := openIndex(idxAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern stats: %v\n", err)
		return ExitErr
	}
	defer ix.Close()
	st, err := ix.Stats()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern stats: %v\n", err)
		return ExitErr
	}
	fmt.Printf("文档数: %d\n", st.Docs)
	fmt.Printf("词项数: %d(段间去重)\n", st.Terms)
	fmt.Printf("段数:   %d\n", st.Segments)
	fmt.Printf("磁盘:   %.2f MB\n", float64(st.Bytes)/1024/1024)
	fmt.Printf("平均长度: title=%.2f path=%.2f body=%.2f\n",
		st.AvgLen[1], st.AvgLen[0], st.AvgLen[2])
	return ExitOK
}

// cmdCheck:lantern check。
func cmdCheck(args []string) int {
	_, idxDir := parseGlobal(args)
	idxAbs, _ := resolvePaths(idxDir)
	rep, err := index.CheckIndex(fsx.OsFS(), idxAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern check: %v\n", err)
		return ExitErr
	}
	for _, wn := range rep.Warnings {
		fmt.Printf("警告: %s\n", wn)
	}
	for _, e := range rep.Errors {
		fmt.Printf("错误: %s\n", e)
	}
	verdict := "通过"
	if !rep.OK() {
		verdict = "未通过"
	}
	fmt.Printf("generation=%d 段=%d 文档=%d — %s\n", rep.Generation, rep.Segments, rep.Docs, verdict)
	if !rep.OK() {
		return ExitErr
	}
	return ExitOK
}

// cmdCompact:lantern compact。
func cmdCompact(args []string) int {
	_, idxDir := parseGlobal(args)
	idxAbs, _ := resolvePaths(idxDir)
	ix, err := openIndex(idxAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern compact: %v\n", err)
		return ExitErr
	}
	defer ix.Close()
	w, err := ix.AcquireWriter(true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern compact: %v\n", err)
		return ExitErr
	}
	defer w.Close()
	if err := w.Compact(); err != nil {
		fmt.Fprintf(os.Stderr, "lantern compact: %v\n", err)
		return ExitErr
	}
	segs, err := ix.Snapshot()
	if err == nil {
		fmt.Printf("合并完成:当前 %d 个段\n", len(segs.Segments()))
		segs.Close()
	} else {
		fmt.Println("合并完成")
	}
	return ExitOK
}
