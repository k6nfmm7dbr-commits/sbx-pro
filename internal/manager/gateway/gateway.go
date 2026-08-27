// Package gateway 实现 Manager 的 WebSocket gateway（开发提示词第十节）。
//
// Agent 主动连接 wss://manager/api/agent/ws，Manager 不主动发起连接、
// 不开放 Agent 端口。Gateway 负责：
//   - WebSocket 握手与升级；
//   - 机器认证（machine_secret 验签 / 校验）；
//   - 分发 Agent 上报消息（heartbeat 等）；
//   - 在线/离线状态维护。
package gateway

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/auth"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/db"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// upgrader 配置 WebSocket 升级参数。
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Origin 策略：原生 Agent 通常不带 Origin（空 Origin 允许）；
	// 拒绝任意第三方浏览器 Origin，防止 CSWSH（跨站 WebSocket 劫持）。
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" || origin == "https://"+r.Host || origin == "http://"+r.Host
	},
}

// challengeEntry 是一次待验证的认证挑战。
type challengeEntry struct {
	challenge string
	expires   time.Time
}

// Gateway 是 Manager 侧 WebSocket 网关。
type Gateway struct {
	db *db.Manager

	mu         sync.Mutex
	conns      map[string]*agentConn // machine_id -> conn
	lastSeen   map[string]time.Time
	challenges map[string]challengeEntry // machine_id -> 待验证 challenge

	// OnTaskResult 是 task_result 回传回调（由 tasks 模块注入）。
	OnTaskResult func(*protocol.TaskResult)

	// OnTrafficDelta 是流量增量回调（由 traffic 模块注入），返回入库错误。
	OnTrafficDelta func(*protocol.TrafficDelta) error

	// OnIPSync 是在线 IP 快照回调（由 api 层注入，存 ip_sessions）。
	OnIPSync func(*protocol.IPSnapshot)
}

type agentConn struct {
	machineID string
	conn      *websocket.Conn
	send      chan []byte // 串行写队列
	done      chan struct{}
}

// New 构造 Gateway。
func New(d *db.Manager) *Gateway {
	return &Gateway{
		db:         d,
		conns:      make(map[string]*agentConn),
		lastSeen:   make(map[string]time.Time),
		challenges: make(map[string]challengeEntry),
	}
}

// HandleWS 处理 /api/agent/ws 的 WebSocket 升级与握手。
//
// 认证采用 Ed25519 challenge-response：
//  1. Agent 发 hello（仅 machine_id）；
//  2. Manager 回 hello_ack 带一次性 challenge；
//  3. Agent 用私钥对 challenge 签名，二次发 hello（machine_id + signature + signed_data）；
//  4. Manager 用公钥验签，通过则进入消息循环。
func (g *Gateway) HandleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Warn("WebSocket 升级失败", "err", err)
		return
	}

	// 第一步：读首个 hello（仅 machine_id），限时。
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}
	env, err := protocol.UnmarshalEnvelope(data)
	if err != nil || env.Type != protocol.MsgHello {
		_ = conn.Close()
		return
	}
	var hello protocol.Hello
	if err := env.PayloadInto(&hello); err != nil || hello.MachineID == "" {
		_ = conn.Close()
		return
	}

	// 生成一次性 challenge 并回给 Agent。
	challenge, err := auth.NewChallenge()
	if err != nil {
		_ = conn.Close()
		return
	}
	g.storeChallenge(hello.MachineID, challenge)
	ackChallenge, _ := protocol.New(protocol.MsgHelloAck, env.ID, protocol.HelloAck{
		MachineID: hello.MachineID, Accepted: false, Challenge: challenge, Reason: "challenge",
	})
	if b, merr := ackChallenge.Marshal(); merr == nil {
		_ = conn.WriteMessage(websocket.TextMessage, b)
	}

	// 第二步：读签名 hello，限时。
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, data2, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}
	env2, err := protocol.UnmarshalEnvelope(data2)
	if err != nil || env2.Type != protocol.MsgHello {
		_ = conn.Close()
		return
	}
	var signedHello protocol.Hello
	if err := env2.PayloadInto(&signedHello); err != nil {
		_ = conn.Close()
		return
	}

	// 认证：验签。
	ok, reason := g.authenticate(hello.MachineID, challenge, signedHello)
	ack := protocol.HelloAck{MachineID: hello.MachineID, Accepted: ok, Reason: reason}
	resp, _ := protocol.New(protocol.MsgHelloAck, env2.ID, ack)
	b, _ := resp.Marshal()
	_ = conn.WriteMessage(websocket.TextMessage, b)
	if !ok {
		_ = conn.Close()
		return
	}

	// 清除读超时，进入消息循环。
	_ = conn.SetReadDeadline(time.Time{})
	g.deleteChallenge(hello.MachineID)

	ac := &agentConn{
		machineID: hello.MachineID,
		conn:      conn,
		send:      make(chan []byte, 32),
		done:      make(chan struct{}),
	}
	g.register(ac)
	defer g.unregister(ac)

	// 写协程：串行化写，避免并发写 panic。
	go g.writeLoop(ac)

	// 标记在线。
	g.markSeen(hello.MachineID)

	// 读循环。
	g.readLoop(ac)
}

func (g *Gateway) storeChallenge(machineID, challenge string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.challenges[machineID] = challengeEntry{challenge: challenge, expires: time.Now().Add(30 * time.Second)}
}

func (g *Gateway) deleteChallenge(machineID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.challenges, machineID)
}

func (g *Gateway) register(ac *agentConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if old, ok := g.conns[ac.machineID]; ok {
		// 同 machine_id 重复连接：关掉旧连接。
		close(old.done)
		_ = old.conn.Close()
		delete(g.conns, ac.machineID)
	}
	g.conns[ac.machineID] = ac
}

func (g *Gateway) unregister(ac *agentConn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if cur, ok := g.conns[ac.machineID]; ok && cur == ac {
		delete(g.conns, ac.machineID)
	}
	close(ac.done)
	_ = ac.conn.Close()
}

func (g *Gateway) markSeen(machineID string) {
	g.mu.Lock()
	g.lastSeen[machineID] = time.Now()
	g.mu.Unlock()
	// 更新数据库 last_seen + status=online。
	_, _ = g.db.SQL().Exec(
		`UPDATE machines SET last_seen = ?, status = 'online' WHERE machine_id = ?`,
		time.Now().Unix(), machineID)
}

func (g *Gateway) writeLoop(ac *agentConn) {
	for {
		select {
		case msg := <-ac.send:
			_ = ac.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := ac.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ac.done:
			return
		}
	}
}

func (g *Gateway) readLoop(ac *agentConn) {
	for {
		_, data, err := ac.conn.ReadMessage()
		if err != nil {
			return
		}
		env, perr := protocol.UnmarshalEnvelope(data)
		if perr != nil {
			continue
		}
		g.dispatch(ac, env)
	}
}

// SendToMachine 向指定机器发送一条消息（串行写）。返回机器是否在线。
func (g *Gateway) SendToMachine(machineID string, typ, id string, payload any) (bool, error) {
	g.mu.Lock()
	ac, ok := g.conns[machineID]
	g.mu.Unlock()
	if !ok {
		return false, nil
	}
	env, err := protocol.New(typ, id, payload)
	if err != nil {
		return false, err
	}
	data, err := env.Marshal()
	if err != nil {
		return false, err
	}
	select {
	case ac.send <- data:
		return true, nil
	case <-time.After(5 * time.Second):
		return false, fmt.Errorf("写队列已满")
	}
}

// sendToConn 向指定连接发送一条消息（串行写队列，非阻塞超时）。
func (g *Gateway) sendToConn(ac *agentConn, typ string, payload any) error {
	env, err := protocol.New(typ, "", payload)
	if err != nil {
		return err
	}
	data, err := env.Marshal()
	if err != nil {
		return err
	}
	select {
	case ac.send <- data:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("写队列已满")
	}
}

// dispatch 处理 Agent 上报的消息。
func (g *Gateway) dispatch(ac *agentConn, env *protocol.Envelope) {
	switch env.Type {
	case protocol.MsgHeartbeat:
		var hb protocol.Heartbeat
		if err := env.PayloadInto(&hb); err != nil {
			return
		}
		g.markSeen(ac.machineID)
		// 更新机器最新元数据。
		g.updateFromHeartbeat(hb)

	case protocol.MsgTaskResult:
		if g.OnTaskResult != nil {
			var tr protocol.TaskResult
			if err := env.PayloadInto(&tr); err == nil {
				g.OnTaskResult(&tr)
			}
		}

	case protocol.MsgTrafficDelta:
		var td protocol.TrafficDelta
		if err := env.PayloadInto(&td); err == nil {
			ack := protocol.TrafficAck{MachineID: td.MachineID, Sequence: td.Sequence, Accepted: true}
			if g.OnTrafficDelta != nil {
				if err := g.OnTrafficDelta(&td); err != nil {
					ack.Accepted = false
					slog.Warn("流量增量入库失败", "machine_id", td.MachineID, "seq", td.Sequence, "err", err)
				}
			}
			_ = g.sendToConn(ac, protocol.MsgTrafficAck, ack)
		}

	case protocol.MsgSyncState:
		// Phase 5+ 处理 config_revision 同步，先记录。

	case protocol.MsgIPSync:
		if g.OnIPSync != nil {
			var snap protocol.IPSnapshot
			if err := env.PayloadInto(&snap); err == nil && snap.MachineID != "" {
				g.OnIPSync(&snap)
			}
		}

	default:
		slog.Debug("未处理的消息类型", "type", env.Type)
	}
}

// updateFromHeartbeat 用心跳刷新机器的动态字段。
func (g *Gateway) updateFromHeartbeat(hb protocol.Heartbeat) {
	if hb.MachineID == "" {
		return
	}
	_, _ = g.db.SQL().Exec(`
		UPDATE machines SET
			hostname = CASE WHEN ? != '' THEN ? ELSE hostname END,
			agent_version = CASE WHEN ? != '' THEN ? ELSE agent_version END,
			singbox_version = CASE WHEN ? != '' THEN ? ELSE singbox_version END,
			ipv4 = CASE WHEN ? != '' THEN ? ELSE ipv4 END,
			ipv6 = CASE WHEN ? != '' THEN ? ELSE ipv6 END,
			applied_revision = ?,
			last_seen = ?,
			status = 'online'
		WHERE machine_id = ?`,
		hb.Hostname, hb.Hostname,
		hb.AgentVersion, hb.AgentVersion,
		hb.SingboxVersion, hb.SingboxVersion,
		hb.IPv4, hb.IPv4,
		hb.IPv6, hb.IPv6,
		hb.AppliedRevision,
		time.Now().Unix(),
		hb.MachineID)
}

// authenticate 校验 challenge-response：用机器公钥验签 challenge。
// 要求签名数据（SignedData）与本次 challenge 一致，防止 replay。
func (g *Gateway) authenticate(machineID, challenge string, signed protocol.Hello) (bool, string) {
	if signed.MachineID != machineID {
		return false, "machine_id 不一致"
	}
	if signed.SignedData != challenge {
		return false, "challenge 不匹配"
	}
	if signed.Signature == "" {
		return false, "缺少签名"
	}

	// 校验 challenge 仍有效（一次性，防过期 replay）。
	g.mu.Lock()
	ce, ok := g.challenges[machineID]
	g.mu.Unlock()
	if !ok || ce.challenge != challenge || time.Now().After(ce.expires) {
		return false, "challenge 已失效"
	}

	pub, err := auth.LoadPublicKey(g.db.SQL(), machineID)
	if err != nil {
		slog.Warn("认证失败：读取公钥", "machine_id", machineID, "err", err)
		return false, "机器未登记"
	}
	valid, err := auth.VerifyChallenge(pub, challenge, signed.Signature)
	if err != nil || !valid {
		return false, "签名校验失败"
	}
	return true, ""
}

var _ = json.Marshal
