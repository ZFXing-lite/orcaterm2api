// Package metrics 记录每个账号/模型的 token 用量。
// 上游不返回真实 token 数，用量为本地估算（CJK 字符≈1 token，
// 拉丁字符≈4 字符 1 token），仅用于趋势观察，不代表上游计费。
package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unicode"
)

// Usage 单项用量。
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	Requests         int64 `json:"requests"`
	Errors           int64 `json:"errors"`
}

// Tracker 用量跟踪器。
type Tracker struct {
	mu      sync.Mutex
	byAcct  map[string]*Usage            // accountID -> 用量
	byModel map[string]*Usage            // model -> 用量
	byPair  map[string]map[string]*Usage // accountID -> model -> 用量
	enabled bool
	stopCh  chan struct{}
}

// New 创建跟踪器。
func New(enabled bool) *Tracker {
	return &Tracker{
		byAcct:  map[string]*Usage{},
		byModel: map[string]*Usage{},
		byPair:  map[string]map[string]*Usage{},
		enabled: enabled,
		stopCh:  make(chan struct{}),
	}
}

// EstimateTokens 粗略估算文本的 token 数。
func EstimateTokens(s string) int64 {
	var n int64
	latin := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			n++
			continue
		}
		if r > 127 {
			n++
			continue
		}
		latin++
		if latin >= 4 {
			n++
			latin = 0
		}
	}
	if latin > 0 {
		n++
	}
	return n
}

// Record 记录一次成功请求。
func (t *Tracker) Record(accountID, model string, promptTokens, completionTokens int64) {
	if !t.enabled {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	inc := func(u *Usage) {
		u.PromptTokens += promptTokens
		u.CompletionTokens += completionTokens
		u.Requests++
	}
	if _, ok := t.byAcct[accountID]; !ok {
		t.byAcct[accountID] = &Usage{}
	}
	inc(t.byAcct[accountID])
	if _, ok := t.byModel[model]; !ok {
		t.byModel[model] = &Usage{}
	}
	inc(t.byModel[model])
	if _, ok := t.byPair[accountID]; !ok {
		t.byPair[accountID] = map[string]*Usage{}
	}
	if _, ok := t.byPair[accountID][model]; !ok {
		t.byPair[accountID][model] = &Usage{}
	}
	inc(t.byPair[accountID][model])
}

// RecordError 记录一次失败。
func (t *Tracker) RecordError(accountID string) {
	if !t.enabled {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.byAcct[accountID]; !ok {
		t.byAcct[accountID] = &Usage{}
	}
	t.byAcct[accountID].Errors++
}

// Snapshot 快照。
func (t *Tracker) Snapshot() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	cp := func(u *Usage) map[string]any {
		return map[string]any{
			"prompt_tokens":     u.PromptTokens,
			"completion_tokens": u.CompletionTokens,
			"total_tokens":      u.PromptTokens + u.CompletionTokens,
			"requests":          u.Requests,
			"errors":            u.Errors,
		}
	}
	byAcct := map[string]any{}
	for id, u := range t.byAcct {
		byAcct[id] = cp(u)
	}
	byModel := map[string]any{}
	for m, u := range t.byModel {
		byModel[m] = cp(u)
	}
	return map[string]any{"by_account": byAcct, "by_model": byModel}
}

// PathStats 接口调用统计。
type PathStats struct {
	mu       sync.Mutex
	requests map[string]int64
	errors   map[string]int64
	inflight map[string]int64
	lastCode map[string]int
	lastErr  map[string]string
}

// NewPathStats 创建。
func NewPathStats() *PathStats {
	return &PathStats{
		requests: map[string]int64{},
		errors:   map[string]int64{},
		inflight: map[string]int64{},
		lastCode: map[string]int{},
		lastErr:  map[string]string{},
	}
}

// Begin 记录请求开始，返回结束回调。
func (s *PathStats) Begin(path string) func(status int, errMsg string) {
	s.mu.Lock()
	s.requests[path]++
	s.inflight[path]++
	s.mu.Unlock()
	return func(status int, errMsg string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.inflight[path]--
		s.lastCode[path] = status
		if status >= 400 {
			s.errors[path]++
			s.lastErr[path] = errMsg
		} else {
			s.lastErr[path] = ""
		}
	}
}

// Snapshot 快照。
func (s *PathStats) Snapshot() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for p, n := range s.requests {
		out = append(out, map[string]any{
			"path":       p,
			"requests":   n,
			"errors":     s.errors[p],
			"inflight":   s.inflight[p],
			"last_code":  s.lastCode[p],
			"last_error": s.lastErr[p],
		})
	}
	return out
}

// Persist 周期性把用量落盘（与 keypool 共用 state 目录思想，独立文件）。
func (t *Tracker) Persist(path string, every time.Duration) {
	if !t.enabled || every <= 0 {
		return
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-tick.C:
			t.save(path)
		}
	}
}

func (t *Tracker) save(path string) {
	snap := t.Snapshot() // Snapshot 内部自己加锁，此处不能预先持锁
	data, err := json.Marshal(snap)
	if err != nil {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	os.Rename(tmp, path)
}

// Close 停止。
func (t *Tracker) Close() { close(t.stopCh) }
