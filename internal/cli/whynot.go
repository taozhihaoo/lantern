package cli

import (
	"fmt"
	"os"
	"strings"

	"lantern/internal/whynot"
)

// cmdWhyNot:lantern why-not "<query>" <path> [--json]。
func cmdWhyNot(args []string) int {
	args, idxDir := parseGlobal(args)
	pv, pos, uerr := parseKnown(args, whyNotFlags)
	if uerr || len(pos) != 2 {
		fmt.Fprintln(os.Stderr, "用法: lantern why-not \"<query>\" <path> [--json]")
		return ExitUsage
	}
	jsonOut := pv.boolean("json")
	q, path := pos[0], pos[1]
	idxAbs, base := resolvePaths(idxDir)
	ix, err := openIndex(idxAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern why-not: %v\n", err)
		return ExitErr
	}
	defer ix.Close()
	snap, err := ix.Snapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern why-not: %v\n", err)
		return ExitErr
	}
	defer snap.Close()
	s := newSearcherCLI(snap)
	rep, err := whynot.Diagnose(ix, s, whynot.Config{Base: base}, q, path)
	if err != nil {
		// 查询语法错误 → 退出码 2(含列号)。
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return ExitUsage
	}
	if jsonOut {
		j, err := rep.RenderJSON()
		if err != nil {
			fmt.Fprintf(os.Stderr, "lantern why-not: %v\n", err)
			return ExitErr
		}
		fmt.Println(strings.TrimSpace(j))
		return ExitOK
	}
	fmt.Print(rep.RenderText())
	return ExitOK
}
