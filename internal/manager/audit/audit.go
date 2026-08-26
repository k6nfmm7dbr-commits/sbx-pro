// Package audit 实现 Manager 的审计日志（开发提示词第四十八节）。
//
// 记录管理员关键操作（创建/修改/删除节点、调整流量、改 IP Limit、
// 添加/删除机器、同步配置等），不记录敏感 password/private_key。
package audit

import (
	"database/sql"
	"time"
)

// Log 写入一条审计日志。
func Log(db *sql.DB, action, machineID, nodeUUID, result, sourceIP string) {
	_, _ = db.Exec(`
		INSERT INTO audit_logs (time, action, machine_id, node_uuid, result, source_ip)
		VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), action, machineID, nodeUUID, result, sourceIP)
}

// Entry 是一条审计记录。
type Entry struct {
	Time      int64  `json:"time"`
	Action    string `json:"action"`
	MachineID string `json:"machine_id"`
	NodeUUID  string `json:"node_uuid"`
	Result    string `json:"result"`
	SourceIP  string `json:"source_ip"`
}

// List 返回最近 N 条审计日志（默认 200）。
func List(db *sql.DB, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.Query(`
		SELECT time, action, machine_id, node_uuid, result, source_ip
		FROM audit_logs ORDER BY time DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Time, &e.Action, &e.MachineID, &e.NodeUUID, &e.Result, &e.SourceIP); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
