// 该文件实现 sbx-agent 的 WebSocket 长连接运行逻辑：
//   - 连接 Manager /api/agent/ws；
//   - 发送 hello 认证帧；
//   - 周期心跳（默认 15s）；
//   - 断线指数退避重连（1s/2s/4s/8s/15s/30s/60s 封顶）。
package client

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// RunConfig 是 Agent 运行参数的聚集。
type RunConfig struct {
	ManagerURL    string
	MachineID     string
	MachineSecret string
	HeartbeatSec  int
	HeartbeatFunc func() protocol.Heartbeat
	OnTask        func(ac *AgentConn, env *protocol.Envelope) // Phase 4 任务回调
}

// Run 建立 WebSocket 连接并持续运行（阻塞），断线自动重连。
// ctx 取消时退出。
func Run(ctx context.Context, cfg RunConfig) error {
	if cfg.HeartbeatSec <= 0 {
		cfg.HeartbeatSec = 15
	}
	backoff := []time.Duration{1, 2, 4, 8, 15, 30, 60}
	idx := 0
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		err := runOnce(ctx, cfg)
		if ctx.Err() != nil {
			return nil
		}
		// 指数退避重连：1s/2s/4s/8s/15s/30s/60s，60s 封顶（不无限增长）。
		d := backoff[idx]
		slog.Warn("与 Manager 断开，准备重连", "err", err, "wait_sec", int(d.Seconds()))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(d):
		}
		if idx < len(backoff)-1 {
			idx++
		}
	}
}

// runOnce 尝试建立一次连接并运行到断开/ctx 取消。
func runOnce(ctx context.Context, cfg RunConfig) error {
	wsURL := wsURL(cfg.ManagerURL)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("连接 Manager 失败: %w", err)
	}
	ac := &AgentConn{conn: conn}
	defer conn.Close()

	// 发送 hello 认证帧。
	hello := protocol.Hello{
		MachineID:     cfg.MachineID,
		MachineSecret: cfg.MachineSecret,
	}
	if err := ac.send(protocol.MsgHello, "", hello); err != nil {
		return err
	}

	// 读 hello_ack。
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	env, err := ac.read()
	if err != nil {
		return fmt.Errorf("等待认证应答失败: %w", err)
	}
	if env.Type != protocol.MsgHelloAck {
		return fmt.Errorf("期望 hello_ack，收到 %s", env.Type)
	}
	var ack protocol.HelloAck
	if err := env.PayloadInto(&ack); err != nil || !ack.Accepted {
		return fmt.Errorf("认证被拒绝: %s", ack.Reason)
	}
	_ = conn.SetReadDeadline(time.Time{})
	slog.Info("已连接 Manager 并通过认证", "machine_id", cfg.MachineID)

	// 心跳 ticker。
	hbTicker := time.NewTicker(time.Duration(cfg.HeartbeatSec) * time.Second)
	defer hbTicker.Stop()

	// 读循环（单独协程），主循环处理心跳与退出。
	readCh := make(chan *protocol.Envelope, 32)
	readErr := make(chan error, 1)
	go func() {
		for {
			e, rerr := ac.read()
			if rerr != nil {
				readErr <- rerr
				return
			}
			readCh <- e
		}
	}()

	// 写串行化由 AgentConn.send 内部互斥锁保证。

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-hbTicker.C:
			hb := cfg.HeartbeatFunc()
			if err := ac.send(protocol.MsgHeartbeat, "", hb); err != nil {
				return err
			}
		case e := <-readCh:
			if e == nil {
				continue
			}
			if cfg.OnTask != nil && protocol.IsTaskType(e.Type) {
				go cfg.OnTask(ac, e)
			}
		case rerr := <-readErr:
			return rerr
		}
	}
}

// AgentConn 封装一条活跃连接，提供串行读/写。
type AgentConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (ac *AgentConn) read() (*protocol.Envelope, error) {
	_, data, err := ac.conn.ReadMessage()
	if err != nil {
		return nil, err
	}
	return protocol.UnmarshalEnvelope(data)
}

// Send 串行化发送一条消息。
func (ac *AgentConn) Send(typ, id string, payload any) error {
	return ac.send(typ, id, payload)
}

func (ac *AgentConn) send(typ, id string, payload any) error {
	env, err := protocol.New(typ, id, payload)
	if err != nil {
		return err
	}
	data, err := env.Marshal()
	if err != nil {
		return err
	}
	ac.writeMu.Lock()
	defer ac.writeMu.Unlock()
	_ = ac.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ac.conn.WriteMessage(websocket.TextMessage, data)
}

// wsURL 由 Manager URL 构造 WebSocket URL。
func wsURL(managerURL string) string {
	u, err := url.Parse(managerURL)
	if err != nil {
		return managerURL + "/api/agent/ws"
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = "/api/agent/ws"
	return u.String()
}
