// Package auth 实现机器身份签发与认证（开发提示词第七节）。
//
// 注册成功后 Manager 为 Agent 签发独立机器身份：
//   - machine_id = UUID v4（不依赖 IP，避免 NAT/VPS 换 IP）；
//   - Ed25519 keypair：私钥下发 Agent（长期认证凭据），公钥存 Manager。
//
// Agent 后续用私钥对连接认证：WebSocket 握手携带 machine_id + 用私钥签名，
// Manager 用公钥验签（Phase 3 实现）。本阶段提供 keypair 生成与 UUID 生成。
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
)

// Identity 是签发给一台机器的身份。
type Identity struct {
	MachineID    string // UUID
	SecretPub    ed25519.PublicKey
	SecretPriv   ed25519.PrivateKey
}

// NewIdentity 生成全新机器身份（UUID + Ed25519 keypair）。
func NewIdentity() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 Ed25519 keypair 失败: %w", err)
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, fmt.Errorf("生成 machine_id 失败: %w", err)
	}
	return &Identity{
		MachineID:  id.String(),
		SecretPub:  pub,
		SecretPriv: priv,
	}, nil
}

// PrivHex 返回私钥的 hex 编码（下发 Agent，Agent 存本地）。
func (i *Identity) PrivHex() string { return hex.EncodeToString(i.SecretPriv) }

// PubHex 返回公钥的 hex 编码（存 Manager agents.secret_pub）。
func (i *Identity) PubHex() string { return hex.EncodeToString(i.SecretPub) }

// StoreIdentity 将机器身份写入 Manager 数据库。
func StoreIdentity(db *sql.DB, id *Identity) error {
	_, err := db.Exec(
		`INSERT INTO agents (machine_id, secret_pub, salt, created_at)
		 VALUES (?, ?, '', strftime('%s','now'))`,
		id.MachineID, id.SecretPub)
	if err != nil {
		return fmt.Errorf("写入机器身份失败: %w", err)
	}
	return nil
}

// VerifySecret 用存储的公钥验签（Phase 3 使用）。此处提供基础能力。
func VerifySecret(pub ed25519.PublicKey, msg, sig []byte) bool {
	return ed25519.Verify(pub, msg, sig)
}
