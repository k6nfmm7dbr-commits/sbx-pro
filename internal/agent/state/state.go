// Package state 定义 sbx-agent 的本地持久化状态（开发提示词第四十节）。
//
// Agent 本地必须无状态化，但需保存：机器身份、Manager URL、认证私钥、
// 同步 sequence、最近已执行 task id、待补传事件等。
// 敏感字段（私钥）以 0600 权限落盘，绝不打印。
package state

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/fsx"
)

// AppDir 返回 Agent 数据目录（SBX_AGENT_DIR 优先，默认 /etc/sbx-agent）。
func AppDir() string {
	if v := os.Getenv("SBX_AGENT_DIR"); v != "" {
		return v
	}
	return "/etc/sbx-agent"
}

// StatePath 返回本地状态文件路径。
func StatePath() string {
	return filepath.Join(AppDir(), "agent.json")
}

// State 是 Agent 本地状态。
type State struct {
	MachineID       string `json:"machine_id"`
	MachineSecret   string `json:"machine_secret"` // Ed25519 私钥 hex（敏感，0600，永不上传）
	ManagerURL      string `json:"manager_url"`
	EnrollToken     string `json:"-"` // 仅注册期使用，不落盘
	Sequence        int64  `json:"sequence"`
	AppliedRevision int64  `json:"applied_revision"`
	RegisteredAt    int64  `json:"registered_at"`
}

// GenerateKeypair 在 Agent 本地生成 Ed25519 keypair。
// 返回公钥 hex 与私钥 hex。私钥只落盘本地，绝不上传。
func GenerateKeypair() (pubHex, privHex string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("生成 Ed25519 keypair 失败: %w", err)
	}
	return hex.EncodeToString(pub), hex.EncodeToString(priv), nil
}

// PrivateKey 返回本地私钥（ed25519.PrivateKey），用于 challenge-response 签名。
func (s *State) PrivateKey() (ed25519.PrivateKey, error) {
	raw, err := hex.DecodeString(s.MachineSecret)
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("本地私钥非法")
	}
	return ed25519.PrivateKey(raw), nil
}

// Load 读取本地状态；不存在返回空 State（首次安装）。
func Load() (*State, error) {
	data, err := os.ReadFile(StatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("读取 agent 状态失败: %w", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("agent 状态损坏: %w", err)
	}
	return &s, nil
}

// Save 原子写回本地状态，0600 权限（含私钥）。
func (s *State) Save() error {
	if err := os.MkdirAll(AppDir(), 0o700); err != nil {
		return fmt.Errorf("创建 agent 目录失败: %w", err)
	}
	data, err := fsx.MarshalIndent(s)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return fsx.WriteFileAtomic(StatePath(), data, 0o600)
}

// Registered 报告是否已完成注册（有 machine_id + 私钥）。
func (s *State) Registered() bool {
	return s.MachineID != "" && s.MachineSecret != ""
}
