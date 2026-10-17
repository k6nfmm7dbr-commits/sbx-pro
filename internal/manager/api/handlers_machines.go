// Package api —— Agent 注册、机器管理、任务与审计（从 server.go 拆出）。
package api

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/audit"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/auth"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/enrollment"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/machines"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/nodes"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/tasks"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/traffic"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// handleRegister 处理 Agent 注册（开发提示词第六节）。
// 请求体为 protocol.Hello（含 enroll_token），校验 token 后签发机器身份。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "read body: " + err.Error()})
		return
	}
	var hello protocol.Hello
	if err := json.Unmarshal(body, &hello); err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid hello: " + err.Error()})
		return
	}
	if hello.EnrollToken == "" {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 enroll_token"})
		return
	}
	if hello.PublicKey == "" {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "缺少 public_key（Agent 本地生成的公钥）"})
		return
	}

	// 先校验 token（不消费），失败直接拒绝。
	if _, err := enrollment.Consume(s.db.SQL(), hello.EnrollToken); err != nil {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}

	// 校验公钥格式（32 字节 ed25519 公钥 hex）。
	if _, err := hex.DecodeString(hello.PublicKey); err != nil || len(mustDecodeHex(hello.PublicKey)) != 32 {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "public_key 非法"})
		return
	}

	// 生成机器身份（Manager 只签发 machine_id，不生成/持有私钥）。
	machineID, err := auth.NewMachineID()
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 写入机器记录 + 公钥 + 消费 token。三件事应原子化；退一步先顺序执行，
	// 失败则回滚（删除 machines/agents 行），避免留脏数据。
	m := &machines.Machine{
		MachineID:    machineID,
		Hostname:     hello.Hostname,
		OS:           hello.OS,
		Kernel:       hello.Kernel,
		Arch:         hello.Arch,
		AgentVersion: hello.AgentVersion,
		Status:       "offline",
	}
	if err := machines.Register(s.db.SQL(), m); err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := auth.StoreIdentity(s.db.SQL(), machineID, hello.PublicKey); err != nil {
		_ = machines.Delete(s.db.SQL(), machineID)
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := enrollment.MarkUsed(s.db.SQL(), hello.EnrollToken, machineID); err != nil {
		_ = machines.Delete(s.db.SQL(), machineID)
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	ack := protocol.HelloAck{
		MachineID: machineID,
		Accepted:  true,
	}
	s.sendJSON(w, http.StatusOK, ack)
	audit.Log(s.db.SQL(), "agent_register", machineID, "", "ok", clientIP(r))
	slog.Info("机器注册成功", "machine_id", machineID, "hostname", hello.Hostname)
}

// mustDecodeHex 解码 hex，失败返回空（用于长度校验）。

// mustDecodeHex 解码 hex，失败返回空（用于长度校验）。
func mustDecodeHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

// handleEnrollmentToken 管理员生成 enrollment token。

// handleEnrollmentToken 管理员生成 enrollment token。
func (s *Server) handleEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthed(w, r) {
		return
	}
	tok, err := enrollment.New(s.db.SQL(), time.Duration(s.cfg.TokenTTL)*time.Second)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]any{
		"token":      tok,
		"expires_in": s.cfg.TokenTTL,
	})
}

// handleMachines 管理员查看机器列表（临时实现，Phase 3 完善）。

// handleMachines 管理员查看机器列表（临时实现，Phase 3 完善）。
func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthed(w, r) {
		return
	}
	list, err := machines.List(s.db.SQL())
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []machines.Machine{}
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"machines": list})
}

// handleDispatchTask 管理员下发任务（测试/WebUI 用）。
// 请求体: {"machine_id":"...","type":"request_status","node_uuid":"","payload":{...}}

// handleDispatchTask 管理员下发任务（测试/WebUI 用）。
// 请求体: {"machine_id":"...","type":"request_status","node_uuid":"","payload":{...}}
func (s *Server) handleDispatchTask(w http.ResponseWriter, r *http.Request) {
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
		NodeUUID  string          `json:"node_uuid"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.MachineID == "" || req.Type == "" {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "machine_id/type 必填"})
		return
	}
	taskID, err := s.dispatcher.Dispatch(req.MachineID, req.NodeUUID, req.Type, req.Payload)
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]string{"task_id": taskID})
}

// handleListTasks 管理员查看任务列表。

// handleListTasks 管理员查看任务列表。
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthed(w, r) {
		return
	}
	status := r.URL.Query().Get("status")
	machine := r.URL.Query().Get("machine_id")
	list, err := tasks.List(s.db.SQL(), status, machine)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []tasks.Task{}
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"tasks": list})
}

// handleListNodes 列出全局节点（脱敏 Public DTO）。

// handleAudit 返回审计日志。
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthed(w, r) {
		return
	}
	list, err := audit.List(s.db.SQL(), 200)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if list == nil {
		list = []audit.Entry{}
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"audit_logs": list})
}

// handleTraffic 返回全局流量汇总（多机）。

// handleTraffic 返回全局流量汇总（多机）。
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthed(w, r) {
		return
	}
	machineID := r.URL.Query().Get("machine_id")
	totals, err := traffic.TotalsByMachine(s.db.SQL(), machineID)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if totals == nil {
		totals = []traffic.TotalNode{}
	}
	var rxSum, txSum int64
	for _, t := range totals {
		rxSum += t.Rx
		txSum += t.Tx
	}
	s.sendJSON(w, http.StatusOK, map[string]any{
		"totals":   totals,
		"rx_total": rxSum,
		"tx_total": txSum,
	})
}

// handleCapabilities 返回协议能力 schema（前端表单权威数据源）。

// handleCapabilities 返回协议能力 schema（前端表单权威数据源）。
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.adminAuthed(w, r) {
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"protocols": nodes.Capabilities()})
}

// handleDeleteMachine 删除机器管理关系（只删 Manager 侧，不卸远端）。

// handleDeleteMachine 删除机器管理关系（只删 Manager 侧，不卸远端）。
func (s *Server) handleDeleteMachine(w http.ResponseWriter, r *http.Request, machineID string) {
	if !s.adminAuthed(w, r) {
		return
	}
	if err := machines.Delete(s.db.SQL(), machineID); err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	audit.Log(s.db.SQL(), "delete_machine", machineID, "", "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, map[string]string{"deleted": machineID})
}

// handleGetNode 返回单节点详情（含敏感 config，仅登录后 share 等场景）。
