package scan

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"lantern/internal/fsx"
	"lantern/internal/index"
)

// StateEntry 是增量对比所需的索引状态。
type StateEntry struct {
	Size  int64
	MTime int64
	SHA   string
}

// Action 描述对某路径的处理动作。
type Action string

const (
	// ActionAdd 新增。
	ActionAdd Action = "add"
	// ActionUpdate 内容变化(或重命名后新路径)。
	ActionUpdate Action = "update"
	// ActionMTimeOnly 内容未变仅 mtime 变化。
	ActionMTimeOnly Action = "mtime"
)

// DocOut 是扫描产出的一个待索引文档。
type DocOut struct {
	Action Action
	Path   string
	Doc    index.Doc
	// PrevSHA 与 PrevMTime 用于摘要(mtime-only 场景)。
	PrevSHA string
}

// Options 是扫描参数。
type Options struct {
	// Base 为索引基目录:文档路径 = 相对 Base 的正斜杠路径。
	Base string
	// Targets 为待扫描目录(相对或绝对,必须在 Base 之下)。
	Targets []string
	// Workers 为解析 goroutine 数(默认 GOMAXPROCS)。
	Workers int
	// MaxSize 为单文件大小上限(默认 2MB)。
	MaxSize int64
	// NoStoreBody 为 true 时不存储正文。
	NoStoreBody bool
	// Fsys 文件系统抽象。
	Fsys fsx.FS
	// State 查询索引状态表。
	State func(path string) (StateEntry, bool)
	// StateList 返回状态表中全部路径(用于删除检测)。
	StateList func() []string
}

// Summary 是扫描汇总(规格 9.7)。
type Summary struct {
	Added     int
	Updated   int
	MTimeOnly int
	Deleted   int
	Unchanged int
	Skipped   map[SkipReason]int
	// Bytes 为本次读取并提取的原始字节量(吞吐计算用)。
	Bytes     int64
	Elapsed   time.Duration
	FilesSeen int
}

// SkipReasonCount 返回某原因的跳过数。
func (s *Summary) SkipReasonCount(r SkipReason) int { return s.Skipped[r] }

// task 是一个待处理文件的内部任务。
type task struct {
	full  string
	rel   string
	size  int64
	mtime int64
}

// Run 执行扫描:遍历目标目录,对比状态表,产出待索引文档与删除列表。
// 结果按路径排序后返回,保证与 Workers 数无关(规格 9.5)。
func Run(o Options) ([]DocOut, []string, *Summary, error) {
	if o.MaxSize <= 0 {
		o.MaxSize = 2 << 20
	}
	if o.Workers <= 0 {
		o.Workers = 1
	}
	if o.Fsys == nil {
		o.Fsys = fsx.OsFS()
	}
	sum := &Summary{Skipped: map[SkipReason]int{}}
	start := time.Now()

	// seen 记录本次扫描到的路径(删除检测用)。
	var mu sync.Mutex
	seen := map[string]bool{}
	var outs []DocOut

	tasks := make(chan task, 256)
	var walkErr error
	var walkWg sync.WaitGroup

	// 遍历:单 goroutine(目录 IO 轻),解析并行。
	walkWg.Add(1)
	go func() {
		defer walkWg.Done()
		defer close(tasks)
		for _, tgt := range o.Targets {
			full := tgt
			if !filepath.IsAbs(full) {
				full = filepath.Join(o.Base, tgt)
			}
			full = filepath.Clean(full)
			rel, err := relPath(o.Base, full)
			if err != nil {
				mu.Lock()
				sum.Skipped[SkipOutside]++
				mu.Unlock()
				continue
			}
			if info, err := o.Fsys.Stat(full); err != nil || !info.IsDir() {
				mu.Lock()
				sum.Skipped[SkipRead]++
				mu.Unlock()
				continue
			}
			chain := NewIgnoreChain()
			o.walkDir(chain, full, rel, o.Base, tasks, &seen, sum, &mu)
		}
	}()

	// workers:读取/嗅探/提取/哈希。
	var procWg sync.WaitGroup
	for w := 0; w < o.Workers; w++ {
		procWg.Add(1)
		go func() {
			defer procWg.Done()
			for t := range tasks {
				out, skip := o.processFile(t)
				mu.Lock()
				if skip != "" {
					sum.Skipped[skip]++
				} else {
					outs = append(outs, out)
					sum.Bytes += t.size
				}
				mu.Unlock()
			}
		}()
	}
	walkWg.Wait()
	procWg.Wait()
	if walkErr != nil {
		return nil, nil, sum, walkErr
	}

	// 按路径排序:结果与 worker 数无关。
	sort.Slice(outs, func(i, j int) bool { return outs[i].Path < outs[j].Path })

	// 删除检测:状态表中存在而本次未见的路径。
	var deletes []string
	if o.StateList != nil {
		for _, p := range o.StateList() {
			if !seen[p] {
				deletes = append(deletes, p)
			}
		}
		sort.Strings(deletes)
	}

	// 与状态表对比分类。
	final := make([]DocOut, 0, len(outs))
	for _, out := range outs {
		prev, ok := o.State(out.Path)
		switch {
		case !ok:
			out.Action = ActionAdd
			sum.Added++
		case prev.SHA != fmt.Sprintf("%x", out.Doc.SHA256):
			out.Action = ActionUpdate
			out.PrevSHA = prev.SHA
			sum.Updated++
		case prev.MTime != out.Doc.MTime:
			out.Action = ActionMTimeOnly
			out.PrevSHA = prev.SHA
			sum.MTimeOnly++
		default:
			sum.Unchanged++
			continue
		}
		final = append(final, out)
	}
	sum.FilesSeen = len(seen)
	sum.Elapsed = time.Since(start)
	return final, deletes, sum, nil
}

// walkDir 递归遍历目录(不跟随符号链接),发送文件任务。
func (o *Options) walkDir(chain *IgnoreChain, full, rel, base string,
	tasks chan<- task, seen *map[string]bool, sum *Summary, mu *sync.Mutex) {
	// 解析本目录的忽略规则文件。
	var rules []Rule
	for _, name := range []string{".lanternignore", ".gitignore"} {
		rp := filepath.Join(full, name)
		if data, err := readSmall(o.Fsys, rp, 1<<20); err == nil {
			rules = append(rules, parseIgnoreFile(RelPath(base, rp), string(data))...)
		}
	}
	chain.Push(rules)

	ents, err := o.Fsys.ReadDir(full)
	if err != nil {
		mu.Lock()
		sum.Skipped[SkipRead]++
		mu.Unlock()
		chain.Pop()
		return
	}
	for _, e := range ents {
		name := e.Name()
		childFull := filepath.Join(full, name)
		childRel := rel + "/" + name
		if rel == "." {
			childRel = name
		}
		isDir := e.IsDir()
		if !isDir && e.Type()&fs.ModeSymlink == 0 {
			// Info 解析链接目标类型;symlink 保持 false。
			if info, err := e.Info(); err == nil {
				isDir = info.IsDir()
			}
		}
		if e.Type()&fs.ModeSymlink != 0 {
			mu.Lock()
			sum.Skipped[SkipSymlink]++
			mu.Unlock()
			continue
		}
		if d := chain.Evaluate(childRel, isDir); d.Ignored {
			mu.Lock()
			sum.Skipped[SkipIgnored]++
			mu.Unlock()
			continue
		}
		if isDir {
			o.walkDir(chain, childFull, childRel, base, tasks, seen, sum, mu)
			continue
		}
		info, err := e.Info()
		if err != nil {
			mu.Lock()
			sum.Skipped[SkipRead]++
			mu.Unlock()
			continue
		}
		if info.Size() > o.MaxSize {
			mu.Lock()
			sum.Skipped[SkipTooLarge]++
			mu.Unlock()
			continue
		}
		mu.Lock()
		(*seen)[childRel] = true
		mu.Unlock()
		tasks <- task{full: childFull, rel: childRel, size: info.Size(), mtime: info.ModTime().Unix()}
	}
	chain.Pop()
}

// processFile 读取并提取单个文件( worker 内执行,不修改共享状态)。
func (o *Options) processFile(t task) (DocOut, SkipReason) {
	data, err := readSmall(o.Fsys, t.full, o.MaxSize)
	if err != nil {
		return DocOut{}, SkipRead
	}
	sum256 := sha256.Sum256(data)
	sn := Sniff(data)
	var ex Extracted
	switch sn.Kind {
	case "binary":
		return DocOut{}, SkipBinary
	case "nonutf8":
		return DocOut{}, SkipNonUTF8
	default:
		text := sn.Text
		ext := extOf(t.rel)
		ex, err = Extractor(ext)(t.rel, []byte(text))
		if err != nil {
			return DocOut{}, SkipRead
		}
	}
	if ex.Title == "" {
		ex.Title = fileNameOf(t.rel)
	}
	doc := index.Doc{
		Path:      t.rel,
		Title:     ex.Title,
		Body:      ex.Body,
		Ext:       extOf(t.rel),
		Size:      t.size,
		MTime:     t.mtime,
		SHA256:    sum256,
		StoreBody: !o.NoStoreBody,
	}
	return DocOut{Path: t.rel, Doc: doc}, ""
}

func extOf(rel string) string {
	name := rel
	if k := strings.LastIndexByte(name, '/'); k >= 0 {
		name = name[k+1:]
	}
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		return strings.ToLower(name[dot+1:])
	}
	return ""
}

// readSmall 读取不超过 limit 的文件。
func readSmall(fsys fsx.FS, path string, limit int64) ([]byte, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, fmt.Errorf("scan: open %s: %w", path, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("scan: stat %s: %w", path, err)
	}
	if st.Size() > limit {
		return nil, fmt.Errorf("scan: %s too large", path)
	}
	buf := make([]byte, st.Size())
	if _, err := f.Read(buf); err != nil && len(buf) == 0 {
		return nil, fmt.Errorf("scan: read %s: %w", path, err)
	}
	return buf, nil
}
