package traffic

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
	for _, stmt := range []string{
		`CREATE TABLE traffic_events (
			event_id INTEGER PRIMARY KEY AUTOINCREMENT, machine_id TEXT NOT NULL,
			sequence INTEGER NOT NULL, node_uuid TEXT NOT NULL DEFAULT '',
			rx_bytes INTEGER NOT NULL DEFAULT 0, tx_bytes INTEGER NOT NULL DEFAULT 0,
			start_time INTEGER NOT NULL DEFAULT 0, end_time INTEGER NOT NULL DEFAULT 0,
			UNIQUE (machine_id, sequence))`,
		`CREATE TABLE traffic_total (
			machine_id TEXT NOT NULL DEFAULT '', node_uuid TEXT NOT NULL DEFAULT '',
			rx INTEGER NOT NULL DEFAULT 0, tx INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (machine_id, node_uuid))`,
		`CREATE TABLE traffic_daily (
			day TEXT NOT NULL, machine_id TEXT NOT NULL DEFAULT '', node_uuid TEXT NOT NULL DEFAULT '',
			rx INTEGER NOT NULL DEFAULT 0, tx INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, machine_id, node_uuid))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func TestIngestDeltaBasic(t *testing.T) {
	db := newTestDB(t)
	d := protocol.TrafficDelta{MachineID: "m1", Sequence: 1, NodeUUID: "n1", RxBytes: 100, TxBytes: 50}
	accepted, err := IngestDelta(db, d)
	if err != nil {
		t.Fatalf("IngestDelta: %v", err)
	}
	if !accepted {
		t.Error("first ingest should be accepted")
	}
	// 汇总正确。
	totals, _ := TotalsByMachine(db, "m1")
	if len(totals) != 1 || totals[0].Rx != 100 || totals[0].Tx != 50 {
		t.Errorf("totals = %+v, want rx=100 tx=50", totals)
	}
}

func TestIngestDeltaDuplicate(t *testing.T) {
	db := newTestDB(t)
	d := protocol.TrafficDelta{MachineID: "m1", Sequence: 1, NodeUUID: "n1", RxBytes: 100, TxBytes: 50}
	_, _ = IngestDelta(db, d)
	// 重复 sequence → 忽略，不重复计入。
	accepted, err := IngestDelta(db, d)
	if err != nil {
		t.Fatalf("duplicate IngestDelta should not error: %v", err)
	}
	if accepted {
		t.Error("duplicate should not be accepted")
	}
	totals, _ := TotalsByMachine(db, "m1")
	if totals[0].Rx != 100 {
		t.Errorf("after duplicate, rx = %d, want 100 (未重复计入)", totals[0].Rx)
	}
}

func TestIngestDeltaMissingMachine(t *testing.T) {
	db := newTestDB(t)
	d := protocol.TrafficDelta{Sequence: 1, RxBytes: 100}
	_, err := IngestDelta(db, d)
	if err == nil {
		t.Error("expected error for missing machine_id")
	}
}

func TestIngestDeltaAccumulate(t *testing.T) {
	db := newTestDB(t)
	_, _ = IngestDelta(db, protocol.TrafficDelta{MachineID: "m1", Sequence: 1, NodeUUID: "n1", RxBytes: 100, TxBytes: 50})
	_, _ = IngestDelta(db, protocol.TrafficDelta{MachineID: "m1", Sequence: 2, NodeUUID: "n1", RxBytes: 50, TxBytes: 25})
	totals, _ := TotalsByMachine(db, "m1")
	if totals[0].Rx != 150 || totals[0].Tx != 75 {
		t.Errorf("accumulated rx=%d tx=%d, want 150/75", totals[0].Rx, totals[0].Tx)
	}
}
