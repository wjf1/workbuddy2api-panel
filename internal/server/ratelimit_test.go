package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// fakeClock 让令牌桶的补充/回收可确定性测试，不依赖真实睡眠。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestLimiter(rps float64, burst int) (*RateLimiter, *fakeClock) {
	l := NewRateLimiter(rps, burst)
	c := &fakeClock{t: time.Unix(1700000000, 0)}
	l.now = c.now
	l.lastSweep = c.t
	return l, c
}

// TestRateLimiterDisabled 默认关闭：rps<=0 返回 nil，调用点 nil 也安全放行。
func TestRateLimiterDisabled(t *testing.T) {
	if NewRateLimiter(0, 0) != nil {
		t.Fatal("rps=0 应返回 nil（不启用）")
	}
	var nilLimiter *RateLimiter
	if ok, _ := nilLimiter.Allow("1.2.3.4"); !ok {
		t.Fatal("nil 限流器必须放行")
	}
}

// TestRateLimiterBurstThenReject 突发额度用尽后拒绝，并给出正的重试等待。
func TestRateLimiterBurstThenReject(t *testing.T) {
	l, _ := newTestLimiter(2, 2) // 2 rps，桶容量 2
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("h1"); !ok {
			t.Fatalf("第 %d 次应放行（突发额度内）", i+1)
		}
	}
	ok, retry := l.Allow("h1")
	if ok {
		t.Fatal("突发额度用尽后应拒绝")
	}
	if retry <= 0 {
		t.Fatalf("拒绝时重试等待必须为正，got %v", retry)
	}
	// 另一主机不受影响——这是"按主机隔离"的核心语义。
	if ok, _ := l.Allow("h2"); !ok {
		t.Fatal("其他主机应独立计数，不应被 h1 拖累")
	}
}

// TestRateLimiterRefill 按时间连续补充：等够 1/rps 秒即恢复一个令牌。
func TestRateLimiterRefill(t *testing.T) {
	l, c := newTestLimiter(1, 1)
	if ok, _ := l.Allow("h"); !ok {
		t.Fatal("首次应放行")
	}
	if ok, _ := l.Allow("h"); ok {
		t.Fatal("令牌耗尽后应立即拒绝")
	}
	c.advance(500 * time.Millisecond) // 半秒只补 0.5 个，仍不足 1
	if ok, _ := l.Allow("h"); ok {
		t.Fatal("仅补 0.5 个令牌时不应放行")
	}
	c.advance(600 * time.Millisecond) // 累计 1.1s，够 1 个
	if ok, _ := l.Allow("h"); !ok {
		t.Fatal("补足令牌后应放行")
	}
}

// TestRateLimiterSweepIdle 空闲桶会被回收，避免 map 随扫描流量无界增长。
func TestRateLimiterSweepIdle(t *testing.T) {
	l, c := newTestLimiter(1, 1)
	l.Allow("old")
	if len(l.buckets) != 1 {
		t.Fatalf("应建立 1 个桶，got %d", len(l.buckets))
	}
	c.advance(idleBucketTTL + time.Minute)
	l.Allow("new") // 触发 sweep
	if _, ok := l.buckets["old"]; ok {
		t.Fatal("空闲超过 TTL 的桶应被回收")
	}
	if _, ok := l.buckets["new"]; !ok {
		t.Fatal("新桶应存在")
	}
}

// TestRateKeyIgnoresSpoofedHeaders 限流键必须取自 TCP 对端，不能被 XFF 伪造绕过。
func TestRateKeyIgnoresSpoofedHeaders(t *testing.T) {
	r1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r1.RemoteAddr = "198.51.100.7:1234"
	r1.Header.Set("X-Forwarded-For", "10.0.0.1")
	r2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r2.RemoteAddr = "198.51.100.7:9999" // 同主机不同源端口
	r2.Header.Set("X-Forwarded-For", "10.0.0.2")

	if rateKey(r1) != rateKey(r2) {
		t.Fatalf("同主机不同端口的请求必须归入同一桶（否则可换端口绕过）: %q vs %q", rateKey(r1), rateKey(r2))
	}
	if rateKey(r1) != "198.51.100.7" {
		t.Fatalf("限流键应为对端主机，got %q", rateKey(r1))
	}
}

// TestRateLimitedPath 监控与探活路径不参与限流（被限流会被误判成服务挂了）。
func TestRateLimitedPath(t *testing.T) {
	in := []string{"/v1/chat/completions", "/v1/models", "/v1/messages", "/v1/messages/count_tokens"}
	out := []string{"/status", "/healthz", "/panel/api/overview", "/"}
	for _, p := range in {
		if !rateLimitedPath(p) {
			t.Errorf("%s 应参与限流", p)
		}
	}
	for _, p := range out {
		if rateLimitedPath(p) {
			t.Errorf("%s 不应参与限流", p)
		}
	}
}

func newLimitedHandler(t *testing.T, limiter *RateLimiter) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	return NewHandler(Config{Pool: p, Upstream: up, RateLimit: limiter})
}

// TestHandlerRejectsWith429 端到端：超限返回 429 + Retry-After，且不打上游。
func TestHandlerRejectsWith429(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	var calls int
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up, RateLimit: l})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := send(); rec.Code != http.StatusOK && rec.Code != http.StatusBadGateway {
		t.Fatalf("首次请求应进入正常链路，got %d", rec.Code)
	}
	before := calls

	rec := send()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限应回 429，got %d body=%s", rec.Code, rec.Body)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" {
		t.Fatalf("429 必须带正数 Retry-After，got %q", ra)
	}
	var e struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("429 响应体不是合法 JSON: %v", err)
	}
	if e.Error.Code != "rate_limit_exceeded" {
		t.Fatalf("OpenAI 面错误码应为 rate_limit_exceeded，got %q", e.Error.Code)
	}
	if calls != before {
		t.Fatal("被限流的请求不应打到上游")
	}
}

// TestHandlerRateLimitSkipsMonitoring 配了限流也不拦 /status 与 /healthz。
func TestHandlerRateLimitSkipsMonitoring(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	h := newLimitedHandler(t, l)

	hit := func(path string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 5; i++ { // 远超桶容量
		if code := hit("/healthz"); code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次 /healthz 被限流，探活会被误判成服务挂了", i+1)
		}
		if code := hit("/status"); code == http.StatusTooManyRequests {
			t.Fatalf("第 %d 次 /status 被限流，面板轮询会误报", i+1)
		}
	}
}

// TestHandlerAnthropicRateLimitBody Anthropic 面 429 用 rate_limit_error，
// Claude Code / ZCode 按该类型决定退避策略。
func TestHandlerAnthropicRateLimitBody(t *testing.T) {
	l, _ := newTestLimiter(1, 1)
	h := newLimitedHandler(t, l)

	body := `{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	send() // 用掉唯一令牌
	rec := send()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("超限应回 429，got %d", rec.Code)
	}
	var e struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("429 响应体不是合法 JSON: %v", err)
	}
	if e.Type != "error" || e.Error.Type != "rate_limit_error" {
		t.Fatalf("Anthropic 面应为 rate_limit_error，got %+v", e)
	}
}
