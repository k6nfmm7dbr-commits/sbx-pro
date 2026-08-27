// sbx-agent 是 sbx-pro 的节点代理（B/C/D 机）。
// 主动连接 Manager，执行节点配置、本地流量统计、quota / IP 限制，
// 并保证 Manager 离线时 sing-box 与本地限制继续自治运行。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/client"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/executor"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/handlers"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/heartbeat"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/iplimit"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/nodesvc"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/quota"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/state"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/sysinfo"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/trafsync"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/version"
)

// trafficSender 实现 trafsync.Sender，串行发送流量增量。
type trafficSender struct {
	mu   sync.Mutex
	conn *client.AgentConn
}

func (s *trafficSender) SendTrafficDelta(td protocol.TrafficDelta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("连接未建立")
	}
	return s.conn.SendTrafficDelta(td)
}

// syncNodeIPs 采集各节点的在线公网源 IP 并上报给 Manager（ip_sync）。
func syncNodeIPs(ac *client.AgentConn, svc *nodesvc.Service, machineID string) {
	list := svc.List()
	if len(list) == 0 {
		return
	}
	byPort, _ := connection.RemoteIPsByPort(
		[]string{"/proc/net/tcp", "/proc/net/tcp6"},
		[]string{"/proc/net/udp", "/proc/net/udp6"},
		func(p string) (string, error) {
			b, err := os.ReadFile(p)
			return string(b), err
		},
	)
	now := time.Now().Unix()
	for _, n := range list {
		nodeUUID := nodes.Str(n, "node_uuid")
		if nodeUUID == "" {
			continue
		}
		port, err := strconv.Atoi(nodes.Str(n, "port"))
		if err != nil || port <= 0 {
			continue
		}
		var ipList []protocol.ActiveIP
		for ip, ar := range byPort[port] {
			ipList = append(ipList, protocol.ActiveIP{IP: ip, Proto: ar.Protocol, LastSeen: now})
		}
		_ = ac.Send(protocol.MsgIPSync, "", protocol.IPSnapshot{
			MachineID: machineID,
			NodeUUID:  nodeUUID,
			LocalPort: port,
			ActiveIPs: ipList,
		})
	}
}

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

	// Agent 本地生成 Ed25519 keypair：公钥上传注册，私钥只落盘本地。
	pubHex, privHex, err := state.GenerateKeypair()
	if err != nil {
		fmt.Fprintln(os.Stderr, "[sbx-agent] 生成身份失败:", err)
		return 1
	}
	hello := protocol.Hello{
		EnrollToken:  *token,
		PublicKey:    pubHex,
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

	// 保存身份到本地（私钥 0600 落盘，绝不上传）。
	st := &state.State{
		MachineID:     ack.MachineID,
		MachineSecret: privHex,
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

	// 流量采集 + 同步（复用原 sbx 采集器 + nft 计数）。
	trafficDB, err := database.Open(filepath.Join(nodesvc.DefaultAppDir(), "traffic.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "[sbx-agent] 打开流量数据库失败:", err)
		return 1
	}
	defer trafficDB.Close()

	qs := quota.New(agentDB.DB, nodesvc.DefaultAppDir())
	ils := iplimit.New(agentDB.DB)
	handlers.Register(exec, svc, qs, ils)

	// quota enforcement（本机限额阻断，周期性检查）。
	enforcer := &quota.Enforcer{
		State: qs,
		Used: func(nodeID string) (int64, error) {
			var rx, tx int64
			err := trafficDB.DB.QueryRowContext(ctx,
				`SELECT rx, tx FROM totals WHERE scope = ?`, "node:"+nodeID).Scan(&rx, &tx)
			if err == sql.ErrNoRows {
				return 0, nil
			}
			return rx + tx, err
		},
		Port: func(nodeID string) (int, error) {
			for _, n := range svc.List() {
				if nodes.IDString(n) == nodeID {
					return strconv.Atoi(nodes.Str(n, "port"))
				}
			}
			return 0, fmt.Errorf("节点 %s 不存在", nodeID)
		},
	}
	enforcer.Run(ctx, 30*time.Second)

	// IP limit enforcement（同时在线 IP 限制）。
	ipEnforcer := &iplimit.Enforcer{
		State: ils,
		PortOf: func(nodeID string) (int, error) {
			for _, n := range svc.List() {
				if nodes.IDString(n) == nodeID {
					return strconv.Atoi(nodes.Str(n, "port"))
				}
			}
			return 0, fmt.Errorf("节点 %s 不存在", nodeID)
		},
		ActiveIPs: iplimit.DefaultActiveIPs,
	}
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := ipEnforcer.Check(); err != nil {
					slog.Warn("IP limit 检查失败", "err", err)
				}
			}
		}
	}()
	tcfg := &config.Config{
		DB:        filepath.Join(nodesvc.DefaultAppDir(), "traffic.db"),
		NodesFile: filepath.Join(nodesvc.DefaultAppDir(), "nodes.json"),
		NftConf:   filepath.Join(nodesvc.DefaultAppDir(), "nft.conf"),
		IptScript: filepath.Join(nodesvc.DefaultAppDir(), "iptables.sh"),
		Backend:   "nft",
		Interval:  2,
		TZ:        "Asia/Shanghai",
	}
	traf := trafsync.New(tcfg, trafficDB, st.MachineID,
		func() []nodes.Node { return svc.List() },
		&trafficSender{})

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
		OnTrafficAck: func(ack protocol.TrafficAck) {
			traf.OnAck(ack)
		},
		OnConnect: func(ac *client.AgentConn) {
			// 连接建立后启动流量同步循环。
			traf.Sender = &trafficSender{conn: ac}
			go func() {
				// 先应用 nft 规则，再周期采集 + 同步。
				if err := traf.ApplyRules(); err != nil {
					slog.Warn("初始应用 nft 规则失败", "err", err)
				}
				ticker := time.NewTicker(30 * time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if err := traf.SyncOnce(ctx); err != nil {
							slog.Warn("流量同步失败", "err", err)
						}
					}
				}
			}()

			// 在线 IP 上报循环（每 30s 采集各节点活跃源 IP）。
			go func() {
				syncNodeIPs(ac, svc, st.MachineID)
				ipTicker := time.NewTicker(30 * time.Second)
				defer ipTicker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ipTicker.C:
						syncNodeIPs(ac, svc, st.MachineID)
					}
				}
			}()
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
