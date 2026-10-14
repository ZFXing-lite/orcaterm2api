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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// CodeAuthExpired 上游 AccessToken 失效的业务码。
const CodeAuthExpired = 10050000

// Credential 一个 OrcaTerm 登录态。
type Credential struct {
	SID    string // ligai 域 sid cookie
	OT     string // ot_session JWT（Bearer）
	UserID string // ot_session JWT 中 userId 声明
}

// Ready 登录态是否完整。
func (c Credential) Ready() bool { return c.SID != "" && c.OT != "" }

// ParseCookie 从原始 Cookie 字符串解析出 ot_session 与 sid。
func ParseCookie(raw string) Credential {
	var c Credential
	for _, part := range strings.Split(raw, ";") {
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
	prompt := []map[string]any{{"type": "text", "text": text}}
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
	EnableTools    bool // setting.mcpServers/uiServers 开关（tools 列表恒为空）
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
}

// NewClient 创建上游客户端；httpClient 为 nil 时使用默认（由调用方注入代理）。
func NewClient(baseURL, product, origin, userAgent string, httpClient *http.Client) *Client {
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
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Product: product, Origin: origin, UserAgent: userAgent, HTTP: httpClient}
}

// chatURL 真实客户端固定携带的查询参数。
func (c *Client) chatURL() string {
	return c.BaseURL + "/assistant/chat?name=" + c.Product + "&mode=0&csrfCode=&sseresume=true"
}

// Chat 发送一轮上游聊天（非流式结构化），返回归一化结果。
func (c *Client) Chat(ctx context.Context, cred Credential, req ChatRequest) (*Outcome, error) {
	body, err := c.buildBody(req, cred.UserID)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setHeaders(httpReq, cred)
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("read upstream response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &BusinessError{Code: resp.StatusCode, Message: fmt.Sprintf("upstream http %d: %s", resp.StatusCode, truncate(string(raw), 300))}
	}
	return parseOutcome(raw)
}

func (c *Client) setHeaders(r *http.Request, cred Credential) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "text/event-stream, application/json")
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
	r.Header.Set("X-Product", c.Product)
	r.Header.Set("X-Seq-Id", newUUID())
	r.Header.Set("X-Csrfcode", "")
	r.Header.Set("X-Referer", c.Origin+"/")
	r.Header.Set("Origin", c.Origin)
	r.Header.Set("User-Agent", c.UserAgent)
}

func (c *Client) buildBody(req ChatRequest, userID string) ([]byte, error) {
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
			"input":          BuildACPPrompt(req.Text, req.Images),
			"id":             newUUID(),
			"emphasisPrompt": "",
			"files":          []any{},
		},
		"stream": false,
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
