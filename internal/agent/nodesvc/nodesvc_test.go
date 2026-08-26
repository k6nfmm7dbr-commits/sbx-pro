package nodesvc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/nodes"
)

func newTestService(t *testing.T) *Service {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	sbConf := filepath.Join(dir, "config.json")
	os.MkdirAll(appDir, 0o755)
	// 初始 sing-box 配置（含 route/outbounds，RebuildConfig 需要）。
	os.WriteFile(sbConf, []byte(`{"inbounds":[],"outbounds":[{"type":"direct","tag":"direct"}],"route":{"final":"direct"}}`), 0o600)

	s := &Service{
		Store:   &nodes.Store{AppDir: appDir, SBConf: sbConf},
		Singbox: "/usr/local/bin/sing-box",
		Service: "sing-box",
	}
	s.checkOnly = func(ctx context.Context, cfgPath string) (string, error) { return "", nil }
	s.restartFn = func(ctx context.Context) error { return nil }
	s.healthFn = func() bool { return true }
	return s
}

func TestAddNodeGeneratesCandidate(t *testing.T) {
	s := newTestService(t)
	s.checkOnly = func(ctx context.Context, cfgPath string) (string, error) { return "", nil }
	s.restartFn = func(ctx context.Context) error { return nil }

	id, err := s.AddNode(nodes.Node{
		"type": "trojan",
		"port": 443,
		"sni":  "example.com",
	})
	if err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if id == "" {
		t.Error("id empty")
	}
	// candidate 配置已生成。
	if _, err := os.Stat(s.Store.SBConf + ".candidate"); err != nil {
		t.Errorf("candidate 配置未生成: %v", err)
	}
	// 应用。
	rc, err := s.Apply(context.Background())
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if rc != 0 {
		t.Errorf("Apply rc = %d, want 0", rc)
	}
	// 正式配置已更新，包含新 inbound。
	data, _ := os.ReadFile(s.Store.SBConf)
	if !strings.Contains(string(data), "example.com") {
		t.Error("正式配置未包含新节点 sni")
	}
}

func TestApplyCheckFailure(t *testing.T) {
	s := newTestService(t)
	s.checkOnly = func(ctx context.Context, cfgPath string) (string, error) {
		return "syntax error", errMock
	}
	s.restartFn = func(ctx context.Context) error { return nil }

	_, _ = s.AddNode(nodes.Node{"type": "trojan", "port": 443, "sni": "bad"})
	rc, err := s.Apply(context.Background())
	if err == nil {
		t.Fatal("expected check failure error, got nil")
	}
	if rc != 1 {
		t.Errorf("rc = %d, want 1", rc)
	}
	// 正式配置未被覆盖。
	data, _ := os.ReadFile(s.Store.SBConf)
	if strings.Contains(string(data), "bad") {
		t.Error("正式配置被错误覆盖")
	}
}

func TestApplyRestartFailureRollback(t *testing.T) {
	s := newTestService(t)
	s.checkOnly = func(ctx context.Context, cfgPath string) (string, error) { return "", nil }
	s.restartFn = func(ctx context.Context) error { return errMock }

	_, _ = s.AddNode(nodes.Node{"type": "trojan", "port": 443, "sni": "example.com"})
	rc, err := s.Apply(context.Background())
	if err == nil {
		t.Fatal("expected restart failure error, got nil")
	}
	if rc != 1 {
		t.Errorf("rc = %d, want 1", rc)
	}
	// 回滚后正式配置应恢复（不含新节点）。
	data, _ := os.ReadFile(s.Store.SBConf)
	if strings.Contains(string(data), "example.com") {
		t.Error("回滚后正式配置仍含新节点")
	}
}

var errMock = &mockErr{}

type mockErr struct{}

func (e *mockErr) Error() string { return "mock error" }
