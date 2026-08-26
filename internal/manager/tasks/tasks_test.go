package tasks

import (
	"database/sql"
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
	if _, err := db.Exec(`
		CREATE TABLE tasks (
			task_id TEXT PRIMARY KEY, machine_id TEXT NOT NULL, node_uuid TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL, payload TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL DEFAULT 'pending',
			result TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL DEFAULT 0,
			sent_at INTEGER NOT NULL DEFAULT 0, done_at INTEGER NOT NULL DEFAULT 0, attempts INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func TestCreateRejectsUnknownType(t *testing.T) {
	db := newTestDB(t)
	_, err := Create(db, "m1", "", "rm -rf", map[string]any{})
	if err == nil {
		t.Fatal("expected error for unknown type, got nil")
	}
}

func TestCreateAndTransition(t *testing.T) {
	db := newTestDB(t)
	task, err := Create(db, "m1", "", protocol.MsgRequestStatus, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if task.ID == "" {
		t.Error("task_id empty")
	}
	// pending -> sent
	if err := MarkSent(db, task.ID); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	got, err := Get(db, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "sent" {
		t.Errorf("status = %s, want sent", got.Status)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", got.Attempts)
	}
	// sent -> success
	if err := Complete(db, task.ID, protocol.TaskSuccess, "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, _ = Get(db, task.ID)
	if got.Status != "success" || got.Result != "done" {
		t.Errorf("after complete: status=%s result=%s", got.Status, got.Result)
	}
}

func TestTransitionConditional(t *testing.T) {
	db := newTestDB(t)
	task, _ := Create(db, "m1", "", protocol.MsgRequestStatus, nil)
	// 期望从 pending→sent 但实际是 pending，成功。
	if err := Transition(db, task.ID, "pending", "running"); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	// 再期望从 pending→sent（已不是 pending），应失败。
	if err := Transition(db, task.ID, "pending", "sent"); err == nil {
		t.Error("expected transition fail (wrong from state), got nil")
	}
}

func TestCreateRejectsNonTaskType(t *testing.T) {
	db := newTestDB(t)
	// heartbeat 不是任务类型。
	_, err := Create(db, "m1", "", protocol.MsgHeartbeat, nil)
	if err == nil {
		t.Error("expected error for non-task type")
	}
}
