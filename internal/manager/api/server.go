// Package api 实现 sbx-manager 的 HTTP 服务（管理员 API + Agent 注册/接入）。
// Phase 2 提供：/healthz、/api/agent/register、/api/enrollment/token。
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/config"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/db"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/gateway"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/ips"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/tasks"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/traffic"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// Server 持有 Manager HTTP 服务依赖。
type Server struct {
	cfg        *config.Config
	db         *db.Manager
	gateway    *gateway.Gateway
	dispatcher *tasks.Dispatcher
	// login 是登录失败限速器（内存态）。
	login *loginThrottle
}

// New 构造 Server。
func New(cfg *config.Config, db *db.Manager) *Server {
	gw := gateway.New(db)
	gw.OnTrafficDelta = func(td *protocol.TrafficDelta) error {
		// 流量增量入库（防重）。重复数据返回 accepted=false 但 err=nil，视为成功。
		_, err := traffic.IngestDelta(db.SQL(), *td)
		return err
	}
	gw.OnIPSync = func(snap *protocol.IPSnapshot) {
		// 在线 IP 快照入库（全量替换该节点），失败仅日志不影响连接。
		list := make([]ips.ActiveIP, 0, len(snap.ActiveIPs))
		for _, a := range snap.ActiveIPs {
			list = append(list, ips.ActiveIP{IP: a.IP, Proto: a.Proto, LastSeen: a.LastSeen})
		}
		if err := ips.Sync(db.SQL(), snap.MachineID, snap.NodeUUID, list); err != nil {
			slog.Warn("在线 IP 入库失败", "machine_id", snap.MachineID, "node", snap.NodeUUID, "err", err)
		}
	}
	s := &Server{
		cfg:        cfg,
		db:         db,
		gateway:    gw,
		dispatcher: tasks.NewDispatcher(db.SQL(), gw, 60*time.Second),
	}
	s.dispatcher.OnTaskComplete = s.handleNodeTaskResult
	s.login = newLoginThrottle()
	return s
}

// Gateway 暴露 gateway（供 service 层注入 sweeper/task 相关）。

// Gateway 暴露 gateway（供 service 层注入 sweeper/task 相关）。
func (s *Server) Gateway() *gateway.Gateway { return s.gateway }

// Dispatcher 暴露任务分发器（供 CLI/测试下发任务）。

// Dispatcher 暴露任务分发器（供 CLI/测试下发任务）。
func (s *Server) Dispatcher() *tasks.Dispatcher { return s.dispatcher }

// NewHTTPServer 构造配置好超时的 *http.Server（不启动监听）。

// Handler 返回装配好中间件（安全头 + panic 恢复）的根 handler。
// NewHTTPServer 与测试共用同一条链，避免测试环境绕过安全头。
func (s *Server) Handler() http.Handler {
	return securityHeaders(s.recoverMiddleware(s))
}

// NewHTTPServer 构造配置好超时的 *http.Server（不启动监听）。
func (s *Server) NewHTTPServer() *http.Server {
	addr := net.JoinHostPort(s.cfg.Listen, strconv.Itoa(s.cfg.Port))
	return &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

// StartOfflineSweeper 启动 Manager 的在线状态维护协程。

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

// ServeHTTP 路由分发。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := strings.TrimRight(r.URL.Path, "/")
	if route == "" {
		route = "/"
	}
	// 浏览器登录态下的状态变更请求加 Origin 兜底（防 CSRF 的第二道防线；
	// SameSite=Strict 是第一道）。Agent 与纯 API 调用不带 Cookie，不受影响。
	if browserStateChanging(r) && !strings.HasPrefix(route, "/api/agent/") && route != "/login" {
		if !originAllowed(r) {
			s.sendJSON(w, http.StatusForbidden, map[string]string{"error": "cross-origin request rejected"})
			return
		}
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
	case sub == "active-ips" && r.Method == http.MethodGet:
		s.handleNodeActiveIPs(w, r, id)
	default:
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleWeb 处理前端页面与静态资源（未登录返回登录页）。

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
