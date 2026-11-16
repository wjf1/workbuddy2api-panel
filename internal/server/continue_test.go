package server

// continueReader 截断续写的行为钉子。
//
// 这组用例直接测读取器（不经 handler），因为续写的核心语义全在帧级拼装上：
//  1. finish=length 纯文本 → 续写并拼成一条流，[DONE] 只出现一次；
//  2. 客户端显式小额度 → 不续写（截断是它的预期语义）；
//  3. 本段含工具调用分片 → 不续写（残缺 JSON 拼接更糟）；
//  4. 续写请求失败 → 原样降级回截断终态；
//  5. 断流（无 finish 无 DONE）→ 只盲重试一次；
//  6. 累计输出到顶 → 停止续写；
//  7. 空截断（正文 0B、思考吃光）→ 关思考重发一次；
//  8. 预告句收尾 → narration 救援；正常句号收尾 → 不救；
//  9. 末条是工具结果 + 短正文 + 长 prompt → toolonly 只采纳工具调用。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// scriptedUpstream 可脚本化的假上游：第 N 次调用返回第 N 个响应，
// 并记录每次请求体（续写断言必需——要检查 assistant 前缀与续写指令）。
type scriptedUpstream struct {
	mu       sync.Mutex
	calls    int
	bodies   []string
	script   func(call int) (status int, body string, isStream bool)
	client   *upstream.Client
	t        *testing.T
	override func(call int) (int, string, bool) // 可选：按调用序号改写（测试用）
}

func newScriptedUpstream(t *testing.T, script func(call int) (int, string, bool)) *scriptedUpstream {
	t.Helper()
	s := &scriptedUpstream{script: script, t: t}
	s.client = &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			call := s.calls
			s.calls++
			s.bodies = append(s.bodies, string(raw))
			s.mu.Unlock()
			status, body, isStream := s.script(call)
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
	return s
}

func (s *scriptedUpstream) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *scriptedUpstream) body(i int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 0 || i >= len(s.bodies) {
		return ""
	}
	return s.bodies[i]
}

// drain 读完整个读取器，返回客户端可见的字节。
func drain(t *testing.T, r io.Reader) string {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(raw)
}

// streamFrame 构造一个 delta 帧（content 或 tool_calls）。
func streamFrame(content string) string {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}}},
	})
	return "data: " + string(b) + "\n\n"
}

// endFrame 构造终态帧（finish_reason + usage）。
func endFrame(finish string, completion, think float64) string {
	usage := map[string]any{"prompt_tokens": 100.0, "completion_tokens": completion, "total_tokens": 100 + completion}
	if think > 0 {
		usage["completion_thinking_tokens"] = think
	}
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		"usage":   usage,
	})
	return "data: " + string(b) + "\n\n"
}

// toolFrame 构造工具调用分片帧（arguments 故意残缺，模拟被截断的工具参数）。
func toolFrame() string {
	tc := map[string]any{
		"index": 0, "id": "call_1", "type": "function",
		"function": map[string]any{"name": "read_file", "arguments": `{"path":"a`},
	}
	delta := map[string]any{"tool_calls": []any{tc}}
	choice := map[string]any{"index": 0, "delta": delta}
	payload := map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk",
		"choices": []any{choice},
	}
	b, _ := json.Marshal(payload)
	return "data: " + string(b) + "\n\n"
}

const doneFrame = "data: [DONE]\n\n"

// newTestReader 构造被测读取器（body 由调用方给，通常是最小 chat 请求）。
func newTestReader(t *testing.T, up *scriptedUpstream, body string, rc io.ReadCloser, floor int64) *continueReader {
	t.Helper()
	a := &auth.Auth{UID: "u1", Nickname: "u1", AccessToken: "at"}
	return newContinueReaderWithFloor(context.Background(), up.client, a, []byte(body), "127.0.0.1", upstream.ChatMeta{}, rc, floor)
}

// TestContinueJoinsTruncatedText finish=length 的纯文本段被续写拼成一条流。
func TestContinueJoinsTruncatedText(t *testing.T) {
	seg1 := streamFrame("前半段内容") + endFrame("length", 100, 0) + doneFrame
	seg2 := streamFrame("后半段内容") + endFrame("stop", 50, 0) + doneFrame
	// 原始段由 strings.Reader 注入、不走 fake：fake 的 call=0 就是第一次续写。
	up := newScriptedUpstream(t, func(call int) (int, string, bool) {
		return 200, seg2, true
	})

	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"写长文"}]}`
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(seg1)), 8192)
	out := drain(t, r)

	// 原始段由 strings.Reader 直接注入、不走 fake，故 callCount 只统计续写调用。
	if up.callCount() != 1 {
		t.Fatalf("应发生 1 次续写调用，实际 %d", up.callCount())
	}
	if !strings.Contains(out, "前半段内容") || !strings.Contains(out, "后半段内容") {
		t.Errorf("两段内容都应出现在客户端可见流中，实际:\n%s", out)
	}
	// 续写请求必须把已输出内容作为 assistant 前缀，并带续写指令。
	b1 := up.body(0)
	if !strings.Contains(b1, "前半段内容") {
		t.Errorf("续写请求未带 assistant 前缀，实际 body:\n%s", b1)
	}
	if !strings.Contains(b1, "被截断") {
		t.Errorf("续写请求未带续写指令，实际 body:\n%s", b1)
	}
	// 终态只应有一个 finish_reason。
	if n := strings.Count(out, `"finish_reason":"`); n != 1 {
		t.Errorf("终态帧应恰好 1 个，实际 %d 个:\n%s", n, out)
	}
}

// TestContinueSkipsSmallClientLimit 客户端显式小额度 → 不续写。
func TestContinueSkipsSmallClientLimit(t *testing.T) {
	seg1 := streamFrame("写到一半") + endFrame("length", 1024, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) { return 200, seg1, true })

	// floor=0：网关没抬过额度，此时客户端显式限额就是它真要的长度。
	body := `{"model":"deepseek-v4.1-flash","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(seg1)), 0)
	drain(t, r)

	if up.callCount() != 0 {
		t.Fatalf("客户端显式限额下不该续写，实际续写调用 %d 次", up.callCount())
	}
}

// TestContinueSkipsToolCallSegment 本段含工具调用分片 → 不续写。
func TestContinueSkipsToolCallSegment(t *testing.T) {
	seg1 := streamFrame("准备读文件") + toolFrame() + endFrame("length", 500, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) { return 200, seg1, true })

	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(seg1)), 8192)
	drain(t, r)

	if up.callCount() != 0 {
		t.Fatalf("含工具调用分片的段不该续写，实际续写调用 %d 次", up.callCount())
	}
}

// TestContinueDegradesOnRetryFailure 续写请求失败 → 原样降级（不吞掉原始内容）。
func TestContinueDegradesOnRetryFailure(t *testing.T) {
	seg1 := streamFrame("原始内容") + endFrame("length", 100, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) {
		return 500, `{"error":"boom"}`, false
	})

	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(seg1)), 8192)
	out := drain(t, r)

	if up.callCount() != 1 {
		t.Fatalf("应尝试一次续写，实际续写调用 %d 次", up.callCount())
	}
	if !strings.Contains(out, "原始内容") {
		t.Errorf("原始内容必须保留，实际:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"length"`) {
		t.Errorf("应降级回截断终态，实际:\n%s", out)
	}
}

// TestContinueBlindRetryOnCutStream 断流（无 finish 无 DONE）→ 盲重试一次。
func TestContinueBlindRetryOnCutStream(t *testing.T) {
	cut := streamFrame("断流前的内容") // 无 finish、无 [DONE]
	seg2 := streamFrame("续上的内容") + endFrame("stop", 80, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) {
		return 200, seg2, true
	})

	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(cut)), 8192)
	out := drain(t, r)

	if up.callCount() != 1 {
		t.Fatalf("断流应触发一次盲重试，实际续写调用 %d 次", up.callCount())
	}
	if !strings.Contains(out, "续上的内容") {
		t.Errorf("盲重试的内容应接进流，实际:\n%s", out)
	}
}

// TestContinueStopsAtTotalCap 累计输出达上限 → 停止续写。
func TestContinueStopsAtTotalCap(t *testing.T) {
	// 下限 8192 × 4 = 32768：首段就报 40000 completion，第二段必须不再发。
	seg1 := streamFrame("很长的一段") + endFrame("length", 40000, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) { return 200, seg1, true })

	body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(seg1)), 8192)
	drain(t, r)

	if up.callCount() != 0 {
		t.Fatalf("累计输出到顶后不该续写，实际续写调用 %d 次", up.callCount())
	}
}

// TestContinueStarvedRetryResendsWithoutThinking 正文 0B、思考吃光 → 关思考重发一次。
func TestContinueStarvedRetryResendsWithoutThinking(t *testing.T) {
	seg1 := streamFrame("") + endFrame("length", 300, 300) + doneFrame
	seg2 := streamFrame("救援回来的正文") + endFrame("stop", 50, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) {
		return 200, seg2, true
	})

	body := `{"model":"deepseek-v4.1-flash","max_tokens":300,"messages":[{"role":"user","content":"hi"}]}`
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(seg1)), 8192)
	out := drain(t, r)

	if up.callCount() != 1 {
		t.Fatalf("空截断应关思考重发一次，实际续写调用 %d 次", up.callCount())
	}
	if !strings.Contains(out, "救援回来的正文") {
		t.Errorf("救援段内容应接进流，实际:\n%s", out)
	}
	b1 := up.body(0)
	var obj map[string]any
	if err := json.Unmarshal([]byte(b1), &obj); err != nil {
		t.Fatalf("重发体不是合法 JSON: %v", err)
	}
	th, _ := obj["thinking"].(map[string]any)
	if th == nil || th["type"] != "disabled" {
		t.Errorf("重发体应关掉思考，实际 thinking=%v", obj["thinking"])
	}
	if _, has := obj["reasoning_effort"]; has {
		t.Error("关思考时不该保留 reasoning_effort")
	}
}

// TestContinueBriefNonStreamReturnsUnchanged 非续写形态原样透传（不额外发请求）。
func TestContinueBriefNonStreamReturnsUnchanged(t *testing.T) {
	seg := streamFrame("正常回答。") + endFrame("stop", 20, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) { return 200, seg, true })

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(seg)), 0)
	out := drain(t, r)

	if up.callCount() != 0 {
		t.Fatalf("正常完整流不该触发任何续写，实际续写调用 %d 次", up.callCount())
	}
	if !strings.Contains(out, "正常回答。") {
		t.Errorf("内容应原样透传，实际:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("终态应原样保留，实际:\n%s", out)
	}
}

// bigPromptEndFrame 同 endFrame，但可指定 prompt_tokens（toolonly 的长会话门槛用）。
func bigPromptEndFrame(finish string, completion, prompt float64) string {
	usage := map[string]any{"prompt_tokens": prompt, "completion_tokens": completion, "total_tokens": prompt + completion}
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "deepseek-v4.1-flash",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		"usage":   usage,
	})
	return "data: " + string(b) + "\n\n"
}

// TestChatHandlerTriggersContinue 端到端：经完整 handler 链路，截断流被续写拼成一条。
//
// 这层验证接线是否正确——读取器单测覆盖不到「handler 把 cont 传给了 stats/StreamHint」
// 这一段。若接线漏了，客户端会看到两段独立输出且只收到截断终态。
func TestChatHandlerTriggersContinue(t *testing.T) {
	seg1 := streamFrame("前半段") + endFrame("length", 100, 0) + doneFrame
	seg2 := streamFrame("后半段") + endFrame("stop", 50, 0) + doneFrame
	// fake 第一次调用（call=0）返回截断段，之后返回续写段。
	up := newScriptedUpstream(t, func(call int) (int, string, bool) {
		if call == 0 {
			return 200, seg1, true
		}
		return 200, seg2, true
	})

	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up.client,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v4.1-flash","stream":true,"messages":[{"role":"user","content":"写长文"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "前半段") || !strings.Contains(body, "后半段") {
		t.Errorf("两段内容都应出现在客户端流中，实际:\n%s", body)
	}
	if up.callCount() != 2 {
		t.Errorf("应发生 2 次上游调用（原始 + 续写），实际 %d", up.callCount())
	}
	// [DONE] 只能出现一次（StreamHint 统一补）。
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] 应恰好 1 个，实际 %d:\n%s", n, body)
	}
}
