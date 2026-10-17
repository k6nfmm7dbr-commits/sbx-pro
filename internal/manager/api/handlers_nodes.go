// Package api —— 节点 CRUD / 分享 / 配额 / IP 限制（从 server.go 拆出）。
package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/audit"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/ips"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/machines"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/nodes"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/tasks"
	sbxnodes "github.com/k6nfmm7dbr-commits/sbx-pro/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// handleListNodes 列出全局节点（脱敏 Public DTO）。
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthed(w, r) {
		return
	}
	list, err := nodes.List(s.db.SQL())
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []nodes.PublicNode{}
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"nodes": list})
}

// handleCreateNode 创建节点：Manager 记录 + 生成节点凭据 + 下发 create_node 任务。
// 节点初始状态 provisioning；任务最终结果驱动 active / create_failed 收敛。

// handleCreateNode 创建节点：Manager 记录 + 生成节点凭据 + 下发 create_node 任务。
// 节点初始状态 provisioning；任务最终结果驱动 active / create_failed 收敛。
func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthed(w, r) {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var req struct {
		MachineID string          `json:"machine_id"`
		Name      string          `json:"name"`
		Protocol  string          `json:"protocol"`
		Port      int             `json:"port"`
		Config    json.RawMessage `json:"config"` // 协议字段（sni/method/version/flow/...）
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.MachineID == "" || req.Protocol == "" || req.Port < 1 || req.Port > 65535 {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "machine_id/protocol/port 必填且端口合法"})
		return
	}
	if !sbxnodes.ValidType(req.Protocol) {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "不支持的协议: " + req.Protocol})
		return
	}
	// 机器必须存在。
	if _, err := machines.Get(s.db.SQL(), req.MachineID); err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "机器不存在"})
		return
	}

	// Manager 权威生成节点凭据（含 uuid/password/psk/reality keypair）。
	userCfg := map[string]any{}
	if len(req.Config) > 0 {
		_ = json.Unmarshal(req.Config, &userCfg)
	}
	cfg, err := nodes.GenerateCredentials(req.Protocol, userCfg)
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cfg["port"] = req.Port

	// Manager 记录节点（provisioning）。
	n, err := nodes.Create(s.db.SQL(), req.MachineID, req.Name, req.Protocol, req.Port, cfg)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 下发 create_node 任务（payload 含节点定义 + node_uuid + 凭据）。
	payload := map[string]any{
		"node_uuid": n.NodeUUID,
		"name":      req.Name,
		"type":      req.Protocol,
		"port":      req.Port,
	}
	for k, v := range cfg {
		payload[k] = v
	}
	taskID, err := s.dispatcher.Dispatch(req.MachineID, n.NodeUUID, protocol.MsgCreateNode, payload)
	if err != nil {
		// 下发立即失败：节点标记 create_failed，不留永久 provisioning 脏数据。
		_ = nodes.UpdateStatus(s.db.SQL(), n.NodeUUID, nodes.StatusProvisioning, nodes.StatusCreateFailed)
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	_ = machines.BumpConfigRevision(s.db.SQL(), req.MachineID)
	audit.Log(s.db.SQL(), "create_node", req.MachineID, n.NodeUUID, "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, map[string]any{
		"node_uuid": n.NodeUUID,
		"task_id":   taskID,
		"status":    nodes.StatusProvisioning,
	})
}

// handleAudit 返回审计日志。

// handleGetNode 返回单节点详情（含敏感 config，仅登录后 share 等场景）。
func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.adminAuthed(w, r) {
		return
	}
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	s.sendJSON(w, http.StatusOK, n.Public())
}

// handleUpdateNode 更新节点（desired），下发 update_node 任务。

// handleUpdateNode 更新节点（desired），下发 update_node 任务。
func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.adminAuthed(w, r) {
		return
	}
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var req struct {
		Name     string          `json:"name"`
		Protocol string          `json:"protocol"`
		Port     int             `json:"port"`
		Config   json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Port < 1 || req.Port > 65535 {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "端口需在 1-65535"})
		return
	}

	// 重新生成凭据（协议可能变化；旧凭据不保留，避免脏配置）。
	userCfg := map[string]any{}
	if len(req.Config) > 0 {
		_ = json.Unmarshal(req.Config, &userCfg)
	}
	cfg, err := nodes.GenerateCredentials(req.Protocol, userCfg)
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cfg["port"] = req.Port

	if _, err := nodes.Update(s.db.SQL(), nodeUUID, req.Name, req.Protocol, req.Port, cfg); err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	payload := map[string]any{"node_uuid": nodeUUID, "name": req.Name, "type": req.Protocol, "port": req.Port}
	for k, v := range cfg {
		payload[k] = v
	}
	taskID, err := s.dispatcher.Dispatch(n.MachineID, nodeUUID, protocol.MsgUpdateNode, payload)
	if err != nil {
		_ = nodes.UpdateStatus(s.db.SQL(), nodeUUID, nodes.StatusUpdatePending, nodes.StatusConfigError)
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = machines.BumpConfigRevision(s.db.SQL(), n.MachineID)
	audit.Log(s.db.SQL(), "update_node", n.MachineID, nodeUUID, "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, map[string]any{"node_uuid": nodeUUID, "task_id": taskID, "status": nodes.StatusUpdatePending})
}

// handleDeleteNode 删除节点：标记 delete_pending + 下发 delete_node 任务。
// 仅在 Agent 确认删除成功后，才由 handleNodeTaskResult 真正删除记录。

// handleDeleteNode 删除节点：标记 delete_pending + 下发 delete_node 任务。
// 仅在 Agent 确认删除成功后，才由 handleNodeTaskResult 真正删除记录。
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.adminAuthed(w, r) {
		return
	}
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	if err := nodes.UpdateStatus(s.db.SQL(), nodeUUID, n.Status, nodes.StatusDeletePending); err != nil {
		s.sendJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	payload := map[string]any{"node_uuid": nodeUUID}
	taskID, err := s.dispatcher.Dispatch(n.MachineID, nodeUUID, protocol.MsgDeleteNode, payload)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_ = machines.BumpConfigRevision(s.db.SQL(), n.MachineID)
	audit.Log(s.db.SQL(), "delete_node", n.MachineID, nodeUUID, "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, map[string]any{"node_uuid": nodeUUID, "task_id": taskID, "status": nodes.StatusDeletePending})
}

// handleNodeShare 返回节点完整分享链接（含 URI / alternate_links，敏感，写审计）。

// handleNodeShare 返回节点完整分享链接（含 URI / alternate_links，敏感，写审计）。
func (s *Server) handleNodeShare(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.adminAuthed(w, r) {
		return
	}
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	m, err := machines.Get(s.db.SQL(), n.MachineID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "机器不存在"})
		return
	}
	host := m.IPv4
	if host == "" {
		host = m.Hostname
	}

	cfg := n.ParseSecret()
	cfg["type"] = n.Protocol
	cfg["name"] = n.Name
	if _, ok := cfg["port"]; !ok {
		cfg["port"] = n.Port
	}
	node := sbxnodes.Node{}
	for k, v := range cfg {
		node[k] = v
	}
	st := &sbxnodes.Store{}
	link := st.LinkFor(node, host, "")

	resp := map[string]any{
		"node_uuid": n.NodeUUID,
		"link":      link,
		"host":      host,
	}
	if n.Protocol == "snell" {
		if surge := st.SnellSurgeFor(node, host, ""); surge != "" {
			resp["alternate_links"] = []string{surge}
		}
	}
	audit.Log(s.db.SQL(), "share_node", n.MachineID, nodeUUID, "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, resp)
}

// handleNodeEnable / handleNodeDisable 启用/停用节点（下发 enable/disable 任务）。

// handleNodeEnable / handleNodeDisable 启用/停用节点（下发 enable/disable 任务）。
func (s *Server) handleNodeEnable(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	s.nodeAction(w, r, nodeUUID, protocol.MsgEnableNode, "enable_node", nodes.StatusUpdatePending)
}

func (s *Server) handleNodeDisable(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	s.nodeAction(w, r, nodeUUID, protocol.MsgDisableNode, "disable_node", nodes.StatusUpdatePending)
}

func (s *Server) handleNodeRestart(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	s.nodeAction(w, r, nodeUUID, protocol.MsgRestartSingbox, "restart_singbox", "")
}

// nodeAction 通用节点动作：下发任务 + 审计。

// nodeAction 通用节点动作：下发任务 + 审计。
func (s *Server) nodeAction(w http.ResponseWriter, r *http.Request, nodeUUID, taskType, auditAction, pendingStatus string) {
	if !s.adminAuthed(w, r) {
		return
	}
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	if pendingStatus != "" {
		_ = nodes.UpdateStatus(s.db.SQL(), nodeUUID, n.Status, pendingStatus)
	}
	payload := map[string]any{"node_uuid": nodeUUID}
	taskID, err := s.dispatcher.Dispatch(n.MachineID, nodeUUID, taskType, payload)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if taskType == protocol.MsgEnableNode || taskType == protocol.MsgDisableNode {
		_ = machines.BumpConfigRevision(s.db.SQL(), n.MachineID)
	}
	audit.Log(s.db.SQL(), auditAction, n.MachineID, nodeUUID, "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, map[string]any{"node_uuid": nodeUUID, "task_id": taskID})
}

// handleNodeQuota 设置节点流量限额。

// handleNodeQuota 设置节点流量限额。
func (s *Server) handleNodeQuota(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.adminAuthed(w, r) {
		return
	}
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	var req struct {
		LimitBytes int64 `json:"limit_bytes"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err := json.Unmarshal(body, &req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if req.LimitBytes < 0 {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "limit_bytes 不能为负"})
		return
	}
	if err := nodes.SetQuota(s.db.SQL(), nodeUUID, req.LimitBytes); err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	taskID, err := s.dispatcher.Dispatch(n.MachineID, nodeUUID, protocol.MsgSetQuota,
		map[string]any{"node_id": n.LocalNodeID, "node_uuid": nodeUUID, "limit_bytes": req.LimitBytes})
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit.Log(s.db.SQL(), "set_quota", n.MachineID, nodeUUID, "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, map[string]any{"node_uuid": nodeUUID, "task_id": taskID})
}

// handleNodeQuotaReset 重置节点流量。

// handleNodeQuotaReset 重置节点流量。
func (s *Server) handleNodeQuotaReset(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.adminAuthed(w, r) {
		return
	}
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	if err := nodes.SetQuota(s.db.SQL(), nodeUUID, 0); err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	taskID, err := s.dispatcher.Dispatch(n.MachineID, nodeUUID, protocol.MsgResetQuota,
		map[string]any{"node_id": n.LocalNodeID, "node_uuid": nodeUUID})
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit.Log(s.db.SQL(), "reset_quota", n.MachineID, nodeUUID, "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, map[string]any{"node_uuid": nodeUUID, "task_id": taskID})
}

// handleNodeIPLimit 设置节点在线 IP 限制。

// handleNodeIPLimit 设置节点在线 IP 限制。
func (s *Server) handleNodeIPLimit(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.adminAuthed(w, r) {
		return
	}
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	var req struct {
		IPLimit int `json:"ip_limit"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err := json.Unmarshal(body, &req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
		return
	}
	if req.IPLimit < 0 {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "ip_limit 不能为负"})
		return
	}
	if err := nodes.SetIPLimit(s.db.SQL(), nodeUUID, req.IPLimit); err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	taskID, err := s.dispatcher.Dispatch(n.MachineID, nodeUUID, protocol.MsgSetIPLimit,
		map[string]any{"node_id": n.LocalNodeID, "node_uuid": nodeUUID, "ip_limit": req.IPLimit})
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit.Log(s.db.SQL(), "set_ip_limit", n.MachineID, nodeUUID, "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, map[string]any{"node_uuid": nodeUUID, "task_id": taskID})
}

// handleNodeActiveIPs 返回节点当前在线 IP 列表。

// handleNodeActiveIPs 返回节点当前在线 IP 列表。
func (s *Server) handleNodeActiveIPs(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.adminAuthed(w, r) {
		return
	}
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	list, err := ips.ListByNode(s.db.SQL(), n.MachineID, nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []ips.ActiveIP{}
	}
	s.sendJSON(w, http.StatusOK, map[string]any{
		"node_uuid":  nodeUUID,
		"active_ips": list,
		"count":      len(list),
	})
}

// handleNodeTaskResult 驱动节点状态机收敛（任务完成回调）。

// handleNodeTaskResult 驱动节点状态机收敛（任务完成回调）。
func (s *Server) handleNodeTaskResult(task *tasks.Task, status protocol.TaskStatus, message string) {
	if task.NodeUUID == "" {
		return
	}
	db := s.db.SQL()
	switch task.Type {
	case protocol.MsgCreateNode:
		if status == protocol.TaskSuccess {
			if localID := parseLocalID(message); localID > 0 {
				_ = nodes.UpdateLocalNodeID(db, task.NodeUUID, localID)
			} else {
				_ = nodes.UpdateStatus(db, task.NodeUUID, nodes.StatusProvisioning, nodes.StatusActive)
			}
		} else {
			_ = nodes.UpdateStatus(db, task.NodeUUID, nodes.StatusProvisioning, nodes.StatusCreateFailed)
		}
	case protocol.MsgUpdateNode:
		if status == protocol.TaskSuccess {
			_ = nodes.UpdateStatus(db, task.NodeUUID, nodes.StatusUpdatePending, nodes.StatusActive)
		} else {
			_ = nodes.UpdateStatus(db, task.NodeUUID, nodes.StatusUpdatePending, nodes.StatusConfigError)
		}
	case protocol.MsgDeleteNode:
		if status == protocol.TaskSuccess {
			_ = nodes.Delete(db, task.NodeUUID)
			_ = ips.DeleteForNode(db, task.MachineID, task.NodeUUID)
		} else {
			_ = nodes.UpdateStatus(db, task.NodeUUID, nodes.StatusDeletePending, nodes.StatusConfigError)
		}
	case protocol.MsgEnableNode:
		if status == protocol.TaskSuccess {
			_ = nodes.UpdateStatus(db, task.NodeUUID, nodes.StatusUpdatePending, nodes.StatusActive)
		} else {
			_ = nodes.UpdateStatus(db, task.NodeUUID, nodes.StatusUpdatePending, nodes.StatusConfigError)
		}
	case protocol.MsgDisableNode:
		if status == protocol.TaskSuccess {
			_ = nodes.UpdateStatus(db, task.NodeUUID, nodes.StatusUpdatePending, nodes.StatusDisabled)
		} else {
			_ = nodes.UpdateStatus(db, task.NodeUUID, nodes.StatusUpdatePending, nodes.StatusConfigError)
		}
	}
}

// parseLocalID 从 handler 返回的 "node_id=2" 提取本地节点 id。

// parseLocalID 从 handler 返回的 "node_id=2" 提取本地节点 id。
func parseLocalID(msg string) int64 {
	for _, part := range strings.Fields(msg) {
		if v, ok := strings.CutPrefix(part, "node_id="); ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

// clientIP 从请求提取来源 IP（考虑反代 X-Forwarded-For，但默认直连取 RemoteAddr）。
