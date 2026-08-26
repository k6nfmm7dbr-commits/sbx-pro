// sbx-agent 是 sbx-pro 的节点代理（B/C/D 机）。
// 主动连接 Manager，执行节点配置、本地流量统计、quota / IP 限制，
// 并保证 Manager 离线时 sing-box 与本地限制继续自治运行。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/client"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/executor"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/handlers"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/heartbeat"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/nodesvc"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/state"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/sysinfo"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/database"
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
		os.Exit(runAgent(args[1:]))

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

// runAgent 处理 `sbx-agent run`：建立 WebSocket 长连接并持续运行。
func runAgent(args []string) int {
	// 读取本地状态，未注册则提示先 enroll。
	st, err := state.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[sbx-agent]", err)
		return 1
	}
	if !st.Registered() {
		fmt.Fprintln(os.Stderr, "[sbx-agent] 尚未注册，请先执行: sbx-agent enroll -t TOKEN -u URL")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hb := heartbeat.New()
	info := sysinfo.Gather()

	// 打开 Agent 本地数据库（身份/task 幂等/traffic/quota 等）。
	agentDB, err := database.Open(filepath.Join(state.AppDir(), "agent.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "[sbx-agent] 打开本地数据库失败:", err)
		return 1
	}
	defer agentDB.Close()

	exec := executor.New(agentDB.DB)
	svc := nodesvc.New("") // 节点数据目录默认 /etc/sbx，可用 SBX_DIR 覆盖
	handlers.Register(exec, svc)

	cfg := client.RunConfig{
		ManagerURL:    st.ManagerURL,
		MachineID:     st.MachineID,
		MachineSecret: st.MachineSecret,
		HeartbeatSec:  15,
		HeartbeatFunc: func() protocol.Heartbeat {
			h := hb.Build(st.MachineID, nil)
			h.Hostname = info.Hostname
			h.AgentVersion = version.Version
			h.AppliedRevision = st.AppliedRevision
			return h
		},
		OnTask: func(ac *client.AgentConn, env *protocol.Envelope) {
			res := exec.Handle(env)
			_ = ac.Send(protocol.MsgTaskResult, env.ID, res)
		},
	}

	slog.Info("sbx-agent 启动，连接 Manager", "manager", st.ManagerURL, "machine_id", st.MachineID)
	if err := client.Run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "[sbx-agent]", err)
		return 1
	}
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
