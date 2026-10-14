// Package keypool 管理上游 OrcaTerm 账号池：加权轮询、冷却、熔断、
// 401 永久停用、跨账号重试、状态持久化。
package keypool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ZFXing-lite/orcaterm2api/internal/config"
	"github.com/ZFXing-lite/orcaterm2api/internal/orca"
)

// FailureKind 失败分类。
type FailureKind int

const (
	FailTransport FailureKind = iota // 传输层失败（代理挂/超时）：只临时降级
	FailRateLimited                   // 429
	FailServerError                   // 5xx / 上游业务错误
	FailAuthExpired                   // code=10050000：登录态失效
)

// Account 池内账号。
type Account struct {
	ID           string
	Name         string
	Cookie       string // 脱敏展示用
	cred         orca.Credential
	Weight       int
	Proxy        string
	Disabled     bool
	DisabledReason string
	CooldownUntil  time.Time
	CooldownReason string
	ConsecErrors int
	InFlight     int
	SuccessCount int64
	FailCount    int64
	LastUsed     time.Time
	LastError    string
	TokenExpiresAt time.Time
	TokenExpired   bool
	CredKind       string // oauth | console
}

func accountID(cookie string) string {
	h := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(h[:])[:16]
}

// AccountIDOf 由 cookie 计算账号 ID（供管理接口按 cookie 定位账号）。
func AccountIDOf(cookie string) string { return accountID(cookie) }

// Options 池行为参数。
type Options struct {
	MaxInFlight        int
	Cooldown429        time.Duration
	Cooldown5xx        time.Duration
	ErrThreshold       int
	ErrCooldown        time.Duration
	DisableOnAuthError bool
	MaxRetries         int
	RetryOn            []int
	Backoff            time.Duration
	StateFile          string
	FlushEvery         time.Duration
}

// OptionsFromConfig 从配置构造。
func OptionsFromConfig(c *config.Config) Options {
	snap := c.Snapshot()
	return Options{
		MaxInFlight:        snap.RateLimit.MaxInFlight,
		Cooldown429:        config.ParseDuration(snap.RateLimit.Cooldown429, 60*time.Second),
		Cooldown5xx:        config.ParseDuration(snap.RateLimit.Cooldown5xx, 30*time.Second),
		ErrThreshold:       snap.RateLimit.ErrThreshold,
		ErrCooldown:        config.ParseDuration(snap.RateLimit.ErrCooldown, 10*time.Minute),
		DisableOnAuthError: snap.RateLimit.DisableOnAuthError,
		MaxRetries:         snap.RateLimit.MaxRetries,
		RetryOn:            snap.RateLimit.RetryOn,
		Backoff:            config.ParseDuration(snap.RateLimit.Backoff, time.Second),
		StateFile:          snap.Metrics.StateFile,
		FlushEvery:         config.ParseDuration(snap.Metrics.FlushEvery, 5*time.Second),
	}
}

// Pool 账号池。
type Pool struct {
	mu       sync.Mutex
	accounts map[string]*Account
	order    []string
	opts     Options
	rng      *rand.Rand
	dirty    bool
	stopCh   chan struct{}
}

// New 创建账号池。
func New(opts Options) *Pool {
	p := &Pool{
		accounts: map[string]*Account{},
		opts:     opts,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
		stopCh:   make(chan struct{}),
	}
	p.loadState()
	go p.flushLoop()
	return p
}

// UpdateOptions 热更新池行为参数（配置热重载时调用）。
func (p *Pool) UpdateOptions(opts Options) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opts = opts
}

// SyncAccounts 按配置对齐账号（新增/更新/删除）。
func (p *Pool) SyncAccounts(cfgs []config.AccountConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, ac := range cfgs {
		if ac.Cookie == "" {
			continue
		}
		id := accountID(ac.Cookie)
		seen[id] = true
		cred := orca.ParseCookie(ac.Cookie)
		a, ok := p.accounts[id]
		if !ok {
			a = &Account{ID: id}
			p.accounts[id] = a
			p.order = append(p.order, id)
		}
		a.Name = ac.Name
		a.Cookie = ac.Cookie
		a.cred = cred
		if ac.Weight > 0 {
			a.Weight = ac.Weight
		} else if a.Weight <= 0 {
			a.Weight = 1
		}
		a.Proxy = ac.Proxy
		if exp, ok := orca.TokenExpiry(cred.OT); ok {
			a.TokenExpiresAt = exp
			a.TokenExpired = time.Now().After(exp)
		} else {
			// 控制台态无 JWT 过期声明，显示为非过期，由探活/失败计数接管
			a.TokenExpiresAt = time.Time{}
			a.TokenExpired = false
		}
		a.CredKind = cred.Kind()
	}
	for id := range p.accounts {
		if !seen[id] {
			delete(p.accounts, id)
		}
	}
	var order []string
	for _, id := range p.order {
		if _, ok := p.accounts[id]; ok {
			order = append(order, id)
		}
	}
	p.order = order
	p.dirty = true
}

// Credential 返回账号的上游凭证。
func (a *Account) Credential() orca.Credential { return a.cred }

// DisplayCookie 脱敏后的 cookie（仅展示长度）。
func (a *Account) DisplayCookie() string {
	if len(a.Cookie) <= 16 {
		return "***"
	}
	return a.Cookie[:8] + "..." + fmt.Sprintf("(%d字符)", len(a.Cookie))
}

func (p *Pool) eligibleLocked(now time.Time) []*Account {
	var out []*Account
	for _, id := range p.order {
		a := p.accounts[id]
		if a.Disabled {
			continue
		}
		if now.Before(a.CooldownUntil) {
			continue
		}
		if p.opts.MaxInFlight > 0 && a.InFlight >= p.opts.MaxInFlight {
			continue
		}
		if !a.cred.Ready() {
			continue
		}
		out = append(out, a)
	}
	return out
}

// Pick 加权随机选号：有效权重排序取 top5，加权抽签；权重相同时 LRU 打破平局。
// 全部冷却中时兜底选最快恢复的那个。
func (p *Pool) Pick() *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	cands := p.eligibleLocked(now)
	if len(cands) == 0 {
		// 兜底：全部冷却中时选最快恢复的那个（仍受 max-in-flight 约束，
		// 全满则返回 nil，避免打爆单号）
		var best *Account
		for _, id := range p.order {
			a := p.accounts[id]
			if a.Disabled || !a.cred.Ready() {
				continue
			}
			if p.opts.MaxInFlight > 0 && a.InFlight >= p.opts.MaxInFlight {
				continue
			}
			if best == nil || a.CooldownUntil.Before(best.CooldownUntil) {
				best = a
			}
		}
		if best == nil {
			return nil
		}
		best.InFlight++
		best.LastUsed = now
		return best
	}
	// 按权重降序，权重相同按 LastUsed 升序（LRU）
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].Weight != cands[j].Weight {
			return cands[i].Weight > cands[j].Weight
		}
		return cands[i].LastUsed.Before(cands[j].LastUsed)
	})
	n := 5
	if len(cands) < n {
		n = len(cands)
	}
	top := cands[:n]
	// 短名单内加权抽签
	total := 0
	for _, a := range top {
		total += a.Weight
	}
	r := p.rng.Intn(total)
	for _, a := range top {
		r -= a.Weight
		if r < 0 {
			a.InFlight++
			a.LastUsed = now
			return a
		}
	}
	top[0].InFlight++
	top[0].LastUsed = now
	return top[0]
}

// Acquire 手动占用一个账号的在途位。
func (p *Pool) Acquire(a *Account) {
	if a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a.InFlight++
	a.LastUsed = time.Now()
}

// Release 释放账号的在途占用。
func (p *Pool) Release(a *Account) {
	if a == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.InFlight > 0 {
		a.InFlight--
	}
}

// ReportSuccess 记录成功。
func (p *Pool) ReportSuccess(a *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a.SuccessCount++
	a.ConsecErrors = 0
	a.LastError = ""
	p.dirty = true
}

// ReportFailure 按分类记录失败并执行冷却/熔断/停用。
func (p *Pool) ReportFailure(a *Account, kind FailureKind, msg string, retryAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	a.FailCount++
	a.LastError = msg
	switch kind {
	case FailAuthExpired:
		if p.opts.DisableOnAuthError {
			a.Disabled = true
			a.DisabledReason = "登录态失效 (code=10050000)，请重新获取 Cookie"
		} else {
			a.CooldownUntil = now.Add(p.opts.ErrCooldown)
			a.CooldownReason = "登录态失效"
		}
	case FailRateLimited:
		d := p.opts.Cooldown429
		if retryAfter > 0 && retryAfter < 10*time.Minute {
			d = retryAfter
		}
		// 或门：冷却期间再次撞 429 不叠加惩罚
		if until := now.Add(d); until.After(a.CooldownUntil) {
			a.CooldownUntil = until
		}
		a.CooldownReason = "429 限速"
	case FailServerError:
		if until := now.Add(p.opts.Cooldown5xx); until.After(a.CooldownUntil) {
			a.CooldownUntil = until
		}
		a.CooldownReason = "上游 5xx/业务错误"
	case FailTransport:
		// 传输层失败只临时降级，不记在账号上
		if until := now.Add(p.opts.ErrCooldown); until.After(a.CooldownUntil) {
			a.CooldownUntil = until
		}
		a.CooldownReason = "传输失败（代理/超时）"
	}
	a.ConsecErrors++
	if kind != FailAuthExpired && p.opts.ErrThreshold > 0 && a.ConsecErrors >= p.opts.ErrThreshold {
		d := p.opts.ErrCooldown << (a.ConsecErrors - p.opts.ErrThreshold)
		if d > 6*time.Hour {
			d = 6 * time.Hour
		}
		if until := now.Add(d); until.After(a.CooldownUntil) {
			a.CooldownUntil = until
		}
		a.CooldownReason = fmt.Sprintf("连续失败 %d 次熔断", a.ConsecErrors)
	}
	p.dirty = true
}

// RetryableStatus 该状态码是否值得换号重试。
func (p *Pool) RetryableStatus(code int) bool {
	for _, c := range p.opts.RetryOn {
		if c == code {
			return true
		}
	}
	return false
}

func (p *Pool) backoff(attempt int) time.Duration {
	d := p.opts.Backoff << attempt
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	// ±25% 抖动
	j := time.Duration(p.rng.Int63n(int64(d) / 2))
	return d - time.Duration(int64(d)/4) + j
}

// DoWithRetry 跨账号重试执行 fn。
// fn 返回 (result, retryable, statusCode, err)。
func (p *Pool) DoWithRetry(ctx context.Context, fn func(a *Account) (any, bool, int, error)) (any, *Account, error) {
	return p.doWithRetry(ctx, fn)
}

func (p *Pool) doWithRetry(ctx context.Context, fn func(a *Account) (any, bool, int, error)) (any, *Account, error) {
	maxAttempts := p.opts.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}
		a := p.Pick()
		if a == nil {
			if lastErr != nil {
				return nil, nil, lastErr
			}
			return nil, nil, fmt.Errorf("无可用上游账号（全部冷却/停用/未配置）")
		}
		res, retryable, status, err := fn(a)
		p.Release(a)
		if err == nil {
			p.ReportSuccess(a)
			return res, a, nil
		}
		lastErr = err
		// 失败分类
		kind := FailServerError
		var authErr *orca.AuthError
		if isAuthErr(err, &authErr) {
			kind = FailAuthExpired
		} else if status == 429 {
			kind = FailRateLimited
		} else if status == 0 {
			kind = FailTransport
		}
		p.ReportFailure(a, kind, err.Error(), 0)
		// 登录态失效不在 RetryOn 列表内，但固定语义就是换号重试
		if !retryable || (authErr == nil && !p.RetryableStatus(status)) {
			return nil, nil, err
		}
		if authErr != nil {
			// 登录态失效换号不等待
			continue
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(p.backoff(attempt)):
		}
	}
	return nil, nil, lastErr
}

func isAuthErr(err error, target **orca.AuthError) bool {
	for err != nil {
		if ae, ok := err.(*orca.AuthError); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// ---- 管理 ----

// Snapshot 账号快照（脱敏）。
func (p *Pool) Snapshot() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]map[string]any, 0, len(p.order))
	for _, id := range p.order {
		a := p.accounts[id]
		cooling := now.Before(a.CooldownUntil)
		var cdLeft float64
		if cooling {
			cdLeft = a.CooldownUntil.Sub(now).Seconds()
		}
		out = append(out, map[string]any{
			"id":               a.ID,
			"name":             a.Name,
			"cookie_preview":   a.DisplayCookie(),
			"weight":           a.Weight,
			"proxy":            a.Proxy,
			"disabled":         a.Disabled,
			"disabled_reason":  a.DisabledReason,
			"cooling":          cooling,
			"cooldown_left_s":  cdLeft,
			"cooldown_reason":  a.CooldownReason,
			"consec_errors":    a.ConsecErrors,
			"in_flight":        a.InFlight,
			"success":          a.SuccessCount,
			"fail":             a.FailCount,
			"last_error":       a.LastError,
			"cred_ready":       a.cred.Ready(),
			"cred_kind":        a.CredKind,
			"token_expires_at": func() string {
				if a.TokenExpiresAt.IsZero() {
					return ""
				}
				return a.TokenExpiresAt.Format(time.RFC3339)
			}(),
			"token_expired": a.TokenExpired,
		})
	}
	return out
}

// Get 按 ID 取账号。
func (p *Pool) Get(id string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.accounts[id]
}

// SetDisabled 启用/停用账号。
func (p *Pool) SetDisabled(id string, disabled bool, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[id]
	if !ok {
		return false
	}
	a.Disabled = disabled
	a.DisabledReason = reason
	if !disabled {
		a.ConsecErrors = 0
		a.CooldownUntil = time.Time{}
	}
	p.dirty = true
	return true
}

// Delete 删除账号。
func (p *Pool) Delete(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.accounts[id]; !ok {
		return false
	}
	delete(p.accounts, id)
	var order []string
	for _, oid := range p.order {
		if oid != id {
			order = append(order, oid)
		}
	}
	p.order = order
	p.dirty = true
	return true
}

// Count 可用/总数。
func (p *Pool) Count() (ready, total int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	total = len(p.accounts)
	for _, a := range p.accounts {
		if !a.Disabled && !now.Before(a.CooldownUntil) && a.cred.Ready() {
			ready++
		}
	}
	return ready, total
}

// Ready 是否有可用账号。
func (p *Pool) Ready() bool {
	r, _ := p.Count()
	return r > 0
}

// Close 停止后台落盘。
func (p *Pool) Close() {
	close(p.stopCh)
	p.saveState()
}

// ---- 持久化 ----

type accountState struct {
	Disabled       bool      `json:"disabled"`
	DisabledReason string    `json:"disabled_reason"`
	CooldownUntil  time.Time `json:"cooldown_until"`
	CooldownReason string    `json:"cooldown_reason"`
	SuccessCount   int64     `json:"success"`
	FailCount      int64     `json:"fail"`
	Weight         int       `json:"weight"`
}

func (p *Pool) statePath() string {
	if p.opts.StateFile == "" {
		return "state/state.json"
	}
	return p.opts.StateFile
}

func (p *Pool) loadState() {
	data, err := os.ReadFile(p.statePath())
	if err != nil {
		return
	}
	var st map[string]accountState
	if json.Unmarshal(data, &st) != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, s := range st {
		a, ok := p.accounts[id]
		if !ok {
			continue
		}
		a.Disabled = s.Disabled
		a.DisabledReason = s.DisabledReason
		a.CooldownUntil = s.CooldownUntil
		a.CooldownReason = s.CooldownReason
		a.SuccessCount = s.SuccessCount
		a.FailCount = s.FailCount
		if s.Weight > 0 {
			a.Weight = s.Weight
		}
	}
}

func (p *Pool) flushLoop() {
	iv := p.opts.FlushEvery
	if iv <= 0 {
		iv = 5 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-t.C:
			p.mu.Lock()
			dirty := p.dirty
			p.dirty = false
			p.mu.Unlock()
			if dirty {
				p.saveState()
			}
		}
	}
}

func (p *Pool) saveState() {
	p.mu.Lock()
	st := make(map[string]accountState, len(p.accounts))
	for id, a := range p.accounts {
		st[id] = accountState{
			Disabled:       a.Disabled,
			DisabledReason:  a.DisabledReason,
			CooldownUntil:  a.CooldownUntil,
			CooldownReason:  a.CooldownReason,
			SuccessCount:    a.SuccessCount,
			FailCount:       a.FailCount,
			Weight:          a.Weight,
		}
	}
	p.mu.Unlock()
	data, err := json.Marshal(st)
	if err != nil {
		return
	}
	path := p.statePath()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	os.Rename(tmp, path)
}
