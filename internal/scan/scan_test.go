package scan

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIgnoreRules(t *testing.T) {
	rules := parseIgnoreFile("root/.gitignore", strings.Join([]string{
		"*.log",        // 任意层级
		"/build",       // 锚定根
		"docs/*.tmp",   // 含 / → 锚定
		"node_ignore/", // 目录
		"!keep.log",    // 取反
		"a/**/c",       // 跨段
		"file?.txt",    // 单字符
	}, "\n"))
	chain := NewIgnoreChain()
	chain.Push(rules)
	cases := []struct {
		rel   string
		isDir bool
		want  bool
	}{
		{"x.log", false, true},
		{"sub/x.log", false, true},
		{"keep.log", false, false}, // 取反
		{"build", true, true},
		{"build/f.o", false, true}, // 目录规则约束后代
		{"docs/a.tmp", false, true},
		{"docs/sub/a.tmp", false, false}, // docs/*.tmp 只一层
		{"node_ignore", true, true},
		{"node_ignore/x", false, true},
		{"a/b/c", false, true},
		{"a/c", false, true}, // ** 零段
		{"file1.txt", false, true},
		{"file12.txt", false, false},
		{"main.go", false, false},
	}
	for _, tc := range cases {
		if got := chain.Evaluate(tc.rel, tc.isDir).Ignored; got != tc.want {
			t.Errorf("Evaluate(%q,dir=%v) = %v, want %v", tc.rel, tc.isDir, got, tc.want)
		}
	}
}

func TestIgnoreDefaultDirs(t *testing.T) {
	chain := NewIgnoreChain()
	chain.Push(nil)
	if !chain.Evaluate(".git/config", false).Ignored {
		t.Fatal(".git must be ignored by default")
	}
	if !chain.Evaluate("sub/node_modules/x.js", false).Ignored {
		t.Fatal("node_modules must be ignored at any level")
	}
	if chain.Evaluate("src/main.go", false).Ignored {
		t.Fatal("normal files must not be ignored")
	}
	rules := parseIgnoreFile("d/.gitignore", "*.log\n")
	if rules[0].Source != "d/.gitignore" || rules[0].Line != 1 {
		t.Fatalf("rule source/line: %+v", rules[0])
	}
}

func TestSniff(t *testing.T) {
	if got := Sniff([]byte("hello 世界")).Kind; got != "utf8" {
		t.Fatalf("utf8 = %s", got)
	}
	if got := Sniff([]byte("text\x00with nul")).Kind; got != "binary" {
		t.Fatalf("nul = %s", got)
	}
	u16 := append([]byte{0xFF, 0xFE}, 0x68, 0x00, 0x69, 0x00) // "hi"
	r := Sniff(u16)
	if r.Kind != "utf16le" || r.Text != "hi" {
		t.Fatalf("utf16le = %s %q", r.Kind, r.Text)
	}
	gbk := []byte{0xC4, 0xE3, 0xBA, 0xC3} // "你好" 的 GBK
	if got := Sniff(gbk).Kind; got != "nonutf8" {
		t.Fatalf("gbk = %s", got)
	}
	junk := bytes.Repeat([]byte{0xFE, 0x01, 0xFF, 0x81}, 100) // 含控制字节 0x01
	if got := Sniff(junk).Kind; got != "binary" {
		t.Fatalf("junk = %s", got)
	}
}

func TestExtractMarkdown(t *testing.T) {
	ex, err := extractMarkdown("a.md", []byte("---\ntitle: x\n---\n# 我的标准题\n\n正文内容"))
	if err != nil {
		t.Fatal(err)
	}
	if ex.Title != "我的标准题" {
		t.Fatalf("title = %q", ex.Title)
	}
	if !strings.Contains(ex.Body, "正文内容") || strings.Contains(ex.Body, "title: x") {
		t.Fatalf("body = %q", ex.Body)
	}
	ex2, _ := extractMarkdown("plain.md", []byte("no heading here"))
	if ex2.Title != "" {
		t.Fatalf("plain title = %q", ex2.Title)
	}
}

func TestExtractHTML(t *testing.T) {
	page := `<html><head><title>My &amp; Title</title><style>body{color:red}</style></head>` +
		`<body><script>alert(1)</script><p>hello<b>world</b></p><div>第二行</div></body></html>`
	ex, err := extractHTML("x.html", []byte(page))
	if err != nil {
		t.Fatal(err)
	}
	if ex.Title != "My & Title" {
		t.Fatalf("title = %q", ex.Title)
	}
	if strings.Contains(ex.Body, "alert") || strings.Contains(ex.Body, "color") {
		t.Fatalf("script/style leaked: %q", ex.Body)
	}
	if !strings.Contains(ex.Body, "helloworld") || !strings.Contains(ex.Body, "第二行") {
		t.Fatalf("body = %q", ex.Body)
	}
}

func TestExtractDocx(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(`<?xml version="1.0"?><w:document><w:body><w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:t> World</w:t></w:r></w:p><w:p><w:r><w:t>第二段</w:t></w:r></w:p></w:body></w:document>`))
	zw.Close()
	ex, err := extractDocxImpl("a.docx", buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ex.Body, "Hello World") || !strings.Contains(ex.Body, "第二段") {
		t.Fatalf("docx body = %q", ex.Body)
	}
}

// writeTree 在 base 下写文件(带目录)。
func writeTree(t *testing.T, base, rel, content string) {
	t.Helper()
	p := filepath.Join(base, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scanOpts(base string, workers int, st map[string]StateEntry) Options {
	return Options{
		Base:    base,
		Targets: []string{"."},
		Workers: workers,
		State: func(p string) (StateEntry, bool) {
			e, ok := st[p]
			return e, ok
		},
		StateList: func() []string {
			out := make([]string, 0, len(st))
			for k := range st {
				out = append(out, k)
			}
			return out
		},
	}
}

// TestScanIncremental 覆盖新增/修改/删除与未变文件不重复索引。
func TestScanIncremental(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, ".gitignore", "*.log\n")
	writeTree(t, base, "a.md", "# Alpha\nalphaDocument uniqueWordOne")
	writeTree(t, base, "sub/b.txt", "betaDocument uniqueWordTwo")
	writeTree(t, base, "skip.log", "logged")
	writeTree(t, base, ".git/x", "x")

	st := map[string]StateEntry{}

	outs, _, sum, err := Run(scanOpts(base, 2, st))
	if err != nil {
		t.Fatal(err)
	}
	if len(outs) != 3 || sum.Added != 3 { // .gitignore + a.md + sub/b.txt
		t.Fatalf("first scan: %d outs %+v skipped=%v", len(outs), outs, sum.Skipped)
	}
	if sum.SkipReasonCount(SkipIgnored) < 2 { // skip.log 与 .git/
		t.Fatalf("expected ignored .log/.git: %v", sum.Skipped)
	}
	for _, d := range outs {
		st[d.Path] = StateEntry{Size: d.Doc.Size, MTime: d.Doc.MTime, SHA: fmt.Sprintf("%x", d.Doc.SHA256)}
	}

	_, _, sum2, _ := Run(scanOpts(base, 2, st))
	if sum2.Added != 0 || sum2.Updated != 0 || sum2.Unchanged != 3 {
		t.Fatalf("second scan: %+v", sum2)
	}

	// 修改 a.md、删除 sub/b.txt、新增 c.md。
	writeTree(t, base, "a.md", "# Alpha\nalphaDocument changed")
	if err := os.Remove(filepath.Join(base, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}
	writeTree(t, base, "c.md", "gammaDocument")
	outs3, deletes3, sum3, _ := Run(scanOpts(base, 2, st))
	acts := map[string]Action{}
	for _, d := range outs3 {
		acts[d.Path] = d.Action
	}
	if acts["a.md"] != ActionUpdate || acts["c.md"] != ActionAdd {
		t.Fatalf("actions = %v", acts)
	}
	if len(deletes3) != 1 || deletes3[0] != "sub/b.txt" {
		t.Fatalf("deletes = %v", deletes3)
	}
	if sum3.MTimeOnly != 0 {
		t.Fatalf("mtime-only should be 0: %+v", sum3)
	}
}

// TestScanMTimeOnly mtime 变化但内容未变 → 仅 mtime 更新。
func TestScanMTimeOnly(t *testing.T) {
	base := t.TempDir()
	writeTree(t, base, "a.md", "same content")
	st := map[string]StateEntry{}
	outs, _, _, _ := Run(scanOpts(base, 1, st))
	for _, d := range outs {
		st[d.Path] = StateEntry{Size: d.Doc.Size, MTime: d.Doc.MTime - 100, SHA: fmt.Sprintf("%x", d.Doc.SHA256)}
	}
	outs2, _, sum, _ := Run(scanOpts(base, 1, st))
	if sum.MTimeOnly != 1 || len(outs2) != 1 || outs2[0].Action != ActionMTimeOnly {
		t.Fatalf("mtime-only: %+v %+v", sum, outs2)
	}
}

// TestScanWorkersDeterministic 结果与 worker 数无关(规格 9.5)。
func TestScanWorkersDeterministic(t *testing.T) {
	base := t.TempDir()
	for i := 0; i < 30; i++ {
		writeTree(t, base, fmt.Sprintf("d%d/f%02d.md", i%3, i),
			fmt.Sprintf("# Doc%d\n共享词汇%d unique%d", i, i%5, i))
	}
	paths := func(workers int) []string {
		st := map[string]StateEntry{}
		outs, _, _, err := Run(scanOpts(base, workers, st))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, d := range outs {
			out = append(out, d.Path)
		}
		return out
	}
	a := paths(1)
	b := paths(8)
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Fatalf("worker count changed results:\n1: %v\n8: %v", a, b)
	}
	if len(a) != 30 {
		t.Fatalf("expected 30 docs, got %d", len(a))
	}
}

func TestScanOutsideRoot(t *testing.T) {
	base := t.TempDir()
	other := t.TempDir()
	o := Options{Base: base, Targets: []string{other}, Workers: 1,
		State: func(string) (StateEntry, bool) { return StateEntry{}, false }}
	_, _, sum, err := Run(o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.SkipReasonCount(SkipOutside) != 1 {
		t.Fatalf("outside count = %d", sum.SkipReasonCount(SkipOutside))
	}
}
