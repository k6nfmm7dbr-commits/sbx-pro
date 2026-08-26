// Package nodes 实现 Manager 的全局节点管理（开发提示词第十三~十八节 / 三十八节）。
//
// 节点以 node_uuid 为全局唯一主键（不依赖 machine_id + local_node_id 组合），
// local_node_id 保留用于与 Agent 本地节点对应。
//
// 状态机（desired/actual 分叉治理）：
//
//	provisioning ──success──▶ active ──update──▶ update_pending ──success──▶ active
//	     │ failure                 │ disable/enable                  │ failure
//	     ▼                         ▼                                 ▼
//	create_failed              disabled ◀──────────────▶ active    config_error
//	active ──delete──▶ delete_pending ──success──▶ (记录删除)
//
// 关键：delete 任务在 Agent 真正删除成功以前，Manager 不提前永久删除记录。
package nodes

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// 节点状态常量（见上方状态机）。
const (
	StatusProvisioning  = "provisioning"
	StatusActive        = "active"
	StatusCreateFailed  = "create_failed"
	StatusUpdatePending = "update_pending"
	StatusDeletePending = "delete_pending"
	StatusDisabled      = "disabled"
	StatusConfigError   = "config_error"
)

// Node 是 Manager 视图中的全局节点记录。
type Node struct {
	NodeUUID    string `json:"node_uuid"`
	MachineID   string `json:"machine_id"`
	LocalNodeID int64  `json:"local_node_id"`
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	Port        int    `json:"port"`
	Status      string `json:"status"`
	Config      string `json:"-"`
	QuotaLimit  int64  `json:"quota_limit"`
	QuotaPeriod string `json:"quota_period"`
	IPLimit     int    `json:"ip_limit"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// PublicNode 是脱敏后的节点 DTO（不含 config 中的敏感凭据）。
type PublicNode struct {
	NodeUUID    string `json:"node_uuid"`
	MachineID   string `json:"machine_id"`
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	Port        int    `json:"port"`
	Status      string `json:"status"`
	QuotaLimit  int64  `json:"quota_limit"`
	IPLimit     int    `json:"ip_limit"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// Public 脱敏转换。
func (n *Node) Public() PublicNode {
	return PublicNode{
		NodeUUID:   n.NodeUUID,
		MachineID:  n.MachineID,
		Name:       n.Name,
		Protocol:   n.Protocol,
		Port:       n.Port,
		Status:     n.Status,
		QuotaLimit: n.QuotaLimit,
		IPLimit:    n.IPLimit,
		CreatedAt:  n.CreatedAt,
		UpdatedAt:  n.UpdatedAt,
	}
}

// Create 创建全局节点记录（status=provisioning）。config 存 JSON（含敏感凭据）。
func Create(db *sql.DB, machineID string, name, protocol string, port int, config map[string]any) (*Node, error) {
	nodeUUID, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("生成 node_uuid 失败: %w", err)
	}
	cfgJSON, _ := json.Marshal(config)
	now := time.Now().Unix()
	n := &Node{
		NodeUUID:  nodeUUID.String(),
		MachineID: machineID,
		Name:      name,
		Protocol:  protocol,
		Port:      port,
		Status:    StatusProvisioning,
		Config:    string(cfgJSON),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := db.Exec(`
		INSERT INTO nodes (node_uuid, machine_id, local_node_id, name, protocol, port, status, config, quota_limit, quota_period, ip_limit, created_at, updated_at)
		VALUES (?, ?, 0, ?, ?, ?, ?, ?, 0, '', 0, ?, ?)`,
		n.NodeUUID, n.MachineID, n.Name, n.Protocol, n.Port, n.Status, n.Config, n.CreatedAt, n.UpdatedAt); err != nil {
		return nil, fmt.Errorf("创建节点记录失败: %w", err)
	}
	return n, nil
}

// Update 更新节点 desired 字段（协议/端口/名称/config），状态置 update_pending。
func Update(db *sql.DB, nodeUUID, name, protocol string, port int, config map[string]any) (*Node, error) {
	n, err := Get(db, nodeUUID)
	if err != nil {
		return nil, err
	}
	cfgJSON, _ := json.Marshal(config)
	now := time.Now().Unix()
	if _, err := db.Exec(`
		UPDATE nodes SET name = ?, protocol = ?, port = ?, config = ?, status = ?, updated_at = ?
		WHERE node_uuid = ?`,
		name, protocol, port, string(cfgJSON), StatusUpdatePending, now, nodeUUID); err != nil {
		return nil, fmt.Errorf("更新节点失败: %w", err)
	}
	n.Name, n.Protocol, n.Port = name, protocol, port
	n.Config = string(cfgJSON)
	n.Status = StatusUpdatePending
	n.UpdatedAt = now
	return n, nil
}

// UpdateLocalNodeID 记录 Agent 回传的 local_node_id，并置为 active。
func UpdateLocalNodeID(db *sql.DB, nodeUUID string, localID int64) error {
	_, err := db.Exec(`UPDATE nodes SET local_node_id = ?, status = ?, updated_at = ? WHERE node_uuid = ?`,
		localID, StatusActive, time.Now().Unix(), nodeUUID)
	return err
}

// UpdateStatus 条件更新节点状态（防并发回退：只在指定 from 状态时转换）。
func UpdateStatus(db *sql.DB, nodeUUID, from, to string) error {
	res, err := db.Exec(`UPDATE nodes SET status = ?, updated_at = ? WHERE node_uuid = ? AND status = ?`,
		to, time.Now().Unix(), nodeUUID, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("节点 %s 状态转换失败: 期望 %s", nodeUUID, from)
	}
	return nil
}

// SetQuota 设置节点流量限额（bytes，0=无限制）。
func SetQuota(db *sql.DB, nodeUUID string, limitBytes int64) error {
	_, err := db.Exec(`UPDATE nodes SET quota_limit = ?, updated_at = ? WHERE node_uuid = ?`,
		limitBytes, time.Now().Unix(), nodeUUID)
	return err
}

// SetIPLimit 设置节点在线 IP 限制（0=无限制）。
func SetIPLimit(db *sql.DB, nodeUUID string, limit int) error {
	_, err := db.Exec(`UPDATE nodes SET ip_limit = ?, updated_at = ? WHERE node_uuid = ?`,
		limit, time.Now().Unix(), nodeUUID)
	return err
}

// Get 查询节点。
func Get(db *sql.DB, nodeUUID string) (*Node, error) {
	n := &Node{}
	err := db.QueryRow(`
		SELECT node_uuid, machine_id, local_node_id, name, protocol, port, status, config, quota_limit, quota_period, ip_limit, created_at, updated_at
		FROM nodes WHERE node_uuid = ?`, nodeUUID).Scan(
		&n.NodeUUID, &n.MachineID, &n.LocalNodeID, &n.Name, &n.Protocol, &n.Port,
		&n.Status, &n.Config, &n.QuotaLimit, &n.QuotaPeriod, &n.IPLimit, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return n, nil
}

// List 列出全局节点（脱敏）。
func List(db *sql.DB) ([]PublicNode, error) {
	rows, err := db.Query(`
		SELECT node_uuid, machine_id, name, protocol, port, status, quota_limit, ip_limit, created_at, updated_at
		FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PublicNode{}
	for rows.Next() {
		var p PublicNode
		if err := rows.Scan(&p.NodeUUID, &p.MachineID, &p.Name, &p.Protocol, &p.Port,
			&p.Status, &p.QuotaLimit, &p.IPLimit, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete 删除节点记录（Manager 侧，仅在 Agent 确认删除成功后调用）。
func Delete(db *sql.DB, nodeUUID string) error {
	_, err := db.Exec(`DELETE FROM nodes WHERE node_uuid = ?`, nodeUUID)
	return err
}

// Secret 返回节点的敏感凭据（config JSON），供 share 接口。
func (n *Node) SecretJSON() string { return n.Config }

// ParseSecret 解析 config JSON 为 map（失败返回空 map）。
func (n *Node) ParseSecret() map[string]any {
	var cfg map[string]any
	_ = json.Unmarshal([]byte(n.Config), &cfg)
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg
}
