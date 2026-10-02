// lantern 命令行入口:仅做参数解析与调用(规格 3)。
package main

import (
	"os"

	"lantern/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
