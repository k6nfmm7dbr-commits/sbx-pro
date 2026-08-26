// Package nodes 实现 Manager 的全局节点管理（开发提示词第十三~十八节 / 三十八节）。
//
// 节点以 node_uuid 为全局唯一主键（不依赖 machine_id + local_node_id 组合），
// local_node_id 保留用于与 Agent 本地节点对应。node_uuid 不绑定 local_node_id，
// 为后续跨机器迁移预留（第三十九节）。
package nodes

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
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
	NodeUUID  string `json:"node_uuid"`
	MachineID string `json:"machine_id"`
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	Port      int    `json:"port"`
	Status    string `json:"status"`
	QuotaLimit int64 `json:"quota_limit"`
	IPLimit   int    `json:"ip_limit"`
	CreatedAt int64  `json:"created_at"`
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
	}
}

// Create 创建全局节点记录。config 存 JSON（含敏感凭据，仅 share 接口读取）。
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
		Status:    "sync_pending",
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

// UpdateLocalNodeID 记录 Agent 回传的 local_node_id。
func UpdateLocalNodeID(db *sql.DB, nodeUUID string, localID int64) error {
	_, err := db.Exec(`UPDATE nodes SET local_node_id = ?, status = 'active', updated_at = ? WHERE node_uuid = ?`,
		localID, time.Now().Unix(), nodeUUID)
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
		SELECT node_uuid, machine_id, name, protocol, port, status, quota_limit, ip_limit, created_at
		FROM nodes ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PublicNode{}
	for rows.Next() {
		var p PublicNode
		if err := rows.Scan(&p.NodeUUID, &p.MachineID, &p.Name, &p.Protocol, &p.Port,
			&p.Status, &p.QuotaLimit, &p.IPLimit, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Delete 删除节点记录（Manager 侧）。
func Delete(db *sql.DB, nodeUUID string) error {
	_, err := db.Exec(`DELETE FROM nodes WHERE node_uuid = ?`, nodeUUID)
	return err
}

// Secret 返回节点的敏感凭据（config JSON），供 share 接口。
func (n *Node) SecretJSON() string { return n.Config }

// GenRealityKeypair 生成 VLESS Reality 密钥对（占位，Phase 5 用 sing-box 生成）。
func GenRealityKeypair() (priv, pub string, err error) {
	// 实际由 sing-box 的 `generate reality-keypair` 生成，此函数仅供占位。
	return "", "", nil
}

var _ = rand.Read
var _ = hex.EncodeToString
