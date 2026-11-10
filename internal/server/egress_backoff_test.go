package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 出口级退避（backoff.go egress 部分 + handler_chat 传输层分支改归因）的行为钉子。
//
// 核心断言：传输层失败**不再记到账号头上**（连败不涨、降权不触发），改记出口；
// 出口连续失败达阈后请求快速失败，不再把同样的等待重复 MaxRotate 遍。

// TestEgressBackoffLadder 阶梯：base·2^(streak-1)，封顶 15s；base<=0 恒 0。
func TestEgressBackoffLadder(t *testing.T) {
	// base=0（测试常态）：恒 0，不真实等待
	for streak := 1; streak <= 6; streak++ {
		if d := egressBackoff(streak, 0); d != 0 {
			t.Fatalf("base=0 时应恒 0, streak=%d got %s", streak, d)
		}
	}

	// 用固定 base 验证阶梯形状（不抖动的断言方式：抖动 ±25%，检查落在区间内）
	cases := []struct {
		streak int
		lo, hi time.Duration
	}{
		{1, 750 * time.Millisecond, 1250 * time.Millisecond},    // 1s ±25%
		{2, 1500 * time.Millisecond, 2500 * time.Millisecond},   // 2s ±25%
		{3, 3 * time.Second, 5 * time.Second},                   // 4s ±25%
		{4, 6 * time.Second, 10 * time.Second},                  // 8s ±25%
		{5, 11250 * time.Millisecond, 18750 * time.Millisecond}, // 15s 封顶 ±25%
		{9, 11250 * time.Millisecond, 18750 * time.Millisecond}, // 更高也封顶
	}
	for _, c := range cases {
		d := egressBackoff(c.streak, 1*time.Second)
		if d < c.lo || d > c.hi {
			t.Errorf("streak=%d: got %s, want in [%s, %s]", c.streak, d, c.lo, c.hi)
		}
	}
}

// TestEgressKeyByRealm 键按域分开：cn 的抖动不压 global 的恢复。
func TestEgressKeyByRealm(t *testing.T) {
	cn := &auth.Auth{UID: "u1"}
	if got := egressKey(cn); got != "direct:cn" {
		t.Fatalf("默认域应为 cn, got %q", got)
	}
	gl := &auth.Auth{UID: "u2", Domain: "www.workbuddy.ai"} // Domain 决定 realm=global
	if got := egressKey(gl); got != "direct:global" {
		t.Fatalf("global 号应记 direct:global, got %q", got)
	}
	if got := egressKey(nil); got != "direct:cn" {
		t.Fatalf("nil 账号应回落 cn, got %q", got)
	}
}

// TestChatTransportErrorNoAccountStreak 传输层失败不涨账号连败（P0 核心断言）。
//
// 旧行为：每次 transport error 都 NoteFailures(acct)——3 次轮转 = 3 个号各记 1 次
// 连败，几轮请求就能把健康号拖进降权。新行为：记出口，账号的 consecutiveFails
// 一个都不涨。
func TestChatTransportErrorNoAccountStreak(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}

	// 出口计数应等于上游调用次数（每次轮转记一次出口）
	if got := p.EgressStreak("direct:cn", time.Now()); got < 1 {
		t.Fatalf("出口计数应 ≥1, got %d", got)
	}
	// 账号连败一个都不涨（NoteFailures 不再被调用）
	for _, uid := range []string{"u1", "u2"} {
		st, ok := p.Status(uid)
		if !ok {
			continue
		}
		if st.ConsecutiveFails != 0 {
			t.Fatalf("账号 %s 连败应为 0（传输层失败不属于账号）, got %d", uid, st.ConsecutiveFails)
		}
	}
}

// TestEgressGiveUpFastFails streak 达 egressGiveUpStreak 后，请求**只打一次上游**
// 就 503（不再把同样的等待与失败重复 MaxRotate 遍）。
//
// 构造：先手动把出口 streak 顶到阈值-1，本请求的第一次传输失败就会到阈值 → break。
func TestEgressGiveUpFastFails(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 预热：前 3 次失败把 streak 顶到 3（< egressGiveUpStreak=4）
	now := time.Now()
	for i := 0; i < egressGiveUpStreak-1; i++ {
		p.NoteEgressFailure("direct:cn", now)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("streak 达阈后应只打 1 次上游就快速失败, got %d 次调用", calls)
	}
}
