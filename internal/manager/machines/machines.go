// Package machines 实现机器注册与查询（开发提示词第七节 / 三十三节）。
package machines

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// Machine 是 Manager 视图中的一台机器记录。
type Machine struct {
	MachineID       string `json:"machine_id"`
	DisplayName     string `json:"display_name"`
	Hostname        string `json:"hostname"`
	IPv4            string `json:"ipv4"`
	IPv6            string `json:"ipv6"`
	OS              string `json:"os"`
	Kernel          string `json:"kernel"`
	Arch            string `json:"arch"`
	AgentVersion    string `json:"agent_version"`
	SingboxVersion  string `json:"singbox_version"`
	LastSeen        int64  `json:"last_seen"`
	CreatedAt       int64  `json:"created_at"`
	Status          string `json:"status"`
	ConfigRevision  int64  `json:"config_revision"`
	AppliedRevision int64  `json:"applied_revision"`
}

// Register 在 machines 表创建一条机器记录。幂等：machine_id 已存在则更新元数据。
// 机器身份（agents 表）由 auth.StoreIdentity 单独写入。
func Register(db *sql.DB, m *Machine) error {
	now := time.Now().Unix()
	if m.CreatedAt == 0 {
		m.CreatedAt = now
	}
	if m.LastSeen == 0 {
		m.LastSeen = now
	}
	_, err := db.Exec(`
		INSERT INTO machines (
			machine_id, display_name, hostname, ipv4, ipv6, os, kernel, arch,
			agent_version, singbox_version, last_seen, created_at, status,
			config_revision, applied_revision
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(machine_id) DO UPDATE SET
			display_name = excluded.display_name,
			hostname = excluded.hostname,
			ipv4 = excluded.ipv4,
			ipv6 = excluded.ipv6,
			os = excluded.os,
			kernel = excluded.kernel,
			arch = excluded.arch,
			agent_version = excluded.agent_version,
			singbox_version = excluded.singbox_version,
			last_seen = excluded.last_seen,
			status = excluded.status`,
		m.MachineID, m.DisplayName, m.Hostname, m.IPv4, m.IPv6, m.OS, m.Kernel,
		m.Arch, m.AgentVersion, m.SingboxVersion, m.LastSeen, m.CreatedAt,
		m.Status, m.ConfigRevision, m.AppliedRevision)
	if err != nil {
		return fmt.Errorf("写入机器记录失败: %w", err)
	}
	return nil
}

// Get 按 machine_id 查询机器记录。
func Get(db *sql.DB, machineID string) (*Machine, error) {
	m := &Machine{}
	err := db.QueryRow(`
		SELECT machine_id, display_name, hostname, ipv4, ipv6, os, kernel, arch,
		       agent_version, singbox_version, last_seen, created_at, status,
		       config_revision, applied_revision
		FROM machines WHERE machine_id = ?`, machineID).
		Scan(&m.MachineID, &m.DisplayName, &m.Hostname, &m.IPv4, &m.IPv6,
			&m.OS, &m.Kernel, &m.Arch, &m.AgentVersion, &m.SingboxVersion,
			&m.LastSeen, &m.CreatedAt, &m.Status, &m.ConfigRevision, &m.AppliedRevision)
	if err != nil {
		return nil, err
	}
	return m, nil
}

// List 返回所有机器（可选 status 过滤）。
func List(db *sql.DB) ([]Machine, error) {
	rows, err := db.Query(`
		SELECT machine_id, display_name, hostname, ipv4, ipv6, os, kernel, arch,
		       agent_version, singbox_version, last_seen, created_at, status,
		       config_revision, applied_revision
		FROM machines ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Machine{}
	for rows.Next() {
		var m Machine
		if err := rows.Scan(&m.MachineID, &m.DisplayName, &m.Hostname, &m.IPv4, &m.IPv6,
			&m.OS, &m.Kernel, &m.Arch, &m.AgentVersion, &m.SingboxVersion,
			&m.LastSeen, &m.CreatedAt, &m.Status, &m.ConfigRevision, &m.AppliedRevision); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// BumpConfigRevision 递增某机器的 desired config_revision（影响 sing-box 配置的变更）。
func BumpConfigRevision(db *sql.DB, machineID string) error {
	_, err := db.Exec(`
		UPDATE machines SET config_revision = config_revision + 1 WHERE machine_id = ?`, machineID)
	return err
}

// Delete 删除机器管理关系（开发提示词第五十节：只删 Manager 侧，不卸远端）。
func Delete(db *sql.DB, machineID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM agents WHERE machine_id = ?`,
		`DELETE FROM nodes WHERE machine_id = ?`,
		`DELETE FROM machines WHERE machine_id = ?`,
	} {
		if _, err := tx.Exec(q, machineID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// JSON 序列化机器（供测试/调试）。
func (m *Machine) JSON() string {
	b, _ := json.Marshal(m)
	return string(b)
}
