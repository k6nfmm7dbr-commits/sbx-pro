// Package api 实现 sbx-manager 的 HTTP 服务（管理员 API + Agent 注册/接入）。
// Phase 2 提供：/healthz、/api/agent/register、/api/enrollment/token。
package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/auth"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/config"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/db"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/enrollment"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/machines"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/protocol"
)

// Server 持有 Manager HTTP 服务依赖。
type Server struct {
	cfg *config.Config
	db  *db.Manager
}

// New 构造 Server。
func New(cfg *config.Config, db *db.Manager) *Server {
	return &Server{cfg: cfg, db: db}
}

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

	case route == "/api/enrollment/token" && r.Method == http.MethodPost:
		s.handleEnrollmentToken(w, r)

	case route == "/api/machines" && r.Method == http.MethodGet:
		s.handleMachines(w, r)

	default:
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
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

// authorized 管理员鉴权：Bearer token（Phase 2 简化为常量时间比较）。
func (s *Server) authorized(r *http.Request) bool {
	token := s.cfg.AdminToken
	if token == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		given := auth[len("Bearer "):]
		if len(given) == len(token) && constantTimeEq(given, token) {
			return true
		}
	}
	return false
}
