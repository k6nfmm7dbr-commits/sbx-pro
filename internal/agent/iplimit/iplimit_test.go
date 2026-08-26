package iplimit

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
	return New(db)
}

func TestSetLimit(t *testing.T) {
	s := newTestState(t)
	if err := s.SetLimit("2", 3); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}
	limits, _ := s.Limits()
	if limits["2"] != 3 {
		t.Errorf("limit = %d, want 3", limits["2"])
	}
	if err := s.SetLimit("2", 0); err != nil {
		t.Fatalf("SetLimit reset: %v", err)
	}
	limits, _ = s.Limits()
	if limits["2"] != 0 {
		t.Errorf("after reset limit = %d, want 0", limits["2"])
	}
}

func TestBuildIPBlockRules(t *testing.T) {
	empty := buildIPBlockRules(map[string]bool{})
	if contains(empty, "elements = {") {
		t.Error("空集合不应有 elements")
	}
	blocked := buildIPBlockRules(map[string]bool{"1.1.1.1": true, "8.8.8.8": true})
	if !contains(blocked, "1.1.1.1") || !contains(blocked, "8.8.8.8") {
		t.Error("应包含被阻断 IP")
	}
	if !contains(blocked, "saddr @blocked drop") {
		t.Error("应包含 saddr drop 规则")
	}
}

func TestSortedIPs(t *testing.T) {
	set := map[string]struct{}{"8.8.8.8": {}, "1.1.1.1": {}, "3.3.3.3": {}}
	got := sortedIPs(set)
	if got[0] != "1.1.1.1" || got[1] != "3.3.3.3" || got[2] != "8.8.8.8" {
		t.Errorf("sorted = %v, want [1.1.1.1 3.3.3.3 8.8.8.8]", got)
	}
}

func TestEnforcerCheck(t *testing.T) {
	s := newTestState(t)
	_ = s.SetLimit("2", 1) // 只允许 1 个 IP
	e := &Enforcer{
		State: s,
		PortOf: func(nodeID string) (int, error) { return 443, nil },
		ActiveIPs: func(port int) (map[string]struct{}, error) {
			return map[string]struct{}{
				"1.1.1.1": {},
				"2.2.2.2": {},
			}, nil
		},
	}
	// 两个活跃 IP，limit=1，应阻断第二个（按排序后的第 2 个）。
	if err := e.Check(); err != nil {
		// 真机上会因 nft 不存在而失败；测试环境跳过 error 检查主体逻辑。
		t.Logf("Check error (expected on non-nft env): %v", err)
	}
	if e.lastBlocked == nil || len(e.lastBlocked) != 1 {
		t.Errorf("lastBlocked = %v, want 1 个被阻断 IP", e.lastBlocked)
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
