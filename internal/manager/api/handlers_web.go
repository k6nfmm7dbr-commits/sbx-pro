// Package api —— 前端页面与登录处理（从 server.go 拆出）。
package api

import (
	"io"
	"net/http"
	"net/url"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/audit"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/webui"
)

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
	case route == "/favicon.ico":
		// 浏览器后台请求的图标：返回 204 避免 404 JSON 噪音。
		w.WriteHeader(http.StatusNoContent)

	default:
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

// handleLogin 处理管理员登录（POST /login，token 表单）。

// handleLogin 处理管理员登录（POST /login，token 表单）。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil || len(body) > 64<<10 {
		http.Redirect(w, r, "/login?error=1", http.StatusFound)
		return
	}
	if s.login.Locked(clientIP(r)) {
		http.Redirect(w, r, "/login?error=locked", http.StatusFound)
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
		s.login.Reset(clientIP(r))
		audit.Log(s.db.SQL(), "admin_login", "", "", "ok", clientIP(r))
		http.SetCookie(w, &http.Cookie{
			Name: "sbx_token", Value: given, Path: "/",
			MaxAge: 604800, HttpOnly: true, SameSite: http.SameSiteStrictMode,
		})
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	// 失败也留痕：便于事后发现爆破行为。
	audit.Log(s.db.SQL(), "admin_login", "", "", "failed", clientIP(r))
	if s.login.Locked(clientIP(r)) {
		// 达到失败阈值后的窗口期内直接拒绝，不给继续尝试的机会。
		http.Redirect(w, r, "/login?error=locked", http.StatusFound)
		return
	}
	s.login.Fail(clientIP(r))
	http.Redirect(w, r, "/login?error=1", http.StatusFound)
}

// assetBytes 从内嵌前端读取文件。

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
