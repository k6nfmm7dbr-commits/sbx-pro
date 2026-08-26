// Package auth 实现机器身份签发与认证（开发提示词第七节 / 11.6）。
//
// 安全模型（Ed25519 challenge-response）：
//   - Agent 本地生成 Ed25519 keypair（private key 只在 Agent，0600 落盘，绝不上传）；
//   - 注册时 Agent 把公钥发给 Manager，Manager 只存公钥；
//   - 连接认证时 Manager 发一次性 challenge，Agent 用私钥签名，Manager 用公钥验签。
//
// machine_id 由 Manager 签发（UUID v4，不依赖 IP，避免 NAT/VPS 换 IP 后身份漂移）。
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNoPublicKey 表示该机器未登记公钥。
var ErrNoPublicKey = errors.New("机器未登记公钥")

// NewMachineID 生成一个全新 machine_id（UUID v4）。
func NewMachineID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("生成 machine_id 失败: %w", err)
	}
	return id.String(), nil
}

// StoreIdentity 将机器公钥写入 Manager 数据库。
// 公钥为 hex 编码的 ed25519.PublicKey（32 字节）。
func StoreIdentity(db *sql.DB, machineID, pubHex string) error {
	pub, err := hex.DecodeString(pubHex)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("公钥非法: %w", err)
	}
	_, err = db.Exec(
		`INSERT INTO agents (machine_id, secret_pub, salt, created_at)
		 VALUES (?, ?, '', strftime('%s','now'))`,
		machineID, pub)
	if err != nil {
		return fmt.Errorf("写入机器身份失败: %w", err)
	}
	return nil
}

// LoadPublicKey 读取机器公钥（返回 ed25519.PublicKey，机器不存在或未登记返回 ErrNoPublicKey）。
func LoadPublicKey(db *sql.DB, machineID string) (ed25519.PublicKey, error) {
	var pub []byte
	err := db.QueryRow(`SELECT secret_pub FROM agents WHERE machine_id = ?`, machineID).Scan(&pub)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoPublicKey
		}
		return nil, fmt.Errorf("读取机器公钥失败: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, ErrNoPublicKey
	}
	return ed25519.PublicKey(pub), nil
}

// NewChallenge 生成一次性随机 challenge（32 字节 hex，64 字符）。
func NewChallenge() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成 challenge 失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// SignChallenge 用私钥对 challenge 签名（Agent 侧使用）。
func SignChallenge(priv ed25519.PrivateKey, challenge string) (string, error) {
	data, err := hex.DecodeString(challenge)
	if err != nil {
		return "", fmt.Errorf("challenge 非法: %w", err)
	}
	sig := ed25519.Sign(priv, data)
	return hex.EncodeToString(sig), nil
}

// VerifyChallenge 用公钥验签 challenge 签名（Manager 侧使用）。
func VerifyChallenge(pub ed25519.PublicKey, challenge, sigHex string) (bool, error) {
	data, err := hex.DecodeString(challenge)
	if err != nil {
		return false, fmt.Errorf("challenge 非法: %w", err)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return false, fmt.Errorf("签名非法: %w", err)
	}
	return ed25519.Verify(pub, data, sig), nil
}
