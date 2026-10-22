package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ── models.aliases 模型映射 ─────────────────────────────────────────────────
//
// 为什么需要：部分 agent 只能填固定模型名（默认 = 模型 ID），无法把请求指向别的
// 模型。映射命中时出站/选号/账本全用真实名，响应侧把 model 回显成客户端原发名
// （严格校验「响应 model == 请求 model」的客户端不会当场报错）；未命中零改动。

// aliasTestHandler 构造一个带 aliases/hidden 快照的 handler，并返回出站 body 捕获口。
//
// 出站 body 由 fake transport 读 r.Body 捕获（prepareBody 已强制 stream + 归一），
// 断言点与 TestCustomModeFingerprintSanitizePreserved 同一位置。
func aliasTestHandler(t *testing.T, aliasEntries []string, hidden []string) (*Handler, *[]byte) {
	t.Helper()
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
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
		Live: livecfg.New(livecfg.Snapshot{
			HiddenModels: livecfg.NormalizeHiddenModels(hidden),
			ModelAliases: livecfg.NormalizeModelAliases(aliasEntries),
		}),
	})
	return h, &sentBody
}

// aliasPost 发一次 chat 请求（stream 控制是否流式）。
func aliasPost(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	return rec
}

// TestModelAliasRewritesOutboundBody 映射命中：出站 body 的 model 换成真实名，
// 响应（聚合体）的 model 回显客户端原发名。
func TestModelAliasRewritesOutboundBody(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := aliasPost(t, h, `{"model":"my-gpt","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(*sent, &out); err != nil {
		t.Fatalf("出站 body 解析失败: %v (%s)", err, *sent)
	}
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2（映射后真实名）", got)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v (%s)", err, rec.Body)
	}
	if got := resp["model"]; got != "my-gpt" {
		t.Errorf("响应 model = %v, want my-gpt（回显客户端原发名）", got)
	}
}

// TestModelAliasStreamEcho 流式：帧内 model 覆写回客户端原发名。
func TestModelAliasStreamEcho(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := aliasPost(t, h, `{"model":"my-gpt","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("出站 model = %v, want glm-5.2", got)
	}
	// 上游帧里 model 是 glm-5.2（sseOK 固定值），出站帧必须已改回 my-gpt。
	body := rec.Body.String()
	if strings.Contains(body, `"model":"glm-5.2"`) {
		t.Errorf("流式帧仍带真实模型名，回显未生效:\n%s", body)
	}
	if !strings.Contains(body, `"model":"my-gpt"`) {
		t.Errorf("流式帧缺客户端原发名:\n%s", body)
	}
}

// TestModelAliasStreamEmptyEchoZeroChange 未映射的流式请求：帧内 model 原样透传
// （echoModel 为空 = 零改动）。
func TestModelAliasStreamEmptyEchoZeroChange(t *testing.T) {
	h, _ := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := aliasPost(t, h, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"model":"glm-5.2"`) {
		t.Errorf("未映射请求的帧 model 应原样保留:\n%s", rec.Body.String())
	}
}

// TestModelAliasCannotBypassHidden 别名目标受黑名单约束（映射在 hidden 检查之前
// 完成，检查的是真实名）。
func TestModelAliasCannotBypassHidden(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, []string{"glm-5.2"})
	rec := aliasPost(t, h, `{"model":"my-gpt","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400（映射目标被隐藏）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "model_unavailable") {
		t.Errorf("body=%s, want model_unavailable", rec.Body)
	}
	if len(*sent) != 0 {
		t.Errorf("被隐藏的映射目标不应打上游，实际出站 body=%s", *sent)
	}
}

// TestModelAliasLookup 查找口径：大小写不敏感；域前缀键只在该域生效；
// 未配置的表（nil）不命中。
func TestModelAliasLookup(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
		Live: livecfg.New(livecfg.Snapshot{
			ModelAliases: livecfg.NormalizeModelAliases([]string{"My-GPT=glm-5.2", "cn:opus=global:gpt-5.4"}),
		}),
	})
	if got, ok := h.modelAlias("cn", "my-gpt"); !ok || got != "glm-5.2" {
		t.Errorf("大小写不敏感查找失败: %q ok=%v", got, ok)
	}
	if got, ok := h.modelAlias("CN", "MY-GPT"); !ok || got != "glm-5.2" {
		t.Errorf("realm 大写归一化失败: %q ok=%v", got, ok)
	}
	if _, ok := h.modelAlias("global", "opus"); ok {
		t.Error("cn: 前缀键不应在 global 域命中")
	}
	if got, ok := h.modelAlias("cn", "opus"); !ok || got != "global:gpt-5.4" {
		t.Errorf("跨域指向值应原样返回: %q ok=%v", got, ok)
	}
	if _, ok := h.modelAlias("cn", ""); ok {
		t.Error("空模型名不应命中")
	}

	// 无表（nil 快照）→ 不命中
	h2 := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	if _, ok := h2.modelAlias("cn", "my-gpt"); ok {
		t.Error("未配置映射时不应命中")
	}
}

// TestModelAliasScopedKeyOnlyOwnRealm 域前缀键只在对应域生效：global: 键不拦
// CN 域请求（CN 域按原样打上游，body 不变）。
func TestModelAliasScopedKeyOnlyOwnRealm(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"global:claude=glm-5.2"}, nil)
	// CN 域同名模型 → 不命中，出站 body 保持客户端原发的 claude
	rec := aliasPost(t, h, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "claude" {
		t.Errorf("global: 键不应改写 CN 域请求: out=%v", got)
	}
}

// TestModelAliasCrossRealmTarget 别名值可带前缀跨域指向：请求命中后按目标名的
// realm 选号、出站 body 用目标裸名、响应仍回显客户端原发名。
func TestModelAliasCrossRealmTarget(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"cn:my-opus=global:gpt-5.4"}, nil)
	rec := aliasPost(t, h, `{"model":"my-opus","messages":[{"role":"user","content":"hi"}]}`)
	// 池里只有 CN 号 → 跨域目标选不到号，请求失败；但出站改写与否由 sent 判定。
	_ = rec
	var out map[string]any
	if err := json.Unmarshal(*sent, &out); err == nil {
		if got := out["model"]; got != "gpt-5.4" {
			t.Errorf("跨域映射出站 model = %v, want gpt-5.4（目标裸名）", got)
		}
	}
	// 池内无 global 账号时不应把 CN 号当 global 用：响应不得是 200。
	if rec.Code == http.StatusOK {
		t.Errorf("无 global 账号时跨域目标不应成功: code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestModelAliasUnmappedZeroChange 未命中映射的请求：出站 body 与响应都保持原样
// （别名字段存在也不影响未映射模型）。
func TestModelAliasUnmappedZeroChange(t *testing.T) {
	h, sent := aliasTestHandler(t, []string{"my-gpt=glm-5.2"}, nil)
	rec := aliasPost(t, h, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(*sent, &out)
	if got := out["model"]; got != "glm-5.2" {
		t.Errorf("未映射模型出站 model = %v, want glm-5.2", got)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v (%s)", err, rec.Body)
	}
	if got := resp["model"]; got != "glm-5.2" {
		t.Errorf("未映射模型响应 model = %v, want glm-5.2", got)
	}
}

// TestModelAliasUnmappedBodyByteIdentical 未命中映射：出站 body 逐字节等于
// 「同一请求在无 aliases 配置时」的字节（现状零回归）。以网关自身归一后的
// 两次出站字节比对实现——别名表存在不得引入任何额外改写。
func TestModelAliasUnmappedBodyByteIdentical(t *testing.T) {
	body := `{"model":"glm-5.2","temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`
	withAlias, sentA := aliasTestHandler(t, []string{"my-gpt=deepseek-v3"}, nil)
	if rec := aliasPost(t, withAlias, body); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	noAlias, sentB := aliasTestHandler(t, nil, nil)
	if rec := aliasPost(t, noAlias, body); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	if string(*sentA) != string(*sentB) {
		t.Errorf("未命中映射时出站 body 必须逐字节不变:\n有别名=%s\n无别名=%s", *sentA, *sentB)
	}
}

// TestModelAliasHotReload 快照整体替换（面板保存）后映射立即生效，旧映射消失。
func TestModelAliasHotReload(t *testing.T) {
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
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	live := livecfg.New(livecfg.Snapshot{ModelAliases: livecfg.NormalizeModelAliases([]string{"a=glm-5.2"})})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: up,
		Live:     live,
	})
	if rec := aliasPost(t, h, `{"model":"a","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(sentBody, &out)
	if got := out["model"]; got != "glm-5.2" {
		t.Fatalf("热改前出站 model = %v, want glm-5.2", got)
	}

	// 面板整表替换：旧映射 a 消失，新映射 b 生效
	live.Store(livecfg.Snapshot{ModelAliases: livecfg.NormalizeModelAliases([]string{"b=deepseek-v3"})})
	if rec := aliasPost(t, h, `{"model":"a","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(sentBody, &out)
	if got := out["model"]; got != "a" {
		t.Errorf("旧映射应随快照替换失效: 出站 model = %v, want a", got)
	}
	if rec := aliasPost(t, h, `{"model":"b","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	_ = json.Unmarshal(sentBody, &out)
	if got := out["model"]; got != "deepseek-v3" {
		t.Errorf("新映射应生效: 出站 model = %v, want deepseek-v3", got)
	}
}
