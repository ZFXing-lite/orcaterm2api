package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/ZFXing-lite/orcaterm2api/internal/config"
	"github.com/ZFXing-lite/orcaterm2api/internal/keypool"
)

// 管理 API：/v0/management/api/*（需面板密码，见 X-Mgmt-Key）。

func (s *Server) handleManagementAPI(w http.ResponseWriter, r *http.Request) {
	if !s.mgmtAllowed(r) {
		writeJSON(w, 403, map[string]string{"error": "管理接口不允许远程访问"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v0/management/api/")
	if path != "login" && !s.checkMgmtAuth(r) {
		writeJSON(w, 401, map[string]string{"error": "管理鉴权失败（X-Mgmt-Key）"})
		return
	}
	switch {
	case path == "login" && r.Method == http.MethodPost:
		s.mgmtLogin(w, r)
	case path == "overview" && r.Method == http.MethodGet:
		s.mgmtOverview(w, r)
	case path == "accounts" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"accounts": s.pool.Snapshot()})
	case path == "accounts" && r.Method == http.MethodPost:
		s.mgmtAddAccount(w, r)
	case path == "accounts/import" && r.Method == http.MethodPost:
		s.mgmtImportAccounts(w, r)
	case strings.HasPrefix(path, "accounts/") && strings.HasSuffix(path, "/toggle") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "accounts/"), "/toggle")
		s.mgmtToggleAccount(w, r, id)
	case strings.HasPrefix(path, "accounts/") && r.Method == http.MethodPut:
		id := strings.TrimPrefix(path, "accounts/")
		s.mgmtUpdateAccount(w, r, id)
	case strings.HasPrefix(path, "accounts/") && r.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, "accounts/")
		s.mgmtDeleteAccount(w, r, id)
	case path == "keys" && r.Method == http.MethodGet:
		s.mgmtListKeys(w, r)
	case path == "keys" && r.Method == http.MethodPost:
		s.mgmtAddKey(w, r)
	case strings.HasPrefix(path, "keys/") && r.Method == http.MethodDelete:
		s.mgmtDeleteKey(w, r, strings.TrimPrefix(path, "keys/"))
	case path == "proxies" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"policy": s.cfg.Snapshot().Proxy.Policy, "proxies": s.proxies.Snapshot()})
	case path == "proxies" && r.Method == http.MethodPost:
		s.mgmtReplaceProxies(w, r)
	case path == "settings" && r.Method == http.MethodGet:
		s.mgmtGetSettings(w, r)
	case path == "settings" && r.Method == http.MethodPut:
		s.mgmtUpdateSettings(w, r)
	case path == "usage" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.tracker.Snapshot())
	default:
		writeJSON(w, 404, map[string]string{"error": "未知管理接口"})
	}
}

func (s *Server) mgmtLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	json.Unmarshal(data, &body)
	secret := s.cfg.Snapshot().RemoteManagement.SecretKey
	if secret == "" || body.Key == secret {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	writeJSON(w, 401, map[string]string{"error": "密码错误"})
}

func (s *Server) mgmtOverview(w http.ResponseWriter, r *http.Request) {
	snap := s.cfg.Snapshot()
	ready, total := s.pool.Count()
	writeJSON(w, 200, map[string]any{
		"service":       "orcaterm2api",
		"ready":         s.pool.Ready(),
		"healthy":       ready,
		"total":         total,
		"proxies":       s.proxies.Count(),
		"default_model": snap.Upstream.DefaultModel,
		"force_model":   snap.Upstream.ForceModel,
		"base_url":      snap.Upstream.BaseURL,
		"api_keys":      len(snap.APIKeys),
		"paths":         s.paths.Snapshot(),
		"usage":         s.tracker.Snapshot(),
	})
}

// ---- 账号 ----

func (s *Server) mgmtAddAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Cookie string `json:"cookie"`
		Weight int    `json:"weight"`
		Proxy  string `json:"proxy"`
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(data, &body); err != nil || strings.TrimSpace(body.Cookie) == "" {
		writeJSON(w, 400, map[string]string{"error": "cookie 不能为空"})
		return
	}
	if body.Weight <= 0 {
		body.Weight = 1
	}
	err := s.cfg.Update(func(c *config.Config) {
		c.Upstream.Keys = append(c.Upstream.Keys, config.AccountConfig{
			Name: body.Name, Cookie: strings.TrimSpace(body.Cookie), Weight: body.Weight, Proxy: body.Proxy,
		})
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.pool.SyncAccounts(s.cfg.Snapshot().Upstream.Keys)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// mgmtImportAccounts 批量导入：每行 "name----cookie" 或只有 cookie。
func (s *Server) mgmtImportAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err := json.Unmarshal(data, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求体错误"})
		return
	}
	added, skipped := 0, 0
	err := s.cfg.Update(func(c *config.Config) {
		existing := map[string]bool{}
		for _, k := range c.Upstream.Keys {
			existing[k.Cookie] = true
		}
		for _, line := range strings.Split(body.Text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			name, cookie := "", line
			if parts := strings.SplitN(line, "----", 2); len(parts) == 2 {
				name, cookie = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			}
			if cookie == "" || existing[cookie] {
				skipped++
				continue
			}
			existing[cookie] = true
			c.Upstream.Keys = append(c.Upstream.Keys, config.AccountConfig{Name: name, Cookie: cookie, Weight: 1})
			added++
		}
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.pool.SyncAccounts(s.cfg.Snapshot().Upstream.Keys)
	writeJSON(w, 200, map[string]any{"ok": true, "added": added, "skipped": skipped})
}

func (s *Server) mgmtToggleAccount(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Disabled bool   `json:"disabled"`
		Reason   string `json:"reason"`
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	json.Unmarshal(data, &body)
	if !s.pool.SetDisabled(id, body.Disabled, body.Reason) {
		writeJSON(w, 404, map[string]string{"error": "账号不存在"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) mgmtUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Name   *string `json:"name"`
		Weight *int    `json:"weight"`
		Proxy  *string `json:"proxy"`
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 65536))
	if err := json.Unmarshal(data, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求体错误"})
		return
	}
	acct := s.pool.Get(id)
	if acct == nil {
		writeJSON(w, 404, map[string]string{"error": "账号不存在"})
		return
	}
	err := s.cfg.Update(func(c *config.Config) {
		for i, k := range c.Upstream.Keys {
			if keypool.AccountIDOf(k.Cookie) == id {
				if body.Name != nil {
					c.Upstream.Keys[i].Name = *body.Name
				}
				if body.Weight != nil && *body.Weight > 0 {
					c.Upstream.Keys[i].Weight = *body.Weight
				}
				if body.Proxy != nil {
					c.Upstream.Keys[i].Proxy = *body.Proxy
				}
			}
		}
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.pool.SyncAccounts(s.cfg.Snapshot().Upstream.Keys)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) mgmtDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if !s.pool.Delete(id) {
		writeJSON(w, 404, map[string]string{"error": "账号不存在"})
		return
	}
	err := s.cfg.Update(func(c *config.Config) {
		var keys []config.AccountConfig
		for _, k := range c.Upstream.Keys {
			if keypool.AccountIDOf(k.Cookie) != id {
				keys = append(keys, k)
			}
		}
		c.Upstream.Keys = keys
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- 下游密钥 ----

func (s *Server) mgmtListKeys(w http.ResponseWriter, r *http.Request) {
	keys := s.cfg.Snapshot().APIKeys
	masked := make([]map[string]any, 0, len(keys))
	for i, k := range keys {
		m := "****"
		if len(k) > 8 {
			m = k[:4] + "****" + k[len(k)-4:]
		}
		masked = append(masked, map[string]any{"index": i, "preview": m})
	}
	writeJSON(w, 200, map[string]any{"keys": masked})
}

func (s *Server) mgmtAddKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err := json.Unmarshal(data, &body); err != nil || strings.TrimSpace(body.Key) == "" {
		writeJSON(w, 400, map[string]string{"error": "key 不能为空"})
		return
	}
	err := s.cfg.Update(func(c *config.Config) {
		c.APIKeys = append(c.APIKeys, strings.TrimSpace(body.Key))
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) mgmtDeleteKey(w http.ResponseWriter, r *http.Request, idxStr string) {
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "index 非法"})
		return
	}
	err = s.cfg.Update(func(c *config.Config) {
		if idx >= 0 && idx < len(c.APIKeys) {
			c.APIKeys = append(c.APIKeys[:idx], c.APIKeys[idx+1:]...)
		}
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ---- 代理 ----

func (s *Server) mgmtReplaceProxies(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string `json:"text"` // 按行粘贴
		Policy string `json:"policy"`
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(data, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求体错误"})
		return
	}
	var urls []string
	for _, line := range strings.Split(body.Text, "\n") {
		if u := strings.TrimSpace(line); u != "" {
			urls = append(urls, u)
		}
	}
	err := s.cfg.Update(func(c *config.Config) {
		c.Proxy.SOCKS5 = urls
		if body.Policy != "" {
			c.Proxy.Policy = body.Policy
		}
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.proxies.Sync(urls)
	if body.Policy != "" {
		s.proxies.SetPolicy(body.Policy)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "count": len(urls)})
}

// ---- 设置 ----

func (s *Server) mgmtGetSettings(w http.ResponseWriter, r *http.Request) {
	snap := s.cfg.Snapshot()
	writeJSON(w, 200, map[string]any{
		"port":              snap.Port,
		"base_url":          snap.Upstream.BaseURL,
		"default_model":     snap.Upstream.DefaultModel,
		"force_model":       snap.Upstream.ForceModel,
		"allow_unknown":     snap.Upstream.AllowUnknownModel,
		"timeout":           snap.Upstream.Timeout,
		"hide_tools":        snap.Upstream.HideTools,
		"max_tool_rounds":   snap.Upstream.MaxToolRounds,
		"proxy_policy":      snap.Proxy.Policy,
		"max_in_flight":     snap.RateLimit.MaxInFlight,
		"cooldown_429":      snap.RateLimit.Cooldown429,
		"cooldown_5xx":      snap.RateLimit.Cooldown5xx,
		"max_retries":       snap.RateLimit.MaxRetries,
		"session_ttl":       snap.Sessions.TTL,
		"allow_remote":      snap.RemoteManagement.AllowRemote,
		"has_secret_key":    snap.RemoteManagement.SecretKey != "",
	})
}

func (s *Server) mgmtUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	data, _ := io.ReadAll(io.LimitReader(r.Body, 65536))
	if err := json.Unmarshal(data, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求体错误"})
		return
	}
	str := func(k string) (string, bool) {
		if v, ok := body[k].(string); ok {
			return v, true
		}
		return "", false
	}
	boolean := func(k string) (bool, bool) {
		if v, ok := body[k].(bool); ok {
			return v, true
		}
		return false, false
	}
	num := func(k string) (int, bool) {
		if v, ok := body[k].(float64); ok {
			return int(v), true
		}
		return 0, false
	}
	err := s.cfg.Update(func(c *config.Config) {
		if v, ok := str("base_url"); ok && v != "" {
			c.Upstream.BaseURL = v
		}
		if v, ok := str("default_model"); ok && v != "" {
			c.Upstream.DefaultModel = v
		}
		if v, ok := boolean("force_model"); ok {
			c.Upstream.ForceModel = v
		}
		if v, ok := boolean("allow_unknown"); ok {
			c.Upstream.AllowUnknownModel = v
		}
		if v, ok := str("timeout"); ok && v != "" {
			c.Upstream.Timeout = v
		}
		if v, ok := boolean("hide_tools"); ok {
			c.Upstream.HideTools = v
		}
		if v, ok := num("max_tool_rounds"); ok && v > 0 {
			c.Upstream.MaxToolRounds = v
		}
		if v, ok := str("proxy_policy"); ok && v != "" {
			c.Proxy.Policy = v
			s.proxies.SetPolicy(v)
		}
		if v, ok := num("max_in_flight"); ok && v > 0 {
			c.RateLimit.MaxInFlight = v
		}
		if v, ok := str("cooldown_429"); ok && v != "" {
			c.RateLimit.Cooldown429 = v
		}
		if v, ok := str("cooldown_5xx"); ok && v != "" {
			c.RateLimit.Cooldown5xx = v
		}
		if v, ok := num("max_retries"); ok && v >= 0 {
			c.RateLimit.MaxRetries = v
		}
		if v, ok := str("session_ttl"); ok && v != "" {
			c.Sessions.TTL = v
		}
		if v, ok := boolean("allow_remote"); ok {
			c.RemoteManagement.AllowRemote = v
		}
		if v, ok := str("secret_key"); ok {
			c.RemoteManagement.SecretKey = v
		}
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
