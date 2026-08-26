package tasks

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/gateway"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// Dispatcher 把任务创建、下发、结果回收、超时判定串起来。
type Dispatcher struct {
	DB      *sql.DB
	Gateway *gateway.Gateway
	// Timeout 是任务超时阈值（默认 60s）。
	Timeout time.Duration

	// OnTaskComplete 是任务最终完成回调（success/failed），由 api 层注入，
	// 用于驱动节点状态机 / revision 同步等 desired-actual 收敛逻辑。
	OnTaskComplete func(task *Task, status protocol.TaskStatus, message string)
}

// NewDispatcher 构造并注入 OnTaskResult 回调。
func NewDispatcher(db *sql.DB, gw *gateway.Gateway, timeout time.Duration) *Dispatcher {
	d := &Dispatcher{DB: db, Gateway: gw, Timeout: timeout}
	if d.Timeout <= 0 {
		d.Timeout = 60 * time.Second
	}
	gw.OnTaskResult = d.handleTaskResult
	return d
}

// Dispatch 创建任务并立即下发给目标机器（若在线）。
// 返回 task_id；机器离线时任务保持 pending，等待机器重连后补发。
func (d *Dispatcher) Dispatch(machineID, nodeUUID, taskType string, payload any) (string, error) {
	task, err := Create(d.DB, machineID, nodeUUID, taskType, payload)
	if err != nil {
		return "", err
	}
	if err := d.sendToAgent(task); err != nil {
		slog.Warn("下发任务失败(机器可能离线)", "task_id", task.ID, "err", err)
		// 任务保持 pending，不失败——由补发循环处理。
	}
	return task.ID, nil
}

// sendToAgent 把任务发送给目标机器并用 MarkSent 标记。
func (d *Dispatcher) sendToAgent(task *Task) error {
	var payload json.RawMessage
	if task.Payload != "" {
		payload = json.RawMessage(task.Payload)
	} else {
		payload = json.RawMessage("{}")
	}
	online, err := d.Gateway.SendToMachine(task.MachineID, task.Type, task.ID, payload)
	if err != nil {
		return err
	}
	if !online {
		return &AgentOfflineError{MachineID: task.MachineID}
	}
	_ = MarkSent(d.DB, task.ID)
	return nil
}

// AgentOfflineError 表示目标机器当前不在线。
type AgentOfflineError struct{ MachineID string }

func (e *AgentOfflineError) Error() string {
	return "机器离线: " + e.MachineID
}

// handleTaskResult 处理 Agent 回传的 task_result。
func (d *Dispatcher) handleTaskResult(tr *protocol.TaskResult) {
	if tr.TaskID == "" {
		return
	}
	if tr.Status == protocol.TaskSuccess {
		_ = Complete(d.DB, tr.TaskID, protocol.TaskSuccess, tr.Message)
	} else {
		_ = Complete(d.DB, tr.TaskID, protocol.TaskFailed, tr.Message)
	}
	slog.Info("任务完成", "task_id", tr.TaskID, "status", tr.Status)

	// 驱动 desired-actual 收敛（节点状态机等）。
	if d.OnTaskComplete != nil {
		if task, err := Get(d.DB, tr.TaskID); err == nil {
			d.OnTaskComplete(task, tr.Status, tr.Message)
		}
	}
}

// RunTimeoutSweeper 周期把超时未完成的任务标记为 timeout。
func (d *Dispatcher) RunTimeoutSweeper(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				d.sweepTimeout()
			}
		}
	}()
}

func (d *Dispatcher) sweepTimeout() {
	sentCutoff := time.Now().Add(-d.Timeout).Unix()
	// 已发送但超时未完成 → timeout；pending 很久的（机器一直离线）也 timeout。
	pendingCutoff := time.Now().Add(-2 * d.Timeout).Unix()

	// 先查出所有超时任务，逐个标记并触发收敛回调。
	rows, err := d.DB.Query(`
		SELECT task_id FROM tasks
		WHERE (status IN ('sent','running') AND sent_at < ?)
		   OR (status = 'pending' AND created_at < ?)`,
		sentCutoff, pendingCutoff)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	rows.Close()

	for _, id := range ids {
		res, _ := d.DB.Exec(`
			UPDATE tasks SET status = 'timeout', done_at = ?
			WHERE task_id = ? AND status IN ('pending','sent','running')`,
			time.Now().Unix(), id)
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if d.OnTaskComplete != nil {
			if task, err := Get(d.DB, id); err == nil {
				d.OnTaskComplete(task, protocol.TaskTimeout, "任务超时")
			}
		}
	}
}
