package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AccountConfig 上游账号：一个 OrcaTerm 登录态。
type AccountConfig struct {
	Name   string `json:"name"`
	Cookie string `json:"cookie"` // 从 OrcaTerm 客户端复制的 Cookie（含 ot_session / sid）
	Weight int    `json:"weight"`
	Proxy  string `json:"proxy"` // 该账号专用 socks5 代理，"none"=强制直连，空=跟随代理池
}

// ProxyConfig SOCKS5 代理池。
type ProxyConfig struct {
	Policy       string   `json:"policy"` // round-robin | random | sticky-key
	HealthEvery  string   `json:"health-every"`
	FailCooldown string   `json:"fail-cooldown"`
	SOCKS5       []string `json:"socks5"`
}

// RateLimitConfig 账号池治理。
type RateLimitConfig struct {
	MaxInFlight        int      `json:"max-in-flight"`
	Cooldown429        string   `json:"cooldown-429"`
	Cooldown5xx        string   `json:"cooldown-5xx"`
	ErrThreshold       int      `json:"err-threshold"`
	ErrCooldown        string   `json:"err-cooldown"`
	DisableOnAuthError bool     `json:"disable-on-auth-error"`
	MaxRetries         int      `json:"max-retries"`
	RetryOn            []int    `json:"retry-on"`
	Backoff            string   `json:"backoff"`
}

// UpstreamConfig 上游 OrcaTerm AI。
type UpstreamConfig struct {
	BaseURL           string          `json:"base-url"`
	Product           string          `json:"product"`
	Origin            string          `json:"origin"`
	UserAgent         string          `json:"user-agent"`
	DefaultModel      string          `json:"default-model"`
	ForceModel        bool            `json:"force-model"`
	AllowUnknownModel bool            `json:"allow-unknown-model"`
	Timeout           string          `json:"timeout"`
	HideTools         bool            `json:"hide-tools"`
	NoAsk             bool            `json:"no-ask"` // 默认开：注入「无人值守，默认执行」行为指令 + ask 自动确认
	MaxToolRounds     int             `json:"max-tool-rounds"`
	Keys              []AccountConfig `json:"keys"`
}

// SessionConfig 会话粘性。
type SessionConfig struct {
	TTL string `json:"ttl"`
	Max int    `json:"max"`
}

// RemoteMgmtConfig 管理面板。
type RemoteMgmtConfig struct {
	AllowRemote bool   `json:"allow-remote"`
	SecretKey   string `json:"secret-key"`
}

// MetricsConfig 用量统计。
type MetricsConfig struct {
	Enabled    bool   `json:"enabled"`
	StateFile  string `json:"state-file"`
	FlushEvery string `json:"flush-every"`
}

// Config 网关全部配置。
type Config struct {
	Port             int              `json:"port"`
	APIKeys          []string         `json:"api-keys"`
	Upstream         UpstreamConfig   `json:"upstream"`
	Proxy            ProxyConfig      `json:"proxy"`
	RateLimit        RateLimitConfig  `json:"rate-limit"`
	Sessions         SessionConfig    `json:"sessions"`
	RemoteManagement RemoteMgmtConfig `json:"remote-management"`
	Metrics          MetricsConfig    `json:"metrics"`

	path string
	mu   *sync.RWMutex
}

// Defaults 返回默认配置。
func Defaults() *Config {
	return &Config{
		mu: &sync.RWMutex{},
		Port:    8317,
		APIKeys: []string{},
		Upstream: UpstreamConfig{
			BaseURL:           "https://lightai.cloud.tencent.com",
			Product:           "orcaterm",
			Origin:            "http://tauri.localhost",
			UserAgent:         "",
			DefaultModel:      "deepseek-v4-flash",
			ForceModel:        false, // 默认跟随下游选择的模型；需要统一出口时再显式开启
			AllowUnknownModel: false,
			Timeout:           "300s",
			HideTools:         true,
			NoAsk:             true,
			MaxToolRounds:     8,
			Keys:              []AccountConfig{},
		},
		Proxy: ProxyConfig{
			Policy:       "round-robin",
			HealthEvery:  "30s",
			FailCooldown: "30s",
			SOCKS5:       []string{},
		},
		RateLimit: RateLimitConfig{
			MaxInFlight:        2,
			Cooldown429:        "60s",
			Cooldown5xx:        "30s",
			ErrThreshold:       3,
			ErrCooldown:        "10m",
			DisableOnAuthError: true,
			MaxRetries:         3,
			RetryOn:            []int{429, 500, 502, 503, 504},
			Backoff:            "1s",
		},
		Sessions: SessionConfig{
			TTL: "30m",
			Max: 1000,
		},
		RemoteManagement: RemoteMgmtConfig{
			AllowRemote: true,
			SecretKey:   "",
		},
		Metrics: MetricsConfig{
			Enabled:    true,
			StateFile:  "state/state.json",
			FlushEvery: "5s",
		},
	}
}

// Load 从 path 读取配置；文件不存在则用默认配置并写回。
func Load(path string) (*Config, error) {
	c := Defaults()
	c.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if err := c.Save(); err != nil {
				return nil, fmt.Errorf("生成默认配置失败: %w", err)
			}
			c.applyEnv()
			return c, nil
		}
		return nil, err
	}
	m, err := parseYAML(string(data))
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	c.applyMap(m)
	c.applyEnv()
	return c, nil
}

// Path 返回配置文件路径。
func (c *Config) Path() string { return c.path }

// ---- map 解码 ----

func str(m map[string]any, key, def string) string {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	}
	return def
}

func boolean(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true", "yes", "on", "1":
				return true
			case "false", "no", "off", "0":
				return false
			}
		}
	}
	return def
}

func integer(m map[string]any, key string, def int) int {
	if v, ok := m[key]; ok && v != nil {
		if s, ok := v.(string); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				return n
			}
		}
	}
	return def
}

func strList(m map[string]any, key string) []string {
	v, ok := m[key]
	if !ok || v == nil {
		return []string{}
	}
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if e != nil {
				out = append(out, fmt.Sprint(e))
			}
		}
		return out
	default:
		return []string{}
	}
}

func intList(m map[string]any, key string, def []int) []int {
	v, ok := m[key]
	if !ok || v == nil {
		return def
	}
	t, ok := v.([]any)
	if !ok {
		return def
	}
	out := make([]int, 0, len(t))
	for _, e := range t {
		if s, ok := e.(string); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
				out = append(out, n)
			}
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func subMap(m map[string]any, key string) map[string]any {
	if v, ok := m[key]; ok {
		if sm, ok := v.(map[string]any); ok {
			return sm
		}
	}
	return map[string]any{}
}

func (c *Config) applyMap(m map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.Port = integer(m, "port", c.Port)
	c.APIKeys = strList(m, "api-keys")

	u := subMap(m, "upstream")
	c.Upstream.BaseURL = str(u, "base-url", c.Upstream.BaseURL)
	c.Upstream.Product = str(u, "product", c.Upstream.Product)
	c.Upstream.Origin = str(u, "origin", c.Upstream.Origin)
	c.Upstream.UserAgent = str(u, "user-agent", c.Upstream.UserAgent)
	c.Upstream.DefaultModel = str(u, "default-model", c.Upstream.DefaultModel)
	c.Upstream.ForceModel = boolean(u, "force-model", c.Upstream.ForceModel)
	c.Upstream.AllowUnknownModel = boolean(u, "allow-unknown-model", c.Upstream.AllowUnknownModel)
	c.Upstream.Timeout = str(u, "timeout", c.Upstream.Timeout)
	c.Upstream.HideTools = boolean(u, "hide-tools", c.Upstream.HideTools)
	c.Upstream.NoAsk = boolean(u, "no-ask", c.Upstream.NoAsk)
	c.Upstream.MaxToolRounds = integer(u, "max-tool-rounds", c.Upstream.MaxToolRounds)
	if kv, ok := u["keys"]; ok && kv != nil {
		if arr, ok := kv.([]any); ok {
			keys := make([]AccountConfig, 0, len(arr))
			for _, e := range arr {
				em, ok := e.(map[string]any)
				if !ok {
					continue
				}
				keys = append(keys, AccountConfig{
					Name:   str(em, "name", ""),
					Cookie: str(em, "cookie", ""),
					Weight: integer(em, "weight", 1),
					Proxy:  str(em, "proxy", ""),
				})
			}
			c.Upstream.Keys = keys
		}
	}

	p := subMap(m, "proxy")
	c.Proxy.Policy = str(p, "policy", c.Proxy.Policy)
	c.Proxy.HealthEvery = str(p, "health-every", c.Proxy.HealthEvery)
	c.Proxy.FailCooldown = str(p, "fail-cooldown", c.Proxy.FailCooldown)
	if _, ok := p["socks5"]; ok {
		var urls []string
		switch t := p["socks5"].(type) {
		case []any:
			for _, e := range t {
				switch v := e.(type) {
				case string:
					urls = append(urls, strings.TrimSpace(v))
				case map[string]any:
					// 兼容 "- url: socks5://..." 写法
					if u, ok := v["url"].(string); ok && strings.TrimSpace(u) != "" {
						urls = append(urls, strings.TrimSpace(u))
					}
				}
			}
		}
		c.Proxy.SOCKS5 = urls
	}

	r := subMap(m, "rate-limit")
	c.RateLimit.MaxInFlight = integer(r, "max-in-flight", c.RateLimit.MaxInFlight)
	c.RateLimit.Cooldown429 = str(r, "cooldown-429", c.RateLimit.Cooldown429)
	c.RateLimit.Cooldown5xx = str(r, "cooldown-5xx", c.RateLimit.Cooldown5xx)
	c.RateLimit.ErrThreshold = integer(r, "err-threshold", c.RateLimit.ErrThreshold)
	c.RateLimit.ErrCooldown = str(r, "err-cooldown", c.RateLimit.ErrCooldown)
	c.RateLimit.DisableOnAuthError = boolean(r, "disable-on-auth-error", c.RateLimit.DisableOnAuthError)
	c.RateLimit.MaxRetries = integer(r, "max-retries", c.RateLimit.MaxRetries)
	c.RateLimit.RetryOn = intList(r, "retry-on", c.RateLimit.RetryOn)
	c.RateLimit.Backoff = str(r, "backoff", c.RateLimit.Backoff)

	s := subMap(m, "sessions")
	c.Sessions.TTL = str(s, "ttl", c.Sessions.TTL)
	c.Sessions.Max = integer(s, "max", c.Sessions.Max)

	rm := subMap(m, "remote-management")
	c.RemoteManagement.AllowRemote = boolean(rm, "allow-remote", c.RemoteManagement.AllowRemote)
	c.RemoteManagement.SecretKey = str(rm, "secret-key", c.RemoteManagement.SecretKey)

	mc := subMap(m, "metrics")
	c.Metrics.Enabled = boolean(mc, "enabled", c.Metrics.Enabled)
	c.Metrics.StateFile = str(mc, "state-file", c.Metrics.StateFile)
	c.Metrics.FlushEvery = str(mc, "flush-every", c.Metrics.FlushEvery)
}

// applyEnv 环境变量覆盖（ORCATERM2API_ 前缀）。
func (c *Config) applyEnv() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v := os.Getenv("ORCATERM2API_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Port = n
		}
	}
	if v := os.Getenv("ORCATERM2API_MGMT_KEY"); v != "" {
		c.RemoteManagement.SecretKey = v
	}
	if v := os.Getenv("ORCATERM2API_ALLOW_REMOTE"); v != "" {
		c.RemoteManagement.AllowRemote = v == "1" || strings.EqualFold(v, "true")
	}
	if v := os.Getenv("ORCATERM2API_API_KEYS"); v != "" {
		c.APIKeys = strings.Split(v, ",")
		for i := range c.APIKeys {
			c.APIKeys[i] = strings.TrimSpace(c.APIKeys[i])
		}
	}
	if v := os.Getenv("ORCATERM2API_BASE_URL"); v != "" {
		c.Upstream.BaseURL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv("ORCATERM2API_DEFAULT_MODEL"); v != "" {
		c.Upstream.DefaultModel = v
	}
}

// Snapshot 返回一份拷贝，供只读使用。
func (c *Config) Snapshot() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := Config{
		Port:             c.Port,
		APIKeys:          append([]string{}, c.APIKeys...),
		Upstream:         c.Upstream,
		Proxy:            c.Proxy,
		RateLimit:        c.RateLimit,
		Sessions:         c.Sessions,
		RemoteManagement: c.RemoteManagement,
		Metrics:          c.Metrics,
		path:             c.path,
	}
	cp.Upstream.Keys = append([]AccountConfig{}, c.Upstream.Keys...)
	cp.Proxy.SOCKS5 = append([]string{}, c.Proxy.SOCKS5...)
	cp.RateLimit.RetryOn = append([]int{}, c.RateLimit.RetryOn...)
	return cp
}

// Update 原子更新配置并回写文件。
func (c *Config) Update(fn func(*Config)) error {
	c.mu.Lock()
	fn(c)
	c.mu.Unlock()
	return c.Save()
}

// Save 原子回写配置文件（tmp + rename，0600）。
func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.path == "" {
		return fmt.Errorf("未指定配置文件路径")
	}
	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(c.render()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

func yamlStr(s string) string {
	if s == "" {
		return `""`
	}
	need := false
	for _, r := range s {
		if r == ':' || r == '#' || r == '\n' || r == '"' || r == '\'' || r == '{' || r == '}' || r == '[' || r == ']' {
			need = true
			break
		}
	}
	if strings.HasPrefix(s, " ") || strings.HasSuffix(s, " ") || need {
		esc := strings.ReplaceAll(s, `\`, `\\`)
		esc = strings.ReplaceAll(esc, `"`, `\"`)
		esc = strings.ReplaceAll(esc, "\n", `\n`)
		esc = strings.ReplaceAll(esc, "\r", `\r`)
		return `"` + esc + `"`
	}
	return s
}

// render 按固定模板生成 YAML。
func (c *Config) render() string {
	var b strings.Builder
	b.WriteString("# orcaterm2api 配置文件（面板修改会自动回写）\n")
	fmt.Fprintf(&b, "port: %d\n\n", c.Port)
	b.WriteString("api-keys:")
	if len(c.APIKeys) == 0 {
		b.WriteString(" []\n")
	} else {
		b.WriteString("\n")
		for _, k := range c.APIKeys {
			fmt.Fprintf(&b, "  - %s\n", yamlStr(k))
		}
	}
	b.WriteString("\nupstream:\n")
	fmt.Fprintf(&b, "  base-url: %s\n", yamlStr(c.Upstream.BaseURL))
	fmt.Fprintf(&b, "  product: %s\n", yamlStr(c.Upstream.Product))
	fmt.Fprintf(&b, "  origin: %s\n", yamlStr(c.Upstream.Origin))
	fmt.Fprintf(&b, "  user-agent: %s\n", yamlStr(c.Upstream.UserAgent))
	fmt.Fprintf(&b, "  default-model: %s\n", yamlStr(c.Upstream.DefaultModel))
	fmt.Fprintf(&b, "  force-model: %v\n", c.Upstream.ForceModel)
	fmt.Fprintf(&b, "  allow-unknown-model: %v\n", c.Upstream.AllowUnknownModel)
	fmt.Fprintf(&b, "  timeout: %s\n", yamlStr(c.Upstream.Timeout))
	fmt.Fprintf(&b, "  hide-tools: %v\n", c.Upstream.HideTools)
	fmt.Fprintf(&b, "  no-ask: %v\n", c.Upstream.NoAsk)
	fmt.Fprintf(&b, "  max-tool-rounds: %d\n", c.Upstream.MaxToolRounds)
	b.WriteString("  keys:\n")
	for _, k := range c.Upstream.Keys {
		fmt.Fprintf(&b, "    - name: %s\n", yamlStr(k.Name))
		fmt.Fprintf(&b, "      cookie: %s\n", yamlStr(k.Cookie))
		fmt.Fprintf(&b, "      weight: %d\n", k.Weight)
		fmt.Fprintf(&b, "      proxy: %s\n", yamlStr(k.Proxy))
	}
	b.WriteString("\nproxy:\n")
	fmt.Fprintf(&b, "  policy: %s\n", yamlStr(c.Proxy.Policy))
	fmt.Fprintf(&b, "  health-every: %s\n", yamlStr(c.Proxy.HealthEvery))
	fmt.Fprintf(&b, "  fail-cooldown: %s\n", yamlStr(c.Proxy.FailCooldown))
	b.WriteString("  socks5:\n")
	for _, u := range c.Proxy.SOCKS5 {
		fmt.Fprintf(&b, "    - url: %s\n", yamlStr(u))
	}
	b.WriteString("\nrate-limit:\n")
	fmt.Fprintf(&b, "  max-in-flight: %d\n", c.RateLimit.MaxInFlight)
	fmt.Fprintf(&b, "  cooldown-429: %s\n", yamlStr(c.RateLimit.Cooldown429))
	fmt.Fprintf(&b, "  cooldown-5xx: %s\n", yamlStr(c.RateLimit.Cooldown5xx))
	fmt.Fprintf(&b, "  err-threshold: %d\n", c.RateLimit.ErrThreshold)
	fmt.Fprintf(&b, "  err-cooldown: %s\n", yamlStr(c.RateLimit.ErrCooldown))
	fmt.Fprintf(&b, "  disable-on-auth-error: %v\n", c.RateLimit.DisableOnAuthError)
	fmt.Fprintf(&b, "  max-retries: %d\n", c.RateLimit.MaxRetries)
	fmt.Fprintf(&b, "  retry-on: [%s]\n", intsJoin(c.RateLimit.RetryOn))
	fmt.Fprintf(&b, "  backoff: %s\n", yamlStr(c.RateLimit.Backoff))
	b.WriteString("\nsessions:\n")
	fmt.Fprintf(&b, "  ttl: %s\n", yamlStr(c.Sessions.TTL))
	fmt.Fprintf(&b, "  max: %d\n", c.Sessions.Max)
	b.WriteString("\nremote-management:\n")
	fmt.Fprintf(&b, "  allow-remote: %v\n", c.RemoteManagement.AllowRemote)
	fmt.Fprintf(&b, "  secret-key: %s\n", yamlStr(c.RemoteManagement.SecretKey))
	b.WriteString("\nmetrics:\n")
	fmt.Fprintf(&b, "  enabled: %v\n", c.Metrics.Enabled)
	fmt.Fprintf(&b, "  state-file: %s\n", yamlStr(c.Metrics.StateFile))
	fmt.Fprintf(&b, "  flush-every: %s\n", yamlStr(c.Metrics.FlushEvery))
	return b.String()
}

func intsJoin(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

// Watch 轮询文件 mtime，变化时重载并回调。
func (c *Config) Watch(stop <-chan struct{}, onChange func()) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	var last time.Time
	if fi, err := os.Stat(c.path); err == nil {
		last = fi.ModTime()
	}
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			fi, err := os.Stat(c.path)
			if err != nil {
				continue
			}
			if fi.ModTime().After(last) {
				last = fi.ModTime()
				if data, err := os.ReadFile(c.path); err == nil {
					if m, err := parseYAML(string(data)); err == nil {
						c.applyMap(m)
						c.applyEnv()
						if onChange != nil {
							onChange()
						}
					}
				}
			}
		}
	}
}

// ParseDuration 解析时长字符串，失败返回默认值。
func ParseDuration(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil {
		return d
	}
	return def
}
