// Package nodes 实现 Manager 的全局节点管理。
// 本文件在 Manager 侧权威生成节点凭据（uuid/password/psk/reality keypair），
// 使 Manager 持有完整节点 config，从而能直接生成分享 URI（LinkFor）。
package nodes

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
	sbxnodes "github.com/k6nfmm7dbr-commits/sbx-pro/internal/nodes"
)

// GenerateCredentials 在 Manager 侧权威生成节点凭据。
// userCfg 为用户填写的协议字段（sni/method/version/flow 等）；
// 缺失的敏感凭据（uuid/password/psk/private_key/...）在此补齐。
func GenerateCredentials(protocol string, userCfg map[string]any) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range userCfg {
		cfg[k] = v
	}
	cfg["type"] = protocol

	switch protocol {
	case "vless":
		id, err := uuid.NewRandom()
		if err != nil {
			return nil, fmt.Errorf("生成 uuid 失败: %w", err)
		}
		cfg["uuid"] = id.String()
		// Reality X25519 keypair。
		priv, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("生成 reality keypair 失败: %w", err)
		}
		cfg["private_key"] = base64.RawURLEncoding.EncodeToString(priv.Bytes())
		cfg["public_key"] = base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes())
		shortID, err := sbxnodes.GenerateHex(8)
		if err != nil {
			return nil, err
		}
		cfg["short_id"] = shortID
		// 空 flow 移除（BuildInbound 用 Truthy 判断）。
		if s, _ := cfg["flow"].(string); s == "" {
			delete(cfg, "flow")
		}

	case "shadowsocks":
		method, _ := cfg["method"].(string)
		if !sbxnodes.IsSS2022Method(method) {
			method = sbxnodes.SS2022Method128
		}
		cfg["method"] = method
		pw, err := sbxnodes.GenerateSS2022Password(method)
		if err != nil {
			return nil, err
		}
		cfg["password"] = pw

	case "trojan", "anytls":
		pw, err := sbxnodes.GenerateHex(16)
		if err != nil {
			return nil, err
		}
		cfg["password"] = pw

	case "snell":
		if _, ok := cfg["version"]; !ok {
			cfg["version"] = "5"
		}
		psk, err := sbxnodes.GenerateHex(16)
		if err != nil {
			return nil, err
		}
		cfg["psk"] = psk

	default:
		return nil, fmt.Errorf("不支持的协议: %s", protocol)
	}
	return cfg, nil
}
