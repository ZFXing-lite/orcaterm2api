package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ZFXing-lite/orcaterm2api/internal/config"
	"github.com/ZFXing-lite/orcaterm2api/internal/keypool"
	"github.com/ZFXing-lite/orcaterm2api/internal/metrics"
	"github.com/ZFXing-lite/orcaterm2api/internal/orca"
	"github.com/ZFXing-lite/orcaterm2api/internal/proxypool"
)

// Engine 聊天编排引擎。
type Engine struct {
	cfg     *config.Config
	pool    *keypool.Pool
	proxies *proxypool.Pool
	tracker *metrics.Tracker

	mu       sync.Mutex
	sessions map[string]*session // sessionKey -> session
	pending  map[string]*pendingCall

	clientMu sync.Mutex
	clients  map[string]*orca.Client // accountID -> client（绑定代理 transport）
}

// session 会话：固定账号 + 上游 conversationId。
type session struct {
	AccountID      string
	ConversationID string
	LastUsed       time.Time
}

// pendingCall 待回填的工具调用。
type pendingCall struct {
	AccountID      string
	ConversationID string
	ToolName       string
	UpstreamModel  string
	CreatedAt      time.Time
}

// New 创建引擎。
func New(cfg *config.Config, pool *keypool.Pool, proxies *proxypool.Pool, tracker *metrics.Tracker) *Engine {
	e := &Engine{
		cfg:      cfg,
		pool:     pool,
		proxies:  proxies,
		tracker:  tracker,
		sessions: map[string]*session{},
		pending:  map[string]*pendingCall{},
		clients:  map[string]*orca.Client{},
	}
	go e.janitor()
	return e
}

func (e *Engine) janitor() {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for range t.C {
		ttl := config.ParseDuration(e.cfg.Snapshot().Sessions.TTL, 30*time.Minute)
		now := time.Now()
		e.mu.Lock()
		for k, s := range e.sessions {
			if now.Sub(s.LastUsed) > ttl {
				delete(e.sessions, k)
			}
		}
		for id, p := range e.pending {
			if now.Sub(p.CreatedAt) > ttl {
				delete(e.pending, id)
			}
		}
		e.mu.Unlock()
	}
}

func newID(prefix string) string {
	var b [12]byte
	rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// clientFor 取账号绑定的上游客户端（含代理）。
// 缓存 key 覆盖全部构造参数，配置热重载后自动使用新 client。
// NoAskBehaviorNote 无人值守行为指令：抑制上游默认的反问（ask）倾向。
// 语义判断交给上游模型——用户明确要讨论时它仍可提问。
const NoAskBehaviorNote = "【运行模式】你正处于无人值守模式：默认直接执行用户的任务并给出最终结果，不要反问用户、不要请求确认、不要要求用户补充信息（信息不足时自行做出合理假设并在结果开头简要说明所做假设）；仅当用户明确表示想讨论、征求意见或等待你确认时，才允许向用户提问。"

// behaviorNote 按配置返回行为指令（空串=不注入）。
func behaviorNote(snap config.Config) string {
	if !snap.Upstream.NoAsk {
		return ""
	}
	return NoAskBehaviorNote
}

// autoConfirmAsk 非流式兜底：上游仍然 ask 时自动回填一轮「直接执行」。
// 仅一次；确认后仍 ask 则把问题原样交给下游（用户可看到并回答）。
func (e *Engine) autoConfirmAsk(ctx context.Context, snap config.Config, acct *keypool.Account, oreq orca.ChatRequest, outcome *orca.Outcome) *orca.Outcome {
	if outcome.Action != "ask" || !snap.Upstream.NoAsk {
		return outcome
	}
	confirm := oreq
	confirm.Text = "是的，请直接执行，无需再向我确认。"
	confirm.Images = nil
	confirm.BehaviorNote = behaviorNote(snap)
	second, err := e.callOne(ctx, acct, confirm)
	if err != nil || second == nil {
		return outcome // 确认轮失败：退回原问题
	}
	return second
}

func (e *Engine) clientFor(a *keypool.Account) *orca.Client {
	snap := e.cfg.Snapshot()
	timeout := config.ParseDuration(snap.Upstream.Timeout, 300*time.Second)

	var pr *proxypool.Proxy
	switch {
	case a.Proxy == "none":
		pr = nil
	case a.Proxy != "":
		if parsed, err := proxypool.Parse(a.Proxy); err == nil {
			pr = parsed
		} else {
			pr = e.proxies.Pick("")
		}
	default:
		pr = e.proxies.Pick("")
	}
	proxyURL := ""
	if pr != nil {
		proxyURL = pr.URL
	}
	key := strings.Join([]string{a.ID, snap.Upstream.BaseURL, snap.Upstream.Product, snap.Upstream.Origin, snap.Upstream.UserAgent, timeout.String(), proxyURL}, "\x00")

	e.clientMu.Lock()
	defer e.clientMu.Unlock()
	if c, ok := e.clients[key]; ok {
		return c
	}
	c := orca.NewClient(snap.Upstream.BaseURL, snap.Upstream.Product, snap.Upstream.Origin, snap.Upstream.UserAgent, newHTTPClient(timeout, pr))
	e.clients[key] = c
	return c
}

// Complete 执行一轮聊天：返回最终文本或工具调用。
func (e *Engine) Complete(ctx context.Context, req *ChatRequest, sessionKey string) (*ChatResponse, error) {
	snap := e.cfg.Snapshot()

	// ---- 模型解析 ----
	modelName := req.Model
	if snap.Upstream.ForceModel || modelName == "" {
		modelName = snap.Upstream.DefaultModel
	}
	upstreamModel, err := orca.ResolveModel(modelName, defaultUpstreamModel(snap), snap.Upstream.AllowUnknownModel)
	if err != nil {
		return nil, &HTTPError{Status: 404, Code: "model_not_found", Message: err.Error()}
	}

	// ---- 工具声明校验（上游能力边界）----
	if err := validateTools(req); err != nil {
		return nil, err
	}
	unsupported := unsupportedTools(req.Tools)

	// ---- 工具结果回填优先 ----
	if results := e.extractToolResults(req); len(results) > 0 {
		return e.continueToolTurn(ctx, snap, req, results, sessionKey, unsupported)
	}

	// ---- 组装本轮输入（上游服务端保存历史，只发本轮）----
	if sessionKey != "" {
		return e.sessionTurn(ctx, snap, req, sessionKey, modelName, upstreamModel, unsupported)
	}
	return e.pooledTurn(ctx, snap, req, modelName, upstreamModel, unsupported)
}

// sessionTurn 会话粘性路径：固定账号，不跨号重试。
func (e *Engine) sessionTurn(ctx context.Context, snap config.Config, req *ChatRequest, sessionKey, modelName, upstreamModel string, unsupported []string) (*ChatResponse, error) {
	convID, acct, isNew := e.resolveSession(sessionKey)
	if acct == nil {
		return nil, &HTTPError{Status: 503, Code: "no_upstream_account", Message: "无可用上游账号"}
	}
	defer e.pool.Release(acct) // resolveSession 内 Pick 已占用

	text, images := buildTurnInput(req, isNew)
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return nil, &HTTPError{Status: 400, Code: "empty_input", Message: "消息内容为空"}
	}
	outcome, err := e.callOne(ctx, acct, orca.ChatRequest{
		ConversationID: convID,
		Text:           text,
		Images:         images,
		UpstreamModel:  upstreamModel,
		HideTools:      snap.Upstream.HideTools,
		EnableTools:    len(req.Tools) > 0,
		BehaviorNote:   behaviorNote(snap),
	})
	if err != nil {
		return nil, err
	}
	outcome = e.autoConfirmAsk(ctx, snap, acct, orca.ChatRequest{
		ConversationID: convID,
		UpstreamModel:  upstreamModel,
		HideTools:      snap.Upstream.HideTools,
		EnableTools:     len(req.Tools) > 0,
		BehaviorNote:    behaviorNote(snap),
	}, outcome)
	resp := e.buildResponse(req, snap, outcome, acct.ID, upstreamModel, sessionKey, convID, unsupported, text)
	e.track(acct.ID, modelName, resp.Usage, nil)
	return resp, nil
}

// pooledTurn 无会话路径：账号池跨号重试。
func (e *Engine) pooledTurn(ctx context.Context, snap config.Config, req *ChatRequest, modelName, upstreamModel string, unsupported []string) (*ChatResponse, error) {
	text, images := buildTurnInput(req, true)
	if strings.TrimSpace(text) == "" && len(images) == 0 {
		return nil, &HTTPError{Status: 400, Code: "empty_input", Message: "消息内容为空"}
	}
	convID := orca.NewConversationID(snap.Upstream.Product)
	oreq := orca.ChatRequest{
		ConversationID: convID,
		Text:           text,
		Images:         images,
		UpstreamModel:  upstreamModel,
		HideTools:      snap.Upstream.HideTools,
		EnableTools:    len(req.Tools) > 0,
		BehaviorNote:   behaviorNote(snap),
	}
	res, acct, err := e.pool.DoWithRetry(ctx, func(a *keypool.Account) (any, bool, int, error) {
		out, uerr := e.callOne(ctx, a, oreq)
		if uerr != nil {
			retryable, status, herr := e.retryableOf(uerr)
			return nil, retryable, status, herr
		}
		out = e.autoConfirmAsk(ctx, snap, a, oreq, out)
		return out, false, 200, nil
	})
	if err != nil {
		if he, ok := err.(*HTTPError); ok {
			return nil, he
		}
		return nil, &HTTPError{Status: 502, Code: "upstream_error", Message: err.Error()}
	}
	outcome := res.(*orca.Outcome)
	resp := e.buildResponse(req, snap, outcome, acct.ID, upstreamModel, "", convID, unsupported, text)
	e.track(acct.ID, modelName, resp.Usage, nil)
	return resp, nil
}

// continueToolTurn 工具结果回填：沿用 pending 的账号与会话。
func (e *Engine) continueToolTurn(ctx context.Context, snap config.Config, req *ChatRequest, results []toolResult, sessionKey string, unsupported []string) (*ChatResponse, error) {
	if len(results) > 1 {
		return nil, &HTTPError{Status: 400, Code: "multiple_tool_results", Message: "一次只能回传一个工具结果"}
	}
	r := results[0]
	e.mu.Lock()
	pc, ok := e.pending[r.ToolCallID]
	if ok {
		delete(e.pending, r.ToolCallID)
	}
	e.mu.Unlock()
	if !ok {
		// 不是本网关发出的调用：当作普通输入
		return e.Complete(ctx, &ChatRequest{
			Model: req.Model,
			Messages: []Message{
				{Role: "user", Content: r.Content},
			},
			User:     req.User,
			Metadata: req.Metadata,
		}, sessionKey)
	}
	acct := e.pool.Get(pc.AccountID)
	if acct == nil || acct.Disabled || !acct.Credential().Ready() {
		return nil, &HTTPError{Status: 410, Code: "session_expired", Message: "工具调用的账号已不可用，请重新发起对话"}
	}
	e.pool.Acquire(acct)
	defer e.pool.Release(acct)
	input := orca.BuildToolResultPrompt(pc.ToolName, r.Content)
	outcome, err := e.callOne(ctx, acct, orca.ChatRequest{
		ConversationID: pc.ConversationID,
		Text:           input,
		UpstreamModel:  pc.UpstreamModel,
		HideTools:      snap.Upstream.HideTools,
		EnableTools:    true,
	})
	if err != nil {
		return nil, err
	}
	resp := e.buildResponse(req, snap, outcome, acct.ID, pc.UpstreamModel, sessionKey, pc.ConversationID, unsupported, input)
	e.track(acct.ID, req.Model, resp.Usage, nil)
	return resp, nil
}

// resolveSession 会话粘性：命中即定；账号不可用则建新会话。
func (e *Engine) resolveSession(sessionKey string) (convID string, acct *keypool.Account, isNew bool) {
	snap := e.cfg.Snapshot()
	e.mu.Lock()
	s, ok := e.sessions[sessionKey]
	if ok {
		s.LastUsed = time.Now()
	}
	e.mu.Unlock()
	if ok {
		if a := e.pool.Get(s.AccountID); a != nil && !a.Disabled && a.Credential().Ready() {
			e.pool.Acquire(a)
			return s.ConversationID, a, false
		}
	}
	a := e.pool.Pick() // Pick 已占用 InFlight
	if a == nil {
		return "", nil, true
	}
	convID = orca.NewConversationID(snap.Upstream.Product)
	e.mu.Lock()
	max := snap.Sessions.Max
	if max <= 0 {
		max = 1000
	}
	if len(e.sessions) >= max {
		var oldest string
		var oldestT time.Time
		for k, ss := range e.sessions {
			if oldest == "" || ss.LastUsed.Before(oldestT) {
				oldest, oldestT = k, ss.LastUsed
			}
		}
		delete(e.sessions, oldest)
	}
	e.sessions[sessionKey] = &session{AccountID: a.ID, ConversationID: convID, LastUsed: time.Now()}
	e.mu.Unlock()
	return convID, a, true
}

// callOne 在固定账号上单次调用上游。
func (e *Engine) callOne(ctx context.Context, acct *keypool.Account, oreq orca.ChatRequest) (*orca.Outcome, error) {
	outcome, err := e.clientFor(acct).Chat(ctx, acct.Credential(), oreq)
	if err != nil {
		retryable, status, herr := e.retryableOf(err)
		_ = retryable
		kind := keypool.FailServerError
		switch {
		case isAuthError(err):
			kind = keypool.FailAuthExpired
		case status == 429:
			kind = keypool.FailRateLimited
		case status == 0:
			kind = keypool.FailTransport
		}
		e.pool.ReportFailure(acct, kind, err.Error(), 0)
		if he, ok := herr.(*HTTPError); ok {
			return nil, he
		}
		return nil, herr
	}
	e.pool.ReportSuccess(acct)
	return outcome, nil
}

// retryableOf 把上游错误映射为 (retryable, httpStatus, httpErr)。
// 返回的 httpErr 在跨号重试耗尽后原样透传给下游，状态码保留上游语义。
func (e *Engine) retryableOf(err error) (bool, int, error) {
	if _, ok := err.(*orca.AuthError); ok {
		return true, 401, &HTTPError{Status: 401, Code: "upstream_auth_expired", Message: err.Error()} // 换号重试（该号会被停用）
	}
	if be, ok := err.(*orca.BusinessError); ok {
		if be.Code == 429 {
			return true, 429, &HTTPError{Status: 429, Code: "rate_limited", Message: be.Error()}
		}
		if be.Code >= 500 {
			return true, be.Code, &HTTPError{Status: 502, Code: "upstream_error", Message: be.Error()}
		}
		return false, be.Code, &HTTPError{Status: 502, Code: "upstream_business_error", Message: be.Error()}
	}
	if _, ok := err.(*orca.ProtocolError); ok {
		return false, 0, &HTTPError{Status: 502, Code: "upstream_protocol_error", Message: err.Error()}
	}
	return true, 0, &HTTPError{Status: 502, Code: "upstream_transport_error", Message: err.Error()}
}

func isAuthError(err error) bool {
	for err != nil {
		if _, ok := err.(*orca.AuthError); ok {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// buildResponse 把上游结果转为网关响应。
func (e *Engine) buildResponse(req *ChatRequest, snap config.Config, outcome *orca.Outcome, accountID, upstreamModel, sessionKey, convID string, unsupported []string, promptText string) *ChatResponse {
	resp := &ChatResponse{
		Model:            req.Model,
		Text:             outcome.Text,
		Thinking:         outcome.Thinking,
		SessionKey:       sessionKey,
		UpstreamAction:   outcome.Action,
		UnsupportedTools: unsupported,
		ConversationID:   convID,
		FinishReason:     "stop",
	}
	if resp.Model == "" {
		resp.Model = snap.Upstream.DefaultModel
	}
	resp.Usage.PromptTokens = metrics.EstimateTokens(promptText)
	resp.Usage.CompletionTokens = metrics.EstimateTokens(outcome.Text)
	resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens

	if outcome.HasToolCall() {
		tc := ToolCall{ID: newID("call_"), Type: "function"}
		tc.Function.Name = outcome.Tool.Name
		if len(outcome.Tool.Args) > 0 {
			tc.Function.Arguments = string(outcome.Tool.Args)
		} else {
			tc.Function.Arguments = "{}"
		}
		resp.ToolCalls = []ToolCall{tc}
		resp.FinishReason = "tool_calls"
		e.mu.Lock()
		e.pending[tc.ID] = &pendingCall{
			AccountID:      accountID,
			ConversationID: convID,
			ToolName:       outcome.Tool.Name,
			UpstreamModel:  upstreamModel,
			CreatedAt:      time.Now(),
		}
		e.mu.Unlock()
	}
	return resp
}

func (e *Engine) track(accountID, model string, u Usage, _ error) {
	if e.tracker == nil {
		return
	}
	if model == "" {
		model = e.cfg.Snapshot().Upstream.DefaultModel
	}
	e.tracker.Record(accountID, model, u.PromptTokens, u.CompletionTokens)
}

type toolResult struct {
	ToolCallID string
	Name       string
	Content    string
}

// extractToolResults 从消息中提取匹配 pending 的工具结果。
func (e *Engine) extractToolResults(req *ChatRequest) []toolResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []toolResult
	for _, m := range req.Messages {
		if m.Role != "tool" || m.ToolCallID == "" {
			continue
		}
		pc, ok := e.pending[m.ToolCallID]
		if !ok {
			continue
		}
		out = append(out, toolResult{ToolCallID: m.ToolCallID, Name: pc.ToolName, Content: messageText(m)})
	}
	return out
}

// validateTools 上游不支持的能力显式报错。
func validateTools(req *ChatRequest) error {
	if len(req.Tools) == 0 {
		return nil
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			return &HTTPError{Status: 400, Code: "unsupported_tool_type", Message: "只支持 function 类型的工具声明"}
		}
		if t.Function.Name == "" {
			return &HTTPError{Status: 400, Code: "invalid_tool", Message: "工具缺少名称"}
		}
	}
	if tc, ok := req.ToolChoice.(string); ok && tc == "required" {
		return &HTTPError{Status: 400, Code: "unsupported_tool_choice", Message: `tool_choice:"required" 上游不支持`}
	}
	if m, ok := req.ToolChoice.(map[string]any); ok {
		if _, has := m["function"]; has {
			return &HTTPError{Status: 400, Code: "unsupported_tool_choice", Message: "具名强制工具选择上游不支持"}
		}
		if typ, _ := m["type"].(string); typ == "tool" || typ == "function" {
			return &HTTPError{Status: 400, Code: "unsupported_tool_choice", Message: "具名强制工具选择上游不支持"}
		}
	}
	if p, ok := req.ParallelToolCalls.(bool); ok && p {
		return &HTTPError{Status: 400, Code: "unsupported_parallel_tool_calls", Message: "上游一次只产出一个工具调用"}
	}
	return nil
}

// unsupportedTools 列出非 OrcaTerm 原生工具名。
func unsupportedTools(tools []ToolDef) []string {
	var out []string
	for _, t := range tools {
		if !orca.IsNativeTool(t.Function.Name) {
			out = append(out, t.Function.Name)
		}
	}
	return out
}

// buildTurnInput 组装本轮上游输入文本。
// 上游服务端保存历史，本轮只发：新会话时 system + 本轮 user；复用会话时只发本轮 user。
func buildTurnInput(req *ChatRequest, isNew bool) (string, []orca.ImagePart) {
	var sys []string
	var lastUser string
	var images []orca.ImagePart
	for _, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			if isNew {
				if t := messageText(m); t != "" {
					sys = append(sys, t)
				}
			}
		case "user":
			t, imgs := messageParts(m)
			if t != "" || len(imgs) > 0 {
				lastUser = t
				images = imgs
			}
		case "assistant":
			if len(m.ToolCalls) > 0 {
				var sb strings.Builder
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&sb, "[工具调用 %s(%s)]", tc.Function.Name, tc.Function.Arguments)
				}
				if sb.Len() > 0 {
					lastUser = sb.String()
				}
			}
		case "tool":
			if t := messageText(m); t != "" {
				lastUser = t
			}
		}
	}
	text := lastUser
	if isNew && len(sys) > 0 {
		text = strings.Join(sys, "\n") + "\n\n" + text
	}
	return text, images
}

// messageText 提取消息纯文本。
func messageText(m Message) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if pm["type"] == "text" {
				if t, ok := pm["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	case []ContentPart:
		var sb strings.Builder
		for _, p := range c {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// messageParts 提取文本与图片。
func messageParts(m Message) (string, []orca.ImagePart) {
	var text strings.Builder
	var images []orca.ImagePart
	switch c := m.Content.(type) {
	case string:
		return c, nil
	case []any:
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text":
				if t, ok := pm["text"].(string); ok {
					text.WriteString(t)
				}
			case "image_url":
				if iu, ok := pm["image_url"].(map[string]any); ok {
					if u, ok := iu["url"].(string); ok && u != "" {
						images = append(images, orca.ImagePart{URL: u})
					}
				}
			}
		}
	case []ContentPart:
		for _, p := range c {
			switch p.Type {
			case "text":
				text.WriteString(p.Text)
			case "image_url":
				if p.ImageURL != nil && p.ImageURL.URL != "" {
					images = append(images, orca.ImagePart{URL: p.ImageURL.URL})
				}
			}
		}
	}
	return text.String(), images
}

func defaultUpstreamModel(snap config.Config) string {
	for _, m := range orca.Models {
		if m.ID == snap.Upstream.DefaultModel {
			return m.Upstream
		}
	}
	return snap.Upstream.DefaultModel
}

// HTTPError 对外 HTTP 错误。
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

// ToOpenAIResponse 转为 OpenAI JSON 响应体。
func (r *ChatResponse) ToOpenAIResponse() *ChatCompletionResponse {
	msg := Message{Role: "assistant", Content: r.Text}
	if len(r.ToolCalls) > 0 {
		msg.ToolCalls = r.ToolCalls
		msg.Content = nil
		if r.Text != "" {
			msg.Content = r.Text
		}
	}
	return &ChatCompletionResponse{
		ID:      newID("chatcmpl-"),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   r.Model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: r.FinishReason}},
		Usage:   r.Usage,
	}
}
