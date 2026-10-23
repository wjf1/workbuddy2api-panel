package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestJitterOffsetStableAndRanged 同 uid+day 恒等；范围 [0, max*60s)；空 uid / 零窗口为 0。
func TestJitterOffsetStableAndRanged(t *testing.T) {
	const day = "2026-10-24"
	a := jitterOffset("uid-abc", day, 20)
	if a == 0 || a >= 20*time.Minute {
		t.Fatalf("偏移 %v 超出 [0,20m) 或恒零", a)
	}
	if a != jitterOffset("uid-abc", day, 20) {
		t.Fatal("同 uid+day 偏移应稳定")
	}
	if jitterOffset("", day, 20) != 0 {
		t.Fatal("空 uid 应为 0（关闭语义）")
	}
	if jitterOffset("uid-abc", day, 0) != 0 {
		t.Fatal("零窗口应为 0（关闭语义）")
	}
	// 跨日应变化（概率性断言：3 个 uid+相邻两日至少 2 个不同）。
	changed := 0
	for _, uid := range []string{"u1", "u2", "u3", "u4"} {
		if jitterOffset(uid, day, 20) != jitterOffset(uid, "2026-10-25", 20) {
			changed++
		}
	}
	if changed < 2 {
		t.Fatalf("跨日偏移应轮换，changed=%d", changed)
	}
}

// TestJitterSpread 多 uid 分散度：17 个 uid 在 20 分钟窗口上不应聚堆
// （≥ 一半 uid 偏移互不相同——fnv 雪崩下的弱断言，防退化成常量）。
func TestJitterSpread(t *testing.T) {
	seen := map[time.Duration]bool{}
	for i := 0; i < 17; i++ {
		seen[jitterOffset("uid-"+string(rune('a'+i)), "2026-10-24", 20)] = true
	}
	if len(seen) < 9 {
		t.Fatalf("17 个 uid 仅 %d 个不同偏移，疑似聚堆", len(seen))
	}
}

// TestKaBackoffSequence 退避序列 30m → 2h → 6h → 24h 封顶；成功清零。
func TestKaBackoffSequence(t *testing.T) {
	s := New(Config{})
	want := []time.Duration{30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 24 * time.Hour, 24 * time.Hour}
	for i, d := range want {
		before := time.Now()
		s.pushKaBackoff("u1")
		b := s.kaBackoff["u1"]
		got := b.nextAt.Sub(before)
		if got < d-30*time.Second || got > d+30*time.Second {
			t.Fatalf("第 %d 次退避时长 %v want ~%v", i+1, got, d)
		}
		if b.tries != i+1 {
			t.Fatalf("tries=%d want %d", b.tries, i+1)
		}
	}
	s.clearKaBackoff("u1")
	if _, ok := s.kaBackoff["u1"]; ok {
		t.Fatal("clearKaBackoff 后应无记录")
	}
}

// TestKeepaliveBackoffDriven 退避到期项由驱动重试：refresh 成功后清退避。
func TestKeepaliveBackoffDriven(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})

	s.pushKaBackoff("u1")
	// 强制到期：把 nextAt 拨到过去。
	s.kaMu.Lock()
	b := s.kaBackoff["u1"]
	b.nextAt = time.Now().Add(-time.Minute)
	s.kaBackoff["u1"] = b
	s.kaMu.Unlock()

	if n := s.driveKeepaliveBackoff(); n != 1 {
		t.Fatalf("应重试 1 项, got %d", n)
	}
	if f.refreshCalls.Load() != 1 {
		t.Fatalf("refresh 调用=%d want 1", f.refreshCalls.Load())
	}
	if _, ok := s.kaBackoff["u1"]; ok {
		t.Fatal("成功后退避应清零")
	}
	// 未到期项不重试。
	s.pushKaBackoff("u2")
	if n := s.driveKeepaliveBackoff(); n != 0 {
		t.Fatalf("未到期项不应重试, got %d", n)
	}
}

// TestRunKeepaliveScheduledSmart smart 模式：临期号刷新、新鲜号跳过；daily 全量。
func TestRunKeepaliveScheduledSmart(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	now := time.Now()
	p.Add(&auth.Auth{UID: "fresh", AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(59 * 24 * time.Hour).Unix()})  // 59 天后过期：跳过
	p.Add(&auth.Auth{UID: "stale", AccessToken: "at", RefreshToken: "rt", ExpiresAt: now.Add(10 * 24 * time.Hour).Unix()}) // 10 天：刷新
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	// smart：只刷临期号。
	s.SetKeepaliveSmart(true, 14)
	s.runKeepaliveScheduled(context.Background())
	if f.refreshCalls.Load() != 1 {
		t.Fatalf("smart 应只刷 1 个临期号, got %d", f.refreshCalls.Load())
	}

	// daily：全量。
	f.refreshCalls.Store(0)
	s.SetKeepaliveSmart(false, 14)
	s.runKeepaliveScheduled(context.Background())
	if f.refreshCalls.Load() != 2 {
		t.Fatalf("daily 应全量刷 2 个, got %d", f.refreshCalls.Load())
	}
}

// TestProbeDeadRevives 死号探测：refresh 成功复活 + 回调；banned 不探测。
func TestProbeDeadRevives(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "dead", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "banned", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.NoteSessionDead("dead")
	p.NoteSessionDead("dead")
	p.NoteSessionDead("dead") // 满阈禁用 → session_dead
	p.Disable("banned", "account banned by upstream (11140 request illegal), re-login required")

	var recovered []string
	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})
	s.SetRecoveredHook(func(uid, nickname string) { recovered = append(recovered, uid) })

	if n := s.RunProbeDeadNow(); n != 1 {
		t.Fatalf("应只探测 1 个 session_dead 号（banned 不探）, got %d", n)
	}
	st, ok := p.Status("dead")
	if !ok || st.Disabled {
		t.Fatalf("探测成功后应复活: disabled=%v", st.Disabled)
	}
	if len(recovered) != 1 || recovered[0] != "dead" {
		t.Fatalf("复活回调=%v want [dead]", recovered)
	}
	if f.refreshCalls.Load() != 1 {
		t.Fatalf("refresh 调用=%d want 1", f.refreshCalls.Load())
	}
}

// TestProbeDeadFailsSilently 探测失败（上游仍 12153/裸 401）：保持禁用、不计数、不 panic。
func TestProbeDeadFailsSilently(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><head><title>401 Authorization Required</title></head><body>401</body></html>`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "dead", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.NoteSessionDead("dead")
	p.NoteSessionDead("dead")
	p.NoteSessionDead("dead")

	s := New(Config{Pool: p, Upstream: &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}})
	if n := s.RunProbeDeadNow(); n != 1 {
		t.Fatalf("探测账号数=%d want 1", n)
	}
	st, _ := p.Status("dead")
	if !st.Disabled {
		t.Fatal("探测失败应保持禁用")
	}
	if st.SessionDeadFails != 0 {
		t.Fatalf("探测失败不应计数, sdfails=%d", st.SessionDeadFails)
	}
}
