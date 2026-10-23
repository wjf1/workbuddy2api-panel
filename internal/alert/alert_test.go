package alert

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func testCfg() Config {
	return Config{MassKillAccounts: 3, MassKillWindowMin: 10, CooldownMin: 30}
}

// TestNoteDisabledFiltersReasons 只统计会话失效类禁用：banned / 人工禁用不进事件流。
func TestNoteDisabledFiltersReasons(t *testing.T) {
	m := New(Config{MassKillAccounts: 1, MassKillWindowMin: 10, CooldownMin: 30}, "h", "test")
	m.NoteDisabled("u1", "封号号", "account banned by upstream (11140 request illegal), re-login required")
	m.NoteDisabled("u2", "手动", "manual disable (panel)")
	if p := m.Payload(); p != nil {
		t.Fatalf("banned/manual 不应触发 mass_kill, got %+v", p)
	}
	m.NoteDisabled("u3", "死号", "12153 session dead")
	p := m.Payload()
	if p == nil {
		t.Fatal("session dead 应触发 mass_kill（阈值 1）")
	}
	mk := p.(*MassKill)
	if mk.Count != 1 || len(mk.UIDs) != 1 || mk.UIDs[0] != "u3" {
		t.Fatalf("count=%d uids=%v want 1/[u3]", mk.Count, mk.UIDs)
	}
}

// TestMassKillThresholdAndDistinct 达标判定按 distinct uid：同号重复禁用只算一次。
func TestMassKillThresholdAndDistinct(t *testing.T) {
	m := New(testCfg(), "h", "test")
	m.NoteDisabled("u1", "a", "12153 session dead")
	m.NoteDisabled("u1", "a", "12153 session dead") // 同号重复：不达标
	if m.Payload() != nil {
		t.Fatal("同 uid 重复不应达标")
	}
	m.NoteDisabled("u2", "b", "refresh session dead")
	if m.Payload() != nil {
		t.Fatal("2 个 uid 不应达标（阈值 3）")
	}
	m.NoteDisabled("u3", "c", "12153 session dead")
	p := m.Payload()
	if p == nil {
		t.Fatal("3 个 uid 应达标")
	}
	mk := p.(*MassKill)
	if mk.Count != 3 || mk.WindowMinutes != 10 {
		t.Fatalf("count=%d window=%d want 3/10", mk.Count, mk.WindowMinutes)
	}
	if mk.FirstAt.IsZero() || mk.LastAt.IsZero() {
		t.Fatal("first_at/last_at 应非零")
	}
}

// TestSetConfigWindowAging 热改窗口后老事件老化，横幅自然消失。
func TestSetConfigWindowAging(t *testing.T) {
	m := New(Config{MassKillAccounts: 1, MassKillWindowMin: 10, CooldownMin: 30}, "h", "test")
	m.NoteDisabled("u1", "a", "12153 session dead")
	if m.Payload() == nil {
		t.Fatal("阈值 1 应立即达标")
	}
	m.SetConfig(Config{MassKillAccounts: 5, MassKillWindowMin: 10, CooldownMin: 30})
	if m.Payload() != nil {
		t.Fatal("阈值提到 5 后 1 个事件不应达标")
	}
	m.SetConfig(Config{MassKillAccounts: 1, MassKillWindowMin: 10, CooldownMin: 30})
	if m.Payload() == nil {
		t.Fatal("阈值改回 1 应重新达标")
	}
}

// TestWebhookFiredWithCooldown 达标外发 webhook + 冷却抑制第二次。
// 外发评估在每次 NoteDisabled 内即时触发（pool hook 的 goroutine 上下文），
// 窗口内 distinct uid 首次达到阈值的那一刻发出，载荷 count = 当时窗口内 distinct uid 数。
func TestWebhookFiredWithCooldown(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var lastBody webhookPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer srv.Close()

	m := New(Config{WebhookURL: srv.URL, MassKillAccounts: 3, MassKillWindowMin: 10, CooldownMin: 30}, "host-x", "1.16.0-test")
	m.NoteDisabled("u1", "甲", "12153 session dead")
	m.NoteDisabled("u2", "乙", "12153 session dead")
	m.maybeNotify() // 显式评估：2 < 3 未达标，不应外发
	mu.Lock()
	if calls != 0 {
		t.Fatalf("未达标不应外发, calls=%d", calls)
	}
	mu.Unlock()
	m.NoteDisabled("u3", "丙", "12153 session dead") // 第 3 号到达即达标，内部同步外发
	m.maybeNotify()                                  // 冷却内重复评估被抑制
	mu.Lock()
	if calls != 1 {
		t.Fatalf("webhook 调用次数=%d want 1", calls)
	}
	if lastBody.Event != "mass_session_dead" || lastBody.Count != 3 || lastBody.Host != "host-x" {
		t.Fatalf("载荷不符: event=%s count=%d host=%s", lastBody.Event, lastBody.Count, lastBody.Host)
	}
	if len(lastBody.Accounts) != 3 || lastBody.Accounts[0].Nickname != "甲" {
		t.Fatalf("accounts=%+v", lastBody.Accounts)
	}
	mu.Unlock()

	// 冷却内再次达标（新事件）不重复外发。
	m.NoteDisabled("u4", "丁", "12153 session dead")
	m.maybeNotify()
	mu.Lock()
	if calls != 1 {
		t.Fatalf("冷却内外发应被抑制, calls=%d", calls)
	}
	mu.Unlock()
}

// TestWebhookFailureSilent webhook 端点挂掉：静默（不 panic、不阻塞），横幅照常。
func TestWebhookFailureSilent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	srv.Close() // 立即关闭：连接必失败
	m := New(Config{WebhookURL: srv.URL, MassKillAccounts: 1, MassKillWindowMin: 10, CooldownMin: 30}, "h", "test")
	m.NoteDisabled("u1", "a", "12153 session dead")
	done := make(chan struct{})
	go func() { m.maybeNotify(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("外发失败应快速返回（重试 1 次上限）")
	}
	if m.Payload() == nil {
		t.Fatal("webhook 失败不影响横幅")
	}
}

// TestTrimHardCap 事件硬上限 1 小时：窗口配置成超大值也不无限增长。
func TestTrimHardCap(t *testing.T) {
	m := New(Config{MassKillAccounts: 1, MassKillWindowMin: 10, CooldownMin: 30}, "h", "test")
	m.mu.Lock()
	// 伪造 2 小时前的旧事件（直接注入，绕过 At=time.Now()）。
	m.events = append(m.events, evt{UID: "old", At: time.Now().Add(-2 * time.Hour)})
	m.mu.Unlock()
	m.SetConfig(Config{MassKillAccounts: 1, MassKillWindowMin: 100000, CooldownMin: 30})
	m.NoteDisabled("u1", "a", "12153 session dead")
	mk := m.Payload().(*MassKill)
	if mk.Count != 1 || mk.UIDs[0] != "u1" {
		t.Fatalf("超过 1h 硬上限的旧事件应被裁剪: %+v", mk)
	}
}
