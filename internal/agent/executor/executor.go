// Package executor 实现 Agent 的任务执行器（开发提示词第十四节 / 四十一节）。
//
// 关键安全要求：
//   - 只接受白名单任务类型，绝不执行任意 shell command；
//   - 每个 task_id 幂等：重复收到同一 task_id 直接返回此前结果；
//   - 已执行 task_id 持久化到本地 SQLite，重启后依然幂等。
package executor

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// Handler 是单个任务类型的执行函数。
type Handler func(db *sql.DB, payload json.RawMessage) (string, error)

// Executor 是 Agent 任务执行器。
type Executor struct {
	DB       *sql.DB
	handlers map[string]Handler
	mu       sync.Mutex
	running  map[string]bool // 防并发的 in-flight task
}

// New 构造执行器并注册白名单 handler。
func New(db *sql.DB) *Executor {
	e := &Executor{
		DB:       db,
		handlers: make(map[string]Handler),
		running:  make(map[string]bool),
	}
	e.ensureSchema()
	return e
}

// Register 注册一个任务类型的 handler（白名单）。
func (e *Executor) Register(taskType string, h Handler) {
	e.handlers[taskType] = h
}

// IsKnown 报告任务类型是否已注册（白名单检查）。
func (e *Executor) IsKnown(taskType string) bool {
	_, ok := e.handlers[taskType]
	return ok
}

// Handle 处理一条任务消息。幂等：若 task_id 已执行，直接返回历史结果。
func (e *Executor) Handle(env *protocol.Envelope) *protocol.TaskResult {
	taskID := env.ID
	if taskID == "" {
		return &protocol.TaskResult{Status: protocol.TaskFailed, Message: "缺少 task_id"}
	}

	// 幂等：查本地是否已执行。
	if prev, ok := e.loadResult(taskID); ok {
		return prev
	}

	handler, ok := e.handlers[env.Type]
	if !ok {
		return &protocol.TaskResult{
			TaskID: taskID, Status: protocol.TaskFailed,
			Message: "不支持的任务类型: " + env.Type,
		}
	}

	// 防并发重复执行同一 task_id。
	e.mu.Lock()
	if e.running[taskID] {
		e.mu.Unlock()
		return &protocol.TaskResult{TaskID: taskID, Status: protocol.TaskFailed, Message: "任务执行中"}
	}
	e.running[taskID] = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.running, taskID)
		e.mu.Unlock()
	}()

	msg, err := handler(e.DB, env.Payload)
	var res *protocol.TaskResult
	if err != nil {
		res = &protocol.TaskResult{TaskID: taskID, Status: protocol.TaskFailed, Message: err.Error()}
	} else {
		res = &protocol.TaskResult{TaskID: taskID, Status: protocol.TaskSuccess, Message: msg}
	}
	res.CompletedAt = time.Now().Unix()
	e.saveResult(res)
	return res
}

// ensureSchema 创建本地 task 幂等表。
func (e *Executor) ensureSchema() {
	_, _ = e.DB.Exec(`
		CREATE TABLE IF NOT EXISTS task_results (
			task_id TEXT PRIMARY KEY,
			status  TEXT NOT NULL,
			message TEXT NOT NULL DEFAULT '',
			completed_at INTEGER NOT NULL DEFAULT 0
		)`)
}

// loadResult 读取历史结果，返回是否已存在。
func (e *Executor) loadResult(taskID string) (*protocol.TaskResult, bool) {
	var status, message string
	var completedAt int64
	err := e.DB.QueryRow(
		`SELECT status, message, completed_at FROM task_results WHERE task_id = ?`,
		taskID).Scan(&status, &message, &completedAt)
	if err == sql.ErrNoRows {
		return nil, false
	}
	if err != nil {
		return nil, false
	}
	return &protocol.TaskResult{
		TaskID:      taskID,
		Status:      protocol.TaskStatus(status),
		Message:     message,
		CompletedAt: completedAt,
	}, true
}

// saveResult 持久化结果（幂等写入）。
func (e *Executor) saveResult(res *protocol.TaskResult) {
	_, _ = e.DB.Exec(
		`INSERT OR REPLACE INTO task_results (task_id, status, message, completed_at)
		 VALUES (?, ?, ?, ?)`,
		res.TaskID, string(res.Status), res.Message, res.CompletedAt)
}

var _ = fmt.Sprint
