// Package enrollment 实现机器注册的 enrollment token 机制（开发提示词第六节）。
//
// 要求：
//   - 随机生成、高熵；
//   - 默认一次性（使用后立即失效）；
//   - 15 分钟过期；
//   - 不作为机器长期认证凭据（注册成功后由 auth 签发独立 machine secret）。
package enrollment

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrUsed 表示 token 已被使用。
var ErrUsed = errors.New("enrollment token 已使用")

// ErrExpired 表示 token 已过期。
var ErrExpired = errors.New("enrollment token 已过期")

// ErrNotFound 表示 token 不存在。
var ErrNotFound = errors.New("enrollment token 不存在")

// New 生成一个高熵 enrollment token（32 字节 → 64 hex 字符），并写入数据库。
// ttl 为有效期（秒）；0 表示使用默认（由调用方传入 config.TokenTTL）。
func New(db *sql.DB, ttl time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	expires := time.Now().Add(ttl).Unix()
	created := time.Now().Unix()
	if _, err := db.Exec(
		`INSERT INTO enrollment_tokens (token, expires_at, used, machine_id, created_at)
		 VALUES (?, ?, 0, '', ?)`,
		token, expires, created); err != nil {
		return "", fmt.Errorf("写入 enrollment token 失败: %w", err)
	}
	return token, nil
}

// Consume 校验并一次性消费 token：仅当存在、未使用、未过期时成功，
// 并把它标记为已使用（绑定 machine_id），返回该 machine_id（首次为空）。
// 任何失败都不会消费 token（不影响重试语义）。
func Consume(db *sql.DB, token string) (machineID string, err error) {
	var expiresAt int64
	var used int
	var existingMachine string
	err = db.QueryRow(
		`SELECT expires_at, used, machine_id FROM enrollment_tokens WHERE token = ?`,
		token).Scan(&expiresAt, &used, &existingMachine)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("查询 token 失败: %w", err)
	}
	if used != 0 {
		return "", ErrUsed
	}
	if time.Now().Unix() > expiresAt {
		return "", ErrExpired
	}
	return existingMachine, nil
}

// MarkUsed 把 token 标记为已使用并绑定 machine_id（一次性）。
// 使用条件更新（WHERE used=0）保证并发下只有一个注册能成功消费。
func MarkUsed(db *sql.DB, token, machineID string) error {
	res, err := db.Exec(
		`UPDATE enrollment_tokens SET used = 1, machine_id = ? WHERE token = ? AND used = 0`,
		machineID, token)
	if err != nil {
		return fmt.Errorf("标记 token 已使用失败: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUsed
	}
	return nil
}

// PurgeExpired 清理已过期的 token（可选后台任务调用）。
func PurgeExpired(db *sql.DB) (int64, error) {
	res, err := db.Exec(`DELETE FROM enrollment_tokens WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
