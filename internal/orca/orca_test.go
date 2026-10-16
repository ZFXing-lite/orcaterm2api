package orca

import (
	"net/http"
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
	if c.Kind() != "oauth" || !c.Ready() {
		t.Errorf("桌面态应 Ready，kind=%q", c.Kind())
	}
	// sid 为可选 API 会话 cookie（上游自动发放），缺失不影响就绪
	c2 := ParseCookie("ot_session=AAA;")
	if c2.Kind() != "oauth" || !c2.Ready() {
		t.Errorf("缺 sid 的桌面态应 Ready，kind=%q", c2.Kind())
	}
}

func TestParseCookieConsole(t *testing.T) {
	c := ParseCookie("uin=o123456789001; skey=fake-skey-demo; userId=123456789012; userAccountType=txcloud; userAccountLoginMethod=console")
	if c.Kind() != "console" || !c.Ready() {
		t.Errorf("控制台态应 Ready，kind=%q", c.Kind())
	}
	if c.Uin != "o123456789001" || c.Skey != "fake-skey-demo" || c.UserID != "123456789012" {
		t.Errorf("字段提取错误: %+v", c)
	}
	if c.Raw == "" || !strings.Contains(c.Raw, "skey=fake-skey-demo") {
		t.Errorf("Raw 应保真存储原始串: %q", c.Raw)
	}
	// 只有 uin 没 skey → 不完整
	c2 := ParseCookie("uin=o987654;")
	if c2.Kind() != "" || c2.Ready() {
		t.Errorf("缺 skey 不应 Ready，kind=%q", c2.Kind())
	}
	// 两种形态都没有 → 无效
	c3 := ParseCookie("language=zh; other=x;")
	if c3.Kind() != "" || c3.Ready() {
		t.Error("无登录态 cookie 不应 Ready")
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

func TestSetHeadersByKind(t *testing.T) {
	cl := NewClient("", "", "", "", nil)
	// 桌面态：Bearer + ot_session cookie
	req1, _ := http.NewRequest(http.MethodPost, cl.chatURL(), nil)
	cred1 := ParseCookie("ot_session=AAA.BBB; sid=SID1; userId=123456")
	cl.setHeaders(req1, cred1)
	if got := req1.Header.Get("Authorization"); got != "Bearer AAA.BBB" {
		t.Errorf("桌面态 Authorization=%q", got)
	}
	if ck := req1.Header.Get("Cookie"); !strings.Contains(ck, "ot_session=AAA.BBB") || !strings.Contains(ck, "sid=SID1") {
		t.Errorf("桌面态 Cookie=%q", ck)
	}
	// 控制台态：无 Authorization，保真重放 Raw，头集切 Web 版流量
	req2, _ := http.NewRequest(http.MethodPost, cl.chatURL(), nil)
	cred2 := ParseCookie("uin=o987654; skey=SKY; userId=123456789012")
	cl.setHeaders(req2, cred2)
	if got := req2.Header.Get("Authorization"); got != "" {
		t.Errorf("控制台态不应带 Authorization, got %q", got)
	}
	ck := req2.Header.Get("Cookie")
	if !strings.Contains(ck, "uin=o987654") || !strings.Contains(ck, "skey=SKY") || !strings.Contains(ck, "userId=123456789012") {
		t.Errorf("控制台态 Cookie=%q", ck)
	}
	// 指纹对齐：Origin/Referer 切 Web 版，桌面特有头移除，浏览器头补齐
	if got := req2.Header.Get("Origin"); got != DefaultWebOrigin {
		t.Errorf("控制台态 Origin 应切 Web 版, got %q", got)
	}
	if got := req2.Header.Get("Referer"); got != DefaultWebOrigin+"/" {
		t.Errorf("控制台态 Referer=%q", got)
	}
	if got := req2.Header.Get("X-Referer"); got != "" {
		t.Errorf("Web 流量无 X-Referer, got %q", got)
	}
	if got := req2.Header.Get("X-Csrfcode"); got != "" {
		t.Errorf("Web 流量无 X-Csrfcode 头, got %q", got)
	}
	if got := req2.Header.Get("User-Agent"); got != DefaultWebUserAgent {
		t.Errorf("未配置 UA 时应切 Web 版 UA, got %q", got)
	}
	if got := req2.Header.Get("Sec-Fetch-Site"); got != "same-site" {
		t.Errorf("Sec-Fetch-Site=%q", got)
	}
	// 桌面态保持桌面指纹不被 Web 头污染
	if got := req1.Header.Get("Origin"); got != DefaultOrigin {
		t.Errorf("桌面态 Origin 应保持 tauri, got %q", got)
	}
	if got := req1.Header.Get("Referer"); got != "" {
		t.Errorf("桌面态不应有浏览器 Referer 头, got %q", got)
	}
}

func TestConsoleAuthExpiredVariants(t *testing.T) {
	// 实测抓到的两种控制台态票据失效特征，都必须归为 AuthError（触发停用换号）
	for _, msg := range []string{
		`{"code":500,"message":"NEED_UIN_AND_SKEY"}`,
		`{"code":500,"message":"VERIFY_SKEY_UIN_FAILED"}`,
	} {
		_, err := parseOutcome([]byte(msg))
		if _, ok := err.(*AuthError); !ok {
			t.Errorf("%s 应归为 AuthError, got %T (%v)", msg, err, err)
		}
	}
}

func TestStreamFieldExtractor(t *testing.T) {
	// 完整流：协议 JSON 逐 token，标记劈开 + 转义 + \u 跨 token
	x := NewStreamFieldExtractor()
	var out strings.Builder
	for _, tok := range []string{
		`{"action":"completion","thi`, `nking":"...","taskComp`,
		`letion":"第一段`, `\n第二段`, `\"`, `引号`, `\"`, ` unicode:`, `中文`,
		`"}sth"`, ` after`,
	} {
		out.WriteString(x.Feed(tok))
	}
	got := out.String()
	want := "第一段\n第二段\"引号\" unicode:中文"
	if got != want {
		t.Errorf("提取结果错误:\n got=%q\nwant=%q", got, want)
	}
	if !x.Emitted() {
		t.Error("应标记已产出")
	}
}

func TestStreamFieldExtractorAskQuestion(t *testing.T) {
	x := NewStreamFieldExtractor()
	var out strings.Builder
	for _, tok := range []string{`{"action":"ask","question":"要`, `继续吗` + "\"", `":"x"}`} {
		out.WriteString(x.Feed(tok))
	}
	if out.String() != "要继续吗" {
		t.Errorf("ask 提取错误: %q", out.String())
	}
}

func TestStreamFieldExtractorUnicodeSplit(t *testing.T) {
	// \uXXXX 劈成两个 token：4e2d = 中
	x := NewStreamFieldExtractor()
	out := x.Feed(`{"taskCompletion":"\`) // 反斜杠+u 被劈
	if out != "" {
		t.Errorf("转义未完成不应产出: %q", out)
	}
	out = x.Feed(`u4e`) // hex 不满 4
	if out != "" {
		t.Errorf("hex 不满 4 不应产出: %q", out)
	}
	out += x.Feed(`2d文`) // 补满 hex + 后续正常文本
	if out != "中文" {
		t.Errorf("unicode 跨 token 还原错误: %q", out)
	}
}

func TestStreamFieldExtractorSilentOnReview(t *testing.T) {
	// 工具调用（review）无可见字段：全程静默，由流末聚合兜底
	x := NewStreamFieldExtractor()
	var total string
	for _, tok := range []string{`{"action":"review","tool":{"na`, `me":"shell","args":{"cmd":"ls"}` + `}}`} {
		total += x.Feed(tok)
	}
	if total != "" || x.Emitted() {
		t.Errorf("review 流应静默: total=%q emitted=%v", total, x.Emitted())
	}
}

func TestParseOutcomeTextFenced(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"围栏+信封", "```json\n{\"code\":0,\"data\":{\"action\":\"completion\",\"taskCompletion\":\"你好\"}}\n```"},
		{"围栏+内层", "```json\n{\"action\":\"completion\",\"taskCompletion\":\"你好\"}\n```"},
		{"裸围栏+内层", "```\n{\"action\":\"ask\",\"question\":\"继续吗\"}\n```"},
		{"围栏带尾空白", "  ```JSON\n{\"action\":\"ask\",\"question\":\"继续吗\"}\n```  \n"},
		{"纯信封", "{\"code\":0,\"data\":{\"action\":\"completion\",\"taskCompletion\":\"你好\"}}"},
		{"纯内层", "{\"action\":\"completion\",\"taskCompletion\":\"你好\"}"},
	}
	for _, c := range cases {
		out, err := ParseOutcomeText(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if out.Text != "你好" && out.Text != "继续吗" {
			t.Errorf("%s: 提取文本错误 %q", c.name, out.Text)
		}
	}
}

func TestParseOutcomeTextRejectsGarbage(t *testing.T) {
	if _, err := ParseOutcomeText("```json\n完全不是JSON```"); err == nil {
		t.Error("非 JSON 内容应报协议错误")
	}
}

func TestBuildACPPromptWithNote(t *testing.T) {
	// 行为指令注入 prompt 首块；无指令时保持原形态
	out := BuildACPPromptWithNote("无人值守指令", "你好", nil)
	// map 键序列化按字母序，断言按位置序：指令文本必须出现在用户输入之前
	if strings.Index(out, "无人值守指令") > strings.Index(out, "你好") {
		t.Errorf("指令块应位于用户输入之前: %s", out)
	}
	if !strings.Contains(out, `"text":"你好"`) {
		t.Errorf("用户输入应保留: %s", out)
	}
	plain := BuildACPPrompt("你好", nil)
	if strings.Contains(plain, "无人值守") {
		t.Error("无指令时不应有额外块")
	}
}

func TestParseOutcomeTextLeadingNoise(t *testing.T) {
	// 真实加固场景：模型先说话再给围栏 JSON（前置噪声 + 噪声内含信封）
	in := "好的，这是执行结果：```json\n{\"code\":0,\"data\":{\"action\":\"completion\",\"taskCompletion\":\"最终文本\"}}\n```"
	out, err := ParseOutcomeText(in)
	if err != nil {
		t.Fatalf("前置噪声场景: %v", err)
	}
	if out.Text != "最终文本" {
		t.Errorf("文本提取错误: %q", out.Text)
	}
}

func TestStreamFieldExtractorSkipsEmptyField(t *testing.T) {
	// ask 响应：taskCompletion 空值在前、question 有内容在后——必须跳过空值提取 question
	x := NewStreamFieldExtractor()
	var out strings.Builder
	for _, tok := range []string{
		`{"code":0,"data":{"action":"ask","thinking":"","ta`, `skCompletion":"","question":"要继续吗"}`,
	} {
		out.WriteString(x.Feed(tok))
	}
	if out.String() != "要继续吗" {
		t.Errorf("空值跳过失败: %q", out.String())
	}
}
