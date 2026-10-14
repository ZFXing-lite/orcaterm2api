package orca

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestParseCookie(t *testing.T) {
	c := ParseCookie("ot_session=AAA.BBB.CCC; sid=SID123; userAccountLoginMethod=oauth")
	if c.OT != "AAA.BBB.CCC" || c.SID != "SID123" {
		t.Errorf("got %+v", c)
	}
	if c.Ready() != true {
		t.Error("应为 Ready")
	}
	c2 := ParseCookie("ot_session=AAA;")
	if c2.Ready() {
		t.Error("缺 sid 不应 Ready")
	}
}

func TestParseCookieUserIDFromJWT(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"userId":"123456"}`))
	c := ParseCookie("ot_session=h." + payload + ".s; sid=x")
	if c.UserID != "123456" {
		t.Errorf("UserID=%q", c.UserID)
	}
}

func TestResolveModel(t *testing.T) {
	u, err := ResolveModel("deepseek-v4-flash", "TokenHub/deepseek-v4-flash", false)
	if err != nil || u != "TokenHub/deepseek-v4-flash" {
		t.Errorf("got %q, %v", u, err)
	}
	u, err = ResolveModel("orcaterm-assistant", "TokenHub/deepseek-v4-flash", false)
	if err != nil || u != "TokenHub/deepseek-v4-flash" {
		t.Errorf("alias got %q, %v", u, err)
	}
	if _, err := ResolveModel("nope", "TokenHub/deepseek-v4-flash", false); err == nil {
		t.Error("未知模型应报错")
	}
	if u, err := ResolveModel("nope", "TokenHub/deepseek-v4-flash", true); err != nil || u != "nope" {
		t.Errorf("allowUnknown got %q, %v", u, err)
	}
}

func TestBuildACPPrompt(t *testing.T) {
	s := BuildACPPrompt("hello", []ImagePart{{URL: "https://x/y.png"}})
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	if m["_format"] != "acp-prompt" {
		t.Errorf("format=%v", m["_format"])
	}
	prompt := m["prompt"].([]any)
	if len(prompt) != 2 {
		t.Fatalf("prompt blocks=%d", len(prompt))
	}
}

func TestBuildToolResultPrompt(t *testing.T) {
	s := BuildToolResultPrompt("fetch", "ok output")
	if !strings.HasPrefix(s, "<TOOL_META>返回结果</TOOL_META>\n") {
		t.Errorf("前缀不对: %q", s)
	}
	s2 := BuildToolResultPrompt("fetch", "")
	if !strings.Contains(s2, "拒绝") {
		t.Errorf("空输出应生成拒绝文本: %q", s2)
	}
}

func TestNewConversationID(t *testing.T) {
	id := NewConversationID("orcaterm")
	if !strings.HasPrefix(id, "cid-") || !strings.HasSuffix(id, "-orcaterm") {
		t.Errorf("id=%q", id)
	}
}

func TestParseOutcome(t *testing.T) {
	o, err := parseOutcome([]byte(`{"code":0,"data":{"action":"completion","taskCompletion":"hi"},"message":""}`))
	if err != nil || o.Text != "hi" || o.Action != "completion" {
		t.Errorf("got %+v, %v", o, err)
	}
	// 字符串形态的 code
	_, err = parseOutcome([]byte(`{"code":"10050000","message":"expired"}`))
	if _, ok := err.(*AuthError); !ok {
		t.Errorf("应为 AuthError, got %T %v", err, err)
	}
	// 数字形态的 code
	_, err = parseOutcome([]byte(`{"code":10050000,"message":"expired"}`))
	if _, ok := err.(*AuthError); !ok {
		t.Errorf("应为 AuthError, got %T %v", err, err)
	}
	o, err = parseOutcome([]byte(`{"code":0,"data":{"action":"review","tool":{"type":"mcp","name":"fetch","args":{"url":"https://x"}}},"message":""}`))
	if err != nil || !o.HasToolCall() || o.Tool.Name != "fetch" {
		t.Errorf("got %+v, %v", o, err)
	}
}
