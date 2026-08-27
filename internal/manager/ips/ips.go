// Package ips 实现 Manager 的在线 IP 会话存储与查询。
//
// Agent 定期把各节点的活跃公网源 IP 上报（ip_sync），Manager 存入
// ip_sessions（machine_id + node_uuid + ip + proto + last_seen）。
// 该表用于「查看节点当前在线 IP」展示。
package ips

import (
	"database/sql"
	"fmt"
	"time"
)

// ActiveIP 是一条活跃源 IP 会话。
type ActiveIP struct {
	IP       string `json:"ip"`
	Proto    string `json:"proto"` // tcp | udp
	LastSeen int64  `json:"last_seen"`
}

// Sync 全量替换某节点的在线 IP 会话（先删后插，事务内完成）。
func Sync(db *sql.DB, machineID, nodeUUID string, ips []ActiveIP) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM ip_sessions WHERE machine_id = ? AND node_uuid = ?`,
		machineID, nodeUUID); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, ip := range ips {
		proto := ip.Proto
		if proto == "" {
			proto = "tcp"
		}
		if _, err := tx.Exec(`
			INSERT INTO ip_sessions (machine_id, node_uuid, ip, proto, last_seen)
			VALUES (?, ?, ?, ?, ?)`, machineID, nodeUUID, ip.IP, proto, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListByNode 返回某节点的活跃 IP 列表（按 IP 排序，最近 3 分钟内看作活跃）。
func ListByNode(db *sql.DB, machineID, nodeUUID string) ([]ActiveIP, error) {
	cutoff := time.Now().Add(-3 * time.Minute).Unix()
	rows, err := db.Query(`
		SELECT ip, proto, last_seen FROM ip_sessions
		WHERE machine_id = ? AND node_uuid = ? AND last_seen >= ?
		ORDER BY ip`, machineID, nodeUUID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActiveIP{}
	for rows.Next() {
		var a ActiveIP
		if err := rows.Scan(&a.IP, &a.Proto, &a.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountByNode 返回某节点的当前活跃 IP 数。
func CountByNode(db *sql.DB, machineID, nodeUUID string) (int, error) {
	cutoff := time.Now().Add(-3 * time.Minute).Unix()
	var n int
	err := db.QueryRow(`
		SELECT COUNT(*) FROM ip_sessions
		WHERE machine_id = ? AND node_uuid = ? AND last_seen >= ?`,
		machineID, nodeUUID, cutoff).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// DeleteForNode 删除某节点的在线 IP 会话（节点删除时清理）。
func DeleteForNode(db *sql.DB, machineID, nodeUUID string) error {
	_, err := db.Exec(`DELETE FROM ip_sessions WHERE machine_id = ? AND node_uuid = ?`, machineID, nodeUUID)
	if err != nil {
		return fmt.Errorf("清理 ip_sessions 失败: %w", err)
	}
	return nil
}