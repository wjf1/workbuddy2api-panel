package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// TestMain 默认关闭聊天表格日志（chatLogEnabled=false），消除 go test 期间的 stdout 噪音。
// 断言表格行输出的测试（logging_test.go 中的 ChatLogs/LogChatRow 系列）用 withChatLog 临时开启。
func TestMain(m *testing.M) {
	chatLogEnabled = false
	os.Exit(m.Run())
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bindStore 记录粘性绑定镜像调用（不联网），供 D4 端到端断言绑定收敛到最终成功号。
type bindStore struct {
	redisstore.Noop
	mu     sync.Mutex
	binds  map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}
func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}
func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.binds {
		out[k] = v
	}
	return out
}
func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.binds[key]
	return u, ok
}

// testPoolWith 构建一个所有账号 credits=1000 的池，并注入确定性随机源：
// randInt64N 恒返回 0 → pickWeighted 必选候选集中积分最高者（第一个）。
// 这让依赖"bad 先被选中"的轮转测试（如 TestChatRotatesOnHardCredit）完全确定，
// 不再受加权随机影响而 flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	return p
}

// TestChatLargeBodyNoGatewayLimit 请求体无网关侧上限（max_body_mb 已移除）：
// 数 MB 的合法 body 完整读入并照常打上游，网关不再 413（超限类问题交由上游
// 自然响应，对齐上游 e34cfa4 规约）。
func TestChatLargeBodyNoGatewayLimit(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	pad := strings.Repeat("a", 4<<20) // 4MB 合法 JSON 字符串值
	body := []byte(`{"model":"glm-5.2","messages":[],"pad":"` + pad + `"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s (large valid body must proceed)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1", calls)
	}
}

// TestChatBadParamsFailsFastWithoutPenalty 上游 400 + Unmarshal chat params failed（11101）
// → 请求级错误：不罚账号（无冷却/无禁用/无熔断计数/无 errTotal），**且不轮转**——
// 同一 body 换号必然同样失败。端到端断言只打一次上游、直接回 400、账号完好。
func TestChatBadParamsFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: request-level error must not be retried on other accounts)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（零轮转）", calls)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatBadParams400CarriesUpstreamBody 11101 的 400 响应必须包含上游原始
// 11101 信息（含 requestId，客户端据此排查），且不再出现空洞的 no_healthy_account。
func TestChatBadParams400CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s (want 400)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("400 message should carry upstream 11101 info: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("request-level failure must not be reported as account exhaustion: %s", body)
	}
}

// TestChatModelParamInvalidFailsFastWithoutPenalty 上游 400 + code 11133
// model_param_invalid（模型提供方拒绝参数）→ 请求级错误：**不轮转**（同一 body
// 换号必被同样拒绝）、直接回 400 透传上游原文、账号完好。
//
// 回归背景（2026-10-14 ZCode + cn:kimi-k3-1 实案）：修复前 Classify 把 11133 归
// ErrClient（default 分支喂连败计数 + 继续轮转），网关连换 3 号全撞 11133 → 末端
// 「其余保持 503」→ ZCode 判定可重试、退避重试 11 次（单次退避涨到 28s），会话
// 表现为长时间无响应。修复后应一次上游调用即 400 终止。
func TestChatModelParamInvalidFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, `{"code":11133,"msg":"Invalid request parameters","extError":{"code":"400001","message":"the request parameters were rejected by the model provider"}}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s (want 400: request-level error must not be retried on other accounts)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 0 {
		t.Errorf("calls=%v want bad 1 次、good 0 次（零轮转）", calls)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11133") || !strings.Contains(body, "Invalid request parameters") {
		t.Errorf("400 must carry upstream 11133 info: %s", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("request-level failure must not be reported as account exhaustion: %s", body)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal（零动作策略）。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrModelParamInvalid must not penalize account: %+v", st)
	}
}


// TestChatUpstreamQueueFailsFastWithoutPenalty 上游 429 + 6021 queue.full（免费档排队）
// → 上游服务端队列水位，与账号无关：账号零惩罚、零轮转、直接回 429 upstream_queue_busy，
// 且响应必须带上游排队原文（用户/面板据此确认"不是号的问题"）。
// 这是 2026-10-15「106 秒内 16/17 号全进冷却」的回归守卫。
func TestChatUpstreamQueueFailsFastWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	queueBody := `{"code":6021,"msg":"queue.full.title","requestId":"5a779a031ab0d90ae71eba8b19f5dbf1"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 429, queueBody, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at-a3", ExpiresAt: 9999999999},
	)
	p.SetCredits("a1", 3000, 0) // 确定性源 r=0 → 先选 a1
	p.SetCredits("a2", 2000, 0)
	p.SetCredits("a3", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s (want 429 upstream_queue_busy)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "upstream_queue_busy") {
		t.Errorf("must use distinct code upstream_queue_busy (not rate_limit_exceeded): %s", body)
	}
	if !strings.Contains(body, "queue.full.title") {
		t.Errorf("must pass through upstream queue body untouched: %s", body)
	}
	if strings.Contains(body, "rate_limit_exceeded") {
		t.Errorf("queue must not be reported as account rate limit: %s", body)
	}
	// 零轮转：只打一次上游（队列堵塞时换号无用，且放大请求量）。
	total := 0
	for _, n := range calls {
		total += n
	}
	if total != 1 {
		t.Errorf("upstream calls=%v want exactly 1 (no rotation on queue)", calls)
	}
	// 账号零惩罚：所有号无冷却/无禁用/无熔断/无 errTotal（与 WAF 的软冷却也不同）。
	for _, uid := range []string{"a1", "a2", "a3"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
			t.Errorf("%s must not be penalized by upstream queue: %+v", uid, st)
		}
	}
}

// TestChatUpstreamQueueGateBlocksRotationWhileActive 排队门控激活后，后续请求
// **不再打上游**直接回 429（零成本止损：队列堵着时发出请求只是白白放大并烧号）。
func TestChatUpstreamQueueGateBlocksRotationWhileActive(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 429, `{"code":6020,"msg":"queue.waiting.title","data":{"queue_position":5399,"queue_size":"999+","retry_after":40}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 第一次：真打上游，撞排队 → 门控激活。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("first call code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1 after first call", calls)
	}
	if active, _, _ := h.UpstreamQueueStatus(); !active {
		t.Fatal("queue gate must be active after 6020")
	}

	// 第二次：门控激活期内，直接回 429，**零上游调用**。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second call code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1 (gate must block upstream call entirely)", calls)
	}
	b2 := rec2.Body.String()
	if !strings.Contains(b2, "upstream_queue_busy") {
		t.Errorf("gated response must use upstream_queue_busy: %s", b2)
	}
	if !strings.Contains(b2, "5399") {
		t.Errorf("gated response should surface queue position for the operator: %s", b2)
	}
}

// TestChatUpstreamQueueGateClearsOnSuccess clear 后门控归零，且随后的成功请求
// 不会重新激活（守住「恢复后不被网关自己挡住」）。
func TestChatUpstreamQueueGateClearsOnSuccess(t *testing.T) {
	var ok bool
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if !ok {
			return 429, `{"code":6021,"msg":"queue.full.title"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if active, _, _ := h.UpstreamQueueStatus(); !active {
		t.Fatal("gate must be active after 6021")
	}

	// 手动清除门控条件（模拟上游队列疏通 + 过期）后打一次成功请求。
	h.upstreamQueue.clear()
	ok = true
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("recovered call code=%d body=%s", rec2.Code, rec2.Body)
	}
	if active, _, _ := h.UpstreamQueueStatus(); active {
		t.Error("gate must stay clear after a successful request")
	}
}

// TestQueueGateActivationAndDuration 门控状态机单测：首次命中即激活、
// 时长采信 retry_after 并钳到 [15s,2m]、clear 后归零。
func TestQueueGateActivationAndDuration(t *testing.T) {
	t.Run("retry_after 采信", func(t *testing.T) {
		var g queueGate
		if !g.noteQueue("u1", upstream.QueueInfo{Code: "6020", RetryAfter: 40 * time.Second}) {
			t.Fatal("first hit must activate")
		}
		_, until, info := g.snapshot()
		if d := time.Until(until); d < 35*time.Second || d > 41*time.Second {
			t.Errorf("duration=%v want ~40s", d)
		}
		if info.Code != "6020" {
			t.Errorf("info.Code=%q want 6020", info.Code)
		}
	})
	t.Run("retry_after 过小抬到下限", func(t *testing.T) {
		var g queueGate
		g.noteQueue("u1", upstream.QueueInfo{Code: "6020", RetryAfter: time.Second})
		_, until, _ := g.snapshot()
		if d := time.Until(until); d < 14*time.Second {
			t.Errorf("duration=%v want >=15s (clamped up)", d)
		}
	})
	t.Run("retry_after 过大钳到上限", func(t *testing.T) {
		var g queueGate
		g.noteQueue("u1", upstream.QueueInfo{Code: "6020", RetryAfter: time.Hour})
		_, until, _ := g.snapshot()
		if d := time.Until(until); d > 2*time.Minute+time.Second {
			t.Errorf("duration=%v want <=2m (clamped down)", d)
		}
	})
	t.Run("无 retry_after 回落 estimated_wait", func(t *testing.T) {
		var g queueGate
		g.noteQueue("u1", upstream.QueueInfo{Code: "6020", EstimatedWait: 30 * time.Second})
		_, until, _ := g.snapshot()
		if d := time.Until(until); d < 29*time.Second || d > 31*time.Second {
			t.Errorf("duration=%v want ~30s", d)
		}
	})
	t.Run("clear 归零", func(t *testing.T) {
		var g queueGate
		g.noteQueue("u1", upstream.QueueInfo{Code: "6021"})
		g.clear()
		if active, _, info := g.snapshot(); active || info.Code != "" {
			t.Errorf("after clear: active=%v info=%+v", active, info)
		}
	})
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
	st, ok := h.cfg.Pool.Status("u1")
	if !ok {
		t.Fatal("account status missing")
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 1 ||
		st.TokenUsage.PromptTokens != 1 || st.TokenUsage.CompletionTokens != 1 || st.TokenUsage.TotalTokens != 2 {
		t.Errorf("token usage=%+v", st.TokenUsage)
	}
	if st.TokenUsage.LastLatencyMs < 1 || st.TokenUsage.LastTokensPerSecond == nil || *st.TokenUsage.LastTokensPerSecond <= 0 {
		t.Errorf("latest performance=%+v", st.TokenUsage)
	}
}

func TestChatRecordsCreditForStreamAndSync(t *testing.T) {
	const sseCredit = "data: {\"id\":\"chatcmpl-credit\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10,\"credit\":1.25}}\n\n" +
		"data: [DONE]\n\n"
	for _, stream := range []bool{false, true} {
		name := "sync"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			up := newFakeUpstream(t, func(string) (int, string, bool) {
				return 200, sseCredit, true
			})
			rec := usage.New("")
			h := NewHandler(Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
				Usage:    rec,
			})
			body := `{"model":"glm-5.2","messages":[]}`
			if stream {
				body = `{"model":"glm-5.2","stream":true,"messages":[]}`
			}
			recorder := httptest.NewRecorder()
			h.ServeHTTP(recorder, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body)
			}
			s := rec.Snapshot(24, nil)
			if s.Totals.Credits != 1.25 || s.Totals.CreditSamples != 1 || s.Totals.CreditTokens != 10 || s.Totals.CreditsPer1MTokens != 125000 {
				t.Fatalf("usage totals = %+v, want credit=1.25 tokens=10 ratio=125000", s.Totals)
			}
			if len(s.CreditByAccount) != 1 || s.CreditByAccount[0].Key != "u1" ||
				len(s.CreditByModel) != 1 || s.CreditByModel[0].Key != "glm-5.2" {
				t.Fatalf("credit dimensions = %+v / %+v", s.CreditByAccount, s.CreditByModel)
			}
		})
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", st)
	}
	if st.TokenUsage.RequestCount != 1 || st.TokenUsage.UsageCount != 0 {
		t.Errorf("bad token usage=%+v", st.TokenUsage)
	}
	good, _ := p.Status("good")
	if good.TokenUsage.RequestCount != 1 || good.TokenUsage.TotalTokens != 2 {
		t.Errorf("good token usage=%+v", good.TokenUsage)
	}
}

// TestChatSoftCoolsOnRateLimitBody 端到端回归 issue #28：上游用非 429 状态码
// （400 + 限流文案）表达模型侧限流时，该账号必须进入 CoolSoft 冷却，而不是只换号。
// 修复前 Classify 归 ErrClient → applyErrorPolicy 走 default 分支只换号不罚，
// 账号留在可用池里，下一个请求仍会被选中。
func TestChatSoftCoolsOnRateLimitBody(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":1,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（与 TestChatRotatesOnHardCredit 同一确定性手法）。
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	const soft = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	// 冷却时长取自注入的 SoftCooldown，不依赖真实等待。
	if max := int64(soft / time.Second); st.CoolRemaining <= 0 || st.CoolRemaining > max {
		t.Errorf("cool_remaining_sec=%d want in (0,%d]", st.CoolRemaining, max)
	}

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

func TestApplyErrorPolicySoftRateExponentialBackoff(t *testing.T) {
	// 吸收上游语义：带解析重置时间的 body 对齐墙钟；无时间文案走有界退避，且
	// **冷却中的重复触发不堆加**（旧「每次都翻倍」正是全池被推到 2h 封顶的元凶）。
	// 跨冷却期的堆加语义由 pool 包 CooldownSoftRate 测试覆盖；此处锚定 handler 侧
	// 的调用形态：连续 3 次软限流错误 → 首次 600s，后续不延长。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft_rate 冷却: %+v", i, st)
		}
		if st.SoftStreak != 1 {
			t.Errorf("call %d: 冷却中兜底探测不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		if st.CoolRemaining < 600-3 || st.CoolRemaining > 600 {
			t.Errorf("call %d: cool_remaining_sec=%d want ~600（不堆加）", i, st.CoolRemaining)
		}
	}
}

func TestApplyErrorPolicyNotFoundUsesFixedBase(t *testing.T) {
	// 404 分流：偶发上游 404 的冷却基数固定 60s（notFoundCooldown），不取 soft_rate
	// 的 600s 基数，也不受其配置值影响。Cooldown 重构后 404 是固定时长冷却
	//（不堆加、不喂熔断），成功后自然到期恢复。
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 20 * time.Minute}) // soft_rate 配得很大，验证 404 不受其影响

	notFoundSec := int64(notFoundCooldown / time.Second)
	for i := 1; i <= 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft 冷却: %+v", i, st)
		}
		if st.SoftStreak != 0 {
			t.Errorf("call %d: 404 固定冷却不应推进 soft_streak, got %d", i, st.SoftStreak)
		}
		// 基数取自 notFoundCooldown（60s）而非注入的 soft_rate（20m），且重复触发不延长。
		if st.CoolRemaining < notFoundSec-3 || st.CoolRemaining > notFoundSec {
			t.Errorf("call %d: 404 cool_remaining_sec=%d want ~%d（固定基数 %ds，非 soft_rate）",
				i, st.CoolRemaining, notFoundSec, notFoundSec)
		}
	}
}

// TestNewHandlerSoftCooldownDefault 端到端：未注入 SoftCooldown 时基数回落到 600s（原为 60s）。
func TestNewHandlerSoftCooldownDefault(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 SoftCooldown

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	if st.CoolRemaining <= 599 || st.CoolRemaining > 600 {
		t.Errorf("default soft cooldown cool_remaining_sec=%d want 600", st.CoolRemaining)
	}
}

// TestChatStickyFollowsFinalSuccess 端到端验证 D4：粘性号失败换号成功后，会话绑定收敛到成功号。
func TestChatStickyFollowsFinalSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 先把会话预绑定到 bad（模拟历史粘性），bad 失败、good 成功 → 绑定应切到 good。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	// 预绑定：sess.Bind("conv-1", "bad")，然后请求体带同 conversation_id。
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 绑定必须收敛到最终成功的 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should follow final success to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
}

// TestChatStickySuccessKeepsBinding 粘性号直接成功 → 绑定不变（仍为该号）。
func TestChatStickySuccessKeepsBinding(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "good")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should stay good after success, got %s ok=%v", uid, ok)
	}
}

// TestChatStickyFullFallsBackToRotation 端到端验证 C3 语义：粘性号满载不可用时，
// 请求在同一轮内解绑并回落普通轮换选中健康账号，绑定收敛到最终成功号——而非空耗一轮。
func TestChatStickyFullFallsBackToRotation(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad 占满唯一在途名额：PickByUID 将返回 nil（healthy 但 inFlight 满）→ 解绑 + 回落轮换。
	p.SetMaxInFlight(1)
	p.Acquire("bad")

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 满载粘性号被解绑，绑定收敛到最终成功号 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should fall back to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
	p.Release("bad")
}

func TestChatHardCreditCooldownUntilNextDay4AM(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0) // bad 积分高，确定性源 → 先被选中
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("bad should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 硬信贷冷却必须是"最近一个 04:00"（CST 时区口径），而不是固定 12h/配置时长。
	// 注意：Until 带 cst.Loc 时区，断言必须换算到 cst.Loc 再取 Hour()；直接取
	// Hour() 在 UTC 宿主（CI runner）上会得到 20，与被测逻辑正确性无关。
	cstUntil := st.Until.In(cst.Loc)
	if cstUntil.Hour() != 4 || cstUntil.Minute() != 0 || cstUntil.Second() != 0 {
		t.Errorf("until(CST)=%s want next-day 04:00:00 (raw=%v)", cstUntil, st.Until)
	}
	// 距次日 04:00 最长 28h（凌晨 00:00~04:00 间运行时 now→次日 04:00 跨度 > 24h，属正常）。
	if d := time.Until(st.Until); d <= 0 || d > 28*time.Hour {
		t.Errorf("until %v not within (0,28h]: %v", st.Until, d)
	}
	// 立即换号成功：good 被选中。
	stGood, _ := p.Status("good")
	if stGood.Cooling || stGood.Disabled {
		t.Errorf("good should stay healthy: %+v", stGood)
	}
}

func TestChat429Code14018UsesHardCreditCooldown(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14018,"msg":"Credits exhausted"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 2000)
	p.SetCredits("good", 1000, 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("14018 account should be cooling: %+v ok=%v", st, ok)
	}
	cstUntil := st.Until.In(cst.Loc)
	if st.Reason != "余额不足" || cstUntil.Hour() != 4 || cstUntil.Minute() != 0 {
		t.Fatalf("14018 should use hard-credit cooldown, got reason=%q until=%v (CST %s)", st.Reason, st.Until, cstUntil)
	}
	if got := p.Pick(); got == nil || got.UID != "good" {
		t.Fatalf("hard-cooled 14018 account must not be fallback-picked, got %+v", got)
	}
}

// TestChat6004ModelResetCoolsToParsedTime 端到端回归 issue #31：上游 429 + code 6004
// +「将在 … 重置」→ 冷却 until 精确等于解析时间（而非 600s 固定基数/指数退避），
// 且记录触发模型 → 同模型请求仍被冷却、切模型请求按豁免可选。
func TestChat6004ModelResetCoolsToParsedTime(t *testing.T) {
	// 用未来 5 分钟的重置时间（wall-clock）构造上游响应。
	reset := time.Now().Add(5 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	// 隔离对 breaker 的干扰：熔断阈值默认 3，一次失败不触发。
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	// bad 已进入 6004 模型级独立冷却：账号级不 cooling，台账单行 until ≈ reset。
	st, _ := p.Status("bad")
	if st.Cooling {
		t.Fatalf("6004-with-reset should NOT set account-level cooling: %+v", st)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("want single model ledger row glm-5.3: %+v", st.RateLimitedModels)
	}
	if d := st.RateLimitedModels[0].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", st.RateLimitedModels[0].Until, reset, d)
	}
	// 记录触发模型（bad 池内 private 字段需经 Status 不可见，改用行为断言）：
	// 同模型 glm-5.3 的请求不应选中 bad（仍冷却）；
	// 不同模型 hy3-x 的请求应豁免冷却选中 bad（最高分）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	same := p.PickExcludingForModel(nil, "glm-5.3")
	if same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad (still cooling), got %+v", same)
	}
	diff := p.PickExcludingForModel(nil, "hy3-x")
	if diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should bypass bad soft cooling, got %+v", diff)
	}
}

// TestChat6004WithoutResetFallsBackToBackoff 6004 无时间文案 → 退回 600s 基数软冷却
// （现状不变）。
func TestChat6004WithoutResetFallsBackToBackoff(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000, 0)
	p.SetCredits("good", 1000, 0)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad should be soft cooling: %+v", st)
	}
	// 冷却时长 = 注入 soft 基数(60s)，非解析时间（无重置文案）。
	if st.CoolRemaining <= 0 || st.CoolRemaining > 60 {
		t.Errorf("cool_remaining_sec=%d want ~60 (soft base, not parsed)", st.CoolRemaining)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" || st.RateLimitedModels[0].Kind != "rate_limit" {
		t.Fatalf("rate-limited models=%+v, want audit row for glm-5.3", st.RateLimitedModels)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

// TestChatSessionDeadDisables 带业务信封的 12153 是权威判据：**单发**即禁用。
// 与裸 401 的 3 次门槛（TestChatGateway401RequiresThreeStrikes）对照——防止
// 日后把两条路径误统一成同一口径，弱化 12153 的终态语义。
func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevOut)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
	// 12153 走单发 Disable，不该出现「裸 401 计数」日志（判据来源不同，日志必须能分开）。
	if strings.Contains(logBuf.String(), "裸 401（session dead 计数") {
		t.Errorf("12153 路径不该打裸 401 计数日志：\n%s", logBuf.String())
	}
	if st.SessionDeadFails != 0 {
		t.Errorf("12153 应走单发 Disable，不该动连续计数: %d", st.SessionDeadFails)
	}
}

// TestChatGateway401RequiresThreeStrikes 裸 401（APISIX/openresty HTML 页，无业务
// 信封）单发**不得**永久禁用：网关短时故障或认证规则误配会对整批 token 统一回
// 裸 401，单发禁用会在一次流量轮转里清空整池（修复前该形态落 ErrClient 只换号
// 不罚）。与余额探针同口径：连续 sessionDeadThreshold 次才禁用。
func TestChatGateway401RequiresThreeStrikes(t *testing.T) {
	const apisixHTML = `<html><head><title>401 Authorization Required</title></head>` +
		`<body><center><h1>401 Authorization Required</h1></center><hr><center>openresty</center></body></html>`
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, apisixHTML, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	// 计数进度必须有日志痕迹（面板 session_dead_fails 在涨、日志不能是空的），
	// 文案与余额链路对齐，运维才查得下去。这里钉住精确文案。
	var logBuf bytes.Buffer
	prevOut := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(prevOut)
	send := func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	}

	for i := 1; i <= pool.SessionDeadThreshold()-1; i++ {
		send()
		st, _ := p.Status("u1")
		if st.Disabled {
			t.Fatalf("裸 401 第 %d 次就禁用（want 满 %d 次才禁）: %+v", i, pool.SessionDeadThreshold(), st)
		}
		if st.SessionDeadFails != i {
			t.Fatalf("第 %d 次后 session dead 计数=%d want %d", i, st.SessionDeadFails, i)
		}
	}
	send()
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("连续 %d 次裸 401 应禁用: %+v", pool.SessionDeadThreshold(), st)
	}
	if !strings.Contains(st.DisabledReason, "12153 session dead") {
		t.Fatalf("disabled_reason=%q want 含 12153 session dead", st.DisabledReason)
	}
	// 日志闭环：未达阈打进度，达阈打禁用。文案与余额链路同形，改字先同步 QA。
	logs := logBuf.String()
	for _, want := range []string{
		"裸 401（session dead 计数 1/3）",
		"裸 401（session dead 计数 2/3）",
		"连续 3 次裸 401 — 禁用",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("日志缺 %q；实际日志：\n%s", want, logs)
		}
	}
}

// TestChatGateway401CounterClearsOnSuccess 裸 401 的连续计数必须被成功清零：
// 网关抖动打出的零散裸 401 不能跨请求攒到 3 次把健康号打死（NoteSuccess →
// ClearSessionDead 是这条护栏的落点）。
func TestChatGateway401CounterClearsOnSuccess(t *testing.T) {
	const apisixHTML = `<html><head><title>401 Authorization Required</title></head><body>openresty</body></html>`
	var calls int
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if calls == 2 {
			return 200, sseOK, true // 中间成功一次：代表抖动已过
		}
		return 401, apisixHTML, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	send := func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	}

	send() // 裸 401 → 计数 1
	if st, _ := p.Status("u1"); st.SessionDeadFails != 1 {
		t.Fatalf("第一次裸 401 后计数=%d want 1", st.SessionDeadFails)
	}
	send() // 成功 → 计数清零
	send() // 再裸 401 → 计数应回到 1
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatalf("中间成功过就不该禁用: %+v", st)
	}
	if st.SessionDeadFails != 1 {
		t.Fatalf("成功后再裸 401 计数=%d want 1（NoteSuccess 未清零？）", st.SessionDeadFails)
	}
}

func TestChatTransportErrorDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// 传输错误不喂熔断计数：一次 transport error 不应累计 errTotal 也不应熔断。
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("transport error should not penalize account: %+v", st)
	}
}

func TestChatHTTP5xxPenalizes(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：一次 5xx 即触发熔断（连续失败语义并入熔断器）。
	p.SetBreaker(1, time.Hour, time.Hour)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `{"code":500}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("http 5xx should trip breaker (cooling) with threshold=1: %+v", st)
	}
}

func TestChatHTTP4xxClientDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":400,"msg":"bad request"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("generic 4xx should not penalize account: %+v", st)
	}
}

func TestModelsEndpoint(t *testing.T) {
	// 造桩而非 upstream.New()（后者带真实凭证打上游：无网络/凭证失效即空列表，
	// 且会把 lastFail 写进包级缓存污染后续用例）。目录内容由假上游固定下发。
	resetDynamicModelsCache(t)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"glm-5.2","maxInputTokens":131072,"maxOutputTokens":8192},{"id":"glm-5.1"},{"id":"deepseek-v3"},{"id":"kimi-k3"},{"id":"glm-5.3"}],"agents":[{"name":"cli","models":["glm-5.2","glm-5.1","deepseek-v3","kimi-k3","glm-5.3"]}]}}`, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: up})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("models count=%d", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "cn:glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("cn:glm-5.2 missing")
	}
}

func TestModelsDynamic(t *testing.T) {
	// 清缓存
	resetDynamicModelsCache(t)

	// 假上游返回动态模型（含 agents + maxInputTokens/maxOutputTokens + reasoning 档位）
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192,"reasoning":{"effort":"medium","supportedEfforts":["low","medium","high"]}},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 dynamic models, got %d: %v", len(data), data)
	}
	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["cn:dyn-model-a"] || !ids["cn:glm-9.9"] {
		t.Errorf("dynamic ids missing: %v", ids)
	}

	// 断言字段映射：maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	for _, m := range data {
		mm := m.(map[string]any)
		switch mm["id"] {
		case "dyn-model-a":
			if mm["context_length"].(float64) != 65536 {
				t.Errorf("dyn-model-a context_length=%v want 65536", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 8192 {
				t.Errorf("dyn-model-a max_output_tokens=%v want 8192", mm["max_output_tokens"])
			}
			// reasoning 档位透出：supported_efforts + default_effort
			efforts, _ := mm["supported_efforts"].([]any)
			if len(efforts) != 3 || efforts[0] != "low" {
				t.Errorf("dyn-model-a supported_efforts=%v", mm["supported_efforts"])
			}
			if mm["default_effort"] != "medium" {
				t.Errorf("dyn-model-a default_effort=%v want medium", mm["default_effort"])
			}
		case "dyn-model-b":
			// 上游未返回 reasoning → 两个档位字段都省略（客户端按自身默认）
			if _, has := mm["supported_efforts"]; has {
				t.Errorf("dyn-model-b supported_efforts should be omitted, got %v", mm["supported_efforts"])
			}
			if _, has := mm["default_effort"]; has {
				t.Errorf("dyn-model-b default_effort should be omitted")
			}
		case "glm-9.9":
			if mm["context_length"].(float64) != 262144 {
				t.Errorf("glm-9.9 context_length=%v want 262144", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 32768 {
				t.Errorf("glm-9.9 max_output_tokens=%v want 32768", mm["max_output_tokens"])
			}
		}
	}

	// 第二次调用走缓存（把上游关掉也成功）
	dynamicModelsCache.RLock()
	cached := len(dynamicModelsCache.ids)
	dynamicModelsCache.RUnlock()
	if cached != 3 {
		t.Errorf("cache not populated: %d", cached)
	}
}

func TestModelsDynamicFallsBackToStatic(t *testing.T) {
	// 纯动态化（产品决策）：上游失败 → 空列表（200），不再回落静态表——
	// 拉不出目录即意味着上游不可用，假名单只会让客户端选到 11102 的模型。
	resetDynamicModelsCache(t)

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 0 {
		t.Errorf("pure-dynamic failure should yield empty list, got %d", len(data))
	}
}

func TestModelsFetchFailurePenalizesAccount(t *testing.T) {
	// 吸收上游 9832283：models 拉取失败只进负缓存，不 NoteError——
	// NoteError 喂的是 chat 熔断器，models 端点 5xx 跨界惩罚 chat 通道健康号。
	resetDynamicModelsCache(t)

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour) // 熔断阈值 1：若仍罚号，一次 fetch 失败即熔断
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("models fetch failure must not trip chat breaker: %+v", st)
	}
}

func TestModelsNegativeCacheOnFetchFailure(t *testing.T) {
	resetDynamicModelsCache(t)

	var calls atomic.Int32 // FetchModels 企业/v3 两路并发探测回调，计数须原子
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 连续 3 次请求，上游持续 500 → 只应触发 1 次 fetch（负缓存生效），
	// 纯动态下空列表仍返回 200。
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	// 一轮探测 = 2 次上游调用（企业端点 + /v3/config 并发，两路全失败才进负缓存）。
	if got := calls.Load(); got != 2 {
		t.Errorf("want 2 probes (console + v3), got %d", got)
	}

	// 冷却期结束（把失败时间戳拨回 10 分钟前）→ 应重新 fetch。
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now().Add(-10 * time.Minute)
	dynamicModelsCache.Unlock()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("after cooldown: code=%d", rec.Code)
	}
	if got := calls.Load(); got != 4 {
		t.Errorf("want 4 probes after cooldown (2 rounds x 2), got %d", got)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	// 本用例末尾的 /v1/models 会触发目录拉取：造桩 + 清缓存，避免走真实网络
	// 丢下 lastFail 负缓存污染后续用例。
	resetDynamicModelsCache(t)
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 500, `boom`, false }),
		APIKey:   "secret",
	})
	// 无 key
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key（请求会继续打到上游，但此处上游 client 会失败 —— 只要不是 401 就行）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42, 0)
	// 配置 key 的部署：accounts 全量透出（1.18.1 起未配 key 的部署脱敏 accounts）。
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "sk-test"})
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	// 未配 key：accounts 脱敏为 null（计数汇总保留）——uid/昵称/成本台账不得
	// 对任何能连到端口的人可见。
	h2 := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest("GET", "/status", nil))
	if rec2.Code != 200 {
		t.Fatalf("no-key code=%d", rec2.Code)
	}
	if b2 := rec2.Body.String(); strings.Contains(b2, `"uid":"u1"`) || !strings.Contains(b2, `"accounts":null`) {
		t.Errorf("no-key status must redact accounts: %s", b2)
	}
	if !strings.Contains(rec2.Body.String(), `"total":1`) {
		t.Error("no-key status must keep summary counts")
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
	// Phase 3 汇总字段。
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["total"] != float64(1) || statusBody["healthy"] != float64(1) ||
		statusBody["cooling"] != float64(0) || statusBody["disabled"] != float64(0) ||
		statusBody["in_flight_full"] != float64(0) {
		t.Errorf("summary=%v want total=1 healthy=1 cooling=0 disabled=0 in_flight_full=0", statusBody)
	}
	// Phase v3：池级 sticky_sessions + redis_mode。
	if statusBody["sticky_sessions"] != float64(0) {
		t.Errorf("sticky_sessions=%v want 0", statusBody["sticky_sessions"])
	}
	if statusBody["redis_mode"] != "noop" {
		t.Errorf("redis_mode=%v want noop", statusBody["redis_mode"])
	}
}

// TestStatusInFlightFull /status 透出满载计数：healthy 且占满在途的账号数。
func TestStatusInFlightFull(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	p.Acquire("full")
	defer p.Release("full")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["in_flight_full"] != float64(1) {
		t.Errorf("in_flight_full=%v want 1", statusBody["in_flight_full"])
	}
	if statusBody["healthy"] != float64(2) {
		t.Errorf("healthy=%v want 2 (full is still healthy by state-machine semantics)", statusBody["healthy"])
	}
}

func TestStatusPortraitFields(t *testing.T) {
	// Phase 3：/status 单账号需返回健康画像字段。
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1") // 记录 last_err + err_total（累计，不冷却）
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "sk-test"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	st := body.Accounts[0]
	if !st.Cooling || st.CoolKind != "soft_rate" || st.CoolRemaining <= 0 {
		t.Errorf("cooling portrait=%+v", st)
	}
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.ErrTotal != 1 {
		t.Errorf("err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() {
		t.Error("last_success should be set")
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err should be set")
	}
}

// TestHealthzEmptyPool 空池（healthy=0）→ 503，表示暂不可服务。
func TestHealthzEmptyPool(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (healthy=0)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(0) {
		t.Errorf("healthz json=%v", resp)
	}
}

// TestHealthz503WhenNoHealthy 所有账号禁用/冷却 → 503。
func TestHealthz503WhenNoHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q want json", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=0 total=1", resp)
	}
}

// TestHealthz503WhenAllInFlightFull 全部账号 healthy 但都占满在途 → 503（与 chat 同口径），
// 且 healthy 语义未变（仍为 1）：满载不是状态机健康维度的变化，是探活口径单独叠加。
func TestHealthz503WhenAllInFlightFull(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.Acquire("u1") // 占满唯一在途名额
	defer p.Release("u1")

	if p.ServableNow() {
		t.Fatal("servable should be false when the only healthy account is in-flight full")
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (healthy but all in-flight full)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	// healthy 语义未变：账号仍是 healthy（只占满在途，非冷却/禁用）。
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1 (healthy semantics unchanged)", resp)
	}
}

// TestHealthz200WhenHealthy 有健康账号 → 200。
func TestHealthz200WithHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1", resp)
	}
}

// TestHealthzServiceIdentity /healthz 无论 200 还是 503 都必须带网关身份标识
// （响应体 service 字段 + X-Service 头）：宿主探测打到同端口的旧服务/其他服务时，
// 对方即使返回 2xx 也不带本标识，宿主据此判"假成功"。
func TestHealthzServiceIdentity(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*pool.Pool)
		wantCode int
	}{
		{"healthy", func(*pool.Pool) {}, http.StatusOK},
		{"unhealthy", func(p *pool.Pool) { p.Disable("u1", "session dead") }, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			tc.setup(p)
			h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("X-Service"); got != ServiceName {
				t.Errorf("X-Service=%q want %q", got, ServiceName)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
			}
			if resp["service"] != ServiceName {
				t.Errorf("service=%v want %q", resp["service"], ServiceName)
			}
		})
	}
}

// TestHealthzServiceIdentityWithoutAuth /healthz 保持无鉴权（负载均衡友好）：
// 配了 api_key 也不要求 Bearer，身份字段照常返回。
func TestHealthzServiceIdentityWithoutAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay unauthenticated: code=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// 无 token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// 带 token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz 无鉴权仍 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// TestContentBlockedTriggersDegradedRetry passthrough 模式下首请求 400（11128 文案）
// → 降级重试（Degraded）→ 200，客户端无感。验证第二次出站 body 为 Degraded。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	// 记录每次出站请求体，断言第二次为 Degraded 文本。
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			// 首次（body 含原始 system）返回 400 内容拦截；后续返回 200。
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after degraded retry)", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (first 400 + retry), got %d", len(bodies))
	}
	// 第二次出站 body 的 messages 头部 system 内容应为 Degraded 文本。
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
	if strings.Contains(string(bodies[1]), "原始指纹") {
		t.Errorf("second body should not contain original system: %s", bodies[1])
	}
}

// TestContentBlockedStickyDegraded 降级后新请求直达 Degraded（不再先撞 400）。
func TestContentBlockedStickyDegraded(t *testing.T) {
	var firstCall bool
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if !firstCall {
			firstCall = true
			return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	// 首请求触发降级 → 200。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("first req code=%d", rec.Code)
	}
	// 降级粘性：新请求 Active()=true，body 已被 Rewrite(Degraded)，上游首字节即 200。
	// 但 fake 上游只对 firstCall 返回 400，之后都 200，无法区分"直达"与"重试"。
	// 用 degrade.Active() 直接断言粘性生效。
	if !h.degrade.Active() {
		t.Fatal("degrade should be active after trigger")
	}
}

// TestContentBlockedCustomModeDoesNotDegrade custom 模式不触发降级重试
// （custom 已用自有提示词替换，不应再有 system 来源误报）；仍拦则直接回
// 400 content_blocked 防火墙文案（不轮转、不暴露账号/上游错误码）。
func TestContentBlockedCustomModeDoesNotDegrade(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	// custom 模式下仍拦 → 400 content_blocked（内容终态，换号无意义），不降级重试。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 content_blocked (custom does not degrade)", rec.Code)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, rec.Body.String())
	}
	if env.Error.Code != "content_blocked" {
		t.Errorf("error.code=%q want content_blocked", env.Error.Code)
	}
	// 错误透传（error-passthrough，吸收上游 5755fe3）：message 装上游 body 原文
	// （code/msg 原样），客户端必须看到真实错误才能排查；gateway_hint 并列补充。
	if !strings.Contains(rec.Body.String(), "11128") {
		t.Errorf("message should carry upstream original body (passthrough): %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "gateway_hint") {
		t.Errorf("content_blocked should carry gateway_hint: %s", rec.Body.String())
	}
	if h.degrade.Active() {
		t.Error("degrade should NOT be active in custom mode")
	}
}

// TestContentBlockedDoesNotPenalizeAccount ErrContentBlocked 不罚账号（无冷却/熔断/NoteError）。
func TestContentBlockedDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：若误罚 NoteError 一次即熔断；content_blocked 不应喂熔断。
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
	}
}

// TestCustomModeFingerprintSanitizePreserved custom 端到端：user 消息含 Claude Code
// 指纹句（PR39 fixture 串）→ Rewrite 注入自有 system → 经 sanitize → 出站 body 中
// 该指纹被改写、system 为自有提示词。证明两层（提示词替换 + 清洗）叠加工作。
//
// 两层各自职责（互不替代）：
//   - prompt.Rewrite 替换 system/developer 消息（消灭 system 来源指纹）；
//   - sanitizeMessages 改写 user/assistant 消息中残留的指纹串（兜底用户上下文）。
func TestCustomModeFingerprintSanitizePreserved(t *testing.T) {
	// 捕获出站 body（prepareBody 已强制 stream + 归一 + 清洗后）。
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	up.SanitizeFingerprints.Store(true) // 开启清洗层（与生产一致）
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	const customSys = "我是网关自有提示词"
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: customSys})

	// user 消息含 Claude Code 指纹句（PR39 的 fixture 串，逐字精确指纹）。
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"}
		]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := string(sentBody)
	// 1) system 为自有提示词，旧 system 内容零残留。
	if !strings.Contains(out, customSys) {
		t.Errorf("out body should contain custom system prompt: %s", out)
	}
	if strings.Contains(out, "official CLI for Claude.") && strings.Contains(out, "You are Claude Code, Anthropic's") {
		// 旧 system 原文（含句点）不应以 system 角色出现；但 sanitize 把它改写为
		// "...official CLI tool for Claude."，所以原文 fingerprint 串应消失。
	}
	// 原始指纹串（逐字精确匹配）在出站 body 中应被改写：
	// "official CLI for Claude." → "official CLI tool for Claude."
	// "Main branch (" → "Default branch ("
	if strings.Contains(out, "official CLI for Claude.") {
		t.Errorf("identity fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	if strings.Contains(out, "Main branch (you will usually use this for PRs)") {
		t.Errorf("branch fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	// 改写后的痕迹应在（证明 sanitize 层生效，不是"全删了"）。
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("sanitized identity rewrite missing: %s", out)
	}
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("sanitized branch rewrite missing: %s", out)
	}
	// 2) messages 头部恰好一条 system = 自有提示词（Rewrite 已删旧 system）。
	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, out)
	}
	msgs := obj["messages"].([]any)
	var systemCount int
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" {
			systemCount++
			if mm["content"] != customSys {
				t.Errorf("system content=%v want %q", mm["content"], customSys)
			}
		}
	}
	if systemCount != 1 {
		t.Errorf("want exactly 1 system message, got %d (all=%v)", systemCount, msgs)
	}
}

// TestApplyErrorPolicy14003ModelBusyShortWindow 14003「模型繁忙」不走 soft_rate 阶梯
// （issue #46，上游 f728859）：只停 (账号, 模型) 的 10s 短窗口，账号级 until 不写、
// 账号不进 cooling，避免多号叠加后把 10 秒抖动放大成整分钟断流。
func TestApplyErrorPolicy14003ModelBusyShortWindow(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})
	body := `{"code":14003,"msg":"当前模型请求繁忙，请切换模型或稍后重试"}`

	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "glm-5.2", nil)

	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatalf("14003 不得让账号进入冷却: %+v", st)
	}
	if st.SoftStreak != 0 {
		t.Errorf("14003 不得推进 soft_streak, got %d", st.SoftStreak)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.2" {
		t.Fatalf("rate_limited_models=%+v want 一条 glm-5.2", st.RateLimitedModels)
	}
	// 窗口必须落在 10s 量级（而非 600s soft_rate 基数）。
	if rem := time.Until(st.RateLimitedModels[0].Until); rem <= 0 || rem > 12*time.Second {
		t.Errorf("模型繁忙窗口 remaining=%v want in (0,12s]", rem)
	}

	// 空模型名无从「只停某个模型」：退回账号级既有路径（不得静默不罚）。
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "", nil)
	st2, _ := p.Status("u1")
	if !st2.Cooling {
		t.Errorf("空模型名的 14003 应退回账号级软冷却: %+v", st2)
	}
}

// ── models.hidden 黑名单（列表过滤 + 点名拒绝） ─────────────────────────────
//
// 语义：黑名单 = 「这个模型对本网关不存在」。_v1/models 不列（否则客户端会选到
// 一个必然 400 的模型）、点名调用直接 400（只藏列表不够，知道名字仍可硬调）。
// 裸名条目隐藏两域；"cn:"/"global:" 前缀条目只隐藏该域。

// hiddenChatPost 发一次非流式 chat 请求，返回响应记录器。
func hiddenChatPost(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"` + model + `","messages":[]}`)
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	return rec
}

// TestChatHiddenModelRejected models.hidden 黑名单：点名调用直接 400 拒绝，
// 不打上游、不占选号；未隐藏的模型照常放行。
func TestChatHiddenModelRejected(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsHidden: []string{"glm-5.2", "global:secret-model"}})

	// 裸名隐藏 → 裸名 / cn: / global: 三种写法都拒绝（换前缀不能绕过）
	for _, model := range []string{"glm-5.2", "cn:glm-5.2", "global:glm-5.2"} {
		rec := hiddenChatPost(t, h, model)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("model %q: code=%d, want 400", model, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "model_unavailable") {
			t.Errorf("model %q: body=%s, want model_unavailable", model, rec.Body.String())
		}
	}
	// 前缀精确隐藏：只藏 global 域
	if rec := hiddenChatPost(t, h, "global:secret-model"); rec.Code != http.StatusBadRequest {
		t.Errorf("global:secret-model: code=%d, want 400", rec.Code)
	}
	if calls != 0 {
		t.Errorf("hidden models must not reach upstream: calls=%d", calls)
	}

	// cn:secret-model 不在黑名单 → 放行并打到上游
	rec := hiddenChatPost(t, h, "cn:secret-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("cn:secret-model: code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("allowed model: upstream calls=%d, want 1", calls)
	}
}

// TestChatHiddenEmptyConfigZeroChange 空黑名单 = 零改动：所有模型照常放行。
func TestChatHiddenEmptyConfigZeroChange(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	if rec := hiddenChatPost(t, h, "glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d, want 1", calls)
	}
}

// presetDynamicModelsCache 预置 CN 模型目录缓存（避免联网拉取），返回恢复函数。
func presetDynamicModelsCache(infos []upstream.ModelInfo) func() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	return func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}
}

// collectModelIDs 请求 /v1/models 并返回 id 集合。
func collectModelIDs(t *testing.T, h *Handler) map[string]bool {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range resp.Data {
		ids[m.ID] = true
	}
	return ids
}

// TestModelsHiddenFiltered models.hidden 黑名单：/v1/models 列表里过滤掉隐藏模型（CN 域）。
func TestModelsHiddenFiltered(t *testing.T) {
	restore := presetDynamicModelsCache([]upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "deepseek-v3"}})
	defer restore()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"data":[]}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsHidden: []string{"deepseek-v3"}})

	ids := collectModelIDs(t, h)
	if !ids["cn:glm-5.2"] {
		t.Errorf("visible model missing from list: ids=%v", ids)
	}
	if ids["cn:deepseek-v3"] {
		t.Errorf("hidden model leaked into /v1/models: ids=%v", ids)
	}
}

// TestModelsHiddenScopedCNOnly 带前缀条目只隐藏该域：cn:legacy 在 CN 列表消失，
// 但裸名模型仍两域可见（global 侧同一条 id 不受 cn: 前缀条目影响）。
func TestModelsHiddenScopedCNOnly(t *testing.T) {
	restore := presetDynamicModelsCache([]upstream.ModelInfo{{ID: "legacy-model"}, {ID: "glm-5.2"}})
	defer restore()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"data":[]}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelsHidden: []string{"cn:legacy-model"}})

	ids := collectModelIDs(t, h)
	if ids["cn:legacy-model"] {
		t.Errorf("cn: 前缀条目应隐藏 CN 域条目: ids=%v", ids)
	}
	if !ids["cn:glm-5.2"] {
		t.Errorf("非黑名单模型不应受影响: ids=%v", ids)
	}
	// 同一 id 在 global 域不受 cn: 前缀条目约束（判据按 realm 前缀精确匹配）。
	if h.hiddenModel("global", "legacy-model") {
		t.Error("cn: 前缀条目不得隐藏 global 域")
	}
	if !h.hiddenModel("cn", "legacy-model") {
		t.Error("cn: 前缀条目应隐藏 cn 域")
	}
}

// globalModelsFakeClient 构造一个 global 域可达的 fake 上游：/v3/config 下发固定
// 模型目录，其余路径 404（企业端点家族探测失败但被容忍，不影响并集结果）。
func globalModelsFakeClient(t *testing.T, ids []string) *upstream.Client {
	t.Helper()
	models := make([]string, 0, len(ids))
	for _, id := range ids {
		models = append(models, `{"id":"`+id+`"}`)
	}
	payload := `{"code":0,"data":{"models":[` + strings.Join(models, ",") + `]}}`
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/v3/config") {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(payload)),
				}, nil
			}
			return &http.Response{
				StatusCode: 404,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":404,"msg":"not found"}`)),
			}, nil
		})},
		ChatBaseCN:     "https://fake.example",
		BillingBaseCN:  "https://fake.example",
		ChatBaseGlobal: "https://fake-global.example",
		GlobalEnabled:  true,
	}
}

// TestModelsHiddenFilteredGlobal global 域同样过滤：裸名条目（两域）与
// "global:" 前缀条目都要从 global 名单里消失，未隐藏的照常列出。
func TestModelsHiddenFilteredGlobal(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	restore := presetDynamicModelsCache([]upstream.ModelInfo{{ID: "glm-5.2"}})
	defer restore()

	up := globalModelsFakeClient(t, []string{"gpt-5.4", "glm-5.2", "hide-me"})
	p := testPoolWith(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{
		Pool:          p,
		Upstream:      up,
		GlobalEnabled: true,
		ModelsHidden:  []string{"glm-5.2", "global:hide-me"},
	})

	ids := collectModelIDs(t, h)
	if ids["global:glm-5.2"] {
		t.Errorf("裸名条目应同时隐藏 global 域: ids=%v", ids)
	}
	if ids["global:hide-me"] {
		t.Errorf("global: 前缀条目应隐藏 global 域: ids=%v", ids)
	}
	if !ids["global:gpt-5.4"] {
		t.Errorf("未隐藏的 global 模型应照常列出: ids=%v", ids)
	}
}

// TestHiddenModelHotReload 快照整体替换（面板保存）后黑名单立即生效/失效。
func TestHiddenModelHotReload(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	live := livecfg.New(livecfg.Snapshot{HiddenModels: livecfg.NormalizeHiddenModels([]string{"glm-5.2"})})
	h := NewHandler(Config{Pool: p, Upstream: up, Live: live})

	if rec := hiddenChatPost(t, h, "glm-5.2"); rec.Code != http.StatusBadRequest {
		t.Fatalf("配置后应拒绝: code=%d", rec.Code)
	}
	// 面板清空黑名单 → 无需重启即放行
	live.Store(livecfg.Snapshot{HiddenModels: livecfg.NormalizeHiddenModels(nil)})
	if rec := hiddenChatPost(t, h, "glm-5.2"); rec.Code != http.StatusOK {
		t.Fatalf("清空黑名单后应放行: code=%d body=%s", rec.Code, rec.Body.String())
	}
}
