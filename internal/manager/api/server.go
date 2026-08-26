// Package api 实现 sbx-manager 的 HTTP 服务（管理员 API + Agent 注册/接入）。
// Phase 2 提供：/healthz、/api/agent/register、/api/enrollment/token。
package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/audit"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/auth"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/config"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/db"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/enrollment"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/gateway"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/machines"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/nodes"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/tasks"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/traffic"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/webui"
	sbxnodes "github.com/k6nfmm7dbr-commits/sbx-pro/internal/nodes"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// Server 持有 Manager HTTP 服务依赖。
type Server struct {
	cfg        *config.Config
	db         *db.Manager
	gateway    *gateway.Gateway
	dispatcher *tasks.Dispatcher
}

// New 构造 Server。
func New(cfg *config.Config, db *db.Manager) *Server {
	gw := gateway.New(db)
	gw.OnTrafficDelta = func(td *protocol.TrafficDelta) error {
		// 流量增量入库（防重）。重复数据返回 accepted=false 但 err=nil，视为成功。
		_, err := traffic.IngestDelta(db.SQL(), *td)
		return err
	}
	s := &Server{
		cfg:        cfg,
		db:         db,
		gateway:    gw,
		dispatcher: tasks.NewDispatcher(db.SQL(), gw, 60*time.Second),
	}
	s.dispatcher.OnTaskComplete = s.handleNodeTaskResult
	return s
}

// Gateway 暴露 gateway（供 service 层注入 sweeper/task 相关）。
func (s *Server) Gateway() *gateway.Gateway { return s.gateway }

// Dispatcher 暴露任务分发器（供 CLI/测试下发任务）。
func (s *Server) Dispatcher() *tasks.Dispatcher { return s.dispatcher }

// NewHTTPServer 构造配置好超时的 *http.Server（不启动监听）。
func (s *Server) NewHTTPServer() *http.Server {
	addr := net.JoinHostPort(s.cfg.Listen, strconv.Itoa(s.cfg.Port))
	return &http.Server{
		Addr:              addr,
		Handler:           s.recoverMiddleware(s),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

// StartOfflineSweeper 启动 Manager 的在线状态维护协程。
func (s *Server) StartOfflineSweeper(ctx context.Context, interval time.Duration) {
	s.gateway.RunOfflineSweeper(ctx, interval)
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("HTTP 处理器异常", "err", rec, "path", r.URL.Path)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) sendJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// ServeHTTP 路由分发。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := strings.TrimRight(r.URL.Path, "/")
	if route == "" {
		route = "/"
	}
	switch {
	case route == "/healthz" && r.Method == http.MethodGet:
		s.sendJSON(w, http.StatusOK, map[string]any{"ok": true})

	case route == "/api/agent/register" && r.Method == http.MethodPost:
		s.handleRegister(w, r)

	case route == "/api/agent/ws":
		s.gateway.HandleWS(w, r)

	case route == "/api/enrollment/token" && r.Method == http.MethodPost:
		s.handleEnrollmentToken(w, r)

	case route == "/api/machines" && r.Method == http.MethodGet:
		s.handleMachines(w, r)

	case strings.HasPrefix(route, "/api/machines/") && r.Method == http.MethodDelete:
		s.handleDeleteMachine(w, r, strings.TrimPrefix(route, "/api/machines/"))

	case route == "/api/capabilities" && r.Method == http.MethodGet:
		s.handleCapabilities(w, r)

	case route == "/api/tasks" && r.Method == http.MethodPost:
		s.handleDispatchTask(w, r)

	case route == "/api/tasks" && r.Method == http.MethodGet:
		s.handleListTasks(w, r)

	case route == "/api/nodes" && r.Method == http.MethodGet:
		s.handleListNodes(w, r)

	case route == "/api/nodes" && r.Method == http.MethodPost:
		s.handleCreateNode(w, r)

	case strings.HasPrefix(route, "/api/nodes/"):
		s.routeNode(w, r, strings.TrimPrefix(route, "/api/nodes/"))

	case route == "/api/traffic" && r.Method == http.MethodGet:
		s.handleTraffic(w, r)

	case route == "/api/audit" && r.Method == http.MethodGet:
		s.handleAudit(w, r)

	default:
		s.handleWeb(w, r, route)
	}
}

// routeNode 分发 /api/nodes/:id[/sub[/sub2]] 的节点子路由。
func (s *Server) routeNode(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	sub2 := ""
	if len(parts) > 2 {
		sub2 = parts[2]
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.handleGetNode(w, r, id)
	case sub == "" && r.Method == http.MethodPut:
		s.handleUpdateNode(w, r, id)
	case sub == "" && r.Method == http.MethodDelete:
		s.handleDeleteNode(w, r, id)
	case sub == "share" && r.Method == http.MethodGet:
		s.handleNodeShare(w, r, id)
	case sub == "enable" && r.Method == http.MethodPost:
		s.handleNodeEnable(w, r, id)
	case sub == "disable" && r.Method == http.MethodPost:
		s.handleNodeDisable(w, r, id)
	case sub == "restart" && r.Method == http.MethodPost:
		s.handleNodeRestart(w, r, id)
	case sub == "quota" && sub2 == "reset" && r.Method == http.MethodPost:
		s.handleNodeQuotaReset(w, r, id)
	case sub == "quota" && r.Method == http.MethodPost:
		s.handleNodeQuota(w, r, id)
	case sub == "ip-limit" && r.Method == http.MethodPost:
		s.handleNodeIPLimit(w, r, id)
	default:
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleWeb 处理前端页面与静态资源（未登录返回登录页）。
func (s *Server) handleWeb(w http.ResponseWriter, r *http.Request, route string) {
	switch {
	case route == "/login" && r.Method == http.MethodPost:
		s.handleLogin(w, r)

	case route == "/login" && r.Method == http.MethodGet:
		s.serveAsset(w, r, "login.html", "text/html; charset=utf-8")

	case route == "/" || route == "/index.html":
		if !s.authorized(r) {
			s.serveAsset(w, r, "login.html", "text/html; charset=utf-8")
			return
		}
		s.serveAsset(w, r, "index.html", "text/html; charset=utf-8")

	case route == "/app.js":
		s.serveAsset(w, r, "app.js", "application/javascript; charset=utf-8")
	case route == "/login.js":
		s.serveAsset(w, r, "login.js", "application/javascript; charset=utf-8")
	case route == "/style.css":
		s.serveAsset(w, r, "style.css", "text/css; charset=utf-8")
	case route == "/extra.css":
		s.serveAsset(w, r, "extra.css", "text/css; charset=utf-8")

	default:
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleLogin 处理管理员登录（POST /login，token 表单）。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || len(body) > 64<<10 {
		http.Redirect(w, r, "/login?error=1", http.StatusFound)
		return
	}
	vals, _ := url.ParseQuery(string(body))
	given := ""
	for _, v := range vals["token"] {
		if v != "" {
			given = v
			break
		}
	}
	if s.cfg.AdminToken != "" && constantTimeEq(given, s.cfg.AdminToken) {
		http.SetCookie(w, &http.Cookie{
			Name: "sbx_token", Value: given, Path: "/",
			MaxAge: 604800, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login?error=1", http.StatusFound)
}

// assetBytes 从内嵌前端读取文件。
func assetBytes(name string) ([]byte, error) {
	f, err := webui.FS().Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// serveAsset 输出内嵌前端资源。
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request, name, ctype string) {
	data, err := assetBytes(name)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

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
func mustDecodeHex(s string) []byte {
	b, _ := hex.DecodeString(s)
	return b
}

// handleEnrollmentToken 管理员生成 enrollment token。
func (s *Server) handleEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleDispatchTask(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	s.sendJSON(w, http.StatusOK, map[string]any{"protocols": nodes.Capabilities()})
}

// handleDeleteMachine 删除机器管理关系（只删 Manager 侧，不卸远端）。
func (s *Server) handleDeleteMachine(w http.ResponseWriter, r *http.Request, machineID string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleGetNode(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleNodeShare(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) nodeAction(w http.ResponseWriter, r *http.Request, nodeUUID, taskType, auditAction, pendingStatus string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleNodeQuota(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleNodeQuotaReset(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func (s *Server) handleNodeIPLimit(w http.ResponseWriter, r *http.Request, nodeUUID string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
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
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authorized 管理员鉴权：支持 Bearer header 与 sbx_token cookie。
func (s *Server) authorized(r *http.Request) bool {
	token := s.cfg.AdminToken
	if token == "" {
		return true
	}
	// 1. Bearer header。
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		given := auth[len("Bearer "):]
		if len(given) == len(token) && constantTimeEq(given, token) {
			return true
		}
	}
	// 2. cookie（前端登录后使用）。
	if c, err := r.Cookie("sbx_token"); err == nil && c.Value != "" {
		if len(c.Value) == len(token) && constantTimeEq(c.Value, token) {
			return true
		}
	}
	return false
}
