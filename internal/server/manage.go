package server

import (
	"sync"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZFXing-lite/orcaterm2api/internal/config"
	"github.com/ZFXing-lite/orcaterm2api/internal/keypool"
	"github.com/ZFXing-lite/orcaterm2api/internal/orca"
)

// 管理 API：/v0/management/api/*（需面板密码，见 X-Mgmt-Key）。

func (s *Server) handleManagementAPI(w http.ResponseWriter, r *http.Request) {
	if !s.mgmtAllowed(r) {
		writeJSON(w, 403, map[string]string{"error": "管理接口不允许远程访问"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v0/management/api/")
	// pair/consume 由一次性配对码鉴权（书签脚本无管理密钥），豁免 X-Mgmt-Key
	if path != "login" && path != "pair/consume" && !s.checkMgmtAuth(r) {
		writeJSON(w, 401, map[string]string{"error": "管理鉴权失败（X-Mgmt-Key）"})
		return
	}
	if path == "pair/consume" {
		// 书签脚本跨域提交：允许任意来源，鉴权完全依赖一次性配对码
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
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
	case strings.HasPrefix(path, "keys/") && strings.HasSuffix(path, "/reveal") && r.Method == http.MethodGet:
		s.mgmtRevealKey(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "keys/"), "/reveal"))
	case path == "models/refresh" && r.Method == http.MethodPost:
		s.mgmtModelsRefresh(w, r)
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
	case path == "pair/code" && r.Method == http.MethodPost:
		s.mgmtCreatePairCode(w, r)
	case path == "pair/consume" && r.Method == http.MethodPost:
		s.mgmtPairConsume(w, r)
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

// ---- 一次性配对码收号（书签脚本 → 自动落盘）----

// pairSession 一对一配对会话：码即凭证，一次性消费。
type pairSession struct {
	Code    string
	Expires time.Time
}

const (
	pairTTL      = 15 * time.Minute
	pairMaxLive  = 64 // 未消费配对码上限，防滥用
	pairProbeTTL = 15 * time.Second
)

// mgmtCreatePairCode 生成一次性配对码 + 配套书签脚本。
func (s *Server) mgmtCreatePairCode(w http.ResponseWriter, r *http.Request) {
	s.pairMu.Lock()
	now := time.Now()
	for k, v := range s.pairs {
		if now.After(v.Expires) {
			delete(s.pairs, k)
		}
	}
	if len(s.pairs) >= pairMaxLive {
		s.pairMu.Unlock()
		writeJSON(w, 429, map[string]string{"error": "未消费配对码过多，请先完成或等待过期"})
		return
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		s.pairMu.Unlock()
		writeJSON(w, 500, map[string]string{"error": "随机数生成失败"})
		return
	}
	code := "ORCA-" + strings.ToUpper(hex.EncodeToString(buf))
	s.pairs[code] = &pairSession{Code: code, Expires: now.Add(pairTTL)}
	s.pairMu.Unlock()

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host
	writeJSON(w, 200, map[string]any{
		"code":       code,
		"expires_in": int(pairTTL.Seconds()),
		"bookmark":   pairBookmarklet(base, code),
		"hint":       "把书签拖到浏览器收藏栏，在已登录 OrcaTerm Web 版的页面点击即可自动收号",
	})
}

// pairBookmarklet 生成书签脚本：读取当前页 document.cookie 并投递给配对端点。
func pairBookmarklet(base, code string) string {
	return "javascript:(function(){fetch('" + base + "/v0/management/api/pair/consume'," +
		"{method:'POST',headers:{'Content-Type':'text/plain'}," +
		"body:JSON.stringify({code:'" + code + "',cookie:document.cookie})})" +
		".then(function(r){return r.text()}).then(function(t){alert(t)})" +
		".catch(function(e){alert('发送失败: '+e)})})()"
}

// mgmtPairConsume 配对码收号：校验码 → 实测凭证 → 落盘进池。
// 凭证无效不落盘，返回原因；码先消费后验证，天然防重放。
func (s *Server) mgmtPairConsume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code   string `json:"code"`
		Cookie string `json:"cookie"`
	}
	data, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err := json.Unmarshal(data, &body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "请求体错误"})
		return
	}
	code := strings.TrimSpace(strings.ToUpper(body.Code))
	s.pairMu.Lock()
	sess, ok := s.pairs[code]
	if ok {
		delete(s.pairs, code) // 一次性消费，无论后续成败
	}
	s.pairMu.Unlock()
	if !ok || time.Now().After(sess.Expires) {
		writeJSON(w, 410, map[string]string{"error": "配对码无效或已过期，请回面板重新生成"})
		return
	}

	cred := orca.ParseCookie(body.Cookie)
	if !cred.Ready() {
		writeJSON(w, 400, map[string]string{"error": "cookie 中没有可用登录态（需要 ot_session 或 uin+skey），请先登录 OrcaTerm Web 版"})
		return
	}

	// 落盘前实测凭证，坏票直接拒收
	snap := s.cfg.Snapshot()
	client := orca.NewClient(snap.Upstream.BaseURL, snap.Upstream.Product, snap.Upstream.Origin, snap.Upstream.UserAgent, nil)
	ctx, cancel := context.WithTimeout(r.Context(), pairProbeTTL)
	defer cancel()
	if err := client.Probe(ctx, cred); err != nil {
		writeJSON(w, 400, map[string]any{"error": "凭证实测失败，未入库: " + err.Error(), "kind": cred.Kind()})
		return
	}

	name := "pair-" + cred.Kind()
	if len(cred.UserID) >= 4 {
		name += "-" + cred.UserID[len(cred.UserID)-4:]
	}
	err := s.cfg.Update(func(c *config.Config) {
		for _, k := range c.Upstream.Keys {
			if k.Cookie == body.Cookie {
				return // 已存在（同 cookie 重复配对），不重复追加
			}
		}
		c.Upstream.Keys = append(c.Upstream.Keys, config.AccountConfig{
			Name: name, Cookie: body.Cookie, Weight: 1,
		})
	})
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.pool.SyncAccounts(s.cfg.Snapshot().Upstream.Keys)
	writeJSON(w, 200, map[string]any{"ok": true, "kind": cred.Kind(), "name": name, "hint": "收号成功，回面板查看"})
}

// mgmtRevealKey 单条揭示完整密钥（默认列表仅掩码；明文按需拉取，不落列表响应）。
func (s *Server) mgmtRevealKey(w http.ResponseWriter, r *http.Request, idxStr string) {
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "索引非法"})
		return
	}
	keys := s.cfg.Snapshot().APIKeys
	if idx < 0 || idx >= len(keys) {
		writeJSON(w, 404, map[string]string{"error": "密钥不存在"})
		return
	}
	writeJSON(w, 200, map[string]any{"index": idx, "key": keys[idx]})
}

// mgmtModelsRefresh 逐模型真实探活：用池内一个健康账号对内置模型目录各发一条
// 极小请求，返回每个模型的可用性/延迟/错误（一次显式动作，非自动轮询）。
func (s *Server) mgmtModelsRefresh(w http.ResponseWriter, r *http.Request) {
	acct := s.pool.Pick()
	if acct == nil {
		writeJSON(w, 409, map[string]string{"error": "账号池无可用账号，先添加或启用账号"})
		return
	}
	snap := s.cfg.Snapshot()
	// 探活专用短超时 client：12s 头超时 + 12s 空闲判死（不用全局 300s）
	probeHTTP := &http.Client{Timeout: 15 * time.Second}
	client := orca.NewClient(snap.Upstream.BaseURL, snap.Upstream.Product, snap.Upstream.Origin, snap.Upstream.UserAgent, probeHTTP)
	cred := acct.Credential()

	// 预检：先用第一个模型确认账号登录态，失效则短路全部（不浪费 17 次调用）
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer probeCancel()
	convID := orca.NewConversationID(snap.Upstream.Product)
	first := orca.Models[0]
	_, perr := client.Chat(probeCtx, cred, orca.ChatRequest{
		ConversationID: convID, Text: "hi",
		UpstreamModel: first.Upstream, HideTools: true,
	})
	if perr != nil {
		msg := perr.Error()
		if strings.Contains(msg, "10050000") || strings.Contains(msg, "NEED_UIN_AND_SKEY") || strings.Contains(msg, "VERIFY_SKEY_UIN_FAILED") {
			writeJSON(w, 409, map[string]string{"error": "探测账号登录态已失效，请在面板重新收号（或启用其他账号）"})
			return
		}
	}

	type result struct {
		Model    string `json:"model"`
		Upstream string `json:"upstream"`
		Legacy   bool   `json:"legacy"`
		OK       bool   `json:"ok"`
		MS       int64  `json:"ms"`
		Error    string `json:"error,omitempty"`
	}
	results := make([]result, len(orca.Models))
	sem := make(chan struct{}, 4) // 探活并发上限，避免打爆单号
	var wg sync.WaitGroup
	for i, m := range orca.Models {
		wg.Add(1)
		go func(i int, m orca.Model) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err := client.Chat(ctx, cred, orca.ChatRequest{
				ConversationID: convID, // 同会话：账号只多一条会话记录而不是 18 条
				Text:           "hi",
				UpstreamModel:  m.Upstream,
				HideTools:      true,
			})
			res := result{Model: m.ID, Upstream: m.Upstream, Legacy: m.Legacy, MS: time.Since(start).Milliseconds()}
			switch {
			case err == nil:
				res.OK = true
			default:
				msg := err.Error()
				if strings.Contains(msg, "10050000") || strings.Contains(msg, "NEED_UIN_AND_SKEY") || strings.Contains(msg, "VERIFY_SKEY_UIN_FAILED") {
					res.Error = "账号登录态已失效，请重新收号"
				} else if strings.Contains(strings.ToLower(msg), "model") {
					// 上游拒绝该模型名 → 模型不可用，不是账号问题
					res.Error = msg
				} else {
					res.Error = msg
					res.OK = true // 网络/限速等暂时性错误不判死模型
				}
			}
			results[i] = res
		}(i, m)
	}
	wg.Wait()
	usable := 0
	for _, res := range results {
		if res.OK {
			usable++
		}
	}
	writeJSON(w, 200, map[string]any{
		"models": results, "usable": usable, "total": len(results),
		"account": acct.ID, "probed_at": time.Now().Format(time.RFC3339),
	})
}
