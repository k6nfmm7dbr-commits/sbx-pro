// Package db 定义 Manager 中央数据库 schema 与打开逻辑。
// 复用 internal/database 的 SQLite 封装（modernc 纯 Go，无 cgo）。
package db

import (
	"database/sql"
	"fmt"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/database"
)

// Schema 是 Manager 中央库的表结构（开发提示词第三十七节）。
// 所有迁移放入单事务，任一失败整体回滚。
const Schema = `
CREATE TABLE IF NOT EXISTS machines (
    machine_id       TEXT PRIMARY KEY,
    display_name     TEXT NOT NULL DEFAULT '',
    hostname         TEXT NOT NULL DEFAULT '',
    ipv4             TEXT NOT NULL DEFAULT '',
    ipv6             TEXT NOT NULL DEFAULT '',
    os               TEXT NOT NULL DEFAULT '',
    kernel           TEXT NOT NULL DEFAULT '',
    arch             TEXT NOT NULL DEFAULT '',
    agent_version    TEXT NOT NULL DEFAULT '',
    singbox_version  TEXT NOT NULL DEFAULT '',
    last_seen        INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL DEFAULT 0,
    status           TEXT NOT NULL DEFAULT 'offline',
    config_revision  INTEGER NOT NULL DEFAULT 0,
    applied_revision INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS agents (
    machine_id TEXT PRIMARY KEY,
    secret_pub BLOB NOT NULL,
    salt       TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    token      TEXT PRIMARY KEY,
    expires_at INTEGER NOT NULL,
    used       INTEGER NOT NULL DEFAULT 0,
    machine_id TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS nodes (
    node_uuid     TEXT PRIMARY KEY,
    machine_id    TEXT NOT NULL,
    local_node_id INTEGER NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    protocol      TEXT NOT NULL DEFAULT '',
    port          INTEGER NOT NULL DEFAULT 0,
    status        TEXT NOT NULL DEFAULT 'unknown',
    config        TEXT NOT NULL DEFAULT '{}',
    quota_limit   INTEGER NOT NULL DEFAULT 0,
    quota_period  TEXT NOT NULL DEFAULT '',
    ip_limit      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_nodes_machine ON nodes(machine_id);

CREATE TABLE IF NOT EXISTS node_secrets (
    node_uuid TEXT PRIMARY KEY,
    secret    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS tasks (
    task_id    TEXT PRIMARY KEY,
    machine_id TEXT NOT NULL,
    node_uuid  TEXT NOT NULL DEFAULT '',
    type       TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '{}',
    status     TEXT NOT NULL DEFAULT 'pending',
    result     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL DEFAULT 0,
    sent_at    INTEGER NOT NULL DEFAULT 0,
    done_at    INTEGER NOT NULL DEFAULT 0,
    attempts   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_tasks_machine ON tasks(machine_id);
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status);

CREATE TABLE IF NOT EXISTS traffic_events (
    event_id   INTEGER PRIMARY KEY AUTOINCREMENT,
    machine_id TEXT NOT NULL,
    sequence   INTEGER NOT NULL,
    node_uuid  TEXT NOT NULL DEFAULT '',
    rx_bytes   INTEGER NOT NULL DEFAULT 0,
    tx_bytes   INTEGER NOT NULL DEFAULT 0,
    start_time INTEGER NOT NULL DEFAULT 0,
    end_time   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (machine_id, sequence)
);

CREATE TABLE IF NOT EXISTS traffic_daily (
    day        TEXT NOT NULL,
    machine_id TEXT NOT NULL DEFAULT '',
    node_uuid  TEXT NOT NULL DEFAULT '',
    rx         INTEGER NOT NULL DEFAULT 0,
    tx         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, machine_id, node_uuid)
);

CREATE TABLE IF NOT EXISTS traffic_total (
    machine_id TEXT NOT NULL DEFAULT '',
    node_uuid  TEXT NOT NULL DEFAULT '',
    rx         INTEGER NOT NULL DEFAULT 0,
    tx         INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (machine_id, node_uuid)
);

CREATE TABLE IF NOT EXISTS ip_sessions (
    machine_id TEXT NOT NULL,
    node_uuid  TEXT NOT NULL,
    ip         TEXT NOT NULL,
    proto      TEXT NOT NULL DEFAULT 'tcp',
    last_seen  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_ip_sessions ON ip_sessions(machine_id, node_uuid);

CREATE TABLE IF NOT EXISTS audit_logs (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    time       INTEGER NOT NULL DEFAULT 0,
    action     TEXT NOT NULL DEFAULT '',
    machine_id TEXT NOT NULL DEFAULT '',
    node_uuid  TEXT NOT NULL DEFAULT '',
    result     TEXT NOT NULL DEFAULT '',
    source_ip  TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS settings (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL DEFAULT ''
);
`

// Manager 包装 Manager 中央数据库。
type Manager struct {
	*database.DB
}

// SQL 返回底层 *sql.DB（database.DB 内嵌 *sql.DB，字段名同为 DB）。
func (m *Manager) SQL() *sql.DB { return m.DB.DB }

// Open 打开（必要时创建）Manager 数据库并执行迁移。
// 先跑基础 database.Open（其内部 migrate 建 meta/counter_state 等原 sbx 表，
// 虽 Manager 主体不用，但无害且保留一致性），再跑 Manager 专属 schema。
func Open(path string) (*Manager, error) {
	d, err := database.Open(path)
	if err != nil {
		return nil, err
	}
	m := &Manager{DB: d}
	if err := m.migrate(); err != nil {
		d.Close()
		return nil, err
	}
	return m, nil
}

// migrate 执行 Manager 专属 schema，全部放入单事务。
func (m *Manager) migrate() error {
	tx, err := m.Begin()
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range splitStatements(Schema) {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	return tx.Commit()
}
