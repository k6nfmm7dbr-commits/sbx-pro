// Package protocol 定义 sbx-pro Manager ↔ Agent 之间的通信协议。
//
// 设计原则（见开发提示词第十节 / 四十一节）：
//   - 统一 envelope，携带 version 字段为以后升级预留；
//   - 所有消息使用明确的 type 白名单，Agent 绝不接收任意 shell command；
//   - payload 由接收方按 type 反序列化为强结构体，而非透传字符串；
//   - 任务消息携带唯一 task_id，Agent 据此实现幂等。
//
// 该包为纯数据定义（无 I/O、无并发），可被 Manager 与 Agent 安全共享，
// 也被测试直接引用。
package protocol

import (
	"encoding/json"
	"time"
)

// Now 返回当前 Unix 时间戳（秒）。独立函数便于测试注入。
var Now = func() int64 { return time.Now().Unix() }

// ProtocolVersion 是当前 envelope 协议版本。握手 / 不兼容检测以此为准。
const ProtocolVersion = 1

// Envelope 是所有 WebSocket 消息的统一外层结构。
//
//	{
//	  "version": 1,
//	  "type": "task",
//	  "id": "uuid",
//	  "timestamp": 0,
//	  "payload": {...}
//	}
type Envelope struct {
	Version   int             `json:"version"`
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Timestamp int64           `json:"timestamp"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// New 构造一个已填 version/timestamp 的 envelope。
// payload 若为 nil 则省略；若传入已序列化的 RawMessage 则原样使用。
func New(typ, id string, payload any) (*Envelope, error) {
	var raw json.RawMessage
	if payload != nil {
		if r, ok := payload.(json.RawMessage); ok {
			raw = r
		} else {
			b, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			raw = b
		}
	}
	return &Envelope{
		Version:   ProtocolVersion,
		Type:      typ,
		ID:        id,
		Timestamp: Now(),
		Payload:   raw,
	}, nil
}

// Marshal 序列化 envelope。
func (e *Envelope) Marshal() ([]byte, error) { return json.Marshal(e) }

// UnmarshalEnvelope 反序列化 envelope 并校验协议版本。
func UnmarshalEnvelope(data []byte) (*Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	if e.Version != ProtocolVersion {
		return nil, &VersionError{Got: e.Version, Want: ProtocolVersion}
	}
	if e.Type == "" {
		return nil, &MalformedError{Field: "type"}
	}
	return &e, nil
}

// VersionError 表示协议版本不匹配。
type VersionError struct{ Got, Want int }

func (e *VersionError) Error() string {
	return "协议版本不匹配: got " + itoa(e.Got) + ", want " + itoa(e.Want)
}

// MalformedError 表示 envelope 缺少必需字段。
type MalformedError struct{ Field string }

func (e *MalformedError) Error() string { return "消息缺少必需字段: " + e.Field }

// PayloadInto 将 payload 反序列化到 v。
func (e *Envelope) PayloadInto(v any) error {
	if len(e.Payload) == 0 {
		return &MalformedError{Field: "payload"}
	}
	return json.Unmarshal(e.Payload, v)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
