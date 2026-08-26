package nodes

import (
	"encoding/base64"
	"testing"

	sbxnodes "github.com/k6nfmm7dbr-commits/sbx-pro/internal/nodes"
)

func b64decode(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

func TestGenerateCredentialsVless(t *testing.T) {
	cfg, err := GenerateCredentials("vless", map[string]any{"sni": "example.com"})
	if err != nil {
		t.Fatalf("GenerateCredentials: %v", err)
	}
	if cfg["uuid"] == "" || len(cfg["uuid"].(string)) != 36 {
		t.Errorf("vless uuid 非法: %v", cfg["uuid"])
	}
	if cfg["private_key"] == "" || cfg["public_key"] == "" {
		t.Error("reality keypair 未生成")
	}
	if cfg["short_id"] == "" {
		t.Error("short_id 未生成")
	}
	if cfg["sni"] != "example.com" {
		t.Errorf("sni 未保留: %v", cfg["sni"])
	}
}

func TestGenerateCredentialsShadowsocks(t *testing.T) {
	cfg, err := GenerateCredentials("shadowsocks", map[string]any{"method": sbxnodes.SS2022Method256})
	if err != nil {
		t.Fatalf("GenerateCredentials: %v", err)
	}
	if cfg["method"] != sbxnodes.SS2022Method256 {
		t.Errorf("method 未保留: %v", cfg["method"])
	}
	// 256-gcm 的 password 应为 32 字节 base64。
	pw := cfg["password"].(string)
	decoded, err := b64decode(pw)
	if err != nil || len(decoded) != 32 {
		t.Errorf("shadowsocks 256 password 长度应为 32 字节，得到 %d", len(decoded))
	}
}

func TestGenerateCredentialsTrojanSnell(t *testing.T) {
	cfg, err := GenerateCredentials("trojan", map[string]any{})
	if err != nil {
		t.Fatalf("trojan: %v", err)
	}
	if cfg["password"] == "" {
		t.Error("trojan password 未生成")
	}

	cfg2, err := GenerateCredentials("snell", map[string]any{"version": "6"})
	if err != nil {
		t.Fatalf("snell: %v", err)
	}
	if cfg2["psk"] == "" || cfg2["version"] != "6" {
		t.Errorf("snell 凭据非法: %v", cfg2)
	}
}

func TestGenerateCredentialsUnknown(t *testing.T) {
	if _, err := GenerateCredentials("unknown", map[string]any{}); err == nil {
		t.Error("未知协议应报错")
	}
}
