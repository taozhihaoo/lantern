// Package cli 实现 lantern 命令行:子命令解析、终端渲染(ANSI 高亮,
// Windows 下自动降级)与各命令流程。
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"lantern/internal/fsx"
	"lantern/internal/index"
)

// Exit codes(规格 2):0 成功;1 运行错误;2 用法/查询语法错误。
const (
	ExitOK    = 0
	ExitErr   = 1
	ExitUsage = 2
)

// usage 输出用法。
func usage(w *os.File) {
	fmt.Fprint(w, `lantern — 本地全文搜索引擎

用法:
  lantern index <dir...>     扫描并增量索引 [--workers N] [--max-size 2MB] [--no-store-body]
  lantern watch <dir...>     轮询监听变化并增量更新 [--interval 2s]
  lantern search "<query>"   搜索 [-n 10] [--json] [--explain] [--no-color] [--recency 0.2 --half-life 30d]
  lantern why-not "<query>" <path>  解释该文档为何未命中 [--json]
  lantern serve [--addr 127.0.0.1:7700]  HTTP API + 内嵌网页界面
  lantern stats              文档数、词项数、段数、磁盘占用、各字段平均长度
  lantern check              校验段文件与 manifest 一致性
  lantern compact            强制合并为单个段
索引目录:--index <dir> 或环境变量 LANTERN_DIR,默认 ./.lantern
`)
}

// Main 是 CLI 入口,返回进程退出码。
func Main(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return ExitUsage
	}
	cmd := args[0]
	rest := args[1:]
	switch cmd {
	case "index":
		return cmdIndex(rest)
	case "watch":
		return cmdWatch(rest)
	case "search":
		return cmdSearch(rest)
	case "why-not":
		return cmdWhyNot(rest)
	case "serve":
		return cmdServe(rest)
	case "stats":
		return cmdStats(rest)
	case "check":
		return cmdCheck(rest)
	case "compact":
		return cmdCompact(rest)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return ExitOK
	default:
		fmt.Fprintf(os.Stderr, "lantern: 未知命令 %q\n\n", cmd)
		usage(os.Stderr)
		return ExitUsage
	}
}

// parseGlobal 提取公共 --index 参数,返回剩余参数与索引目录。
func parseGlobal(args []string) ([]string, string) {
	idx := ""
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--index" && i+1 < len(args):
			idx = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--index="):
			idx = strings.TrimPrefix(args[i], "--index=")
		default:
			rest = append(rest, args[i])
		}
	}
	if idx == "" {
		idx = os.Getenv("LANTERN_DIR")
	}
	if idx == "" {
		idx = "./.lantern"
	}
	return rest, idx
}

// resolvePaths 返回(索引目录绝对路径, 基目录=索引目录父目录)。
func resolvePaths(idxDir string) (string, string) {
	abs, err := filepath.Abs(idxDir)
	if err != nil {
		abs = idxDir
	}
	return abs, filepath.Dir(abs)
}

// openIndex 打开索引。
func openIndex(idxDir string) (*index.Index, error) {
	return index.Open(idxDir, fsx.OsFS())
}

// colorEnabled:Windows 下自动降级为无色(规格 3);--no-color 强制关闭。
func colorEnabled(noColor bool) bool {
	if noColor {
		return false
	}
	return runtime.GOOS != "windows"
}

func colorize(enabled bool, s, code string) string {
	if !enabled {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func hiText(enabled bool, s string) string { return colorize(enabled, s, "1;33") }
func hiPath(enabled bool, s string) string { return colorize(enabled, s, "1;36") }
