// Package orca 实现 OrcaTerm 内置 AI 助手的上游协议客户端。
//
// 协议要点（来自对 OrcaTerm 桌面客户端的抓包逆向，细节参见 README）：
//   - POST https://lightai.cloud.tencent.com/assistant/chat?name=orcaterm&mode=0&csrfCode=&sseresume=true
//   - 鉴权：Authorization: Bearer <ot_session> + Cookie(ot_session/sid/userId)
//   - 请求体：conversationId 必须为 "cid-<uuid>-orcaterm" 格式；
//     input.input 是 ACP 信封 {"_format":"acp-prompt","version":1,"prompt":[...]}
//   - 响应：{"code":0,"data":{"action":"completion|ask|review",...}}
//     鉴权失效时 HTTP 200 + code=10050000（数字或字符串两种形态）
//   - 工具调用：data.action=review + data.tool{name,args}；
//     工具结果按 "<TOOL_META>返回结果</TOOL_META>\n<输出>" 回填到同一会话
package orca

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL 上游 ligai 后端地址。
const DefaultBaseURL = "https://lightai.cloud.tencent.com"

// DefaultProduct /assistant/chat 的 name 参数与 X-Product。
const DefaultProduct = "orcaterm"

// DefaultOrigin 真实客户端的 Origin（Tauri）。
const DefaultOrigin = "http://tauri.localhost"

// DefaultUserAgent 真实客户端 UA（抓包取得）。
const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) OrcaTerm/1.0.0 Chrome/120.0.0.0 Safari/537.36"

// DefaultWebOrigin / DefaultWebUserAgent：OrcaTerm Web 版指纹（HAR 实测）。
// 控制台态（uin/skey）票据来自浏览器，请求头须与 Web 版真实流量对齐，
// 避免出现「控制台 cookie + 桌面 origin」这种真实世界不存在的组合。
const DefaultWebOrigin = "https://orcaterm.cloud.tencent.com"

const DefaultWebUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/155.0.0.0 Safari/537.36"

// CodeAuthExpired 上游 AccessToken 失效的业务码。
const CodeAuthExpired = 10050000

// Credential 一个 OrcaTerm 登录态，支持两种形态：
//   - OAuth 桌面态：ot_session JWT（Bearer 鉴权）
//   - 控制台态：cloud.tencent.com 的 uin/skey cookie（直接重放，抓包实证可用）
type Credential struct {
	SID    string // ligai 域 sid cookie（可选，API 会话层，缺失时上游自动发放）
	OT     string // ot_session JWT（Bearer）
	UserID string // userId：桌面态从 JWT 解析，控制台态从 cookie 提取
	Uin    string // 控制台态 uin（腾讯云账号标识）
	Skey   string // 控制台态 skey（登录票据）
	Raw    string // 原始 cookie 串（控制台态保真重放）
}

// Kind 凭证形态："oauth" | "console" | ""（无效）。
func (c Credential) Kind() string {
	switch {
	case c.OT != "":
		return "oauth"
	case c.Uin != "" && c.Skey != "":
		return "console"
	default:
		return ""
	}
}

// Ready 登录态是否可用（OAuth 态认 ot_session；控制台态认 uin+skey）。
func (c Credential) Ready() bool { return c.Kind() != "" }

// ParseCookie 从原始 Cookie 字符串解析登录态。
// 支持桌面客户端 .cookies 复制的 ot_session/sid 形态，
// 也支持浏览器 document.cookie 复制的 uin/skey/userId 控制台态。
func ParseCookie(raw string) Credential {
	var c Credential
	c.Raw = strings.TrimSpace(strings.Join(strings.Fields(raw), " "))
	for _, part := range strings.Split(c.Raw, ";") {
		part = strings.TrimSpace(part)
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k, v := strings.TrimSpace(kv[0]), strings.TrimSpace(kv[1])
		switch k {
		case "ot_session":
			c.OT = v
		case "sid":
			c.SID = v
		case "userId":
			c.UserID = v
		case "uin":
			c.Uin = v
		case "skey":
			c.Skey = v
		}
	}
	if c.UserID == "" && c.OT != "" {
		c.UserID = jwtUserID(c.OT)
	}
	return c
}

// jwtUserID 从 JWT payload 取 userId 声明（不验签，仅本地读取）。
func jwtUserID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	p := parts[1]
	if pad := len(p) % 4; pad > 0 {
		p += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(p)
		if err != nil {
			return ""
		}
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	switch v := m["userId"].(type) {
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		return v
	}
	return ""
}

// TokenExpiry 读出 ot_session 的过期时间（用于提前提示重新登录）。
func TokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	p := parts[1]
	if pad := len(p) % 4; pad > 0 {
		p += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// NewConversationID 生成 "cid-<uuid>-<product>" 格式的会话 ID。
func NewConversationID(product string) string {
	return "cid-" + newUUID() + "-" + product
}

// ---- 模型目录 ----

// Model 模型条目：ID 为网关对外展示名，Upstream 为上游 setting.model。
type Model struct {
	ID       string
	Upstream string
	Legacy   bool // 是否已不在 OrcaTerm 当前模型选择器中
}

// Models 网关支持的模型目录。
var Models = []Model{
	{ID: "deepseek-v4-pro", Upstream: "TokenHub/deepseek-v4-pro"},
	{ID: "deepseek-v4-flash", Upstream: "TokenHub/deepseek-v4-flash"},
	{ID: "hy3", Upstream: "Hunyuan3/hy3"},
	{ID: "hy4-preview", Upstream: "Hunyuan3/hy4-preview"},
	{ID: "kimi-k3", Upstream: "TokenHub/kimi-k3"},
	{ID: "glm-5.2", Upstream: "TokenHub/glm-5.2"},
	{ID: "glm-5.3", Upstream: "TokenHub/glm-5.3"},
	{ID: "glm-5.3-flash", Upstream: "TokenHub/glm-5.3-flash"},
	{ID: "deepseek-chat-latest", Upstream: "TokenHub/deepseek-chat-latest", Legacy: true},
	{ID: "glm-4.7", Upstream: "TokenHub/glm-4.7", Legacy: true},
	{ID: "kimi-k2", Upstream: "TokenHub/kimi-k2", Legacy: true},
	{ID: "kimi-k2.5", Upstream: "TokenHub/kimi-k2.5", Legacy: true},
	{ID: "kimi-k2-thinking", Upstream: "TokenHub/kimi-k2-thinking", Legacy: true},
	{ID: "kimi-k2.5-thinking", Upstream: "TokenHub/kimi-k2.5-thinking", Legacy: true},
	{ID: "hunyuan-t1", Upstream: "Hunyuan3/hunyuan-t1", Legacy: true},
	{ID: "hunyuan-turbos", Upstream: "Hunyuan3/hunyuan-turbos", Legacy: true},
	{ID: "qwen3", Upstream: "TokenHub/qwen3", Legacy: true},
}

// AliasModel 兼容别名，指向默认模型。
const AliasModel = "orcaterm-assistant"

// ResolveModel 把对外模型名解析为上游 model id。
// 接受目录 ID、上游全名；未知模型按 allowUnknown 决定直通或报错。
func ResolveModel(name, defUpstream string, allowUnknown bool) (string, error) {
	if name == "" || name == AliasModel {
		return defUpstream, nil
	}
	for _, m := range Models {
		if m.ID == name || m.Upstream == name {
			return m.Upstream, nil
		}
	}
	if allowUnknown {
		return name, nil
	}
	return "", fmt.Errorf("unknown model %q", name)
}

// ---- ACP 提示词 ----

// ImagePart 图片输入。
type ImagePart struct {
	URL string // data URI 或远程 URL
}

// BuildACPPrompt 构造上游 input.input 的 ACP 信封。
func BuildACPPrompt(text string, images []ImagePart) string {
	return BuildACPPromptWithNote("", text, images)
}

// BuildACPPromptWithNote 在 prompt 数组最前注入行为指令块（上游把多块
// prompt 视为同一轮输入，桌面客户端同样以首块注入环境指令）。
func BuildACPPromptWithNote(note, text string, images []ImagePart) string {
	var prompt []map[string]any
	if strings.TrimSpace(note) != "" {
		prompt = append(prompt, map[string]any{"type": "text", "text": note})
	}
	prompt = append(prompt, map[string]any{"type": "text", "text": text})
	for _, img := range images {
		if mime, data, ok := splitDataURI(img.URL); ok {
			prompt = append(prompt, map[string]any{
				"type": "image", "data": data, "mimeType": mime,
			})
			continue
		}
		prompt = append(prompt, map[string]any{
			"type": "image_url", "image_url": map[string]string{"url": img.URL},
		})
	}
	b, _ := json.Marshal(map[string]any{
		"_format": "acp-prompt",
		"version": 1,
		"prompt":  prompt,
	})
	return string(b)
}

func splitDataURI(u string) (mime, data string, ok bool) {
	if !strings.HasPrefix(u, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(u, "data:")
	parts := strings.SplitN(rest, ",", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	meta := parts[0]
	mime = strings.SplitN(meta, ";", 2)[0]
	if mime == "" {
		mime = "image/png"
	}
	return mime, parts[1], true
}

// ---- 工具结果回填 ----

const (
	toolMetaOpen  = "<TOOL_META>"
	toolMetaClose = "</TOOL_META>"
	// toolResultLabel 真机在 TOOL_META 内写入的固定标签。
	toolResultLabel = "返回结果"
)

// BuildToolResultPrompt 构造工具结果回填文本（与真机格式一致）。
// 空输出视为拒绝执行，生成 OrcaTerm 可识别的拒绝文本。
func BuildToolResultPrompt(toolName, output string) string {
	if strings.TrimSpace(output) == "" {
		return toolMetaOpen + toolResultLabel + toolMetaClose + "\n" +
			"用户拒绝执行工具 " + toolName + "，请根据已有信息继续回答。"
	}
	if strings.Contains(output, toolMetaOpen) {
		return output // 已是协议文本，原样透传
	}
	return toolMetaOpen + toolResultLabel + toolMetaClose + "\n" + output
}

// ---- 上游请求/响应 ----

// ChatRequest 上游聊天请求。
type ChatRequest struct {
	ConversationID string
	Text           string // 纯文本（客户端负责拼好 system/历史摘要）
	Images         []ImagePart
	UpstreamModel  string
	HideTools      bool
	EnableTools    bool   // setting.mcpServers/uiServers 开关（tools 列表恒为空）
	BehaviorNote   string // 行为指令：注入 prompt 首块（无人值守/默认执行等）
}

// UpstreamTool 上游 data.tool。
type UpstreamTool struct {
	Type        string          `json:"type"`
	MCPServer   string          `json:"mcpServer"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Args        json.RawMessage `json:"args"`
}

// Outcome 上游单轮调用结果。
type Outcome struct {
	HTTPStatus int
	Action     string // completion | ask | review
	Text       string
	Thinking   string
	Tool       *UpstreamTool
	Raw        string
}

// HasToolCall 是否为工具调用。
func (o *Outcome) HasToolCall() bool { return o != nil && o.Action == "review" && o.Tool != nil }

// AuthError 上游鉴权失效错误（HTTP 200 + code=10050000）。
type AuthError struct {
	Code    int
	Message string
}

func (e *AuthError) Error() string { return fmt.Sprintf("upstream auth expired (code=%d): %s", e.Code, e.Message) }

// isConsoleAuthExpired 控制台态（uin/skey）票据失效的两种上游特征（实测）：
// NEED_UIN_AND_SKEY（票据缺失）与 VERIFY_SKEY_UIN_FAILED（skey 过期/校验失败）。
// ParseOutcomeText 从流式聚合文本解析结果。
// 兼容两种形态：完整信封 {"code":0,"data":{...}}（非流式响应体的逐 token 重放）
// 或内层协议 JSON {"action":...}（流式只吐 data 内容）——无票据无法实测断言，
// 两种都接住。
func ParseOutcomeText(full string) (*Outcome, error) {
	t := stripOutputShell(full)
	if strings.TrimSpace(t) == "" {
		return nil, &ProtocolError{Msg: "上游未返回内容"}
	}
	// 形态一：完整信封 {"code":0,"data":{...}}
	if out, err := parseOutcome([]byte(t)); err == nil {
		return out, nil
	}
	// 形态二：前置噪声防御——取最外层 {...} 跨度（模型可能先说话再给 JSON）
	body := outermostJSON(t)
	if body == "" {
		body = t
	}
	// 跨度本身可能是完整信封（噪声在围栏外），直接解
	if out, err := parseOutcome([]byte(body)); err == nil {
		return out, nil
	}
	wrapped := []byte(`{"code":0,"data":` + body + `}`)
	if out, err := parseOutcome(wrapped); err == nil {
		return out, nil
	}
	return parseOutcome([]byte(t))
}

// stripOutputShell 剥掉上游模型输出习惯包裹的 markdown 代码围栏
// 与前后空白噪声（实测上游 stream:true 的 token 流可能吐
// "```json\n{...}\n```"，模型给协议 JSON 加了围栏）。
func stripOutputShell(s string) string {
	s = strings.TrimSpace(s)
	lower := strings.ToLower(s)
	for _, fence := range []string{"```json\n", "```json\r\n", "```json "} {
		if strings.HasPrefix(lower, fence) {
			s = s[len(fence):]
			break
		}
	}
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// outermostJSON 提取最外层花括号跨度；找不到返回空。
func outermostJSON(s string) string {
	i := strings.Index(s, "{")
	if i < 0 {
		return ""
	}
	j := strings.LastIndex(s, "}")
	if j <= i {
		return ""
	}
	return s[i : j+1]
}

func isConsoleAuthExpired(msg string) bool {
	return strings.Contains(msg, "NEED_UIN_AND_SKEY") || strings.Contains(msg, "VERIFY_SKEY_UIN_FAILED")
}

// IsConsoleAuthExpired 控制台态票据失效特征判断（导出版，供面板/探测层使用）。
func IsConsoleAuthExpired(msg string) bool { return isConsoleAuthExpired(msg) }

// ProtocolError 上游协议错误。
type ProtocolError struct{ Msg string }

func (e *ProtocolError) Error() string { return "upstream protocol error: " + e.Msg }

// BusinessError 上游业务错误（code != 0）。
type BusinessError struct {
	Code    int
	Message string
}

func (e *BusinessError) Error() string {
	return fmt.Sprintf("upstream business error (code=%d): %s", e.Code, e.Message)
}

// Client 上游客户端。
type Client struct {
	BaseURL   string
	Product   string
	Origin    string
	UserAgent string
	HTTP      *http.Client

	// UA 是否为用户显式配置：控制台态仅在未配置时自动切 Web 版 UA
	uaConfigured bool
}

// NewClient 创建上游客户端；httpClient 为 nil 时使用默认（由调用方注入代理）。
func NewClient(baseURL, product, origin, userAgent string, httpClient *http.Client) *Client {
	uaConfigured := userAgent != ""
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if product == "" {
		product = DefaultProduct
	}
	if origin == "" {
		origin = DefaultOrigin
	}
	if userAgent == "" {
		userAgent = DefaultUserAgent
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 300 * time.Second}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Product: product, Origin: origin, UserAgent: userAgent, HTTP: httpClient, uaConfigured: uaConfigured}
}

// chatURL 真实客户端固定携带的查询参数。
func (c *Client) chatURL() string {
	return c.BaseURL + "/assistant/chat?name=" + c.Product + "&mode=0&csrfCode=&sseresume=true"
}

// Chat 发送一轮上游聊天，返回归一化结果。
// 传输层走 stream:true 的 SSE 分块读取（绕过 EdgeOne 对单响应的大小/时长限制，
// 长文本不再截断），在网关内聚合出与原非流式完全一致的协议 JSON 后解析。
func (c *Client) Chat(ctx context.Context, cred Credential, req ChatRequest) (*Outcome, error) {
	ts, err := c.ChatStreamCollect(ctx, cred, req)
	if err != nil {
		return nil, err
	}
	// 消费整条流完成聚合（Next 边读边累积协议 JSON 文本）
	for {
		if _, err := ts.Next(); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	return ParseOutcomeText(ts.Full())
}

// chatStreamCollect 发起 SSE 流式请求并完整聚合。
// 返回 token 流对象（含逐块回调能力，供真流式透传复用）。
func (c *Client) ChatStreamCollect(ctx context.Context, cred Credential, req ChatRequest) (*TokenStream, error) {
	body, err := c.buildStreamBody(req, cred.UserID)
	if err != nil {
		return nil, err
	}
	// 流式调用禁总超时（长生成可远超 300s），改用空闲看门狗：
	// 块间 90s 无数据判死；探活等短超时场景沿用 Client 配置。
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq, cred)
	streamClient := &http.Client{Transport: c.HTTP.Transport}
	if c.HTTP.Transport == nil {
		streamClient.Transport = http.DefaultTransport
	}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, &BusinessError{Code: resp.StatusCode, Message: fmt.Sprintf("upstream http %d: %s", resp.StatusCode, truncate(string(raw), 300))}
	}

	_, cancel := context.WithCancel(ctx)
	ts := &TokenStream{resp: resp, cancel: cancel}
	idle := 90 * time.Second
	if c.HTTP.Timeout > 0 && c.HTTP.Timeout < idle {
		idle = c.HTTP.Timeout
	}
	ts.idle = idle
	ts.watchdog = time.AfterFunc(idle, cancel)
	go func() {
		<-ctx.Done() // 外部取消/空闲判死：强制中断阻塞读
		resp.Body.Close()
	}()
	return ts, nil
}

// TokenStream 上游 SSE token 流：逐块产出 content，支持聚合与透传两种消费方式。
type TokenStream struct {
	mu       sync.Mutex
	resp     *http.Response
	sc       *bufio.Scanner
	buf      strings.Builder // 聚合的协议 JSON 文本
	raw      strings.Builder // 非 SSE 形态兜底：上游忽略 stream 参数时收整块 JSON
	err      error
	done     bool
	watchdog *time.Timer
	idle     time.Duration
	cancel   context.CancelFunc
	sawDone bool // 上游 SSE 是否显式发过 data: [DONE]
}

// Next 返回下一个 content 增量；流结束返回 ("", io.EOF)。
// 失败返回错误（网络中断/上游错误块/空闲判死）。
func (t *TokenStream) Next() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return "", t.eofOrErr()
	}
	if t.sc == nil {
		t.sc = bufio.NewScanner(t.resp.Body)
		t.sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	}
	for {
		if t.watchdog != nil && t.idle > 0 {
			t.watchdog.Reset(t.idle)
		}
		if !t.sc.Scan() {
			// 流中断暴露为可重试错误。TCP 正常 FIN 时 scanner.Err()==nil
			// 无法靠它区分——用 [DONE] 见证标志判定半截
			//（单体 JSON 兜底走 raw 通道，不受此判定影响）
			if serr := t.sc.Err(); serr != nil {
				t.err = fmt.Errorf("upstream stream interrupted: %w", serr)
			} else if !t.sawDone && t.buf.Len() > 0 && t.raw.Len() == 0 {
				t.err = errors.New("upstream stream ended prematurely (missing [DONE])")
			}
			t.finish()
			return "", t.eofOrErr()
		}
		line := strings.TrimSpace(t.sc.Text())
		if line == "" || strings.HasPrefix(line, ":") { // SSE 注释/空行
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// 非 SSE 载荷行：兜底聚合（上游可能忽略 stream 参数返回单体 JSON）
			t.raw.WriteString(line)
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			t.sawDone = true
			t.finish()
			return "", t.eofOrErr()
		}
		// 上游错误行：data: [ERROR]: ...（实测形态）
		if strings.HasPrefix(payload, "[ERROR]") {
			t.err = fmt.Errorf("upstream stream error: %s", truncate(payload, 300))
			t.finish()
			return "", t.err
		}
		var chunk struct {
			Content string `json:"content"`
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue // 非 JSON 载荷行，跳过
		}
		if chunk.Code == 10050000 || isConsoleAuthExpired(chunk.Message) {
			t.err = &AuthError{Code: chunk.Code, Message: chunk.Message}
			t.finish()
			return "", t.err
		}
		if chunk.Code == 429 {
			t.err = &BusinessError{Code: 429, Message: chunk.Message}
			t.finish()
			return "", t.err
		}
		t.buf.WriteString(chunk.Content)
		return chunk.Content, nil
	}
}

// Full 聚合全部收到的 content（流读尽后调用）。
// SSE 形态聚合为逐 token 拼接；上游忽略 stream 参数时回退为单体 JSON 文本。
func (t *TokenStream) Full() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.buf.Len() > 0 {
		return t.buf.String()
	}
	return t.raw.String()
}

func (t *TokenStream) finish() {
	t.done = true
	if t.watchdog != nil {
		t.watchdog.Stop()
	}
	if t.cancel != nil {
		t.cancel()
	}
}

func (t *TokenStream) eofOrErr() error {
	if t.err != nil {
		return t.err
	}
	return io.EOF
}

func (c *Client) setHeaders(r *http.Request, cred Credential) {
	// 公共头（桌面/浏览器两种真实流量共有）
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream, application/json")
	r.Header.Set("X-Product", c.Product)
	r.Header.Set("X-Seq-Id", newUUID())
	r.Header.Set("X-Csrfcode", "")
	r.Header.Set("X-Referer", c.Origin+"/")
	r.Header.Set("Origin", c.Origin)
	r.Header.Set("User-Agent", c.UserAgent)

	switch cred.Kind() {
	case "oauth":
		// 桌面态：Bearer + ot_session cookie，指纹与桌面客户端抓包一致
		r.Header.Set("Authorization", "Bearer "+cred.OT)
		parts := []string{"ot_session=" + cred.OT, "userAccountLoginMethod=oauth", "userAccountType=wechat"}
		if cred.UserID != "" {
			parts = append(parts, "userId="+cred.UserID)
		}
		// sid 属于 ligai 域 cookie；有则带上
		if cred.SID != "" {
			parts = append(parts, "sid="+cred.SID)
		}
		r.Header.Set("Cookie", strings.Join(parts, "; "))
	case "console":
		// 控制台态：保真重放 uin/skey cookie 串（实测不带 ot_session/sid 亦可）。
		// 头集切 OrcaTerm Web 版真实流量（HAR 实测），消除「控制台票据 + 桌面 origin」
		// 这种真实世界不存在的组合。
		r.Header.Del("Authorization")
		r.Header.Del("X-Referer")  // 桌面端自定义头，Web 版流量无
		r.Header.Del("X-Csrfcode") // Web 版 csrfCode 只出现在 URL query
		cookie := cred.Raw
		if strings.TrimSpace(cookie) == "" {
			cookie = "uin=" + cred.Uin + "; skey=" + cred.Skey
		}
		if cred.UserID != "" && !strings.Contains(cookie, "userId=") {
			cookie += "; userId=" + cred.UserID
		}
		r.Header.Set("Cookie", cookie)
		r.Header.Set("Origin", DefaultWebOrigin)
		r.Header.Set("Referer", DefaultWebOrigin+"/")
		if !c.uaConfigured {
			r.Header.Set("User-Agent", DefaultWebUserAgent)
		}
		r.Header.Set("Sec-Fetch-Site", "same-site")
		r.Header.Set("Sec-Fetch-Mode", "cors")
		r.Header.Set("Sec-Fetch-Dest", "empty")
		r.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	}
}

// Probe 轻量凭证体检：GET conversation/list 只读接口，code=0 即健康。
// 用于配对收号落盘前验证，不产生会话残留。
func (c *Client) Probe(ctx context.Context, cred Credential) error {
	if !cred.Ready() {
		return &AuthError{Code: 10050000, Message: "credential incomplete"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/assistant/conversation/list?offset=0&limit=1", nil)
	if err != nil {
		return err
	}
	c.setHeaders(req, cred)
	probeClient := c.HTTP
	if probeClient == nil {
		probeClient = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe: http status %d", resp.StatusCode)
	}
	var env struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("probe: bad json: %w", err)
	}
	if env.Code == 10050000 {
		return &AuthError{Code: env.Code, Message: env.Message}
	}
	if isConsoleAuthExpired(env.Message) {
		return &AuthError{Code: 10050000, Message: env.Message}
	}
	if env.Code != 0 {
		return fmt.Errorf("probe: code=%d message=%s", env.Code, env.Message)
	}
	return nil
}

func (c *Client) buildBody(req ChatRequest, userID string) ([]byte, error) {
	return c.buildBodyStream(req, userID, false)
}

// buildStreamBody stream:true 形态（SSE 分块传输，绕过单响应上限）。
func (c *Client) buildStreamBody(req ChatRequest, userID string) ([]byte, error) {
	return c.buildBodyStream(req, userID, true)
}

func (c *Client) buildBodyStream(req ChatRequest, userID string, stream bool) ([]byte, error) {
	mcpServers, uiServers := []string{}, []string{}
	if req.EnableTools {
		mcpServers = []string{"mcp-server-orcaterm-oauth"}
		uiServers = []string{"ui-tools-orcaterm-explorer"}
	}
	setting := map[string]any{
		"mcpServers":                       mcpServers,
		"uiServers":                        uiServers,
		"tools":                            []any{},
		"approvedTools":                    []any{},
		"approvedMCPTools":                 []any{},
		"enableAutoSubtaskExecution":       false,
		"env":                              []any{},
		"model":                            req.UpstreamModel,
		"modelDesc":                        map[string]any{},
		"hideToolMessage":                  req.HideTools,
		"isNormalToolMessageIgnored":       req.HideTools,
		"shouldHideDirectOutputToolReview": req.HideTools,
	}
	body := map[string]any{
		"conversationId": req.ConversationID,
		"user": map[string]any{
			"id":      userID,
			"setting": setting,
		},
		"input": map[string]any{
			"type":           "start",
			"input":          BuildACPPromptWithNote(req.BehaviorNote, req.Text, req.Images),
			"id":             newUUID(),
			"emphasisPrompt": "",
			"files":          []any{},
		},
		"stream": stream,
	}
	return json.Marshal(body)
}

// upstreamCode 兼容上游 code 的数字/字符串两种形态。
type upstreamCode int

func (c *upstreamCode) UnmarshalJSON(b []byte) error {
	t := strings.TrimSpace(string(b))
	if t == "" || t == "null" {
		*c = 0
		return nil
	}
	if strings.HasPrefix(t, `"`) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			*c = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("upstream code %q is not numeric", s)
		}
		*c = upstreamCode(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*c = upstreamCode(n)
	return nil
}

type upstreamResp struct {
	Code    upstreamCode `json:"code"`
	Data    *upstreamData `json:"data"`
	Message string        `json:"message"`
}

type upstreamData struct {
	Action         string        `json:"action"`
	Thinking       string        `json:"thinking"`
	TaskCompletion string        `json:"taskCompletion"`
	Question       string        `json:"question"`
	Options        []string      `json:"options"`
	Tool           *UpstreamTool `json:"tool"`
}

func parseOutcome(raw []byte) (*Outcome, error) {
	var resp upstreamResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &ProtocolError{Msg: "解析上游 JSON 失败: " + err.Error()}
	}
	if int(resp.Code) == CodeAuthExpired {
		return nil, &AuthError{Code: int(resp.Code), Message: resp.Message}
	}
	// 控制台态（uin/skey）票据失效：code=500 + 特征 message，同为鉴权失效
	if isConsoleAuthExpired(resp.Message) {
		return nil, &AuthError{Code: CodeAuthExpired, Message: resp.Message}
	}
	if resp.Code != 0 {
		return nil, &BusinessError{Code: int(resp.Code), Message: resp.Message}
	}
	if resp.Data == nil {
		return nil, &ProtocolError{Msg: "上游 data 缺失"}
	}
	d := resp.Data
	out := &Outcome{HTTPStatus: http.StatusOK, Action: d.Action, Thinking: d.Thinking, Raw: string(raw)}
	switch d.Action {
	case "completion":
		if d.Tool != nil || strings.TrimSpace(d.TaskCompletion) == "" {
			return nil, &ProtocolError{Msg: "completion 负载非法"}
		}
		out.Text = d.TaskCompletion
	case "ask":
		if d.Tool != nil || strings.TrimSpace(d.Question) == "" {
			return nil, &ProtocolError{Msg: "ask 负载非法"}
		}
		out.Text = d.Question
	case "review":
		if d.Tool == nil {
			return nil, &ProtocolError{Msg: "review 缺少 tool"}
		}
		out.Tool = d.Tool
		out.Text = d.TaskCompletion // 可能附带说明文本
	default:
		return nil, &ProtocolError{Msg: "未知 action: " + d.Action}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ---- OrcaTerm 原生工具名（快照，用于提示客户端声明了上游没有的工具） ----

// NativeTools 服务端下发的原生工具名快照（非权威全量）。
var NativeTools = map[string]bool{
	"fetch": true,
	"list_terminals": true, "get_terminal_detail": true, "get_terminal_output": true,
	"get_active_terminal": true, "execute_terminal_command": true, "send_terminal_signal": true,
	"get_command_history": true,
	"list_connect_configs": true, "select_connect_config": true, "connect_by_history_session": true,
	"connect_share": true, "connect_share_visit": true, "get_confirmed_session": true, "close_tunnel": true,
	"execute_command": true, "create_command": true, "query_commands_status": true,
	"run_commands": true, "run_sandbox_task": true, "submit_agent_tasks": true, "query_agent_tasks_status": true,
	"read_cloud_space_file": true, "read_cloud_space_file_list": true, "create_cloud_space_file": true,
	"create_file": true, "create_directory": true, "delete_local": true, "delete_remote": true,
	"delete_history_connection": true, "remote_read": true, "remote_write": true,
	"remote_edit": true, "remote_multi_edit": true, "remote_glob": true, "remote_grep": true,
	"open_file_manager": true, "close_file_manager": true, "open_tab_page": true, "close_tab_page": true,
	"open_next_tab_page": true, "open_last_tab_page": true, "open_proj": true, "open_session_recorder": true,
	"open_light": true, "search_light": true, "close_panel": true, "close_approval_panel": true, "send_offer": true,
	"get_orcaterm_settings": true, "update_orcaterm_settings": true,
	"create_firewall_rules": true, "delete_firewall_rules": true,
}

// IsNativeTool 是否为 OrcaTerm 原生工具。
func IsNativeTool(name string) bool { return NativeTools[name] }

// StreamFieldExtractor 流式字段提取器：从逐 token 的协议 JSON 文本流中
// 实时提取可见字段（taskCompletion / question）内容并还原转义，
// 供下游真流式透传；工具调用（review）等无可见字段的场景保持静默，
// 由流末聚合路径兜底。
type StreamFieldExtractor struct {
	state    int // 0=找标记 1=产出中 2=已结束
	pending  strings.Builder // WAIT 态的尾部缓冲（防标记跨 token 劈开）
	markers  []string
	backslash bool // EMIT 态：上一字符是裸反斜杠
	hexBuf   []byte  // EMIT 态：\uXXXX 不足 4 位的缓冲
	emitted  bool
}

// NewStreamFieldExtractor 默认提取 taskCompletion 与 question。
// 标记同时兼容紧凑（"k":"v"）与宽松（"k": "v"）两种序列化形态。
func NewStreamFieldExtractor() *StreamFieldExtractor {
	return &StreamFieldExtractor{markers: []string{
		`"taskCompletion":"`, `"taskCompletion": "`,
		`"question":"`, `"question": "`,
	}}
}

// Feed 喂入一个 content token，返回其中可见文本（可能为空）。
func (x *StreamFieldExtractor) Feed(token string) string {
	return x.yield(x.feed(token))
}

func (x *StreamFieldExtractor) feed(token string) string {
	switch x.state {
	case 0:
		x.pending.WriteString(token)
		buf := x.pending.String()
		// review（工具调用）先行静默：其 taskCompletion 只是附带说明，
		// 不应作为可见文本透传给下游（完整工具调用由流末聚合路径产出）
		if i := strings.Index(buf, `"action":"review"`); i >= 0 || strings.Contains(buf, `"action": "review"`) {
			x.state = 2
			return ""
		}
		for _, m := range x.markers {
			i := strings.Index(buf, m)
			if i < 0 {
				continue
			}
			rest := buf[i+len(m):]
			if strings.HasPrefix(rest, `"`) {
				// 空值字段（如 ask 响应里的 "taskCompletion":""）：
				// 跳过它继续找真正有内容的标记（question）
				continue
			}
			x.state = 1
			x.pending.Reset()
			return x.emitUnescaped(rest)
		}
		// 未命中：保留「最长标记-1」的尾部，防标记跨 token 劈开
		keep := 0
		for _, m := range x.markers {
			if len(m)-1 > keep {
				keep = len(m) - 1
			}
		}
		if len(buf) > keep {
			x.pending.Reset()
			x.pending.WriteString(buf[len(buf)-keep:])
		}
		return ""
	case 1:
		return x.emitUnescaped(token)
	default:
		return ""
	}
}

// yield 统一产出收口：记录「已产出可见文本」，engine 据此关闭换号重试窗口。
func (x *StreamFieldExtractor) yield(s string) string {
	if s != "" {
		x.emitted = true
	}
	return s
}

// emitUnescaped 还原 JSON 字符串转义；遇到未转义的引号即字段结束。
func (x *StreamFieldExtractor) emitUnescaped(s string) string {
	var out strings.Builder
	i := 0
	// 先消化上一 token 遗留的转义缓冲
	if x.hexBuf != nil && len(x.hexBuf) > 0 {
		need := 4 - len(x.hexBuf)
		if len(s) >= need {
			for j := 0; j < need; j++ {
				x.hexBuf = append(x.hexBuf, s[j])
			}
			out.WriteRune(hexRune(x.hexBuf))
			x.hexBuf = x.hexBuf[:0]
			i = need
		} else {
			x.hexBuf = append(x.hexBuf, s...)
			return "" // 整个 token 都不够补 \u，继续攒
		}
	}
	if x.backslash && i < len(s) {
		x.backslash = false
		nc := s[i]
		if nc == 'u' { // 反斜杠与 u 劈开：转入 hex 收集
			if i+5 < len(s) {
				out.WriteRune(hexRune([]byte(s[i+1 : i+5])))
				i += 5
			} else {
				x.hexBuf = append(x.hexBuf, s[i+1:]...)
				i = len(s)
			}
		} else {
			out.WriteString(unescapePair(s[i : i+1]))
			i++
		}
	}
	for i < len(s) {
		ch := s[i]
		if ch == '\\' {
			if i+1 >= len(s) {
				x.backslash = true // 反斜杠劈在 token 尾
				break
			}
			nc := s[i+1]
			if nc == 'u' {
				if i+6 <= len(s) {
					out.WriteRune(hexRune([]byte(s[i+2 : i+6])))
					i += 6
				} else {
					x.hexBuf = append(x.hexBuf, s[i+2:]...) // \u 劈开，攒 hex
					i = len(s)
				}
			} else {
				out.WriteString(unescapePair(s[i+1 : i+2]))
				i += 2
			}
			continue
		}
		if ch == '"' { // 字段结束
			x.state = 2
			return out.String()
		}
		out.WriteByte(ch)
		i++
	}
	return out.String()
}

// Emittd 是否已产出过可见文本（engine 据此判定可下发/重试窗口）。
func (x *StreamFieldExtractor) Emitted() bool { return x.emitted }

func hexRune(b []byte) rune {
	v := 0
	for _, c := range b {
		v = v*16 + hexVal(c)
	}
	return rune(v)
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 0
}

func unescapePair(s string) string {
	if len(s) == 0 {
		return ""
	}
	switch s[0] {
	case 'n':
		return "\n"
	case 't':
		return "\t"
	case 'r':
		return "\r"
	case 'b':
		return "\b"
	case 'f':
		return "\f"
	case '/':
		return "/"
	case '"', '\\':
		return s
	}
	return `\` + s // 未知转义保守原样
}
