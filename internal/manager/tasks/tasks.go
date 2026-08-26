// Package tasks 实现 Manager 的任务系统（开发提示词第十一节）。
//
// 任务状态机：pending → sent → running → success | failed | timeout
//
// Manager 创建任务 → 保存数据库 → 发送 Agent → Agent 执行（幂等）→
// task_result 回传 → Manager 更新状态。
package tasks

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// Task 是 Manager 视图中的任务记录。
type Task struct {
	ID        string `json:"task_id"`
	MachineID string `json:"machine_id"`
	NodeUUID  string `json:"node_uuid"`
	Type      string `json:"type"`
	Payload   string `json:"payload"`
	Status    string `json:"status"`
	Result    string `json:"result"`
	CreatedAt int64  `json:"created_at"`
	SentAt    int64  `json:"sent_at"`
	DoneAt    int64  `json:"done_at"`
	Attempts  int    `json:"attempts"`
}

// Create 创建一条新任务，返回 task_id。type 必须是白名单任务类型。
func Create(db *sql.DB, machineID, nodeUUID, taskType string, payload any) (*Task, error) {
	if !protocol.IsTaskType(taskType) {
		return nil, fmt.Errorf("不支持的任务类型: %q", taskType)
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("生成 task_id 失败: %w", err)
	}
	var payloadJSON string
	if payload != nil {
		b, merr := json.Marshal(payload)
		if merr != nil {
			return nil, merr
		}
		payloadJSON = string(b)
	} else {
		payloadJSON = "{}"
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
		INSERT INTO tasks (task_id, machine_id, node_uuid, type, payload, status, result, created_at, sent_at, done_at, attempts)
		VALUES (?, ?, ?, ?, ?, 'pending', '', ?, 0, 0, 0)`,
		id.String(), machineID, nodeUUID, taskType, payloadJSON, now); err != nil {
		return nil, fmt.Errorf("创建任务失败: %w", err)
	}
	return &Task{
		ID:        id.String(),
		MachineID: machineID,
		NodeUUID:  nodeUUID,
		Type:      taskType,
		Payload:   payloadJSON,
		Status:    "pending",
		CreatedAt: now,
	}, nil
}

// Transition 原子转换任务状态（条件更新，防止并发下状态回退/覆盖）。
func Transition(db *sql.DB, taskID, from, to string) error {
	res, err := db.Exec(
		`UPDATE tasks SET status = ? WHERE task_id = ? AND status = ?`, to, taskID, from)
	if err != nil {
		return fmt.Errorf("更新任务状态失败: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("任务 %s 状态转换失败: 期望 %s", taskID, from)
	}
	return nil
}

// MarkSent 任务标记为已发送（sent），并递增 attempts。
func MarkSent(db *sql.DB, taskID string) error {
	_, err := db.Exec(`
		UPDATE tasks SET status = 'sent', sent_at = ?, attempts = attempts + 1
		WHERE task_id = ? AND status = 'pending'`, time.Now().Unix(), taskID)
	return err
}

// Complete 完成任务：写入最终状态与结果。
func Complete(db *sql.DB, taskID string, status protocol.TaskStatus, result string) error {
	_, err := db.Exec(`
		UPDATE tasks SET status = ?, result = ?, done_at = ?
		WHERE task_id = ? AND status IN ('pending', 'sent', 'running')`,
		string(status), result, time.Now().Unix(), taskID)
	return err
}

// Get 查询单条任务。
func Get(db *sql.DB, taskID string) (*Task, error) {
	t := &Task{}
	var sentAt, doneAt sql.NullInt64
	err := db.QueryRow(`
		SELECT task_id, machine_id, node_uuid, type, payload, status, result, created_at, sent_at, done_at, attempts
		FROM tasks WHERE task_id = ?`, taskID).Scan(
		&t.ID, &t.MachineID, &t.NodeUUID, &t.Type, &t.Payload, &t.Status, &t.Result,
		&t.CreatedAt, &sentAt, &doneAt, &t.Attempts)
	if err != nil {
		return nil, err
	}
	if sentAt.Valid {
		t.SentAt = sentAt.Int64
	}
	if doneAt.Valid {
		t.DoneAt = doneAt.Int64
	}
	return t, nil
}

// List 按状态/机器查询任务（可选过滤）。
func List(db *sql.DB, statusFilter, machineFilter string) ([]Task, error) {
	q := `SELECT task_id, machine_id, node_uuid, type, payload, status, result, created_at, sent_at, done_at, attempts FROM tasks`
	var args []any
	var conds []string
	if statusFilter != "" {
		conds = append(conds, "status = ?")
		args = append(args, statusFilter)
	}
	if machineFilter != "" {
		conds = append(conds, "machine_id = ?")
		args = append(args, machineFilter)
	}
	if len(conds) > 0 {
		q += " WHERE " + joinAnd(conds)
	}
	q += " ORDER BY created_at DESC LIMIT 200"
	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var t Task
		var sentAt, doneAt sql.NullInt64
		if err := rows.Scan(&t.ID, &t.MachineID, &t.NodeUUID, &t.Type, &t.Payload,
			&t.Status, &t.Result, &t.CreatedAt, &sentAt, &doneAt, &t.Attempts); err != nil {
			return nil, err
		}
		if sentAt.Valid {
			t.SentAt = sentAt.Int64
		}
		if doneAt.Valid {
			t.DoneAt = doneAt.Int64
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func joinAnd(conds []string) string {
	out := ""
	for i, c := range conds {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out
}
