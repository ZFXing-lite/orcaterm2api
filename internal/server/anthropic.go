package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ZFXing-lite/orcaterm2api/internal/relay"
)

// Anthropic Messages API 适配：/v1/messages（x-api-key）。
// 客户端 Base URL 填 http://host:port（注意不是 /v1）。

type anthropicContentBlock struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	Source    map[string]any `json:"source,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     any            `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   any            `json:"content,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

type anthropicRequest struct {
	Model      string             `json:"model"`
	MaxTokens  int                `json:"max_tokens"`
	System     any                `json:"system,omitempty"`
	Messages   []anthropicMessage `json:"messages"`
	Tools      []anthropicTool    `json:"tools,omitempty"`
	ToolChoice any                `json:"tool_choice,omitempty"`
	Stream     bool               `json:"stream,omitempty"`
	Metadata   map[string]any     `json:"metadata,omitempty"`
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	if !s.checkDownstreamAuth(r) {
		writeAnthropicError(w, 401, "authentication_error", "下游 API Key 无效")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "读取请求体失败")
		return
	}
	var areq anthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "请求体不是合法 JSON")
		return
	}
	creq, herr := anthropicToChat(&areq)
	if herr != nil {
		writeOpenAIError(w, herr)
		return
	}
	// 会话键：metadata.user_id 或 user 字段
	sessionKey := ""
	if areq.Metadata != nil {
		if v, ok := areq.Metadata["user_id"].(string); ok && v != "" {
			sessionKey = "user:" + v
		}
	}
	if sk := strings.TrimSpace(r.Header.Get("X-Session-Id")); sk != "" {
		sessionKey = "hdr:" + sk
	}

	resp, err := s.engine.Complete(r.Context(), creq, sessionKey)
	if err != nil {
		writeAnthropicError(w, errToHTTP(err).Status, "api_error", errToHTTP(err).Message)
		return
	}
	if len(resp.UnsupportedTools) > 0 {
		w.Header().Set("X-OrcaTerm-Unsupported-Tools", strings.Join(resp.UnsupportedTools, ","))
	}
	if resp.UpstreamAction != "" {
		w.Header().Set("X-OrcaTerm-Action", resp.UpstreamAction)
	}
	if areq.Stream {
		writeAnthropicSSE(w, resp)
		return
	}
	writeJSON(w, 200, chatToAnthropic(resp))
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// anthropicToChat 把 Anthropic 请求转为网关内部请求。
func anthropicToChat(areq *anthropicRequest) (*relay.ChatRequest, *relay.HTTPError) {
	creq := &relay.ChatRequest{Model: areq.Model, Stream: areq.Stream, MaxTokens: areq.MaxTokens}
	// system
	switch sys := areq.System.(type) {
	case string:
		if sys != "" {
			creq.Messages = append(creq.Messages, relay.Message{Role: "system", Content: sys})
		}
	case []any:
		var sb strings.Builder
		for _, b := range sys {
			if bm, ok := b.(map[string]any); ok && bm["type"] == "text" {
				if t, ok := bm["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		if sb.Len() > 0 {
			creq.Messages = append(creq.Messages, relay.Message{Role: "system", Content: sb.String()})
		}
	}
	for _, m := range areq.Messages {
		switch m.Role {
		case "user":
			msgs, herr := anthropicUserToMessages(m.Content)
			if herr != nil {
				return nil, herr
			}
			creq.Messages = append(creq.Messages, msgs...)
		case "assistant":
			blocks, toolCalls, herr := anthropicBlocksToParts2(m.Content)
			if herr != nil {
				return nil, herr
			}
			creq.Messages = append(creq.Messages, relay.Message{Role: "assistant", Content: blocks, ToolCalls: toolCalls})
		default:
			return nil, &relay.HTTPError{Status: 400, Code: "invalid_role", Message: "不支持的 role: " + m.Role}
		}
	}
	for _, t := range areq.Tools {
		td := relay.ToolDef{Type: "function"}
		td.Function.Name = t.Name
		td.Function.Description = t.Description
		td.Function.Parameters = t.InputSchema
		creq.Tools = append(creq.Tools, td)
	}
	if areq.ToolChoice != nil {
		if m, ok := areq.ToolChoice.(map[string]any); ok {
			if typ, _ := m["type"].(string); typ == "any" {
				creq.ToolChoice = "required"
			} else if typ == "tool" {
				creq.ToolChoice = m // validateTools 会显式报错
			}
		}
	}
	return creq, nil
}

// anthropicUserToMessages 把 user 消息转为网关消息；
// tool_result 块拆成独立的 tool 消息（带 tool_call_id），以便闭环工具调用。
func anthropicUserToMessages(content any) ([]relay.Message, *relay.HTTPError) {
	switch c := content.(type) {
	case string:
		if c == "" {
			return nil, nil
		}
		return []relay.Message{{Role: "user", Content: c}}, nil
	case []any:
		var parts []relay.ContentPart
		var tools []relay.Message
		for _, b := range c {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch bm["type"] {
			case "tool_result":
				toolUseID, _ := bm["tool_use_id"].(string)
				text := anthropicBlockText(bm["content"])
				tools = append(tools, relay.Message{Role: "tool", ToolCallID: toolUseID, Content: text})
			case "text":
				if t, ok := bm["text"].(string); ok && t != "" {
					parts = append(parts, relay.ContentPart{Type: "text", Text: t})
				}
			case "image":
				src, _ := bm["source"].(map[string]any)
				if src == nil {
					continue
				}
				switch src["type"] {
				case "base64":
					media, _ := src["media_type"].(string)
					data, _ := src["data"].(string)
					if media == "" {
						media = "image/png"
					}
					parts = append(parts, relay.ContentPart{Type: "image_url",
						ImageURL: &relay.ImageURLPart{URL: "data:" + media + ";base64," + data}})
				case "url":
					if u, ok := src["url"].(string); ok && u != "" {
						parts = append(parts, relay.ContentPart{Type: "image_url",
							ImageURL: &relay.ImageURLPart{URL: u}})
					}
				}
			}
		}
		var out []relay.Message
		if len(parts) > 0 {
			out = append(out, relay.Message{Role: "user", Content: parts})
		}
		out = append(out, tools...)
		return out, nil
	}
	return nil, &relay.HTTPError{Status: 400, Code: "invalid_content", Message: "content 格式不支持"}
}

func anthropicBlocksToParts2(content any) ([]relay.ContentPart, []relay.ToolCall, *relay.HTTPError) {
	var parts []relay.ContentPart
	var calls []relay.ToolCall
	switch c := content.(type) {
	case string:
		if c != "" {
			parts = append(parts, relay.ContentPart{Type: "text", Text: c})
		}
		return parts, nil, nil
	case []any:
		for _, b := range c {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch bm["type"] {
			case "text":
				if t, ok := bm["text"].(string); ok && t != "" {
					parts = append(parts, relay.ContentPart{Type: "text", Text: t})
				}
			case "image":
				src, _ := bm["source"].(map[string]any)
				if src == nil {
					continue
				}
				switch src["type"] {
				case "base64":
					media, _ := src["media_type"].(string)
					data, _ := src["data"].(string)
					if media == "" {
						media = "image/png"
					}
					parts = append(parts, relay.ContentPart{Type: "image_url",
						ImageURL: &relay.ImageURLPart{URL: "data:" + media + ";base64," + data}})
				case "url":
					if u, ok := src["url"].(string); ok && u != "" {
						parts = append(parts, relay.ContentPart{Type: "image_url",
							ImageURL: &relay.ImageURLPart{URL: u}})
					}
				}
			case "tool_use":
				id, _ := bm["id"].(string)
				name, _ := bm["name"].(string)
				tc := relay.ToolCall{ID: id, Type: "function"}
				tc.Function.Name = name
				if input := bm["input"]; input != nil {
					ib, _ := json.Marshal(input)
					tc.Function.Arguments = string(ib)
				} else {
					tc.Function.Arguments = "{}"
				}
				calls = append(calls, tc)
			case "tool_result":
				toolUseID, _ := bm["tool_use_id"].(string)
				text := anthropicBlockText(bm["content"])
				parts = append(parts, relay.ContentPart{Type: "text", Text: text})
				// 标记 tool_call_id：用一个特殊 part 由上层转为 tool 消息
				_ = toolUseID
			}
		}
		return parts, calls, nil
	}
	return nil, nil, &relay.HTTPError{Status: 400, Code: "invalid_content", Message: "content 格式不支持"}
}

// 注意：tool_result 在 user 消息内出现时，需要转为独立的 tool 消息。
// 为简化，anthropicToChat 后处理：把紧跟 tool_result 的文本拆出。
// 实际做法：在转换 user 消息时检测 tool_result 块并生成 tool 消息。
func anthropicBlockText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, b := range c {
			if bm, ok := b.(map[string]any); ok && bm["type"] == "text" {
				if t, ok := bm["text"].(string); ok {
					sb.WriteString(t)
				}
			} else if s, ok := b.(string); ok {
				sb.WriteString(s)
			}
		}
		return sb.String()
	}
	return ""
}

// chatToAnthropic 网关结果转 Anthropic 响应。
func chatToAnthropic(resp *relay.ChatResponse) map[string]any {
	var content []map[string]any
	stopReason := "end_turn"
	if resp.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": resp.Text})
	}
	for _, tc := range resp.ToolCalls {
		var input any = map[string]any{}
		if tc.Function.Arguments != "" {
			var m any
			if json.Unmarshal([]byte(tc.Function.Arguments), &m) == nil {
				input = m
			}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
		})
		stopReason = "tool_use"
	}
	if content == nil {
		content = []map[string]any{}
	}
	return map[string]any{
		"id":          newMsgID(),
		"type":        "message",
		"role":        "assistant",
		"model":       resp.Model,
		"content":     content,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  resp.Usage.PromptTokens,
			"output_tokens": resp.Usage.CompletionTokens,
		},
	}
}

func newMsgID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "msg_" + base64.RawURLEncoding.EncodeToString(b[:])
}

// writeAnthropicSSE 合成 Anthropic SSE。
func writeAnthropicSSE(w http.ResponseWriter, resp *relay.ChatResponse) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	send := func(event string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		if flusher != nil {
			flusher.Flush()
		}
	}
	msgID := newMsgID()
	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": resp.Model,
			"content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": resp.Usage.PromptTokens, "output_tokens": 0},
		},
	})
	idx := 0
	if resp.Text != "" {
		send("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		for _, piece := range splitTextAnthropic(resp.Text) {
			send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": idx,
				"delta": map[string]any{"type": "text_delta", "text": piece},
			})
		}
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	for _, tc := range resp.ToolCalls {
		send("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{}},
		})
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		send("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		idx++
	}
	stopReason := "end_turn"
	if len(resp.ToolCalls) > 0 {
		stopReason = "tool_use"
	}
	send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason},
		"usage": map[string]any{"output_tokens": resp.Usage.CompletionTokens},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}

func splitTextAnthropic(s string) []string {
	const n = 120
	var out []string
	rs := []rune(s)
	for len(rs) > 0 {
		m := n
		if len(rs) < m {
			m = len(rs)
		}
		out = append(out, string(rs[:m]))
		rs = rs[m:]
	}
	return out
}
