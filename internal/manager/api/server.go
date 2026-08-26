// Package api 实现 sbx-manager 的 HTTP 服务（管理员 API + Agent 注册/接入）。
// Phase 2 提供：/healthz、/api/agent/register、/api/enrollment/token。
package api

import (
	"context"
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
	gw.OnTrafficDelta = func(td *protocol.TrafficDelta) {
		// 流量增量入库（防重）。失败仅日志，不影响连接。
		if _, err := traffic.IngestDelta(db.SQL(), *td); err != nil {
			slog.Warn("流量增量入库失败", "machine_id", td.MachineID, "seq", td.Sequence, "err", err)
		}
	}
	return &Server{
		cfg:        cfg,
		db:         db,
		gateway:    gw,
		dispatcher: tasks.NewDispatcher(db.SQL(), gw, 60*time.Second),
	}
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

	case route == "/api/tasks" && r.Method == http.MethodPost:
		s.handleDispatchTask(w, r)

	case route == "/api/tasks" && r.Method == http.MethodGet:
		s.handleListTasks(w, r)

	case route == "/api/nodes" && r.Method == http.MethodGet:
		s.handleListNodes(w, r)

	case route == "/api/nodes" && r.Method == http.MethodPost:
		s.handleCreateNode(w, r)

	case strings.HasPrefix(route, "/api/nodes/") && strings.HasSuffix(route, "/share") &&
		r.Method == http.MethodGet:
		s.handleNodeShare(w, r, route)

	case route == "/api/traffic" && r.Method == http.MethodGet:
		s.handleTraffic(w, r)

	case route == "/api/audit" && r.Method == http.MethodGet:
		s.handleAudit(w, r)

	default:
		s.handleWeb(w, r, route)
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

	// 先校验 token（不消费），失败直接拒绝。
	if _, err := enrollment.Consume(s.db.SQL(), hello.EnrollToken); err != nil {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		return
	}

	// 生成机器身份。
	id, err := auth.NewIdentity()
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 写入机器记录 + 身份 + 消费 token。三件事应原子化；退一步先顺序执行，
	// 失败则回滚身份（删除 agents 行），避免留脏数据。
	m := &machines.Machine{
		MachineID:   id.MachineID,
		Hostname:    hello.Hostname,
		OS:          hello.OS,
		Kernel:      hello.Kernel,
		Arch:        hello.Arch,
		AgentVersion: hello.AgentVersion,
		Status:      "offline",
	}
	if err := machines.Register(s.db.SQL(), m); err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := auth.StoreIdentity(s.db.SQL(), id); err != nil {
		_ = machines.Delete(s.db.SQL(), id.MachineID)
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := enrollment.MarkUsed(s.db.SQL(), hello.EnrollToken, id.MachineID); err != nil {
		_ = machines.Delete(s.db.SQL(), id.MachineID)
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	ack := protocol.HelloAck{
		MachineID:     id.MachineID,
		MachineSecret: id.PrivHex(),
		Accepted:      true,
	}
	s.sendJSON(w, http.StatusOK, ack)
	audit.Log(s.db.SQL(), "agent_register", id.MachineID, "", "ok", clientIP(r))
	slog.Info("机器注册成功", "machine_id", id.MachineID, "hostname", hello.Hostname)
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
		Config    json.RawMessage `json:"config"` // 协议字段（sni/uuid/password/...）
	}
	if err := json.Unmarshal(body, &req); err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.MachineID == "" || req.Protocol == "" || req.Port < 1 || req.Port > 65535 {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "machine_id/protocol/port 必填且端口合法"})
		return
	}

	// 生成节点凭据（占位：Phase 5 实际用 sing-box generate / crypto/rand）。
	cfg := map[string]any{}
	if len(req.Config) > 0 {
		_ = json.Unmarshal(req.Config, &cfg)
	}
	cfg["type"] = req.Protocol
	cfg["port"] = req.Port

	// Manager 记录节点。
	n, err := nodes.Create(s.db.SQL(), req.MachineID, req.Name, req.Protocol, req.Port, cfg)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// 下发 create_node 任务（payload 含节点定义 + node_uuid）。
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
		s.sendJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	audit.Log(s.db.SQL(), "create_node", req.MachineID, n.NodeUUID, "ok", clientIP(r))
	s.sendJSON(w, http.StatusOK, map[string]any{
		"node_uuid": n.NodeUUID,
		"task_id":   taskID,
	})
}

// handleNodeShare 返回节点分享链接（敏感接口，需登录）。
func (s *Server) handleNodeShare(w http.ResponseWriter, r *http.Request, route string) {
	if !s.authorized(r) {
		s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	nodeUUID := strings.TrimSuffix(strings.TrimPrefix(route, "/api/nodes/"), "/share")
	n, err := nodes.Get(s.db.SQL(), nodeUUID)
	if err != nil {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "节点不存在"})
		return
	}
	// 分享链接由 Agent 生成（依赖本机 host 等），Phase 5 先返回节点敏感凭据 JSON。
	var cfg map[string]any
	_ = json.Unmarshal([]byte(n.Config), &cfg)
	s.sendJSON(w, http.StatusOK, map[string]any{
		"node_uuid": n.NodeUUID,
		"config":    cfg,
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
