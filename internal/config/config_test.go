package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseExample(t *testing.T) {
	data, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Skip("config.example.yaml 不存在")
	}
	m, err := parseYAML(string(data))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	c := Defaults()
	c.applyMap(m)
	if c.Port != 8317 {
		t.Errorf("port=%d, want 8317", c.Port)
	}
	if c.Upstream.DefaultModel != "deepseek-v4-flash" {
		t.Errorf("default-model=%q", c.Upstream.DefaultModel)
	}
	want := []int{429, 500, 502, 503, 504}
	if len(c.RateLimit.RetryOn) != len(want) {
		t.Fatalf("retry-on=%v, want %v", c.RateLimit.RetryOn, want)
	}
	for i, n := range want {
		if c.RateLimit.RetryOn[i] != n {
			t.Fatalf("retry-on=%v, want %v", c.RateLimit.RetryOn, want)
		}
	}
	if len(c.Upstream.Keys) != 1 || c.Upstream.Keys[0].Name != "号1" {
		t.Errorf("keys=%+v", c.Upstream.Keys)
	}
	if !c.Upstream.ForceModel || c.RateLimit.DisableOnAuthError != true {
		t.Errorf("bool 字段解析异常")
	}
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := Defaults()
	c.Upstream.Keys = []AccountConfig{
		{Name: "a", Cookie: "ot_session=x; sid=y", Weight: 3, Proxy: "none"},
	}
	c.Proxy.SOCKS5 = []string{"socks5://u:p@1.2.3.4:1080"}
	c.path = path
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	c2, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c2.Upstream.Keys) != 1 || c2.Upstream.Keys[0].Cookie != "ot_session=x; sid=y" {
		t.Errorf("cookie 往返失真: %+v", c2.Upstream.Keys)
	}
	if len(c2.Proxy.SOCKS5) != 1 {
		t.Errorf("socks5 往返失真: %v", c2.Proxy.SOCKS5)
	}
	if c2.Upstream.Keys[0].Weight != 3 || c2.Upstream.Keys[0].Proxy != "none" {
		t.Errorf("账号字段往返失真: %+v", c2.Upstream.Keys[0])
	}
}

func TestParseCookieYAML(t *testing.T) {
	// cookie 含冒号/分号/等号，必须完整保留
	src := "upstream:\n  keys:\n    - name: n\n      cookie: \"ot_session=abc:def; sid=x=y\"\n      weight: 1\n"
	m, err := parseYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	c := Defaults()
	c.applyMap(m)
	if got := c.Upstream.Keys[0].Cookie; got != "ot_session=abc:def; sid=x=y" {
		t.Errorf("cookie=%q", got)
	}
}
