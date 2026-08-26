// Package nodesvc 实现 Agent 的节点服务（开发提示词第十二~十八节）。
//
// 复用原 sbx internal/nodes 的核心函数（BuildInbound/RebuildConfig/
// WriteCandidate/WriteNodesCandidate/LinkFor 等），并新增：
//   - 节点 add/remove/edit 的服务化封装（替代 CLI 参数解析，返回强类型结果）；
//   - candidate → sing-box check → 原子替换 → restart → health check 安全链；
//   - 失败回滚（绝不覆盖有效旧配置，绝不谎报成功）。
package nodesvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/fsx"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/nodes"
)

// Service 是 Agent 节点服务。
type Service struct {
	Store     *nodes.Store
	Singbox   string // sing-box 二进制路径
	Service   string // systemd 服务名（默认 sing-box）
	checkOnly func(ctx context.Context, cfgPath string) (string, error) // 测试注入
	restartFn func(ctx context.Context) error                            // 测试注入
	healthFn  func() bool                                                // 测试注入
}

// New 构造节点服务。singbox 默认 /usr/local/bin/sing-box。
// appDir 是节点数据目录（nodes.json/state.json 所在，默认 /etc/sbx，
// 与 sing-box 配套；可通过 SBX_DIR 环境变量覆盖）。
func New(appDir string) *Service {
	if appDir == "" {
		appDir = DefaultAppDir()
	}
	s := &Service{
		Store:   &nodes.Store{AppDir: appDir, SBConf: "/etc/sing-box/config.json"},
		Singbox: "/usr/local/bin/sing-box",
		Service: "sing-box",
	}
	s.checkOnly = s.check
	s.restartFn = s.restart
	s.healthFn = s.isRunning
	return s
}

// DefaultAppDir 返回节点数据目录（SBX_DIR 优先，默认 /etc/sbx）。
func DefaultAppDir() string {
	if v := os.Getenv("SBX_DIR"); v != "" {
		return v
	}
	return "/etc/sbx"
}

// check 用 sing-box check 校验配置。
func (s *Service) check(ctx context.Context, cfgPath string) (string, error) {
	cmd := exec.CommandContext(ctx, s.Singbox, "check", "-c", cfgPath)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// restart 重启 sing-box systemd 服务。
func (s *Service) restart(ctx context.Context) error {
	return exec.CommandContext(ctx, "systemctl", "restart", s.Service).Run()
}

// isRunning 检查 sing-box 是否运行正常。
func (s *Service) isRunning() bool {
	return exec.Command("systemctl", "is-active", "--quiet", s.Service).Run() == nil
}

// ---- 节点查询（只读）----

// List 返回本地节点列表（宽松读取，仅展示）。
func (s *Service) List() []nodes.Node {
	return nodes.LoadToolNodes(s.Store.NodesPath())
}

// Info 返回单个节点信息。
func (s *Service) Info(id string) (nodes.Node, error) {
	for _, n := range s.List() {
		if nodes.IDString(n) == id {
			return n, nil
		}
	}
	return nil, fmt.Errorf("未找到节点 id=%s", id)
}

// Links 返回单个节点分享链接（复用原 LinkFor）。
func (s *Service) Links(id string) (string, error) {
	n, err := s.Info(id)
	if err != nil {
		return "", err
	}
	if nodes.Str(n, "type") == "snell" {
		// Snell 双格式：URI + Surge。
		uri := s.Store.LinkFor(n, "", "")
		surge := s.Store.SnellSurgeFor(n, "", "")
		if surge != "" {
			return uri + "\n" + surge, nil
		}
		return uri, nil
	}
	return s.Store.LinkFor(n, "", ""), nil
}

// ---- 节点变更（严格读取 + candidate）----

// AddNode 新增节点，生成 candidate 配置，返回节点 id。
// node 已含 type/port/name 及协议字段（uuid/password/sni/...）。
func (s *Service) AddNode(node nodes.Node) (string, error) {
	list, err := nodes.LoadToolNodesStrict(s.Store.NodesPath())
	if err != nil {
		return "", err
	}
	nid, err := nodes.NextID(s.Store, list)
	if err != nil {
		return "", err
	}
	node["id"] = json.Number(strconv.FormatInt(nid, 10))
	if nodes.Str(node, "name") == "" {
		node["name"] = fmt.Sprintf("%s-%d", nodes.Str(node, "type"), nid)
	}
	// trojan / anytls 需要自签 TLS 证书（复用原 sbx cmdAdd 的行为）。
	if t := nodes.Str(node, "type"); t == "trojan" || t == "anytls" {
		node["cert"] = s.Store.CertDir() + "/cert.pem"
		node["key"] = s.Store.CertDir() + "/key.pem"
	}
	list = append(list, node)
	if _, _, err := s.writeCandidates(list); err != nil {
		return "", err
	}
	return strconv.FormatInt(nid, 10), nil
}

// RemoveNode 删除节点并生成 candidate。
func (s *Service) RemoveNode(id string) error {
	list, err := nodes.LoadToolNodesStrict(s.Store.NodesPath())
	if err != nil {
		return err
	}
	idx := -1
	for i, n := range list {
		if nodes.IDString(n) == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("未找到节点 id=%s", id)
	}
	keep := append(append([]nodes.Node{}, list[:idx]...), list[idx+1:]...)
	_, _, err = s.writeCandidates(keep)
	return err
}

// EditNode 修改节点端口/SNI/名称等，生成 candidate。
func (s *Service) EditNode(id string, changes map[string]string) error {
	list, err := nodes.LoadToolNodesStrict(s.Store.NodesPath())
	if err != nil {
		return err
	}
	idx := -1
	for i, n := range list {
		if nodes.IDString(n) == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("未找到节点 id=%s", id)
	}
	target := list[idx]

	for key, val := range changes {
		switch key {
		case "name", "sni", "psk", "method":
			target[key] = val
		case "port":
			p, perr := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			if perr != nil || p < 1 || p > 65535 {
				return fmt.Errorf("端口需在 1-65535: %q", val)
			}
			target["port"] = json.Number(strconv.FormatInt(p, 10))
		default:
			return fmt.Errorf("不支持的修改字段: %q", key)
		}
	}
	_, _, err = s.writeCandidates(list)
	return err
}

// writeCandidates 校验节点列表并写出 candidate 配置 + candidate nodes。
func (s *Service) writeCandidates(list []nodes.Node) (string, string, error) {
	if err := validateNodes(list); err != nil {
		return "", "", err
	}
	cfg, err := nodes.RebuildConfig(s.Store, list)
	if err != nil {
		return "", "", err
	}
	cand, err := nodes.WriteCandidate(s.Store, cfg)
	if err != nil {
		return "", "", err
	}
	nodesCand, err := nodes.WriteNodesCandidate(s.Store, list)
	if err != nil {
		return "", "", err
	}
	return cand, nodesCand, nil
}

// validateNodes 复用原 sbx 的语义校验（id/port 唯一、type 合法）。因原函数未导出，这里重实现。
func validateNodes(list []nodes.Node) error {
	seenID := map[int64]bool{}
	seenPort := map[int64]bool{}
	for _, n := range list {
		id, ok := strictInt64(n["id"])
		if !ok || id <= 0 {
			return fmt.Errorf("节点 id 非法: %v", n["id"])
		}
		if seenID[id] {
			return fmt.Errorf("节点 id 重复: %d", id)
		}
		seenID[id] = true
		port, ok := strictInt64(n["port"])
		if !ok || port < 1 || port > 65535 {
			return fmt.Errorf("节点 %d 端口非法: %v", id, n["port"])
		}
		if seenPort[port] {
			return fmt.Errorf("节点端口重复: %d", port)
		}
		seenPort[port] = true
		if !nodes.ValidType(nodes.Str(n, "type")) {
			return fmt.Errorf("节点 %d 类型不受支持: %q", id, nodes.Str(n, "type"))
		}
	}
	return nil
}

// strictInt64 严格整数校验。
func strictInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case json.Number:
		n, err := t.Int64()
		if err != nil {
			return 0, false
		}
		return n, true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		if t == float64(int64(t)) {
			return int64(t), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// ---- 应用安全链（candidate → check → 原子替换 → restart → rollback）----

// Apply 把 candidate 配置安全应用到 sing-box。返回三态：
// 0=完全成功，2=节点成功但服务重启失败（partial），1=失败已回滚。
func (s *Service) Apply(ctx context.Context) (int, error) {
	candCfg := s.Store.SBConf + ".candidate"
	candNodes := s.Store.NodesPath() + ".candidate"

	// 1. candidate 必须存在。
	if _, err := os.Stat(candCfg); err != nil {
		return 1, fmt.Errorf("候选配置不存在: %s", candCfg)
	}

	// 2. sing-box check。
	ctxT, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := s.checkOnly(ctxT, candCfg); err != nil {
		return 1, fmt.Errorf("sing-box 配置校验失败: %s", strings.TrimSpace(out))
	}

	// 3. 备份正式配置（fail-closed：备份失败即中止）。
	if _, err := os.Stat(s.Store.SBConf); err == nil {
		if err := copyFile(s.Store.SBConf, s.Store.SBConf+".bak"); err != nil {
			return 1, fmt.Errorf("配置备份失败: %w", err)
		}
	}
	hasNodesBackup := false
	if _, err := os.Stat(s.Store.NodesPath()); err == nil {
		if err := copyFile(s.Store.NodesPath(), s.Store.NodesPath()+".bak"); err != nil {
			return 1, fmt.Errorf("节点数据备份失败: %w", err)
		}
		hasNodesBackup = true
	}

	// 4. 原子替换配置 + 节点数据。
	if err := fsx.RenameAtomic(candCfg, s.Store.SBConf); err != nil {
		s.rollback(hasNodesBackup)
		return 1, fmt.Errorf("配置替换失败: %w", err)
	}
	if _, err := os.Stat(candNodes); err == nil {
		if err := fsx.RenameAtomic(candNodes, s.Store.NodesPath()); err != nil {
			s.rollback(hasNodesBackup)
			return 1, fmt.Errorf("节点数据替换失败: %w", err)
		}
	}

	// 5. restart。
	if err := s.restartFn(ctx); err != nil {
		// 重启失败 → 回滚。
		s.rollback(hasNodesBackup)
		// 尝试用旧配置恢复服务。
		_ = s.restartFn(ctx)
		return 1, fmt.Errorf("sing-box 重启失败，已回滚: %w", err)
	}

	// 6. health check。
	if !s.healthFn() {
		s.rollback(hasNodesBackup)
		_ = s.restartFn(ctx)
		return 1, fmt.Errorf("sing-box 重启后未进入运行状态，已回滚")
	}

	// 7. 成功，清理备份与 candidate。
	_ = os.Remove(s.Store.SBConf + ".bak")
	_ = os.Remove(s.Store.NodesPath() + ".bak")
	_ = os.Remove(s.Store.NodesPath() + ".candidate")
	return 0, nil
}

// rollback 恢复备份配置与节点数据。
func (s *Service) rollback(hasNodesBackup bool) {
	if _, err := os.Stat(s.Store.SBConf + ".bak"); err == nil {
		_ = copyFile(s.Store.SBConf+".bak", s.Store.SBConf)
	}
	if hasNodesBackup {
		if _, err := os.Stat(s.Store.NodesPath() + ".bak"); err == nil {
			_ = copyFile(s.Store.NodesPath()+".bak", s.Store.NodesPath())
		}
	}
	_ = os.Remove(s.Store.SBConf + ".candidate")
	_ = os.Remove(s.Store.NodesPath() + ".candidate")
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o600)
}
