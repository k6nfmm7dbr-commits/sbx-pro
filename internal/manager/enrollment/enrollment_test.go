package enrollment

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T) *sql.DB {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE enrollment_tokens (
			token TEXT PRIMARY KEY, expires_at INTEGER NOT NULL,
			used INTEGER NOT NULL DEFAULT 0, machine_id TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL DEFAULT 0
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func TestTokenLifecycle(t *testing.T) {
	db := newTestDB(t)

	tok, err := New(db, 15*time.Minute)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(tok) != 64 {
		t.Errorf("token length = %d, want 64 hex chars", len(tok))
	}

	// 未被使用、未过期 → 校验通过，返回空 machine_id。
	mid, err := Consume(db, tok)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if mid != "" {
		t.Errorf("consume machine_id = %q, want empty on first use", mid)
	}

	// 标记已使用并绑定。
	if err := MarkUsed(db, tok, "machine-1"); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	// 二次消费 → ErrUsed。
	if _, err := Consume(db, tok); err != ErrUsed {
		t.Errorf("second Consume = %v, want ErrUsed", err)
	}
	// 二次 MarkUsed → ErrUsed。
	if err := MarkUsed(db, tok, "machine-2"); err != ErrUsed {
		t.Errorf("second MarkUsed = %v, want ErrUsed", err)
	}
}

func TestTokenNotFound(t *testing.T) {
	db := newTestDB(t)
	if _, err := Consume(db, "nonexistent"); err != ErrNotFound {
		t.Errorf("Consume(nonexistent) = %v, want ErrNotFound", err)
	}
}

func TestTokenExpired(t *testing.T) {
	db := newTestDB(t)
	// 用负 TTL 构造立即过期的 token。
	tok, err := New(db, -1*time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := Consume(db, tok); err != ErrExpired {
		t.Errorf("Consume(expired) = %v, want ErrExpired", err)
	}
}

func TestPurgeExpired(t *testing.T) {
	db := newTestDB(t)
	_, err := New(db, -1*time.Second) // 已过期
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = New(db, 15*time.Minute) // 未过期
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	n, err := PurgeExpired(db)
	if err != nil {
		t.Fatalf("PurgeExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("PurgeExpired removed %d, want 1", n)
	}
}

func TestTokenUniqueness(t *testing.T) {
	db := newTestDB(t)
	a, _ := New(db, time.Minute)
	b, _ := New(db, time.Minute)
	if a == b {
		t.Error("two tokens must differ")
	}
}
