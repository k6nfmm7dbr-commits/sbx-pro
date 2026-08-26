// sbx-agent 是 sbx-pro 的节点代理（B/C/D 机）。
// 主动连接 Manager，执行节点配置、本地流量统计、quota / IP 限制，
// 并保证 Manager 离线时 sing-box 与本地限制继续自治运行。
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/client"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/state"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/sysinfo"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/version"
)

func main() {
	setupLogging()
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Println("sbx-agent: 请在参数中指定子命令（enroll / run / version）")
		os.Exit(2)
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Printf("sbx-agent v%s\n", version.Version)

	case "enroll":
		os.Exit(runEnroll(args[1:]))

	case "run":
		fmt.Println("sbx-agent run: 待实现（Phase 3）")

	case "help", "-h", "--help":
		usage()

	default:
		usage()
		os.Exit(2)
	}
}

// runEnroll 处理 `sbx-agent enroll -t TOKEN -u URL`。
func runEnroll(args []string) int {
	fs := flag.NewFlagSet("enroll", flag.ContinueOnError)
	token := fs.String("t", "", "enrollment token")
	url := fs.String("u", "", "manager url (https://panel.example.com)")
	fs.Parse(args)

	if *token == "" || *url == "" {
		fmt.Fprintln(os.Stderr, "用法: sbx-agent enroll -t TOKEN -u https://panel.example.com")
		return 2
	}

	info := sysinfo.Gather()
	hello := protocol.Hello{
		EnrollToken:  *token,
		Hostname:     info.Hostname,
		AgentVersion: version.Version,
		OS:           info.OS,
		Kernel:       info.Kernel,
		Arch:         info.Arch,
	}

	c := client.New(*url)
	ack, err := c.Enroll(hello)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[sbx-agent] 注册失败:", err)
		return 1
	}

	// 保存身份到本地（私钥 0600 落盘）。
	st := &state.State{
		MachineID:     ack.MachineID,
		MachineSecret: ack.MachineSecret,
		ManagerURL:    *url,
	}
	if err := st.Save(); err != nil {
		fmt.Fprintln(os.Stderr, "[sbx-agent] 保存身份失败:", err)
		return 1
	}

	fmt.Printf("机器已成功接入管理面板\n")
	fmt.Printf("  machine_id: %s\n", ack.MachineID)
	return 0
}

func usage() {
	fmt.Printf("sbx-agent v%s — SBX Pro 节点代理\n\n", version.Version)
	fmt.Println("用法:")
	fmt.Println("  sbx-agent enroll -t TOKEN -u URL  使用 enrollment token 注册")
	fmt.Println("  sbx-agent run                     连接管理面板并持续运行")
	fmt.Println("  sbx-agent version                 版本信息")
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
