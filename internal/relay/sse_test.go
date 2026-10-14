package relay

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteSSE(t *testing.T) {
	resp := &ChatResponse{
		Model:        "deepseek-v4-flash",
		Text:         "你好世界",
		FinishReason: "stop",
		Usage:        Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	w := httptest.NewRecorder()
	WriteSSE(w, resp, true)
	body := w.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("缺少 [DONE]")
	}
	if !strings.Contains(body, "你好") {
		t.Error("缺少文本内容")
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Error("缺少 finish_reason")
	}
	if !strings.Contains(body, `"prompt_tokens":10`) {
		t.Error("缺少 usage")
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type=%q", ct)
	}
}

func TestWriteSSEToolCalls(t *testing.T) {
	tc := ToolCall{ID: "call_1", Type: "function"}
	tc.Function.Name = "fetch"
	tc.Function.Arguments = `{"url":"https://x"}`
	resp := &ChatResponse{Model: "m", ToolCalls: []ToolCall{tc}, FinishReason: "tool_calls"}
	w := httptest.NewRecorder()
	WriteSSE(w, resp, false)
	body := w.Body.String()
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Error("缺少 tool_calls 结束原因")
	}
	if !strings.Contains(body, `"name":"fetch"`) {
		t.Error("缺少工具名")
	}
}

func TestToOpenAIResponse(t *testing.T) {
	resp := &ChatResponse{Model: "m", Text: "hi", FinishReason: "stop", Usage: Usage{TotalTokens: 3}}
	o := resp.ToOpenAIResponse()
	if o.Object != "chat.completion" || len(o.Choices) != 1 {
		t.Errorf("got %+v", o)
	}
	if o.Choices[0].Message.Content != "hi" {
		t.Errorf("content=%v", o.Choices[0].Message.Content)
	}
}
