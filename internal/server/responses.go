package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ZFXing-lite/orcaterm2api/internal/relay"
)

// OpenAI Responses API 最小适配：/v1/responses（Codex / Qoder 等客户端）。
// input 支持字符串或消息数组；流式返回合成事件。

type responsesInputItem struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	Type    string `json:"type"` // message / function_call_output 等
}

type responsesRequest struct {
	Model  string `json:"model"`
	Input  any    `json:"input"`
	Stream bool   `json:"stream"`
	User   string `json:"user,omitempty"`
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
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
	var rreq responsesRequest
	if err := json.Unmarshal(body, &rreq); err != nil {
		writeOpenAIError(w, &relay.HTTPError{Status: 400, Code: "invalid_request", Message: "请求体不是合法 JSON"})
		return
	}
	creq, herr := responsesToChat(&rreq)
	if herr != nil {
		writeOpenAIError(w, herr)
		return
	}
	sessionKey := ""
	if sk := strings.TrimSpace(r.Header.Get("X-Session-Id")); sk != "" {
		sessionKey = "hdr:" + sk
	}
	resp, err := s.engine.Complete(r.Context(), creq, sessionKey)
	if err != nil {
		writeOpenAIError(w, errToHTTP(err))
		return
	}
	if rreq.Stream {
		writeResponsesSSE(w, resp)
		return
	}
	writeJSON(w, 200, chatToResponses(resp))
}

func responsesToChat(rreq *responsesRequest) (*relay.ChatRequest, *relay.HTTPError) {
	creq := &relay.ChatRequest{Model: rreq.Model, Stream: rreq.Stream, User: rreq.User}
	switch in := rreq.Input.(type) {
	case string:
		if in != "" {
			creq.Messages = append(creq.Messages, relay.Message{Role: "user", Content: in})
		}
	case []any:
		for _, it := range in {
			im, ok := it.(map[string]any)
			if !ok {
				continue
			}
			role, _ := im["role"].(string)
			if role == "" {
				role = "user"
			}
			var text string
			switch c := im["content"].(type) {
			case string:
				text = c
			case []any:
				var sb strings.Builder
				for _, p := range c {
					if pm, ok := p.(map[string]any); ok {
						if t, ok := pm["text"].(string); ok {
							sb.WriteString(t)
						}
					}
				}
				text = sb.String()
			}
			if text != "" || role == "user" {
				creq.Messages = append(creq.Messages, relay.Message{Role: role, Content: text})
			}
		}
	case nil:
		// 空输入
	default:
		return nil, &relay.HTTPError{Status: 400, Code: "invalid_input", Message: "input 格式不支持"}
	}
	if len(creq.Messages) == 0 {
		return nil, &relay.HTTPError{Status: 400, Code: "empty_input", Message: "input 为空"}
	}
	return creq, nil
}

func chatToResponses(resp *relay.ChatResponse) map[string]any {
	respID := newResponsesID("resp_")
	msgID := newResponsesID("msg_")
	output := []map[string]any{
		{
			"type": "message", "id": msgID, "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": resp.Text, "annotations": []any{}}},
		},
	}
	for _, tc := range resp.ToolCalls {
		output = append(output, map[string]any{
			"type": "function_call", "id": tc.ID, "call_id": tc.ID,
			"name": tc.Function.Name, "arguments": tc.Function.Arguments, "status": "completed",
		})
	}
	status := "completed"
	if len(resp.ToolCalls) > 0 {
		status = "incomplete"
	}
	return map[string]any{
		"id": respID, "object": "response", "created_at": time.Now().Unix(),
		"model": resp.Model, "status": status,
		"output": output,
		"usage": map[string]any{
			"input_tokens": resp.Usage.PromptTokens, "output_tokens": resp.Usage.CompletionTokens,
			"total_tokens": resp.Usage.TotalTokens,
		},
	}
}

func newResponsesID(prefix string) string {
	return prefix + strings.ReplaceAll(newMsgID()[4:], "_", "")
}

func writeResponsesSSE(w http.ResponseWriter, resp *relay.ChatResponse) {
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
	respID := newResponsesID("resp_")
	send("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": respID, "object": "response", "model": resp.Model}})
	for _, piece := range splitTextAnthropic(resp.Text) {
		send("response.output_text.delta", map[string]any{
			"type": "response.output_text.delta", "item_id": newResponsesID("msg_"),
			"output_index": 0, "content_index": 0, "delta": piece,
		})
	}
	send("response.completed", map[string]any{"type": "response.completed", "response": chatToResponses(resp)})
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}
