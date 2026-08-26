package executor

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"

	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func newExecutor(t *testing.T) *Executor {
	db := newTestDB(t)
	e := New(db)
	e.Register("request_status", func(db *sql.DB, payload json.RawMessage) (string, error) {
		return "ok", nil
	})
	e.Register("failing_task", func(db *sql.DB, payload json.RawMessage) (string, error) {
		return "", &testError{}
	})
	return e
}

type testError struct{}

func (e *testError) Error() string { return "模拟执行失败" }

func makeEnv(typ, id string) *protocol.Envelope {
	e, _ := protocol.New(typ, id, map[string]any{})
	return e
}

func TestExecutorSuccess(t *testing.T) {
	e := newExecutor(t)
	res := e.Handle(makeEnv("request_status", "task-1"))
	if res.Status != protocol.TaskSuccess {
		t.Errorf("status = %s, want success", res.Status)
	}
}

func TestExecutorIdempotency(t *testing.T) {
	e := newExecutor(t)
	first := e.Handle(makeEnv("request_status", "task-2"))
	if first.Status != protocol.TaskSuccess {
		t.Fatalf("first = %s, want success", first.Status)
	}
	// 第二次执行同一 task_id，应返回此前结果而非重新执行。
	second := e.Handle(makeEnv("request_status", "task-2"))
	if second.Status != protocol.TaskSuccess {
		t.Errorf("second = %s, want success (idempotent)", second.Status)
	}
}

func TestExecutorUnknownType(t *testing.T) {
	e := newExecutor(t)
	if e.IsKnown("request_status") != true {
		t.Error("request_status should be known")
	}
	if e.IsKnown("rm -rf") {
		t.Error("arbitrary type must not be known")
	}
	res := e.Handle(makeEnv("delete_everything", "task-3"))
	if res.Status != protocol.TaskFailed {
		t.Errorf("unknown type status = %s, want failed", res.Status)
	}
}

func TestExecutorFailure(t *testing.T) {
	e := newExecutor(t)
	res := e.Handle(makeEnv("failing_task", "task-4"))
	if res.Status != protocol.TaskFailed {
		t.Errorf("status = %s, want failed", res.Status)
	}
	if res.Message != "模拟执行失败" {
		t.Errorf("message = %q, want 模拟执行失败", res.Message)
	}
}

func TestExecutorMissingTaskID(t *testing.T) {
	e := newExecutor(t)
	res := e.Handle(makeEnv("request_status", ""))
	if res.Status != protocol.TaskFailed {
		t.Errorf("status = %s, want failed (missing task_id)", res.Status)
	}
}
