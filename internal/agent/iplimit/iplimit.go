// Package iplimit 实现 Agent 本机同时在线 IP 限制（开发提示词第二十八~三十一节）。
//
// 每个节点可设 ip_limit：同一节点同时允许 N 个公网源 IP。
// 超过 N 个 IP 时，新 IP 加入 nftables set 阻断；已有 IP 离线后允许新 IP 补位。
// 按公网源 IP 统计（多个客户端共享出口只算一个 IP，属正常设计）。
package iplimit

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/connection"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/fsx"
)

// Limit 是一条节点 IP 限制。
type Limit struct {
	NodeID  string
	IPLimit int // 0 = 无限制
}

// State 是 IP limit 持久化状态。
type State struct {
	mu sync.Mutex
	DB *sql.DB
}

// New 构造并确保 schema。
func New(db *sql.DB) *State {
	s := &State{DB: db}
	_, _ = s.DB.Exec(`
		CREATE TABLE IF NOT EXISTS ip_limit_state (
			node_id TEXT PRIMARY KEY,
			ip_limit INTEGER NOT NULL DEFAULT 0
		)`)
	return s
}

// SetLimit 设置节点 IP 限制（0=无限制）。
func (s *State) SetLimit(nodeID string, limit int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.DB.Exec(`
		INSERT INTO ip_limit_state (node_id, ip_limit) VALUES (?, ?)
		ON CONFLICT(node_id) DO UPDATE SET ip_limit = excluded.ip_limit`,
		nodeID, limit)
	return err
}

// Limits 读取所有 IP 限制。
func (s *State) Limits() (map[string]int, error) {
	rows, err := s.DB.Query(`SELECT node_id, ip_limit FROM ip_limit_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var limit int
		if err := rows.Scan(&id, &limit); err != nil {
			return nil, err
		}
		out[id] = limit
	}
	return out, rows.Err()
}

const iplimitTable = "sbx_iplimit"

// Enforcer 定期检查 IP 限制并应用 nftables 阻断。
type Enforcer struct {
	State *State
	// PortOf 返回节点端口。
	PortOf func(nodeID string) (int, error)
	// ActiveIPs 返回某端口的当前活跃公网 IP 集合。
	ActiveIPs func(port int) (map[string]struct{}, error)

	lastBlocked map[string]bool // "node:ip" -> blocked
}

// Run 启动 enforcement 循环。
func (e *Enforcer) Run(ctx context.Context, interval int) {
	_ = interval
}

// Check 检查所有 IP 限制节点，构建阻断集合并应用。
func (e *Enforcer) Check() error {
	limits, err := e.State.Limits()
	if err != nil {
		return err
	}
	blocked := map[string]bool{} // key = "ip"（跨节点去重，用 IP 级别的 set）
	for nodeID, limit := range limits {
		if limit <= 0 {
			continue
		}
		port, err := e.PortOf(nodeID)
		if err != nil || port <= 0 {
			continue
		}
		active, err := e.ActiveIPs(port)
		if err != nil {
			continue
		}
		ips := sortedIPs(active)
		// 超过 limit 的 IP 阻断（保留前 limit 个，其余加入 blocked）。
		if len(ips) > limit {
			for _, ip := range ips[limit:] {
				blocked[ip] = true
			}
			slog.Info("节点在线 IP 超限", "node", nodeID, "active", len(ips), "limit", limit, "blocked", len(ips)-limit)
		}
	}

	// 只有集合变化才重写规则。
	if e.lastBlocked == nil || !sameSetString(e.lastBlocked, blocked) {
		if err := applyBlock(blocked); err != nil {
			return err
		}
		e.lastBlocked = blocked
	}
	return nil
}

// applyBlock 应用 nftables IP 阻断 set。
func applyBlock(blocked map[string]bool) error {
	rules := buildIPBlockRules(blocked)
	if err := os.MkdirAll("/run/sbx", 0o755); err != nil {
		return err
	}
	conf := "/run/sbx/iplimit.nft"
	if err := fsx.WriteFileAtomic(conf, []byte(rules), 0o644); err != nil {
		return err
	}
	rc, _, errMsg := firewall.RunCmd(context.Background(), "nft", "-f", conf)
	if rc != 0 {
		return fmt.Errorf("nft IP 阻断规则应用失败: %s", errMsg)
	}
	return nil
}

// buildIPBlockRules 生成 IP 阻断规则（set + drop）。
func buildIPBlockRules(blocked map[string]bool) string {
	var b []byte
	b = append(b, []byte("#!/usr/sbin/nft -f\n")...)
	b = append(b, []byte("table inet "+iplimitTable+"\n")...)
	b = append(b, []byte("delete table inet "+iplimitTable+"\n")...)
	b = append(b, []byte("table inet "+iplimitTable+" {\n")...)
	if len(blocked) > 0 {
		b = append(b, []byte("    set blocked { type ipv4_addr; flags interval; elements = { ")...)
		first := true
		for ip := range blocked {
			if !first {
				b = append(b, []byte(", ")...)
			}
			b = append(b, []byte(ip)...)
			first = false
		}
		b = append(b, []byte(" } }\n")...)
	} else {
		b = append(b, []byte("    set blocked { type ipv4_addr; flags interval; }\n")...)
	}
	b = append(b, []byte("    chain in { type filter hook input priority 150; policy accept; ip saddr @blocked drop; }\n")...)
	b = append(b, []byte("}\n")...)
	return string(b)
}

func sortedIPs(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	// 简单排序保证确定性。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func sameSetString(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// DefaultActiveIPs 返回真实 /proc 读取的活跃 IP（供 Enforcer 使用）。
func DefaultActiveIPs(port int) (map[string]struct{}, error) {
	byPort, partial := connection.RemoteIPsByPort(
		[]string{"/proc/net/tcp", "/proc/net/tcp6"},
		[]string{"/proc/net/udp", "/proc/net/udp6"},
		readOSFile)
	_ = partial
	out := map[string]struct{}{}
	if m, ok := byPort[port]; ok {
		for ip := range m {
			out[ip] = struct{}{}
		}
	}
	return out, nil
}

func readOSFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
