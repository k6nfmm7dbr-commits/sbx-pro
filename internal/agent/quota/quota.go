// Package quota 实现 Agent 本机流量限额 enforcement（开发提示词第二十五~二十七节）。
//
// 流量限制必须在 Agent 本机执行（不依赖 Manager 实时判断）：
//   - 定期读本地累计流量（totals，scope node:<id>）；
//   - used >= limit 时用 nftables 阻断该节点端口；
//   - 提高额度后解除阻断，节点恢复；
//   - quota 状态存本地 SQLite，重启后恢复 enforcement。
package quota

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/firewall"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/fsx"
)

// Limit 是一条节点限额。
type Limit struct {
	NodeID string // 本地节点 id（如 "2"）
	LimitBytes int64 // 0 = 无限制
	Exceeded bool   // 是否已达上限
}

// State 是 quota 持久化状态。
type State struct {
	mu    sync.Mutex
	DB    *sql.DB
	AppDir string
}

// New 构造 quota 状态并确保 schema。
func New(db *sql.DB, appDir string) *State {
	s := &State{DB: db, AppDir: appDir}
	s.ensureSchema()
	return s
}

func (s *State) ensureSchema() {
	_, _ = s.DB.Exec(`
		CREATE TABLE IF NOT EXISTS quota_state (
			node_id TEXT PRIMARY KEY,
			limit_bytes INTEGER NOT NULL DEFAULT 0,
			exceeded INTEGER NOT NULL DEFAULT 0
		)`)
}

// SetLimit 设置节点限额（bytes，0=无限制）。
func (s *State) SetLimit(nodeID string, bytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.DB.Exec(`
		INSERT INTO quota_state (node_id, limit_bytes, exceeded) VALUES (?, ?, 0)
		ON CONFLICT(node_id) DO UPDATE SET limit_bytes = excluded.limit_bytes`,
		nodeID, bytes)
	return err
}

// ResetQuota 清除节点限额（解除限制）。
func (s *State) ResetQuota(nodeID string) error {
	return s.SetLimit(nodeID, 0)
}

// Limits 读取所有限额（用于 enforcement）。
func (s *State) Limits() (map[string]Limit, error) {
	rows, err := s.DB.Query(`SELECT node_id, limit_bytes, exceeded FROM quota_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Limit{}
	for rows.Next() {
		var l Limit
		if err := rows.Scan(&l.NodeID, &l.LimitBytes, &l.Exceeded); err != nil {
			return nil, err
		}
		out[l.NodeID] = l
	}
	return out, rows.Err()
}

// MarkExceeded 记录节点已超限（持久化）。
func (s *State) MarkExceeded(nodeID string, exceeded bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := 0
	if exceeded {
		v = 1
	}
	_, _ = s.DB.Exec(`UPDATE quota_state SET exceeded = ? WHERE node_id = ?`, v, nodeID)
}

// ---- nftables 阻断 ----

const quotaTable = "sbx_quota"

// ApplyBlock 用 nftables 阻断一组端口（set 方式，避免 rule 无限增长）。
func ApplyBlock(blockedPorts map[int]bool) error {
	rules := buildBlockRules(blockedPorts)
	if err := os.MkdirAll("/run/sbx", 0o755); err != nil {
		return err
	}
	conf := "/run/sbx/quota.nft"
	if err := fsx.WriteFileAtomic(conf, []byte(rules), 0o644); err != nil {
		return err
	}
	rc, _, errMsg := firewall.RunCmd(context.Background(), "nft", "-f", conf)
	if rc != 0 {
		return fmt.Errorf("nft 阻断规则应用失败: %s", errMsg)
	}
	return nil
}

// ClearBlock 清除所有阻断规则。
func ClearBlock() error {
	rc, _, errMsg := firewall.RunCmd(context.Background(), "nft", "delete", "table", "inet", quotaTable)
	if rc != 0 && !firewall.IsMissingMsg(errMsg) {
		return fmt.Errorf("nft 阻断规则清除失败: %s", errMsg)
	}
	return nil
}

// buildBlockRules 生成阻断端口的 nft 规则（priority 200，早于计数链 300）。
func buildBlockRules(blockedPorts map[int]bool) string {
	var b []byte
	b = append(b, []byte("#!/usr/sbin/nft -f\n")...)
	b = append(b, []byte("table inet "+quotaTable+"\n")...)
	b = append(b, []byte("delete table inet "+quotaTable+"\n")...)
	b = append(b, []byte("table inet "+quotaTable+" {\n")...)
	if len(blockedPorts) == 0 {
		b = append(b, []byte("    set blocked { type inet_service; }\n")...)
		b = append(b, []byte("    chain in { type filter hook input priority 200; policy accept; }\n")...)
		b = append(b, []byte("    chain out { type filter hook output priority 200; policy accept; }\n")...)
		b = append(b, []byte("}\n")...)
		return string(b)
	}
	// set 元素。
	b = append(b, []byte("    set blocked { type inet_service; elements = { ")...)
	first := true
	for p := range blockedPorts {
		if !first {
			b = append(b, []byte(", ")...)
		}
		b = append(b, []byte(strconv.Itoa(p))...)
		first = false
	}
	b = append(b, []byte(" } }\n")...)
	b = append(b, []byte("    chain in { type filter hook input priority 200; policy accept; ")...)
	b = append(b, []byte("tcp dport @blocked drop; udp dport @blocked drop; }\n")...)
	b = append(b, []byte("    chain out { type filter hook output priority 200; policy accept; ")...)
	b = append(b, []byte("tcp sport @blocked drop; udp sport @blocked drop; }\n")...)
	b = append(b, []byte("}\n")...)
	return string(b)
}

// ---- enforcement 循环 ----

// Enforcer 定期检查限额并应用阻断。
type Enforcer struct {
	State *State
	// Used 返回某节点累计流量（rx+tx）。
	Used func(nodeID string) (int64, error)
	// Port 返回某节点的端口。
	Port func(nodeID string) (int, error)

	lastBlocked map[int]bool
}

// Run 启动 enforcement 循环（周期检查）。
func (e *Enforcer) Run(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := e.check(); err != nil {
					slog.Warn("quota enforcement 检查失败", "err", err)
				}
			}
		}
	}()
}

// check 检查所有限额节点，构建阻断端口集合并应用。
func (e *Enforcer) check() error {
	limits, err := e.State.Limits()
	if err != nil {
		return err
	}
	blocked := map[int]bool{}
	for nodeID, l := range limits {
		if l.LimitBytes <= 0 {
			continue
		}
		used, err := e.Used(nodeID)
		if err != nil {
			continue
		}
		if used >= l.LimitBytes {
			port, perr := e.Port(nodeID)
			if perr == nil && port > 0 {
				blocked[port] = true
			}
			if !l.Exceeded {
				e.State.MarkExceeded(nodeID, true)
				slog.Info("节点流量已用尽，阻断端口", "node", nodeID, "used", used, "limit", l.LimitBytes, "port", port)
			}
		} else if l.Exceeded {
			e.State.MarkExceeded(nodeID, false)
			slog.Info("节点额度已恢复", "node", nodeID, "used", used, "limit", l.LimitBytes)
		}
	}

	// 只有集合变化才重写规则（避免频繁 exec）。
	if e.lastBlocked == nil || !sameSet(e.lastBlocked, blocked) {
		if err := ApplyBlock(blocked); err != nil {
			return err
		}
		e.lastBlocked = blocked
	}
	return nil
}

func sameSet(a, b map[int]bool) bool {
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
