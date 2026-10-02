package cli

import (
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"lantern/internal/server"
)

// cmdServe:lantern serve [--addr 127.0.0.1:7700](规格 10)。
func cmdServe(args []string) int {
	args, idxDir := parseGlobal(args)
	pv, _, uerr := parseKnown(args, serveFlags)
	if uerr {
		fmt.Fprintln(os.Stderr, "lantern serve: 参数错误")
		return ExitUsage
	}
	addr := pv.str("addr", "127.0.0.1:7700")
	idxAbs, base := resolvePaths(idxDir)
	ix, err := openIndex(idxAbs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lantern serve: %v\n", err)
		return ExitErr
	}
	defer ix.Close()

	srv := server.New(ix, base)
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		_ = httpSrv.Close()
	}()
	fmt.Printf("lantern serve 监听 http://%s(只读;Ctrl-C 退出)\n", addr)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "lantern serve: %v\n", err)
		return ExitErr
	}
	return ExitOK
}
