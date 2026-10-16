package server

import (
	"path/filepath"
	"encoding/json"
	"strings"
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
	// Load 走真实落盘路径：Update/SyncAccounts 的写回在测试中同样生效
	cfg, err := config.Load(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("测试配置初始化失败: %v", err)
	}
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

// ---- 一次性配对码收号 ----

// fakeUpstream 模拟 lightai 的 conversation/list 探活端点。
func fakeUpstream(t *testing.T, authOK bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !authOK {
			w.Write([]byte(`{"code":500,"message":"NEED_UIN_AND_SKEY"}`))
			return
		}
		w.Write([]byte(`{"code":0,"data":[],"message":"OK"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pairReq(t *testing.T, path, mgmtKey, body string) *http.Request {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest("POST", "/v0/management/api/"+path, nil)
	} else {
		req = httptest.NewRequest("POST", "/v0/management/api/"+path, strings.NewReader(body))
	}
	req.RemoteAddr = "127.0.0.1:51234"
	if mgmtKey != "" {
		req.Header.Set("X-Mgmt-Key", mgmtKey)
	}
	return req
}

// 全链路：生成码 → 书签消费 → 凭证实测 → 落盘进池。
func TestPairFlowHappyPath(t *testing.T) {
	s := newTestServer(t)
	s.cfg.Update(func(c *config.Config) { c.RemoteManagement.SecretKey = "k" })
	up := fakeUpstream(t, true)
	s.cfg.Update(func(c *config.Config) { c.Upstream.BaseURL = up.URL })

	// 无钥匙不能生成
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/code", "", ""))
	if w.Code != 401 {
		t.Fatalf("无钥匙生成码应 401, got %d", w.Code)
	}
	// 生成码
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/code", "k", ""))
	if w.Code != 200 {
		t.Fatalf("生成码失败: %d %s", w.Code, w.Body.String())
	}
	var gen struct{ Code, Bookmark string }
	if err := json.Unmarshal(w.Body.Bytes(), &gen); err != nil || gen.Code == "" {
		t.Fatalf("生成码响应异常: %s", w.Body.String())
	}
	if !strings.Contains(gen.Bookmark, gen.Code) || !strings.Contains(gen.Bookmark, "pair/consume") {
		t.Fatalf("书签未嵌入码或端点: %s", gen.Bookmark)
	}

	// 消费：书签脚本不带管理钥匙（配对码即凭证），text/plain 身体装 JSON
	cookie := `{"code":"` + gen.Code + `","cookie":"uin=o987654; skey=SKY; userId=111222333444"}`
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/consume", "", cookie))
	if w.Code != 200 {
		t.Fatalf("消费失败: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"kind":"console"`) {
		t.Fatalf("应识别控制台态: %s", w.Body.String())
	}
	// CORS 头
	if w.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("消费端点应带 CORS 头")
	}
	// 账号进池
	if snap := s.pool.Snapshot(); len(snap) != 1 || snap[0]["cred_kind"] != "console" {
		t.Fatalf("账号未落盘: %+v", snap)
	}

	// 一次性：同码二次消费应 410
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/consume", "", cookie))
	if w.Code != 410 {
		t.Fatalf("码应一次性, got %d", w.Code)
	}
	// 未知码
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/consume", "", `{"code":"ORCA-DEADBEEF","cookie":"uin=o1; skey=s"}`))
	if w.Code != 410 {
		t.Fatalf("未知码应 410, got %d", w.Code)
	}
	// 坏凭证（无 uin/skey/ot_session）不落盘
	w = httptest.NewRecorder()
	s2 := newTestServer(t)
	_ = s2
	// 重生成一个码测坏 cookie
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/code", "k", ""))
	var gen2 struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &gen2)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/consume", "", `{"code":"`+gen2.Code+`","cookie":"language=zh; other=x"}`))
	if w.Code != 400 {
		t.Fatalf("无登录态 cookie 应 400, got %d", w.Code)
	}
}

// 探活失败（票据失效）拒绝落盘。
func TestPairProbeFailRejects(t *testing.T) {
	s := newTestServer(t)
	s.cfg.Update(func(c *config.Config) { c.RemoteManagement.SecretKey = "k" })
	up := fakeUpstream(t, false) // 上游回 NEED_UIN_AND_SKEY
	s.cfg.Update(func(c *config.Config) { c.Upstream.BaseURL = up.URL })

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/code", "k", ""))
	var gen struct{ Code string }
	json.Unmarshal(w.Body.Bytes(), &gen)

	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/consume", "", `{"code":"`+gen.Code+`","cookie":"uin=o987654; skey=BAD"}`))
	if w.Code != 400 {
		t.Fatalf("坏票应拒收, got %d %s", w.Code, w.Body.String())
	}
	if snap := s.pool.Snapshot(); len(snap) != 0 {
		t.Fatalf("坏票不应落盘: %+v", snap)
	}
	// 坏票消费后码已烧毁（防重放试票）
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, pairReq(t, "pair/consume", "", `{"code":"`+gen.Code+`","cookie":"uin=o987654; skey=BAD"}`))
	if w.Code != 410 {
		t.Fatalf("码应已消费, got %d", w.Code)
	}
}

func TestSplitForTypewriter(t *testing.T) {
	// rune 安全切块：多字节字符不被劈开；小块原样返回
	small := "短文本"
	if got := splitForTypewriter(small); len(got) != 1 || got[0] != small {
		t.Errorf("小块应原样: %v", got)
	}
	long := strings.Repeat("中", 50) // 50 runes, 150 bytes
	parts := splitForTypewriter(long)
	if len(parts) != 4 { // 16+16+16+2
		t.Fatalf("切块数错误: %d", len(parts))
	}
	for i, p := range parts {
		for _, r := range p {
			if r != '中' {
				t.Errorf("块 %d 含非中文字符: %q", i, p)
			}
		}
	}
	if len([]rune(parts[3])) != 2 {
		t.Errorf("末块长度错误: %q", parts[3])
	}
}
