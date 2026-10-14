package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ZFXing-lite/orcaterm2api/internal/config"
	"github.com/ZFXing-lite/orcaterm2api/internal/keypool"
	"github.com/ZFXing-lite/orcaterm2api/internal/metrics"
	"github.com/ZFXing-lite/orcaterm2api/internal/proxypool"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Defaults()
	pool := keypool.New(keypool.Options{FlushEvery: 0})
	t.Cleanup(pool.Close)
	proxies := proxypool.New("round-robin", 0, 0)
	t.Cleanup(proxies.Close)
	tracker := metrics.New(false)
	t.Cleanup(tracker.Close)
	return New(cfg, pool, proxies, tracker)
}

// statusRecorder 必须透传 http.Flusher，否则 SSE 流式响应会被缓冲到结束。
func TestStatusRecorderImplementsFlusher(t *testing.T) {
	w := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	if _, ok := interface{}(rec).(http.Flusher); !ok {
		t.Fatal("statusRecorder 未实现 http.Flusher，SSE 流式会被缓冲")
	}
	rec.Flush() // 不应 panic
}

// 空面板密码时管理端点仅限回环访问，允许远程也不放开。
func TestMgmtAllowedEmptySecretLoopbackOnly(t *testing.T) {
	s := newTestServer(t)

	remote := httptest.NewRequest("GET", "/v0/management/panel", nil)
	remote.RemoteAddr = "203.0.113.9:51234"
	local := httptest.NewRequest("GET", "/v0/management/panel", nil)
	local.RemoteAddr = "127.0.0.1:51234"

	if s.cfg.Snapshot().RemoteManagement.SecretKey != "" {
		t.Fatal("前置条件异常：默认密码应为空")
	}
	if s.mgmtAllowed(remote) {
		t.Fatal("空密码应拒绝远程访问")
	}
	if !s.mgmtAllowed(local) {
		t.Fatal("空密码应允许本机访问")
	}

	// 设置密码后按 allow-remote 决定
	s.cfg.Update(func(c *config.Config) { c.RemoteManagement.SecretKey = "k" })
	if !s.mgmtAllowed(remote) {
		t.Fatal("设置了密码且 allow-remote=true 应允许远程")
	}
}

// pprof 属于诊断端点，必须走管理鉴权。
func TestPprofRequiresMgmtAuth(t *testing.T) {
	s := newTestServer(t)
	s.cfg.Update(func(c *config.Config) { c.RemoteManagement.SecretKey = "test-secret" })
	h := s.Handler()

	remote := httptest.NewRequest("GET", "/debug/pprof/goroutine", nil)
	remote.RemoteAddr = "203.0.113.9:51234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, remote)
	if w.Code != 403 && w.Code != 401 {
		t.Fatalf("远程未鉴权访问 pprof 应被拒绝，得到 %d", w.Code)
	}

	// 无密码仅本地可访问
	s2 := newTestServer(t)
	w = httptest.NewRecorder()
	local := httptest.NewRequest("GET", "/debug/pprof/goroutine", nil)
	local.RemoteAddr = "127.0.0.1:51234"
	s2.Handler().ServeHTTP(w, local)
	if w.Code == 403 || w.Code == 401 {
		t.Fatalf("本地无密码访问 pprof 不应被拒，得到 %d", w.Code)
	}
}

// query 传密码的旁路必须关闭：违反鉴权时返回 401。
func TestMgmtRejectsQueryKey(t *testing.T) {
	s := newTestServer(t)
	s.cfg.Update(func(c *config.Config) { c.RemoteManagement.SecretKey = "test-secret" })
	h := s.Handler()

	remote := httptest.NewRequest("GET", "/v0/management/api/settings?key=test-secret", nil)
	remote.RemoteAddr = "203.0.113.9:51234"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, remote)
	if w.Code != 401 {
		t.Fatalf("携 query key 不应通过鉴权，应得到 401，得到 %d", w.Code)
	}
}
