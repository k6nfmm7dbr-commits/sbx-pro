package api

// 管理面安全加固，对齐 sbx 单机版的基线：
//   - 管理鉴权 fail-closed（令牌未配置时拒绝而不是放行）
//   - 登录失败限速（按来源 IP，内存态，无需持久化）
//   - 浏览器状态变更请求的 Origin 兜底（Cookie + SameSite=Strict 之外的
//     第二道防线；API 调用方不带 Cookie、通常也不带 Origin，不受影响）
//   - 统一安全响应头

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// adminAuthed 是所有管理 API 的统一入口：令牌未配置时 fail-closed，
// 已配置但不对时 401。返回 true 表示放行。
func (s *Server) adminAuthed(w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(s.cfg.AdminToken) == "" {
		// 明确拒绝而不是放行：一个「忘配令牌的面板」在公网上等于
		// 无密码管理后台，宁可让用户先去配置。
		s.sendJSON(w, http.StatusServiceUnavailable,
			map[string]string{"error": "管理令牌未配置；请先在配置中设置 admin_token 再使用管理 API"})
		return false
	}
	if s.authorized(r) {
		return true
	}
	s.sendJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	return false
}

// loginThrottle 记录每个来源 IP 的连续登录失败，超限后锁定一段时间。
// 内存态即可：重启清零不构成额外风险（攻击者重开窗口的成本远大于收益），
// 也不需要为它建表。
type loginThrottle struct {
	mu       sync.Mutex
	fails    map[string][]time.Time // ip -> 窗口内的失败时间
	window   time.Duration
	maxFails int
	lockFor  time.Duration
	locked   map[string]time.Time // ip -> 锁定截止
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		fails:    map[string][]time.Time{},
		window:   10 * time.Minute,
		maxFails: 5,
		lockFor:  10 * time.Minute,
		locked:   map[string]time.Time{},
	}
}

// Locked 报告该 IP 当前是否被锁定。
func (t *loginThrottle) Locked(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if until, ok := t.locked[ip]; ok {
		if time.Now().Before(until) {
			return true
		}
		delete(t.locked, ip)
		delete(t.fails, ip)
	}
	return false
}

// Fail 记录一次失败；达到阈值即锁定。
func (t *loginThrottle) Fail(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	keep := t.fails[ip][:0]
	for _, ts := range t.fails[ip] {
		if now.Sub(ts) < t.window {
			keep = append(keep, ts)
		}
	}
	keep = append(keep, now)
	t.fails[ip] = keep
	if len(keep) >= t.maxFails {
		t.locked[ip] = now.Add(t.lockFor)
		delete(t.fails, ip)
	}
}

// Reset 登录成功后清空该 IP 的失败记录与锁定状态。
func (t *loginThrottle) Reset(ip string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, ip)
	delete(t.locked, ip)
}

// browserStateChanging 识别「浏览器发起的状态变更请求」：
// 带 Cookie（登录态）且方法会改数据。这类请求需要 Origin 兜底。
// API 调用方（Bearer / 无 Cookie）与 Agent 注册（无 Cookie）不受影响。
func browserStateChanging(r *http.Request) bool {
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
	default:
		return false
	}
	_, hasCookie := r.Cookie("sbx_token")
	return hasCookie == nil
}

// originAllowed 校验 Origin 与请求 Host 同源；无 Origin 视为非浏览器，放行。
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	return origin == scheme+"://"+host ||
		// 反代场景 scheme 可能判断错，退一步：只要 Origin 的 host 部分与
		// 请求 Host 一致就放行（scheme 不同通常是我们检测失误，不是攻击）。
		strings.HasSuffix(origin, "://"+host)
}

// securityHeaders 统一安全响应头。对 HTML/JSON/静态资源一视同仁；
// HSTS 只在 TLS（直连或反代 X-Forwarded-Proto）下追加。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if h.Get("X-Content-Type-Options") == "" {
			h.Set("X-Content-Type-Options", "nosniff")
		}
		if h.Get("Content-Security-Policy") == "" {
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:")
		}
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}
