package server

import (
	_ "embed"
	"encoding/json"
	"net/http/pprof"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/ZFXing-lite/orcaterm2api/internal/config"
	"github.com/ZFXing-lite/orcaterm2api/internal/keypool"
	"github.com/ZFXing-lite/orcaterm2api/internal/metrics"
	"github.com/ZFXing-lite/orcaterm2api/internal/orca"
	"github.com/ZFXing-lite/orcaterm2api/internal/proxypool"
	"github.com/ZFXing-lite/orcaterm2api/internal/relay"
)

//go:embed panel.html
var panelHTML string

// Server 网关 HTTP 服务。
type Server struct {
	cfg     *config.Config
	pool    *keypool.Pool
	proxies *proxypool.Pool
	engine  *relay.Engine
	tracker *metrics.Tracker
	paths   *metrics.PathStats
	mux     *http.ServeMux
	pairMu  sync.Mutex
	pairs   map[string]*pairSession
}

// New 创建服务。
func New(cfg *config.Config, pool *keypool.Pool, proxies *proxypool.Pool, tracker *metrics.Tracker) *Server {
	s := &Server{
		cfg:     cfg,
		pool:    pool,
		proxies: proxies,
		engine:  relay.New(cfg, pool, proxies, tracker),
		tracker: tracker,
		paths:   metrics.NewPathStats(),
		mux:     http.NewServeMux(),
		pairs:   map[string]*pairSession{},
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/v1/chat/completions", s.withPathStats("/v1/chat/completions", s.handleChatCompletions))
	s.mux.HandleFunc("/v1/messages", s.withPathStats("/v1/messages", s.handleAnthropicMessages))
	s.mux.HandleFunc("/v1/responses", s.withPathStats("/v1/responses", s.handleResponses))
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/v1/models/", s.handleModelByID)
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/status", s.handleStatus)
	s.mux.HandleFunc("/v0/management/panel", s.handlePanel)
	s.mux.HandleFunc("/v0/management/api/", s.handleManagementAPI)
	// pprof 属于内部诊断端点，与管理面板同权限：仅本地或已认证的管理员可访问
	s.mux.HandleFunc("/debug/pprof/", s.withMgmtAuth(pprof.Index))
	s.mux.HandleFunc("/debug/pprof/goroutine", s.withMgmtAuth(pprof.Handler("goroutine").ServeHTTP))
}

// withMgmtAuth 用管理面板访问策略包裹诊断 handler。
func (s *Server) withMgmtAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.mgmtAllowed(r) || !s.checkMgmtAuth(r) {
			writeJSON(w, 403, map[string]string{"error": "forbidden"})
			return
		}
		h(w, r)
	}
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) withPathStats(path string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		done := s.paths.Begin(path)
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h(rec, r)
		done(rec.status, rec.errMsg)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	errMsg string
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush 透传 http.Flusher：包装后 SSE 流式路径必须仍能逐块冲刷。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---- 下游鉴权 ----

func (s *Server) checkDownstreamAuth(r *http.Request) bool {
	keys := s.cfg.Snapshot().APIKeys
	var active []string
	for _, k := range keys {
		if strings.TrimSpace(k) != "" {
			active = append(active, k)
		}
	}
	if len(active) == 0 {
		return true
	}
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		got := strings.TrimPrefix(auth, "Bearer ")
		for _, k := range active {
			if got == k {
				return true
			}
		}
	}
	if xkey := r.Header.Get("x-api-key"); xkey != "" {
		for _, k := range active {
			if xkey == k {
				return true
			}
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, he *relay.HTTPError) {
	writeJSON(w, he.Status, map[string]any{
		"error": map[string]any{"message": he.Message, "type": he.Code, "code": he.Code},
	})
}

func errToHTTP(err error) *relay.HTTPError {
	if he, ok := err.(*relay.HTTPError); ok {
		return he
	}
	return &relay.HTTPError{Status: 500, Code: "internal_error", Message: err.Error()}
}

// sessionKeyOf 会话粘性键：X-Session-Id > user > metadata 会话字段。
func sessionKeyOf(r *http.Request, req *relay.ChatRequest) string {
	if v := strings.TrimSpace(r.Header.Get("X-Session-Id")); v != "" {
		return "hdr:" + v
	}
	if req.User != "" {
		return "user:" + req.User
	}
	if req.Metadata != nil {
		for _, k := range []string{"conversation_id", "conversationId", "conversationID"} {
			if v, ok := req.Metadata[k].(string); ok && v != "" {
				return "meta:" + v
			}
		}
	}
	return ""
}

// ---- /v1/chat/completions ----

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.checkDownstreamAuth(r) {
		writeOpenAIError(w, &relay.HTTPError{Status: 401, Code: "invalid_api_key", Message: "下游 API Key 无效"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, &relay.HTTPError{Status: 400, Code: "read_error", Message: "读取请求体失败"})
		return
	}
	var req relay.ChatRequest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		writeOpenAIError(w, &relay.HTTPError{Status: 400, Code: "invalid_request", Message: "请求体不是合法 JSON: " + err.Error()})
		return
	}
	// ToolChoice/ParallelToolCalls 可能是多种 JSON 形态，保留原始值
	var raw map[string]any
	if json.Unmarshal(body, &raw) == nil {
		if v, ok := raw["tool_choice"]; ok {
			req.ToolChoice = v
		}
		if v, ok := raw["parallel_tool_calls"]; ok {
			req.ParallelToolCalls = v
		}
	}

	resp, err := s.engine.Complete(r.Context(), &req, sessionKeyOf(r, &req))
	if err != nil {
		writeOpenAIError(w, errToHTTP(err))
		return
	}
	if len(resp.UnsupportedTools) > 0 {
		w.Header().Set("X-OrcaTerm-Unsupported-Tools", strings.Join(resp.UnsupportedTools, ","))
	}
	if resp.UpstreamAction != "" {
		w.Header().Set("X-OrcaTerm-Action", resp.UpstreamAction)
	}
	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		relay.WriteSSE(w, resp, includeUsage)
		return
	}
	writeJSON(w, 200, resp.ToOpenAIResponse())
}

// ---- /v1/models ----

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	items := make([]relay.ModelInfo, 0, len(orca.Models)+1)
	for _, m := range orca.Models {
		items = append(items, relay.ModelInfo{ID: m.ID, Object: "model", Created: 1760000000, OwnedBy: "orcaterm"})
	}
	items = append(items, relay.ModelInfo{ID: orca.AliasModel, Object: "model", Created: 1760000000, OwnedBy: "orcaterm"})
	writeJSON(w, 200, map[string]any{"object": "list", "data": items})
}

func (s *Server) handleModelByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/models/")
	for _, m := range orca.Models {
		if m.ID == id || m.Upstream == id {
			writeJSON(w, 200, relay.ModelInfo{ID: m.ID, Object: "model", Created: 1760000000, OwnedBy: "orcaterm"})
			return
		}
	}
	if id == orca.AliasModel {
		writeJSON(w, 200, map[string]any{
			"id": orca.AliasModel, "object": "model", "created": 1760000000, "owned_by": "orcaterm",
			"alias_of": s.cfg.Snapshot().Upstream.DefaultModel, "deprecated": true,
		})
		return
	}
	writeJSON(w, 404, map[string]any{"error": map[string]any{"message": "模型不存在: " + id, "code": "model_not_found"}})
}

// ---- /healthz /status ----

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ready, total := s.pool.Count()
	writeJSON(w, 200, map[string]any{
		"service": "orcaterm2api",
		"ready":   s.pool.Ready(),
		"healthy": ready,
		"total":   total,
		"proxies": s.proxies.Count(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !s.checkDownstreamAuth(r) {
		writeOpenAIError(w, &relay.HTTPError{Status: 401, Code: "invalid_api_key", Message: "下游 API Key 无效"})
		return
	}
	snap := s.cfg.Snapshot()
	ready, total := s.pool.Count()
	writeJSON(w, 200, map[string]any{
		"service":       "orcaterm2api",
		"ready":         s.pool.Ready(),
		"healthy":       ready,
		"total":         total,
		"default_model": snap.Upstream.DefaultModel,
		"force_model":   snap.Upstream.ForceModel,
		"base_url":      snap.Upstream.BaseURL,
		"accounts":      s.pool.Snapshot(),
		"proxies":       s.proxies.Snapshot(),
		"paths":         s.paths.Snapshot(),
		"usage":         s.tracker.Snapshot(),
	})
}

// ---- 管理面板 ----

// handlePanel 管理面板入口。
func (s *Server) handlePanel(w http.ResponseWriter, r *http.Request) {
	if !s.mgmtAllowed(r) {
		writeJSON(w, 403, map[string]string{"error": "管理面板仅允许本地访问（remote-management.allow-remote=false 或未设置 secret-key）"})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(panelHTML))
}

func (s *Server) mgmtAllowed(r *http.Request) bool {
	rm := s.cfg.Snapshot().RemoteManagement
	// 未设置面板密码：无论 allow-remote 为何值，都只允许本地回环访问，
	// 避免默认配置下管理接口在公网裸奔。
	if rm.SecretKey == "" {
		return isLoopbackRequest(r)
	}
	if rm.AllowRemote {
		return true
	}
	return isLoopbackRequest(r)
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkMgmtAuth 校验管理面板密码（X-Mgmt-Key 或 Bearer）。
func (s *Server) checkMgmtAuth(r *http.Request) bool {
	secret := s.cfg.Snapshot().RemoteManagement.SecretKey
	if secret == "" {
		return true // 能走到这里说明 mgmtAllowed 已确认是本地回环
	}
	if got := r.Header.Get("X-Mgmt-Key"); got == secret {
		return true
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if strings.TrimPrefix(auth, "Bearer ") == secret {
			return true
		}
	}
	return false
}
