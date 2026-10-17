package iplimit

import (
	"database/sql"
	"errors"
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
	var applied map[string]bool
	e := &Enforcer{
		State:  s,
		PortOf: func(nodeID string) (int, error) { return 443, nil },
		ActiveIPs: func(port int) (map[string]struct{}, error) {
			return map[string]struct{}{
				"1.1.1.1": {},
				"2.2.2.2": {},
			}, nil
		},
		// 注入阻断器：不依赖 nft，测试的是「阻断决策」而不是环境。
		ApplyBlock: func(blocked map[string]bool) error {
			applied = map[string]bool{}
			for ip := range blocked {
				applied[ip] = true
			}
			return nil
		},
	}
	// 两个活跃 IP，limit=1，应阻断排序后的第二个（2.2.2.2）。
	if err := e.Check(); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(applied) != 1 || !applied["2.2.2.2"] {
		t.Errorf("applied = %v, want 只阻断 2.2.2.2", applied)
	}
	if e.LastError() != nil {
		t.Errorf("LastError 应为 nil: %v", e.LastError())
	}

	// 集合不变时不应重复应用（幂等/降噪）。
	applied = nil
	if err := e.Check(); err != nil {
		t.Fatalf("Check 2: %v", err)
	}
	if applied != nil {
		t.Error("集合未变化时不应重新应用")
	}

	// 应用失败必须暴露给调用方与 LastError，且不更新 lastBlocked。
	// （换一个新 IP 使阻断集合变化，触发重新应用。）
	e.ActiveIPs = func(port int) (map[string]struct{}, error) {
		return map[string]struct{}{"1.1.1.1": {}, "3.3.3.3": {}}, nil
	}
	e.ApplyBlock = func(map[string]bool) error { return errors.New("nft boom") }
	if err := e.Check(); err == nil {
		t.Fatal("应用失败应返回错误")
	}
	if e.LastError() == nil || e.LastError().Error() != "nft boom" {
		t.Errorf("LastError = %v, want nft boom", e.LastError())
	}
}

func TestEnforcerLimitLifted(t *testing.T) {
	s := newTestState(t)
	_ = s.SetLimit("2", 1)
	var applied []map[string]bool
	e := &Enforcer{
		State:  s,
		PortOf: func(nodeID string) (int, error) { return 443, nil },
		ActiveIPs: func(port int) (map[string]struct{}, error) {
			return map[string]struct{}{"1.1.1.1": {}, "2.2.2.2": {}}, nil
		},
		ApplyBlock: func(blocked map[string]bool) error {
			applied = append(applied, cloneSet(blocked))
			return nil
		},
	}
	if err := e.Check(); err != nil {
		t.Fatal(err)
	}
	// 提高额度后：阻断集合应清空（放行），并重新应用一次空集合。
	if err := s.SetLimit("2", 5); err != nil {
		t.Fatal(err)
	}
	if err := e.Check(); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 2 || len(applied[1]) != 0 {
		t.Errorf("提高额度后应应用空集合，applied = %v", applied)
	}
}

func cloneSet(m map[string]bool) map[string]bool {
	out := map[string]bool{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
