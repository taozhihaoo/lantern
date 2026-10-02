// Package scan 实现目录扫描与增量索引输入:
// 忽略规则(.gitignore/.lanternignore 子集)、二进制嗅探、编码转码、
// 文本提取器(markdown/html/txt/json/csv/code/docx)与增量对比。
package scan

import (
	"bufio"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"lantern/internal/fsx"
)

// Rule 是一条忽略规则,携带来源文件与行号(供 Why-not 引用)。
type Rule struct {
	Negate   bool   // ! 前缀:取反
	DirOnly  bool   // / 结尾:仅匹配目录
	Anchored bool   // 以 / 开头或含中间 /:锚定规则文件所在目录
	Pattern  string // 处理后的模式(保留原样供展示)
	segs     []string
	Source   string // 来源文件路径
	Line     int    // 行号(1-based)
}

// Describe 输出规则的来源与行号。
func (r Rule) Describe() string {
	return fmt.Sprintf("%s:%d: %s", r.Source, r.Line, r.Pattern)
}

// parseIgnoreFile 解析一个忽略规则文件的内容。
func parseIgnoreFile(source, content string) []Rule {
	var rules []Rule
	sc := bufio.NewScanner(strings.NewReader(content))
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		line = strings.TrimRight(line, " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := Rule{Source: source, Line: lineNo, Pattern: line}
		if strings.HasPrefix(line, "!") {
			r.Negate = true
			line = line[1:]
		}
		if strings.HasSuffix(line, "/") {
			r.DirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") {
			r.Anchored = true
			line = strings.TrimPrefix(line, "/")
		} else if strings.Contains(line, "/") {
			r.Anchored = true
			line = strings.TrimPrefix(line, "./")
		}
		r.Pattern = line
		r.segs = strings.Split(line, "/")
		if line != "" {
			rules = append(rules, r)
		}
	}
	return rules
}

// matchSegs 匹配模式段与路径段(** 跨段,* ? 不跨段)。
func matchSegs(pat, p []string) bool {
	if len(pat) == 0 {
		return len(p) == 0
	}
	if pat[0] == "**" {
		// ** 匹配零个或多个段。
		for i := 0; i <= len(p); i++ {
			if matchSegs(pat[1:], p[i:]) {
				return true
			}
		}
		return false
	}
	if len(p) == 0 {
		return false
	}
	if !matchSeg(pat[0], p[0]) {
		return false
	}
	return matchSegs(pat[1:], p[1:])
}

// matchSeg 单段匹配:* 不跨 /,? 单字符。
func matchSeg(pat, s string) bool {
	// 经典双指针通配。
	i, j := 0, 0
	star := -1
	starJ := 0
	for j < len(s) {
		if i < len(pat) && (pat[i] == '?' || pat[i] == s[j]) {
			i++
			j++
		} else if i < len(pat) && pat[i] == '*' {
			star = i
			starJ = j
			i++
		} else if star >= 0 {
			i = star + 1
			starJ++
			j = starJ
		} else {
			return false
		}
	}
	for i < len(pat) && pat[i] == '*' {
		i++
	}
	return i == len(pat)
}

// ruleMatches 判断规则是否命中相对路径(isDir 表示目标是目录)。
// 目录规则(DirOnly)命中目录本身及其全部后代。
func ruleMatches(r Rule, rel string, isDir bool) bool {
	p := strings.Split(rel, "/")
	if r.DirOnly {
		// 目录规则:路径的某前缀(段级)命中模式即可。
		if r.Anchored {
			return len(p) >= len(r.segs) && matchSegs(r.segs, p[:len(r.segs)])
		}
		for i := 0; i+len(r.segs) <= len(p); i++ {
			if matchSegs(r.segs, p[i:i+len(r.segs)]) {
				return true
			}
		}
		return false
	}
	if r.Anchored {
		if matchSegs(r.segs, p) {
			return true
		}
		// 模式命中某级目录祖先 → 后代全部忽略(目录剪枝语义)。
		return len(p) > len(r.segs) && matchSegs(r.segs, p[:len(r.segs)])
	}
	// 非锚定:在任意段边界起匹配。
	for i := 0; i < len(p); i++ {
		if matchSegs(r.segs, p[i:]) {
			return true
		}
		// 祖先前缀命中。
		if i+len(r.segs) < len(p) && matchSegs(r.segs, p[i:i+len(r.segs)]) {
			return true
		}
	}
	return false
}

// DefaultIgnores 是默认忽略的目录名。
var DefaultIgnores = []string{".git", "node_modules", ".lantern"}

// IgnoreChain 管理一条目录链(root → 当前目录)上的忽略规则。
// 评估顺序:从最深目录的规则文件到最浅,首个"决定"生效;
// 同一文件内靠后的规则优先(git 语义)。
type IgnoreChain struct {
	levels [][]Rule // levels[0] = root 目录的规则
}

// NewIgnoreChain 创建空链。
func NewIgnoreChain() *IgnoreChain { return &IgnoreChain{} }

// Push 追加一层(进入子目录)。
func (c *IgnoreChain) Push(rules []Rule) { c.levels = append(c.levels, rules) }

// Pop 退出一层。
func (c *IgnoreChain) Pop() { c.levels = c.levels[:len(c.levels)-1] }

// Decision 是忽略判定结果。
type Decision struct {
	Ignored bool
	Rule    *Rule
}

// Evaluate 判断 rel(相对 root,正斜杠)是否被忽略。
// 默认忽略目录名(.git/node_modules/.lantern)始终生效。
func (c *IgnoreChain) Evaluate(rel string, isDir bool) Decision {
	// 默认忽略:任意层级的目录名。
	for _, name := range DefaultIgnores {
		for _, seg := range strings.Split(rel, "/") {
			if seg == name {
				return Decision{Ignored: true, Rule: &Rule{
					Pattern: name + "/(内置默认)", Source: "(default)", Line: 0,
				}}
			}
		}
	}
	// 深层优先。
	for i := len(c.levels) - 1; i >= 0; i-- {
		var hit *Rule
		for j := range c.levels[i] {
			r := &c.levels[i][j]
			if ruleMatches(*r, rel, isDir) {
				hit = r // 同文件内后者覆盖前者
			}
		}
		if hit != nil {
			return Decision{Ignored: !hit.Negate, Rule: hit}
		}
	}
	return Decision{}
}

// RelPath 返回正斜杠规范化相对路径。
func RelPath(base, full string) string {
	r, err := relPath(base, full)
	if err != nil {
		return path.Clean(strings.ReplaceAll(full, "\\", "/"))
	}
	return r
}

// LoadDirRules 读取目录内的 .lanternignore 与 .gitignore 规则
// (供 Why-not 文档层复用;source 记录来源文件相对路径)。
func LoadDirRules(fsys fsx.FS, dir, base string) []Rule {
	var rules []Rule
	for _, name := range []string{".lanternignore", ".gitignore"} {
		full := dir + string(filepath.Separator) + name
		if data, err := readSmall(fsys, full, 1<<20); err == nil {
			rules = append(rules, parseIgnoreFile(RelPath(base, full), string(data))...)
		}
	}
	return rules
}
