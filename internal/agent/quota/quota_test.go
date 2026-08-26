package quota

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestState(t *testing.T) *State {
	db, err := sql.Open("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, t.TempDir())
}

func TestSetAndResetQuota(t *testing.T) {
	s := newTestState(t)
	if err := s.SetLimit("2", 1000); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	limits, _ := s.Limits()
	if limits["2"].LimitBytes != 1000 {
		t.Errorf("limit = %d, want 1000", limits["2"].LimitBytes)
	}
	// reset
	if err := s.ResetQuota("2"); err != nil {
		t.Fatalf("ResetQuota: %v", err)
	}
	limits, _ = s.Limits()
	if limits["2"].LimitBytes != 0 {
		t.Errorf("after reset limit = %d, want 0", limits["2"].LimitBytes)
	}
}

func TestMarkExceeded(t *testing.T) {
	s := newTestState(t)
	_ = s.SetLimit("2", 1000)
	s.MarkExceeded("2", true)
	limits, _ := s.Limits()
	if !limits["2"].Exceeded {
		t.Error("Exceeded should be true")
	}
	s.MarkExceeded("2", false)
	limits, _ = s.Limits()
	if limits["2"].Exceeded {
		t.Error("Exceeded should be false")
	}
}

func TestBuildBlockRules(t *testing.T) {
	empty := buildBlockRules(map[int]bool{})
	if contains(empty, "drop") {
		t.Error("空阻断集合不应有 drop 规则")
	}
	blocked := buildBlockRules(map[int]bool{443: true, 8080: true})
	if !contains(blocked, "443") || !contains(blocked, "8080") {
		t.Error("阻断规则应包含端口")
	}
	if !contains(blocked, "drop") {
		t.Error("阻断规则应包含 drop")
	}
}

func TestSameSet(t *testing.T) {
	if !sameSet(map[int]bool{1: true}, map[int]bool{1: true}) {
		t.Error("same set should be equal")
	}
	if sameSet(map[int]bool{1: true}, map[int]bool{2: true}) {
		t.Error("different set should not be equal")
	}
	if sameSet(map[int]bool{1: true}, map[int]bool{1: true, 2: true}) {
		t.Error("different size should not be equal")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
