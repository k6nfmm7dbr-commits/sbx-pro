// sbx-manager 是 sbx-pro 的中央管理面板（A 机）。
// 负责：机器管理、全局节点管理、任务下发、流量汇总、在线 IP 管理、WebUI。
// 不代理任何用户流量——用户流量始终直达节点机 B/C/D。
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/config"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/service"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/version"
)

func main() {
	setupLogging()
	args := os.Args[1:]
	if len(args) == 0 {
		os.Exit(serveOrDefault())
	}
	switch args[0] {
	case "serve":
		os.Exit(service.Serve())
	case "version", "--version", "-v":
		fmt.Printf("sbx-manager v%s\n", version.Version)
	case "ensure-admin-token":
		tok, err := config.EnsureAdminToken()
		if err != nil {
			fmt.Fprintln(os.Stderr, "[sbx-manager]", err)
			os.Exit(1)
		}
		fmt.Println(tok)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		os.Exit(2)
	}
}

func serveOrDefault() int {
	return service.Serve()
}

func usage() {
	fmt.Printf("sbx-manager v%s — SBX Pro 中央管理面板\n\n", version.Version)
	fmt.Println("用法:")
	fmt.Println("  sbx-manager serve               启动管理面板")
	fmt.Println("  sbx-manager ensure-admin-token  生成/查看管理令牌")
	fmt.Println("  sbx-manager version             版本信息")
}

func setupLogging() {
	level := slog.LevelInfo
	switch os.Getenv("SBX_LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
}
