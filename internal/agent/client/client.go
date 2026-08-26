// Package client 实现 sbx-agent 到 Manager 的 HTTP/WebSocket 通信客户端。
// Phase 2 提供 enroll（注册）；Phase 3 补充 WebSocket 长连接。
package client

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// Client 是 Agent 到 Manager 的客户端。
type Client struct {
	ManagerURL string
	HTTP       *http.Client
}

// New 构造 Client。managerURL 形如 https://panel.example.com（自动补 /api/agent/register 等）。
func New(managerURL string) *Client {
	managerURL = strings.TrimRight(managerURL, "/")
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &Client{
		ManagerURL: managerURL,
		HTTP: &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}
}

// Enroll 使用 enrollment token 向 Manager 注册，返回签发身份。
func (c *Client) Enroll(hello protocol.Hello) (*protocol.HelloAck, error) {
	body, err := json.Marshal(hello)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Post(c.ManagerURL+"/api/agent/register",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("注册请求失败: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return nil, fmt.Errorf("注册被拒绝(%d): %s", resp.StatusCode, msg)
	}
	var ack protocol.HelloAck
	if err := json.Unmarshal(data, &ack); err != nil {
		return nil, fmt.Errorf("解析注册响应失败: %w", err)
	}
	if !ack.Accepted {
		return nil, fmt.Errorf("注册被拒绝: %s", ack.Reason)
	}
	return &ack, nil
}
