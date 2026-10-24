package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// rateLimitBody 6004 模型级限流（带重置文案 → CooldownSoftForModel 记原模型冷却）。
const rateLimitBody = `{"code":6004,"msg":"当前模型额度已用完，将在 2030-01-01 00:00:00 重置"}`

var sseOKBody = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

// fallbackUpstream 可捕获每次请求 model 字段的假上游。behavior 返回每跳的响应。
type fallbackUpstream struct {
	models  []string // 每跳出站 body 的 model
	respond func(call int) (int, string, bool)
}

func (f *fallbackUpstream) client() *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			f.models = append(f.models, extractModelField(string(body)))
			status, respBody, isStream := f.respond(len(f.models))
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(respBody)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

func extractModelField(body string) string {
	i := strings.Index(body, `"model":"`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(`"model":"`):]
	if j := strings.Index(rest, `"`); j >= 0 {
		return rest[:j]
	}
	return ""
}

// TestModelFallbackOn6004SameAccountRetry 6004 触发降级：**同账号**换等价模型重试
// 成功；冷却记在原模型名下（等价模型不被误冷却）；出站 body 的 model 换成等价模型。
func TestModelFallbackOn6004SameAccountRetry(t *testing.T) {
	fu := &fallbackUpstream{respond: func(call int) (int, string, bool) {
		if call == 1 {
			return 429, rateLimitBody, false // 原模型 6004
		}
		return 200, sseOKBody, true // 等价模型成功
	}}
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: fu.client(),
		Live: livecfg.New(livecfg.Snapshot{ModelFallback: map[string]string{"glm-5.2": "glm-5.1"}})})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200（降级后成功）", rec.Code, rec.Body)
	}
	// 同账号两跳：第一跳 model=glm-5.2（6004），第二跳 model=glm-5.1（降级成功）。
	if len(fu.models) != 2 || fu.models[0] != "glm-5.2" || fu.models[1] != "glm-5.1" {
		t.Fatalf("出站模型序列=%v want [glm-5.2 glm-5.1]（同账号两跳）", fu.models)
	}
	// 冷却记账：原模型被冷却，等价模型不受牵连（降级才有效的关键）。
	if blk := p.ModelBlocked("glm-5.2"); !blk.Blocked {
		t.Fatal("原模型 glm-5.2 应被记 6004 冷却")
	}
	if blk := p.ModelBlocked("glm-5.1"); blk.Blocked {
		t.Fatal("等价模型 glm-5.1 不应被冷却（误记则降级白做）")
	}
}

// TestModelFallbackNoAccountBailout 全池原模型 6004 冷却、选不到号：直接降级到
// 等价模型（若只在上游 6004 后降级，这里会先报 503 根本走不到降级——恰好在
// 最需要降级的场景失效）。
func TestModelFallbackNoAccountBailout(t *testing.T) {
	fu := &fallbackUpstream{respond: func(call int) (int, string, bool) {
		return 200, sseOKBody, true
	}}
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999})
	// 原模型已被冷却（全池撞限流后的典型状态）。
	p.CooldownSoftForModel("a1", 0, time.Now().Add(time.Hour), "glm-5.2", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, Upstream: fu.client(),
		Live: livecfg.New(livecfg.Snapshot{ModelFallback: map[string]string{"glm-5.2": "glm-5.1"}})})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s want 200（选不到号时兜底降级成功）", rec.Code, rec.Body)
	}
	if len(fu.models) != 1 || fu.models[0] != "glm-5.1" {
		t.Fatalf("出站模型=%v want [glm-5.1]", fu.models)
	}
}

// TestModelFallbackNoLoop A→B、B→A 互指配置不得无限循环：fallbackTried 记录
// 已试目标，A→B→A→(B 已试) 终止；MaxRotate 兜底，请求正常失败不挂死。
func TestModelFallbackNoLoop(t *testing.T) {
	fu := &fallbackUpstream{respond: func(call int) (int, string, bool) {
		return 429, rateLimitBody, false // 两模型都 6004
	}}
	p := testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: fu.client(),
		Live: livecfg.New(livecfg.Snapshot{ModelFallback: map[string]string{
			"glm-5.2": "glm-5.1",
			"glm-5.1": "glm-5.2",
		}})})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if fu.models[len(fu.models)-1] != "glm-5.1" && !strings.Contains(rec.Body.String(), "error") {
		t.Fatalf("末端应有错误响应: code=%d", rec.Code)
	}
	// 最多 4 跳（A→B→A，B 再试被 fallbackTried 拦截；MaxRotate=3 默认下 3 跳封顶）。
	if len(fu.models) > 4 {
		t.Fatalf("防循环失效：上游调用 %d 次（应 ≤4）", len(fu.models))
	}
}

// TestModelFallbackNotConfigured 未配置降级：6004 行为与老版本一致（只换号不换模型）。
func TestModelFallbackNotConfigured(t *testing.T) {
	fu := &fallbackUpstream{respond: func(call int) (int, string, bool) {
		return 429, rateLimitBody, false
	}}
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range []*auth.Auth{
		{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
	} {
		p.Add(a)
		p.SetCredits(a.UID, 1000, 0)
	}
	h := NewHandler(Config{Pool: p, Upstream: fu.client()})
	h.ServeHTTP(rec2(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	for _, m := range fu.models {
		if m != "glm-5.2" {
			t.Fatalf("未配置降级时应只打原模型，出站=%v", fu.models)
		}
	}
}

func rec2() *httptest.ResponseRecorder { return httptest.NewRecorder() }
