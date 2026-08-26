// Package handlers 注册 Agent 的任务处理器（白名单 → 内部 Go 函数）。
//
// 关键安全约束（开发提示词第四十一节）：
//   - 绝不接受任意 shell command；
//   - 每个任务类型对应一个内部函数；
//   - 节点变更走 candidate → check → 原子替换安全链。
package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/executor"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/nodesvc"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/agent/quota"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// contextForTask 返回任务执行用的 context（带超时）。
func contextForTask() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 60*time.Second)
}

// applyNode 执行 candidate → sing-box 安全应用，返回可读消息。
func applyNode(svc *nodesvc.Service) (string, error) {
	ctx, cancel := contextForTask()
	defer cancel()
	rc, err := svc.Apply(ctx)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("applied(rc=%d)", rc), nil
}

// Register 注册所有任务处理器到 executor。
// svc 是 Agent 节点服务（管理 sing-box + nodes.json）。
// qs 是 quota 状态（可为 nil，则跳过 quota 相关处理器注册）。
func Register(e *executor.Executor, svc *nodesvc.Service, qs *quota.State) {
	// request_status：返回 Agent 状态（基础可用性探测）。
	e.Register(protocol.MsgRequestStatus, func(db *sql.DB, payload json.RawMessage) (string, error) {
		return "ok", nil
	})

	// sync_config：把 candidate 应用到 sing-box（触发现有 candidate）。
	e.Register(protocol.MsgSyncConfig, func(db *sql.DB, payload json.RawMessage) (string, error) {
		return applyNode(svc)
	})

	// create_node：payload 为节点定义（type/port/name/...）。
	e.Register(protocol.MsgCreateNode, func(db *sql.DB, payload json.RawMessage) (string, error) {
		var m map[string]any
		if err := json.Unmarshal(payload, &m); err != nil {
			return "", fmt.Errorf("节点定义解析失败: %w", err)
		}
		n := nodes.Node{}
		for k, v := range m {
			n[k] = v
		}
		id, err := svc.AddNode(n)
		if err != nil {
			return "", err
		}
		if _, err := applyNode(svc); err != nil {
			return "", fmt.Errorf("节点已生成但应用失败: %w", err)
		}
		return "node_id=" + id, nil
	})

	// update_node：payload 含 node_id + 变更字段。
	e.Register(protocol.MsgUpdateNode, func(db *sql.DB, payload json.RawMessage) (string, error) {
		var req struct {
			NodeID  string            `json:"node_id"`
			Changes map[string]string `json:"changes"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return "", fmt.Errorf("更新定义解析失败: %w", err)
		}
		if err := svc.EditNode(req.NodeID, req.Changes); err != nil {
			return "", err
		}
		if _, err := applyNode(svc); err != nil {
			return "", fmt.Errorf("节点已修改但应用失败: %w", err)
		}
		return "updated", nil
	})

	// delete_node：payload 含 node_id。
	e.Register(protocol.MsgDeleteNode, func(db *sql.DB, payload json.RawMessage) (string, error) {
		var req struct {
			NodeID string `json:"node_id"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return "", fmt.Errorf("删除定义解析失败: %w", err)
		}
		if err := svc.RemoveNode(req.NodeID); err != nil {
			return "", err
		}
		if _, err := applyNode(svc); err != nil {
			return "", fmt.Errorf("节点已删除但应用失败: %w", err)
		}
		return "deleted", nil
	})

	// enable_node / disable_node：占位（Phase 5 只做整体节点状态，具体启停后续）。
	e.Register(protocol.MsgEnableNode, func(db *sql.DB, payload json.RawMessage) (string, error) {
		return "enabled(待实现)", nil
	})
	e.Register(protocol.MsgDisableNode, func(db *sql.DB, payload json.RawMessage) (string, error) {
		return "disabled(待实现)", nil
	})

	// restart_singbox：占位。
	e.Register(protocol.MsgRestartSingbox, func(db *sql.DB, payload json.RawMessage) (string, error) {
		return "restarted(待实现)", nil
	})

	// set_quota / reset_quota：设置/重置节点流量限额。
	if qs != nil {
		e.Register(protocol.MsgSetQuota, func(db *sql.DB, payload json.RawMessage) (string, error) {
			var req struct {
				NodeID     string `json:"node_id"`
				LimitBytes int64  `json:"limit_bytes"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				return "", fmt.Errorf("quota 定义解析失败: %w", err)
			}
			if req.NodeID == "" {
				return "", fmt.Errorf("缺少 node_id")
			}
			if err := qs.SetLimit(req.NodeID, req.LimitBytes); err != nil {
				return "", err
			}
			return "quota_set", nil
		})
		e.Register(protocol.MsgResetQuota, func(db *sql.DB, payload json.RawMessage) (string, error) {
			var req struct {
				NodeID string `json:"node_id"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				return "", fmt.Errorf("quota 定义解析失败: %w", err)
			}
			if err := qs.ResetQuota(req.NodeID); err != nil {
				return "", err
			}
			return "quota_reset", nil
		})
	}
}
