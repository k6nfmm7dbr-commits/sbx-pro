// Package nodes 实现 Manager 的全局节点管理。
// 本文件定义协议能力 schema（capabilities），作为前端表单的权威数据源，
// 避免前端硬编码协议白名单与后端漂移。
package nodes

// ProtocolField 是协议的一个可配置字段。
type ProtocolField struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"` // text | password | number | select
	Required    bool     `json:"required"`
	Placeholder string   `json:"placeholder,omitempty"`
	Options     []string `json:"options,omitempty"`
}

// ProtocolCap 是单个协议的能力描述。
type ProtocolCap struct {
	Type   string          `json:"type"`
	Label  string          `json:"label"`
	Fields []ProtocolField `json:"fields"`
}

// Capabilities 返回所有受支持协议的字段 schema（权威定义，与 BuildInbound 对齐）。
func Capabilities() []ProtocolCap {
	return []ProtocolCap{
		{
			Type:  "vless",
			Label: "VLESS Reality",
			Fields: []ProtocolField{
				{Key: "sni", Label: "回落域名 (SNI)", Type: "text", Required: true, Placeholder: "example.com"},
				{Key: "flow", Label: "Flow", Type: "select", Options: []string{"", "xtls-rprx-vision"}},
			},
		},
		{
			Type:  "shadowsocks",
			Label: "Shadowsocks 2022",
			Fields: []ProtocolField{
				{Key: "method", Label: "加密算法", Type: "select",
					Options: []string{"2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm"},
					Required: true},
			},
		},
		{
			Type:  "trojan",
			Label: "Trojan",
			Fields: []ProtocolField{
				{Key: "sni", Label: "SNI", Type: "text", Placeholder: "留空自动生成"},
			},
		},
		{
			Type:  "anytls",
			Label: "AnyTLS",
			Fields: []ProtocolField{
				{Key: "sni", Label: "SNI", Type: "text", Placeholder: "留空自动生成"},
			},
		},
		{
			Type:  "snell",
			Label: "Snell",
			Fields: []ProtocolField{
				{Key: "version", Label: "版本", Type: "select", Options: []string{"5", "6"}, Required: true},
				{Key: "obfs_mode", Label: "混淆模式 (仅 v5)", Type: "select", Options: []string{"none", "http", "tls"}},
			},
		},
	}
}

// ProtocolLabel 返回协议的用户可见名称（Snell 区分版本）。
func ProtocolLabel(t string) string {
	for _, c := range Capabilities() {
		if c.Type == t {
			return c.Label
		}
	}
	return t
}
