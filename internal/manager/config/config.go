// Package config 定义 sbx-manager 的配置（manager.json）。
// 沿用原 sbx config 的 Load/LoadStrict fail-closed 哲学与原子写回。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/fsx"
)

// AppDir 返回 Manager 数据目录（SBX_PRO_DIR 优先，默认 /etc/sbx-pro）。
func AppDir() string {
	if v := os.Getenv("SBX_PRO_DIR"); v != "" {
		return v
	}
	return "/etc/sbx-pro"
}

// ConfPath 返回 Manager 配置文件路径。
func ConfPath() string {
	if v := os.Getenv("SBX_PRO_CONF"); v != "" {
		return v
	}
	return filepath.Join(AppDir(), "manager.json")
}

func defaultConf() map[string]any {
	dir := AppDir()
	return map[string]any{
		"db":          filepath.Join(dir, "manager.db"),
		"listen":      "0.0.0.0",
		"port":        json.Number("8080"),
		"admin_token": "",
		"tls_cert":    "",
		"tls_key":     "",
		"token_ttl":   json.Number("900"), // enrollment token 有效期（秒），默认 15 分钟
	}
}

// Config 是合并后的运行配置。
type Config struct {
	raw map[string]any

	DB          string
	Listen      string
	Port        int
	AdminToken  string
	TLSCert     string
	TLSKey      string
	TokenTTL    int64
}

// Load 宽松读取（只读场景）。
func Load() *Config {
	c, _ := load(ConfPath(), false)
	return c
}

// LoadStrict 严格读取（fail-closed）。
func LoadStrict() (*Config, error) {
	c, err := load(ConfPath(), true)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func load(path string, strict bool) (*Config, error) {
	c := &Config{raw: defaultConf()}
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		// 全新安装：使用默认值
	case err != nil:
		if strict {
			return nil, fmt.Errorf("配置文件读取失败: %w", err)
		}
	default:
		file, derr := decodeConfig(data)
		if derr != nil {
			if strict {
				return nil, fmt.Errorf("配置文件损坏: %w", derr)
			}
		} else {
			for k, v := range file {
				c.raw[k] = v
			}
		}
	}
	c.normalize()
	return c, nil
}

func decodeConfig(data []byte) (map[string]any, error) {
	var file map[string]any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&file); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("存在多个 JSON 值")
		}
		return nil, fmt.Errorf("尾部存在非法数据: %w", err)
	}
	return file, nil
}

// Validate 语义校验。
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port 必须在 1-65535: %d", c.Port)
	}
	if c.DB == "" {
		return fmt.Errorf("db 路径不能为空")
	}
	if c.TokenTTL < 1 || c.TokenTTL > 86400 {
		return fmt.Errorf("token_ttl 必须在 1-86400 秒: %d", c.TokenTTL)
	}
	// TLS 证书与密钥必须成对出现。
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("tls_cert / tls_key 必须成对配置")
	}
	return nil
}

func (c *Config) normalize() {
	c.DB = c.str("db")
	c.Listen = c.str("listen")
	c.Port = int(c.int("port"))
	c.AdminToken = strings.TrimSpace(c.str("admin_token"))
	c.TLSCert = c.str("tls_cert")
	c.TLSKey = c.str("tls_key")
	c.TokenTTL = c.int("token_ttl")
	if c.TokenTTL <= 0 {
		c.TokenTTL = 900
	}
}

func (c *Config) str(key string) string {
	v := c.raw[key]
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(t)
	}
}

func (c *Config) int(key string) int64 {
	v := c.raw[key]
	switch t := v.(type) {
	case json.Number:
		if i, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return i
		}
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return i
		}
	case float64:
		return int64(t)
	}
	return 0
}

// Get 返回原始值（供 CLI）。
func (c *Config) Get(key string) any { return c.raw[key] }

// EnsureAdminToken 保证 admin_token 非空，为空则生成 32 位 hex 随机令牌写回。
func EnsureAdminToken() (string, error) {
	c, err := LoadStrict()
	if err != nil {
		return "", fmt.Errorf("拒绝生成管理令牌: %w", err)
	}
	if c.AdminToken != "" {
		return c.AdminToken, nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	c.raw["admin_token"] = tok
	c.normalize()
	data, err := fsx.MarshalIndent(c.raw)
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := fsx.WriteFileAtomic(ConfPath(), data, 0o600); err != nil {
		return "", err
	}
	return tok, nil
}
