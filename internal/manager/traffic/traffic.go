// Package traffic 实现 Manager 的多机流量汇总（开发提示词第二十二/二十四节）。
//
// 每台 Agent 本地采集 → 增量同步（traffic_delta）→ Manager 按 (machine_id, sequence)
// 唯一约束去重 → 汇总到 traffic_daily / traffic_total。
package traffic

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// IngestDelta 接收一条流量增量，防重复计入。
// 通过 (machine_id, sequence) 唯一约束，重复发送同一 sequence 只会被忽略。
// 返回 (是否新数据, error)。重复数据返回 accepted=false 而非错误。
func IngestDelta(db *sql.DB, d protocol.TrafficDelta) (bool, error) {
	if d.MachineID == "" {
		return false, fmt.Errorf("traffic_delta 缺少 machine_id")
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	// 唯一约束：重复 sequence 直接忽略。
	res, err := tx.Exec(`
		INSERT INTO traffic_events (machine_id, sequence, node_uuid, rx_bytes, tx_bytes, start_time, end_time)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.MachineID, d.Sequence, d.NodeUUID, d.RxBytes, d.TxBytes, d.StartTime, d.EndTime)
	if err != nil {
		if isUniqueViolation(err) {
			return false, nil // 重复数据，忽略
		}
		return false, fmt.Errorf("写入 traffic_event 失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}

	// 累加到 traffic_total（node_uuid 维度，machine_id 维度也保留）。
	day := time.Now().Format("2006-01-02")
	if _, err := tx.Exec(`
		INSERT INTO traffic_total (machine_id, node_uuid, rx, tx)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(machine_id, node_uuid) DO UPDATE SET rx = rx + excluded.rx, tx = tx + excluded.tx`,
		d.MachineID, d.NodeUUID, d.RxBytes, d.TxBytes); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`
		INSERT INTO traffic_daily (day, machine_id, node_uuid, rx, tx)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(day, machine_id, node_uuid) DO UPDATE SET rx = rx + excluded.rx, tx = tx + excluded.tx`,
		day, d.MachineID, d.NodeUUID, d.RxBytes, d.TxBytes); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// isUniqueViolation 判断 SQLite 唯一约束冲突（轻量字符串判断，避免引入 sentinel）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return contains(s, "UNIQUE constraint failed") || contains(s, "constraint failed")
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TotalNode 是单节点累计流量。
type TotalNode struct {
	NodeUUID string `json:"node_uuid"`
	MachineID string `json:"machine_id"`
	Rx       int64  `json:"rx"`
	Tx       int64  `json:"tx"`
}

// TotalsByMachine 返回累计流量汇总。machineID 为空时返回所有机器。
func TotalsByMachine(db *sql.DB, machineID string) ([]TotalNode, error) {
	var rows *sql.Rows
	var err error
	if machineID == "" {
		rows, err = db.Query(`
			SELECT machine_id, node_uuid, rx, tx FROM traffic_total ORDER BY machine_id, node_uuid`)
	} else {
		rows, err = db.Query(`
			SELECT machine_id, node_uuid, rx, tx FROM traffic_total
			WHERE machine_id = ? ORDER BY node_uuid`, machineID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TotalNode{}
	for rows.Next() {
		var t TotalNode
		if err := rows.Scan(&t.MachineID, &t.NodeUUID, &t.Rx, &t.Tx); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
