package api

// 管理面 HTTP 层的行为守卫：鉴权 fail-closed、登录流程、限速、
// Origin 兜底与安全响应头。这些是公网面板的第一道墙，
// 与 handler 的业务正确性同等重要。

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/config"
	"github.com/k6nfmm7dbr-commits/sbx-pro/internal/manager/db"
)

const testToken = "unit-test-admin-token"

func newTestServer(t *testing.T, token string) (*Server, *httptest.Server) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	cfg := &config.Config{
		DB:         filepath.Join(t.TempDir(), "x"),
		Listen:     "127.0.0.1",
		Port:       0,
		AdminToken: token,
		TokenTTL:   900,
	}
	s := New(cfg, d)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func doReq(ts *httptest.Server, method, path, body string, hdr map[string]string) *http.Response {
	t := httptest.NewRecorder()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	ts.Client().CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse // 登录是 302，不跟随
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		return t.Result()
	}
	return resp
}

func TestHealthzOpen(t *testing.T) {
	_, ts := newTestServer(t, testToken)
	resp := doReq(ts, http.MethodGet, "/healthz", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz 应无需鉴权，实得 %d", resp.StatusCode)
	}
}

// 无令牌 / 错令牌必须 401；对令牌一票通过或 fail-open 的回归都要拦住。
func TestAdminAPIRequiresToken(t *testing.T) {
	_, ts := newTestServer(t, testToken)
	cases := []struct {
		name string
		hdr  map[string]string
		want int
	}{
		{"无凭据", nil, http.StatusUnauthorized},
		{"错令牌", map[string]string{"Authorization": "Bearer wrong"}, http.StatusUnauthorized},
		{"对令牌", map[string]string{"Authorization": "Bearer " + testToken}, http.StatusOK},
		{"对 Cookie", map[string]string{"Cookie": "sbx_token=" + testToken}, http.StatusOK},
	}
	for _, c := range cases {
		resp := doReq(ts, http.MethodGet, "/api/machines", "", c.hdr)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
}

// 管理令牌未配置时必须 fail-closed（503），而不是把整个管理面裸奔到公网。
func TestAdminAPIFailClosedWithoutToken(t *testing.T) {
	_, ts := newTestServer(t, "")
	resp := doReq(ts, http.MethodGet, "/api/machines", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("空令牌应 503，实得 %d", resp.StatusCode)
	}
	// 登录页仍可达（用户要能先去配置）。
	resp = doReq(ts, http.MethodGet, "/login", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("空令牌下登录页应可达，实得 %d", resp.StatusCode)
	}
}

func TestLoginFlow(t *testing.T) {
	_, ts := newTestServer(t, testToken)
	// 错令牌 → 302 回登录页，不带 Set-Cookie
	resp := doReq(ts, http.MethodPost, "/login", "token=wrong", nil)
	if resp.StatusCode != http.StatusFound || !strings.Contains(resp.Header.Get("Location"), "/login?error=1") {
		t.Fatalf("错令牌应 302 回登录页，实得 %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("错令牌不应下发 Cookie")
	}
	resp.Body.Close()

	// 对令牌 → 302 回首页 + HttpOnly Cookie
	resp = doReq(ts, http.MethodPost, "/login", "token="+url.QueryEscape(testToken), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("对令牌应 302 回首页，实得 %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "sbx_token" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("登录成功应下发 sbx_token Cookie")
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("Cookie 必须 HttpOnly + SameSite=Strict: %+v", cookie)
	}
}

// 连续失败达到阈值后锁定：即使后续给对令牌也拒绝，直到锁定窗口过去。
func TestLoginRateLimit(t *testing.T) {
	s, _ := newTestServer(t, testToken)
	// 直接驱动限速器，避免测试依赖具体阈值数字。
	ip := "203.0.113.7"
	for i := 0; i < s.login.maxFails; i++ {
		s.login.Fail(ip)
	}
	if !s.login.Locked(ip) {
		t.Fatal("达到阈值后应锁定")
	}
	if s.login.Locked("198.51.100.1") {
		t.Fatal("其他 IP 不应被连带锁定")
	}
	s.login.Reset(ip)
	if s.login.Locked(ip) {
		t.Fatal("Reset 后应解除锁定")
	}
}

// 登录态下的跨站状态变更请求必须被 Origin 兜底拦下；
// 同源（或无 Origin 的 API 调用）不受影响。
func TestOriginCheck(t *testing.T) {
	_, ts := newTestServer(t, testToken)
	auth := map[string]string{"Cookie": "sbx_token=" + testToken}
	body := `{"machine_id":"m","task_type":"noop"}`

	resp := doReq(ts, http.MethodPost, "/api/tasks", body,
		map[string]string{"Cookie": auth["Cookie"], "Origin": "https://evil.example.com"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("跨站 Origin 应 403，实得 %d", resp.StatusCode)
	}

	resp = doReq(ts, http.MethodPost, "/api/tasks", body, auth)
	resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Fatal("无 Origin 的 API 调用不应被 Origin 检查拦截")
	}
}

// 安全响应头对所有响应生效。
func TestSecurityHeaders(t *testing.T) {
	_, ts := newTestServer(t, testToken)
	resp := doReq(ts, http.MethodGet, "/healthz", "", nil)
	defer resp.Body.Close()
	for _, h := range []string{"X-Frame-Options", "Referrer-Policy", "Permissions-Policy", "X-Content-Type-Options"} {
		if resp.Header.Get(h) == "" {
			t.Errorf("缺少安全头 %s", h)
		}
	}
}

// Agent 注册走的是 enrollment token 而非管理令牌：错 token 必须 401。
func TestAgentRegisterBadEnrollToken(t *testing.T) {
	_, ts := newTestServer(t, testToken)
	body := `{"enroll_token":"bad","public_key":"` + strings.Repeat("ab", 32) + `"}`
	resp := doReq(ts, http.MethodPost, "/api/agent/register", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误的 enrollment token 应 401，实得 %d", resp.StatusCode)
	}
}
