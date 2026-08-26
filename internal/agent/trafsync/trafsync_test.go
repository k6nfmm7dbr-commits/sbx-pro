package trafsync

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/config"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/database"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// fakeSender 记录发送的 delta，用于断言 sequence 唯一性与 delta 值。
type fakeSender struct {
	mu    sync.Mutex
	deltas []protocol.TrafficDelta
}

func (f *fakeSender) SendTrafficDelta(td protocol.TrafficDelta) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deltas = append(f.deltas, td)
	return nil
}

func (f *fakeSender) snapshot() []protocol.TrafficDelta {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]protocol.TrafficDelta, len(f.deltas))
	copy(out, f.deltas)
	return out
}

func newTestSync(t *testing.T, machine string) (*Sync, *fakeSender, *database.DB) {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "traffic.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{TZ: "Asia/Shanghai"}
	snd := &fakeSender{}
	s := New(cfg, db, machine, nil, nil)
	return s, snd, db
}

// seedTotals 直接写 totals 表（模拟 Collector 采集后的累计值）。
func seedTotals(t *testing.T, db *database.DB, scope string, rx, tx int64) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO totals (scope, rx, tx, rx_pkts, tx_pkts) VALUES (?, ?, ?, 0, 0)
		ON CONFLICT(scope) DO UPDATE SET rx = excluded.rx, tx = excluded.tx`,
		scope, rx, tx)
	if err != nil {
		t.Fatalf("seed totals: %v", err)
	}
}

// TestComputeDeltasUniqueSequence 验证：同一轮多个 scope 各自分配唯一递增 sequence
//（修复此前的「同一轮共用一个 sequence 导致 UNIQUE 冲突丢流量」bug）。
func TestComputeDeltasUniqueSequence(t *testing.T) {
	s, snd, db := newTestSync(t, "m1")
	defer db.Close()
	s.Sender = snd

	seedTotals(t, db, "system", 100, 200)
	seedTotals(t, db, "node:2", 50, 60)

	if err := s.computeDeltas(); err != nil {
		t.Fatalf("computeDeltas: %v", err)
	}
	if err := s.flushPending(); err != nil {
		t.Fatalf("flushPending: %v", err)
	}
	got := snd.snapshot()
	if len(got) != 2 {
		t.Fatalf("期望 2 条 delta，得到 %d", len(got))
	}
	seen := map[int64]bool{}
	for _, d := range got {
		if seen[d.Sequence] {
			t.Errorf("sequence 重复: %d", d.Sequence)
		}
		seen[d.Sequence] = true
	}
	// 序列必须从 1 开始递增。
	if got[0].Sequence != 1 || got[1].Sequence != 2 {
		t.Errorf("sequence 应为 1,2 递增，得到 %d,%d", got[0].Sequence, got[1].Sequence)
	}
}

// TestBaselinePersistsAcrossRestart 验证：baseline 持久化，重启后不重复上报历史累计。
func TestBaselinePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "traffic.db")

	db, err := database.Open(path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	cfg := &config.Config{TZ: "Asia/Shanghai"}
	snd := &fakeSender{}
	s := New(cfg, db, "m1", nil, snd)
	seedTotals(t, db, "system", 100, 0)
	if err := s.computeDeltas(); err != nil {
		t.Fatalf("computeDeltas: %v", err)
	}
	if err := s.flushPending(); err != nil {
		t.Fatalf("flushPending: %v", err)
	}
	// ACK 删除 pending（模拟 Manager 确认入库）。
	s.OnAck(protocol.TrafficAck{MachineID: "m1", Sequence: 1, Accepted: true})
	db.Close()

	// 模拟重启：重新打开 DB + 重新 New Sync。
	db2, err := database.Open(path)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()
	s2 := New(cfg, db2, "m1", nil, snd)
	if err := s2.computeDeltas(); err != nil {
		t.Fatalf("computeDeltas after restart: %v", err)
	}
	if err := s2.flushPending(); err != nil {
		t.Fatalf("flushPending after restart: %v", err)
	}
	// 重启后基线应恢复，没有新增量 → 不重复发送。
	if got := snd.snapshot(); len(got) != 1 {
		t.Fatalf("重启后不应重复上报，期望仍 1 条 delta，得到 %d", len(got))
	}
	var count int
	if err := db2.QueryRow("SELECT COUNT(*) FROM traffic_pending").Scan(&count); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if count != 0 {
		t.Errorf("ACK + 重启后 pending 应为 0，得到 %d", count)
	}
}

// TestOnAckClearsPending 验证：ACK 后清理 pending，未确认的保留。
func TestOnAckClearsPending(t *testing.T) {
	s, snd, db := newTestSync(t, "m1")
	defer db.Close()
	s.Sender = snd

	seedTotals(t, db, "system", 100, 0)
	seedTotals(t, db, "node:2", 50, 0)
	if err := s.computeDeltas(); err != nil {
		t.Fatalf("computeDeltas: %v", err)
	}

	// ACK sequence=1 → 清理 seq<=1。
	s.OnAck(protocol.TrafficAck{MachineID: "m1", Sequence: 1, Accepted: true})

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM traffic_pending").Scan(&count); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if count != 1 {
		t.Fatalf("ACK 后应剩 1 条 pending，得到 %d", count)
	}

	// 错误 machine_id 的 ACK 不应清理。
	s.OnAck(protocol.TrafficAck{MachineID: "other", Sequence: 99, Accepted: true})
	if err := db.QueryRow("SELECT COUNT(*) FROM traffic_pending").Scan(&count); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if count != 1 {
		t.Fatalf("错误 machine_id 的 ACK 不应清理，得到 %d", count)
	}
}

// TestCounterResetNoNegativeDelta 验证：计数器归零不产生负 delta（防御性置 0）。
func TestCounterResetNoNegativeDelta(t *testing.T) {
	s, snd, db := newTestSync(t, "m1")
	defer db.Close()
	s.Sender = snd

	seedTotals(t, db, "system", 100, 0)
	if err := s.computeDeltas(); err != nil {
		t.Fatalf("computeDeltas: %v", err)
	}
	// 模拟 reset：totals 归零（如 nft reset）。
	seedTotals(t, db, "system", 0, 0)
	if err := s.computeDeltas(); err != nil {
		t.Fatalf("computeDeltas after reset: %v", err)
	}
	if err := s.flushPending(); err != nil {
		t.Fatalf("flushPending: %v", err)
	}
	for _, d := range snd.snapshot() {
		if d.RxBytes < 0 || d.TxBytes < 0 {
			t.Errorf("出现负 delta: %+v", d)
		}
	}
}
