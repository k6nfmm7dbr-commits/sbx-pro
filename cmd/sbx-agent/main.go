// sbx-agent 是 sbx-pro 的节点代理（B/C/D 机）。
// 主动连接 Manager，执行节点配置、本地流量统计、quota / IP 限制，
// 并保证 Manager 离线时 sing-box 与本地限制继续自治运行。
//
// Phase 1：仅搭建可编译骨架；功能按 Phase 2~10 逐步填充。
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/version"
)

func main() {
	setupLogging()
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println("sbx-agent: 尚未启动（Phase 1 骨架）")
		return
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Printf("sbx-agent v%s\n", version.Version)
	case "run":
		fmt.Println("sbx-agent run: 待实现（Phase 2+）")
	case "enroll":
		fmt.Println("sbx-agent enroll: 待实现（Phase 2）")
	default:
		usage()
	}
}

func usage() {
	fmt.Printf("sbx-agent v%s — SBX Pro 节点代理\n\n", version.Version)
	fmt.Println("用法:")
	fmt.Println("  sbx-agent run            连接管理面板并持续运行")
	fmt.Println("  sbx-agent enroll         使用 enrollment token 注册")
	fmt.Println("  sbx-agent version        版本信息")
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
