package keypool

import (
	"testing"
	"time"

	"github.com/ZFXing-lite/orcaterm2api/internal/config"
)

func testPool() *Pool {
	p := New(Options{
		MaxInFlight:        2,
		Cooldown429:        time.Minute,
		Cooldown5xx:        30 * time.Second,
		ErrThreshold:       3,
		ErrCooldown:        10 * time.Minute,
		DisableOnAuthError: true,
		MaxRetries:         2,
		RetryOn:            []int{429, 500},
		Backoff:            time.Millisecond,
		StateFile:          "",
		FlushEvery:         time.Hour,
	})
	p.SyncAccounts([]config.AccountConfig{
		{Name: "a", Cookie: "ot_session=1; sid=s1", Weight: 1},
		{Name: "b", Cookie: "ot_session=2; sid=s2", Weight: 3},
	})
	return p
}

func TestPickWeighted(t *testing.T) {
	p := testPool()
	defer p.Close()
	counts := map[string]int{}
	for i := 0; i < 400; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatal("Pick 返回 nil")
		}
		counts[a.Name]++
		p.Release(a)
	}
	// b 权重 3，应显著多于 a
	if counts["b"] <= counts["a"] {
		t.Errorf("加权异常: %v", counts)
	}
}

func TestCooldownAndDisable(t *testing.T) {
	p := testPool()
	defer p.Close()
	a := p.Pick()
	p.Release(a)
	p.ReportFailure(a, FailRateLimited, "429", 0)
	if time.Now().After(a.CooldownUntil) {
		t.Error("429 后应进入冷却")
	}
	p.ReportFailure(a, FailAuthExpired, "expired", 0)
	if !a.Disabled {
		t.Error("登录失效后应停用")
	}
	ready, total := p.Count()
	if total != 2 || ready != 1 {
		t.Errorf("ready=%d total=%d", ready, total)
	}
	if !p.SetDisabled(a.ID, false, "") {
		t.Error("重新启用失败")
	}
	if p.Get(a.ID).Disabled {
		t.Error("应已启用")
	}
}

func TestMaxInFlight(t *testing.T) {
	p := testPool()
	defer p.Close()
	var held []*Account
	for i := 0; i < 10; i++ {
		a := p.Pick()
		if a == nil {
			break
		}
		held = append(held, a)
	}
	// 2 账号 × maxInFlight 2 = 4
	if len(held) != 4 {
		t.Errorf("占用数=%d, want 4", len(held))
	}
	for _, a := range held {
		p.Release(a)
	}
}
