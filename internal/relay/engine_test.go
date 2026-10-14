package relay

import (
	"testing"

	"github.com/ZFXing-lite/orcaterm2api/internal/orca"
)

// 跨号重试耗尽后，429 必须原状态码透传给下游，而不是被包装成 502。
func TestRetryableOfKeeps429(t *testing.T) {
	e := &Engine{}
	retryable, status, herr := e.retryableOf(&orca.BusinessError{Code: 429, Message: "rate limit"})
	if !retryable || status != 429 {
		t.Fatalf("429 应可重试且状态保留，得到 retryable=%v status=%d", retryable, status)
	}
	he, ok := herr.(*HTTPError)
	if !ok || he.Status != 429 {
		t.Fatalf("429 应映射为 HTTPError{429}，得到 %#v", herr)
	}

	// 登录态失效换号重试，对外状态 401
	retryable, status, herr = e.retryableOf(&orca.AuthError{Code: 10050000, Message: "expired"})
	if !retryable || status != 401 {
		t.Fatalf("鉴权失效应标记可重试且状态 401，得到 retryable=%v status=%d", retryable, status)
	}
	if he, ok := herr.(*HTTPError); !ok || he.Status != 401 {
		t.Fatalf("鉴权失效应映射为 HTTPError{401}，得到 %#v", herr)
	}
}

// tool_choice 的具名强制工具必须显式报错（此前 {"type":"tool"} 被静默放行）。
func TestValidateToolsRejectsNamedToolChoice(t *testing.T) {
	td := ToolDef{Type: "function"}
	td.Function.Name = "fetch"
	req := &ChatRequest{Tools: []ToolDef{td}, ToolChoice: map[string]any{"type": "tool", "name": "fetch"}}
	if err := validateTools(req); err == nil {
		t.Fatal("tool_choice={type:tool} 应报错")
	}
	req2 := &ChatRequest{Tools: []ToolDef{td}, ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "fetch"}}}
	if err := validateTools(req2); err == nil {
		t.Fatal("OpenAI 形态的具名工具选择也应报错")
	}
	req3 := &ChatRequest{Tools: []ToolDef{td}, ToolChoice: "auto"}
	if err := validateTools(req3); err != nil {
		t.Fatalf("tool_choice=auto 不应报错: %v", err)
	}
}
