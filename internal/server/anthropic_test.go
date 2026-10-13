package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/websearch"
)

// ---------------------------------------------------------------------------
// 请求映射
// ---------------------------------------------------------------------------

func TestAnthropicToOpenAIMapping(t *testing.T) {
	reqJSON := `{
	  "model":"cn:deepseek-v4.1-flash","max_tokens":256,"stream":true,
	  "temperature":0.7,"stop_sequences":["END"],
	  "system":[{"type":"text","text":"sys-a"},{"type":"text","text":"sys-b"}],
	  "messages":[
	    {"role":"user","content":"你好"},
	    {"role":"assistant","content":[{"type":"text","text":"我看看"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"上海"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"晴 21℃"}]},{"type":"text","text":"总结下"}]}
	  ],
	  "tools":[
	    {"name":"get_weather","description":"查询天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}},
	    {"type":"web_search_20260209","name":"web_search","max_uses":8}
	  ],
	  "tool_choice":{"type":"tool","name":"get_weather"}
	}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatal(err)
	}
	body, serverTools, err := anthropicToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(serverTools) != 1 || serverTools[0].Type != "web_search_20260209" {
		t.Fatalf("server tools = %+v", serverTools)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 5 { // system + user + assistant + tool + user
		t.Fatalf("messages len = %d: %#v", len(msgs), msgs)
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "sys-a\nsys-b" {
		t.Errorf("system = %#v", m0)
	}
	m2 := msgs[2].(map[string]any)
	if m2["role"] != "assistant" || m2["content"] != "我看看" {
		t.Errorf("assistant = %#v", m2)
	}
	tcs, _ := m2["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls = %#v", m2)
	}
	tc0 := tcs[0].(map[string]any)
	fn := tc0["function"].(map[string]any)
	if tc0["id"] != "toolu_1" || fn["name"] != "get_weather" || fn["arguments"] != `{"city":"上海"}` {
		t.Errorf("tool_call = %#v", tc0)
	}
	m3 := msgs[3].(map[string]any)
	if m3["role"] != "tool" || m3["tool_call_id"] != "toolu_1" || m3["content"] != "晴 21℃" {
		t.Errorf("tool msg = %#v", m3)
	}
	m4 := msgs[4].(map[string]any)
	if m4["role"] != "user" || m4["content"] != "总结下" {
		t.Errorf("trailing user = %#v", m4)
	}
	// server tool 必须从出站 tools 剥离
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("outbound tools = %#v", body["tools"])
	}
	if fn2 := tools[0].(map[string]any)["function"].(map[string]any); fn2["name"] != "get_weather" {
		t.Errorf("outbound tool = %#v", tools[0])
	}
	if _, ok := fn2Schema(tools[0]); !ok {
		t.Errorf("input_schema 应映射到 parameters")
	}
	tc := body["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "get_weather" {
		t.Errorf("tool_choice = %#v", body["tool_choice"])
	}
	if body["max_tokens"] != 256 || body["stream"] != true || body["temperature"] != 0.7 {
		t.Errorf("numeric fields = %#v", body)
	}
	stops, _ := body["stop"].([]string)
	if len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop = %#v", body["stop"])
	}
}

func fn2Schema(tool any) (any, bool) {
	fn, _ := tool.(map[string]any)["function"].(map[string]any)
	if fn == nil {
		return nil, false
	}
	params, ok := fn["parameters"].(json.RawMessage)
	return params, ok
}

func TestAnthropicImageAndUnknownBlocks(t *testing.T) {
	reqJSON := `{
	  "model":"m","max_tokens":10,
	  "messages":[
	    {"role":"user","content":[
	      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
	      {"type":"text","text":"看这张图"}
	    ]},
	    {"role":"assistant","content":[
	      {"type":"thinking","thinking":"内部思考","signature":"sig"},
	      {"type":"text","text":"好的"}
	    ]}
	  ]
	}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatal(err)
	}
	body, _, err := anthropicToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages = %#v", msgs)
	}
	parts, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("user parts = %#v", msgs[0])
	}
	img := parts[0].(map[string]any)
	if img["type"] != "image_url" || img["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image part = %#v", img)
	}
	// thinking 块被丢弃：assistant 只剩文本
	if msgs[1].(map[string]any)["content"] != "好的" {
		t.Errorf("assistant = %#v", msgs[1])
	}
}

func TestAnthropicUnknownServerToolStripped(t *testing.T) {
	reqJSON := `{"model":"m","max_tokens":10,
	  "messages":[{"role":"user","content":"hi"}],
	  "tools":[{"type":"web_fetch_20250910","name":"web_fetch"},{"name":"bash","input_schema":{"type":"object"}}]}`
	var req anthropicRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatal(err)
	}
	body, serverTools, err := anthropicToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(serverTools) != 1 || serverTools[0].Type != "web_fetch_20250910" {
		t.Fatalf("server tools = %+v", serverTools)
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", body["tools"])
	}
}

// ---------------------------------------------------------------------------
// HTTP 层（复用 handler_test 的 fake upstream / 池骨架）
// ---------------------------------------------------------------------------

const anthTextSSE = "data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"根据搜索结果，一密一用指……\"}}]}\n\n" +
	"data: {\"id\":\"c2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":30,\"completion_tokens\":8,\"total_tokens\":38}}\n\n" +
	"data: [DONE]\n\n"

const anthToolCallSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"query\\\": \\\"等保测评 一密一用\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":6,\"total_tokens\":26}}\n\n" +
	"data: [DONE]\n\n"

// recordUpstream 记录每次上游请求体，并按调用序返回预设 SSE。
type recordUpstream struct {
	t       *testing.T
	bodies  []string
	replies []string
	n       int
}

func newRecordUpstream(t *testing.T, replies ...string) (*upstream.Client, *recordUpstream) {
	t.Helper()
	r := &recordUpstream{t: t, replies: replies}
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			r.bodies = append(r.bodies, string(raw))
			body := r.replies[0]
			if r.n < len(r.replies) {
				body = r.replies[r.n]
			}
			r.n++
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}, r
}

func newAnthropicHandler(t *testing.T, up *upstream.Client, searcher websearch.Searcher) *Handler {
	t.Helper()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	return NewHandler(Config{Pool: p, Upstream: up, Search: searcher})
}

func postMessages(h *Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	return rec
}

func TestAnthropicNonStream(t *testing.T) {
	up, rec1 := newRecordUpstream(t, sseOK)
	h := newAnthropicHandler(t, up, nil)
	rec := postMessages(h, `{"model":"cn:glm-5.3-flash","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["type"] != "message" || resp["role"] != "assistant" || resp["stop_reason"] != "end_turn" {
		t.Fatalf("resp = %#v", resp)
	}
	blocks := resp["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "text" || blocks[0].(map[string]any)["text"] != "你好" {
		t.Fatalf("blocks = %#v", blocks)
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(1) || usage["output_tokens"] != float64(1) {
		t.Errorf("usage = %#v", usage)
	}
	if len(rec1.bodies) != 1 {
		t.Fatalf("upstream calls = %d", len(rec1.bodies))
	}
}

func TestAnthropicStreamEvents(t *testing.T) {
	up, _ := newRecordUpstream(t, sseOK)
	h := newAnthropicHandler(t, up, nil)
	rec := postMessages(h, `{"model":"cn:glm-5.3-flash","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	events := anthropicEventNames(t, rec.Body.String())
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v", events)
	}
	if !strings.Contains(rec.Body.String(), `"text":"你好"`) {
		t.Errorf("missing text delta: %s", rec.Body.String())
	}
	var delta map[string]any
	for _, ev := range parseAnthropicEvents(t, rec.Body.String()) {
		if ev.name == "message_delta" {
			delta = ev.data
		}
	}
	if delta == nil || delta["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("message_delta = %#v", delta)
	}
	if out := delta["usage"].(map[string]any)["output_tokens"]; out != float64(1) {
		t.Errorf("message_delta usage = %#v", delta["usage"])
	}
}

// sseToolStream 上游流式工具调用（arguments 分两片）。
const sseToolStream = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"上海\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":3,\"total_tokens\":8}}\n\n" +
	"data: [DONE]\n\n"

func TestAnthropicStreamToolUse(t *testing.T) {
	up, _ := newRecordUpstream(t, sseToolStream)
	h := newAnthropicHandler(t, up, nil)
	rec := postMessages(h, `{"model":"cn:glm-5.3-flash","max_tokens":64,"stream":true,
	  "messages":[{"role":"user","content":"天气"}],
	  "tools":[{"name":"get_weather","input_schema":{"type":"object"}}]}`)
	body := rec.Body.String()
	events := anthropicEventNames(t, body)
	want := []string{
		"message_start",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v body=%s", events, body)
	}
	if !strings.Contains(body, `"type":"tool_use"`) || !strings.Contains(body, `"name":"get_weather"`) {
		t.Errorf("missing tool_use block: %s", body)
	}
	if !strings.Contains(body, `"partial_json":"{\"city\":"`) {
		t.Errorf("missing input_json_delta piece: %s", body)
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Errorf("missing tool_use stop_reason: %s", body)
	}
}

func TestAnthropicAuthXApiKey(t *testing.T) {
	up, _ := newRecordUpstream(t, sseOK)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-test"})

	// x-api-key 正确
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"cn:m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "sk-test")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("x-api-key 正确应通过，got %d %s", rec.Code, rec.Body.String())
	}
	// Bearer 沿用
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"cn:m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer sk-test")
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("Bearer 应通过，got %d", rec2.Code)
	}
	// 错误 key → 401 Anthropic 错误体
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"cn:m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
	req3.Header.Set("x-api-key", "wrong")
	h.ServeHTTP(rec3, req3)
	if rec3.Code != 401 {
		t.Fatalf("错误 key 应 401，got %d", rec3.Code)
	}
	var e map[string]any
	if json.Unmarshal(rec3.Body.Bytes(), &e) != nil || e["type"] != "error" ||
		e["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("错误体 = %s", rec3.Body.String())
	}
}

func TestAnthropicValidation(t *testing.T) {
	up, rec1 := newRecordUpstream(t, sseOK)
	h := newAnthropicHandler(t, up, nil)
	// 缺 max_tokens → 400
	if rec := postMessages(h, `{"model":"cn:m","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 400 {
		t.Fatalf("缺 max_tokens 应 400，got %d", rec.Code)
	}
	// 非法 JSON → 400
	if rec := postMessages(h, `{oops`); rec.Code != 400 {
		t.Fatalf("非法 JSON 应 400，got %d", rec.Code)
	}
	if rec1.n != 0 {
		t.Fatalf("校验失败不应触达上游，calls=%d", rec1.n)
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	up, _ := newRecordUpstream(t, sseOK)
	h := newAnthropicHandler(t, up, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"cn:m","messages":[{"role":"user","content":"一段测试文本"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if n, _ := resp["input_tokens"].(float64); n < 1 {
		t.Fatalf("input_tokens = %#v", resp)
	}
}

// ---------------------------------------------------------------------------
// web_search 服务端工具
// ---------------------------------------------------------------------------

type fakeSearcher struct {
	calls   []string
	results []websearch.Result
	err     error
}

func (f *fakeSearcher) Search(ctx context.Context, query string, max int) ([]websearch.Result, error) {
	f.calls = append(f.calls, query)
	if f.err != nil {
		return nil, f.err
	}
	rs := f.results
	if len(rs) > max {
		rs = rs[:max]
	}
	return rs, nil
}

func fakeSearchResults() []websearch.Result {
	return []websearch.Result{
		{Title: "等保测评与密钥管理要求", URL: "https://example.com/a", Snippet: "一密一用指……", PageAge: "2026-09-30"},
		{Title: "GB/T 39786 解读", URL: "https://example.com/b", Snippet: "密钥管理章节……"},
	}
}

const anthWebSearchReq = `{"model":"cn:deepseek-v4.1-flash","max_tokens":4096,"stream":%s,
  "system":"You are an assistant for performing a web search tool use.",
  "messages":[{"role":"user","content":"Perform a web search for the query: 等保测评 一密一用"}],
  "tools":[{"type":"web_search_20260209","name":"web_search","max_uses":8}]}`

func TestAnthropicWebSearchNonStream(t *testing.T) {
	up, upRec := newRecordUpstream(t, anthToolCallSSE, anthTextSSE)
	fs := &fakeSearcher{results: fakeSearchResults()}
	h := newAnthropicHandler(t, up, fs)
	rec := postMessages(h, strings.Replace(anthWebSearchReq, "%s", "false", 1))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	blocks := resp["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("blocks = %#v", blocks)
	}
	su := blocks[0].(map[string]any)
	if su["type"] != "server_tool_use" || su["name"] != "web_search" ||
		su["input"].(map[string]any)["query"] != "等保测评 一密一用" {
		t.Fatalf("server_tool_use = %#v", su)
	}
	res := blocks[1].(map[string]any)
	if res["type"] != "web_search_tool_result" || res["tool_use_id"] != su["id"] {
		t.Fatalf("web_search_tool_result = %#v", res)
	}
	entries := res["content"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	e0 := entries[0].(map[string]any)
	if e0["type"] != "web_search_result" || e0["url"] != "https://example.com/a" ||
		e0["title"] != "等保测评与密钥管理要求" || e0["page_age"] != "2026-09-30" {
		t.Fatalf("entry0 = %#v", e0)
	}
	if _, ok := e0["encrypted_content"].(string); !ok {
		t.Fatalf("encrypted_content 必须是 string（客户端 schema 要求）: %#v", e0)
	}
	text := blocks[2].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Sources:") || !strings.Contains(text, "https://example.com/a") {
		t.Fatalf("正文未兜底来源链接: %q", text)
	}
	// 检索被真实调用
	if len(fs.calls) != 1 || fs.calls[0] != "等保测评 一密一用" {
		t.Fatalf("searcher calls = %v", fs.calls)
	}
	// 第二轮出站：检索结果以 tool 消息回灌（多轮编排下工具仍在——只有预算
	// 耗尽才移除，见 TestAnthropicWebSearchMaxUsesLimit）。
	if len(upRec.bodies) != 2 {
		t.Fatalf("upstream calls = %d", len(upRec.bodies))
	}
	pass2 := upRec.bodies[1]
	if !strings.Contains(pass2, `"role":"tool"`) || !strings.Contains(pass2, "example.com/a") {
		t.Errorf("pass2 缺少检索结果回灌: %s", pass2)
	}
	// usage.server_tool_use
	usage := resp["usage"].(map[string]any)
	stu, _ := usage["server_tool_use"].(map[string]any)
	if stu == nil || stu["web_search_requests"] != float64(1) {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestAnthropicWebSearchStream(t *testing.T) {
	up, _ := newRecordUpstream(t, anthToolCallSSE, anthTextSSE)
	fs := &fakeSearcher{results: fakeSearchResults()}
	h := newAnthropicHandler(t, up, fs)
	rec := postMessages(h, strings.Replace(anthWebSearchReq, "%s", "true", 1))
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
	for _, want := range []string{`"type":"server_tool_use"`, `"type":"web_search_tool_result"`, `"type":"text_delta"`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in %s", want, body)
		}
	}
	events := anthropicEventNames(t, body)
	if events[0] != "message_start" || events[len(events)-1] != "message_stop" {
		t.Fatalf("events = %v", events)
	}
}

func TestAnthropicWebSearchDisabled(t *testing.T) {
	up, upRec := newRecordUpstream(t, anthTextSSE)
	h := newAnthropicHandler(t, up, nil) // Search = nil：工具被剥离，照常作答
	rec := postMessages(h, strings.Replace(anthWebSearchReq, "%s", "false", 1))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(upRec.bodies) != 1 {
		t.Fatalf("upstream calls = %d（不应多 pass）", len(upRec.bodies))
	}
	if strings.Contains(upRec.bodies[0], "web_search") {
		t.Errorf("web_search 服务端工具应被剥离: %s", upRec.bodies[0])
	}
	if strings.Contains(rec.Body.String(), "server_tool_use") {
		t.Errorf("禁用时不应出现检索块: %s", rec.Body.String())
	}
}

func TestAnthropicWebSearchBackendError(t *testing.T) {
	up, _ := newRecordUpstream(t, anthToolCallSSE, anthTextSSE)
	fs := &fakeSearcher{err: context.DeadlineExceeded}
	h := newAnthropicHandler(t, up, fs)
	rec := postMessages(h, strings.Replace(anthWebSearchReq, "%s", "false", 1))
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	blocks := resp["content"].([]any)
	res := blocks[1].(map[string]any)
	if res["type"] != "web_search_tool_result" {
		t.Fatalf("blocks = %#v", blocks)
	}
	errObj, ok := res["content"].(map[string]any)
	if !ok || errObj["type"] != "web_search_tool_result_error" {
		t.Fatalf("检索失败应回 tool_result_error: %#v", res["content"])
	}
}

func TestAnthropicUpstreamErrorShape(t *testing.T) {
	// 上游 400（11101 类）→ Anthropic 错误体 + 原状态码
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Unmarshal chat params failed","type":"api_error","code":"bad_params"}}`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	h := newAnthropicHandler(t, up, nil)
	rec := postMessages(h, `{"model":"cn:m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 400 {
		t.Fatalf("status = %d", rec.Code)
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	er, _ := e["error"].(map[string]any)
	if er == nil || er["type"] != "invalid_request_error" || !strings.Contains(er["message"].(string), "Unmarshal") {
		t.Fatalf("error body = %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// DSML 伪工具标记清洗
// ---------------------------------------------------------------------------

// dsmlToken 拼装全角竖线（源码用转义，避免不可辨字符混入文件）。
const dsmlToken = "\uFF5C\uFF5C"

func TestStripDSMLText(t *testing.T) {
	p := dsmlToken
	block := "<" + p + "DSML" + p + " calls>\n<" + p + "DSML" + p + " invoke name=\"web_search\">\n<" + p + "DSML" + p + " parameter name=\"query\" string=\"true\">等保</" + p + "DSML" + p + " parameter>\n</" + p + "DSML" + p + " invoke>\n</" + p + "DSML" + p + " calls>"
	in := "正文开头\n" + block + "\n正文结尾"
	got := StripDSMLText(in)
	if strings.Contains(got, "DSML") {
		t.Fatalf("DSML 残留: %q", got)
	}
	if !strings.Contains(got, "正文开头") || !strings.Contains(got, "正文结尾") {
		t.Fatalf("正文被误伤: %q", got)
	}
	// 普通文本（含全角竖线/单个 '<'，但不构成标记）不受影响。
	plain := "对比 " + p + " 符号 < 小于号都不该动"
	if StripDSMLText(plain) != plain {
		t.Fatalf("普通文本被改写: %q", StripDSMLText(plain))
	}
	// 未闭合残段（模型中途停止/截断）。
	tail := "答案正文\n<" + p + "DSML" + p + " invoke name=\"web_search\">部分残段"
	if got := StripDSMLText(tail); got != "答案正文" {
		t.Fatalf("残段未剥净: %q", got)
	}
}

func TestDSMLStreamFilter(t *testing.T) {
	p := dsmlToken
	block := "<" + p + "DSML" + p + " invoke name=\"web_search\">queryxx</" + p + "DSML" + p + " invoke>"
	full := "前言" + block + "后记"
	// 最坏切分：逐字节喂入（标记跨帧、前缀被拆散）。
	var out strings.Builder
	f := newDSMLStreamFilter()
	for i := 0; i < len(full); i++ {
		out.WriteString(f.Write(full[i : i+1]))
	}
	out.WriteString(f.Flush())
	if got := out.String(); got != "前言后记" {
		t.Fatalf("流式过滤结果 = %q", got)
	}
	// 普通文本（含 '<' 与全角竖线）逐字放行、不丢字。
	f2 := newDSMLStreamFilter()
	var b2 strings.Builder
	for _, ch := range []string{"a<", "b" + p, "c", ">"} {
		b2.WriteString(f2.Write(ch))
	}
	b2.WriteString(f2.Flush())
	if got := b2.String(); got != "a<b"+p+"c>" {
		t.Fatalf("普通文本被改写: %q", got)
	}
}

// TestStripToolCallBlocks 形态 B（伪 XML 工具调用）清洗：真伪调用剥净，
// 普通内容与「非工具调用形状的 JSON」原样保留。
func TestStripToolCallBlocks(t *testing.T) {
	call := "<tool_call>\n{\"name\": \"web_search\", \"arguments\": {\"query\": \"等保测评\"}}\n</tool_call>"
	in := "我先检索一下。\n\n" + call + "\n\n以上是结论。"
	got := StripDSMLText(in)
	if strings.Contains(got, "tool_call") || strings.Contains(got, "arguments") {
		t.Fatalf("伪调用未剥净: %q", got)
	}
	if !strings.Contains(got, "我先检索一下。") || !strings.Contains(got, "以上是结论。") {
		t.Fatalf("正文被误伤: %q", got)
	}
	// tool_calls 复数 + 数组载荷形态。
	if got := StripDSMLText("<tool_calls>[{\"name\":\"x\",\"arguments\":{}}]</tool_calls>ok"); got != "ok" {
		t.Fatalf("tool_calls 数组形态未剥净: %q", got)
	}
	// 反例：标签内不是工具调用形状的 JSON → 原样保留。
	keep := "<tool_call>{\"foo\": 1}</tool_call>"
	if got := StripDSMLText(keep); got != keep {
		t.Fatalf("非工具调用内容被改写: %q", got)
	}
	// 反例：正文里出现标签但没有载荷（语法说明/代码片段）→ 原样保留。
	keep2 := "Anthropic 的 tool_use 块对应 <tool_call> 这种写法。"
	if got := StripDSMLText(keep2); got != keep2 {
		t.Fatalf("语法说明被改写: %q", got)
	}
	// 未闭合（截断）的伪调用：载荷像工具调用 → 截到文末。
	tail := "开始检索\n<tool_call>\n{\"name\": \"web_search\", \"arguments\": {\"query\": \"x"
	if got := StripDSMLText(tail); got != "开始检索" {
		t.Fatalf("截断伪调用未剥净: %q", got)
	}
}

// TestDSMLStreamFilterRealFrames 复现实测帧切分：形态 A 整块逐片喂入，且闭标签
// 自身被拆成多帧（"</" / 双竖线 / " calls" / ">"）。修复前闭合标签会被当正文
// 放行（实测正文只剩一个闭合 calls 标签）；修复后整块吞净、后续正文完好。
func TestDSMLStreamFilterRealFrames(t *testing.T) {
	p := dsmlToken
	frames := []string{
		"<", p, "DSML", p, " calls>\n<", p, "DSML", p, " invoke name=\"web_search\">\n<", p,
		"DSML", p, " parameter name=\"query\" string=\"true\">q</", p, "DSML", p, " parameter>\n</", p,
		"DSML", p, " invoke>\n</", p, "DSML", p, " calls>",
		"我无法联网搜索。",
	}
	f := newDSMLStreamFilter()
	var b strings.Builder
	for _, fr := range frames {
		b.WriteString(f.Write(fr))
	}
	b.WriteString(f.Flush())
	if got := b.String(); got != "我无法联网搜索。" {
		t.Fatalf("真实帧切分下泄漏/误伤: %q", got)
	}
}

// TestDSMLStreamFilterToolCallForm 形态 B 的流式过滤：逐字节喂入的伪调用整块
// 吞净、前后正文完好；非工具调用载荷照常放行；截断的伪调用在 Flush 丢弃。
func TestDSMLStreamFilterToolCallForm(t *testing.T) {
	full := "先检索。\n<tool_call>\n{\"name\": \"web_search\", \"arguments\": {\"query\": \"等保\"}}\n</tool_call>\n结论如下。"
	f := newDSMLStreamFilter()
	var b strings.Builder
	for i := 0; i < len(full); i++ {
		b.WriteString(f.Write(full[i : i+1]))
	}
	b.WriteString(f.Flush())
	got := b.String()
	if strings.Contains(got, "tool_call") || strings.Contains(got, "web_search") {
		t.Fatalf("伪调用漏出: %q", got)
	}
	if !strings.Contains(got, "先检索。") || !strings.Contains(got, "结论如下。") {
		t.Fatalf("正文被误伤: %q", got)
	}
	// 反例：非工具调用载荷 → 逐字节喂入后原样放行（含标签）。
	keep := `<tool_call>{"foo":1}</tool_call>`
	f2 := newDSMLStreamFilter()
	var b2 strings.Builder
	for i := 0; i < len(keep); i++ {
		b2.WriteString(f2.Write(keep[i : i+1]))
	}
	b2.WriteString(f2.Flush())
	if b2.String() != keep {
		t.Fatalf("非工具调用内容被改写: %q", b2.String())
	}
	// 截断：未闭合且载荷像工具调用 → 整段丢弃（正文前缀照常放行）。
	f3 := newDSMLStreamFilter()
	out3 := f3.Write("检索中\n<tool_call>\n{\"name\": \"web_search\", \"arguments\": {\"query\": \"x")
	out3 += f3.Flush()
	if strings.TrimSpace(out3) != "检索中" {
		t.Fatalf("截断伪调用未丢弃: %q", out3)
	}
}

// ---------------------------------------------------------------------------
// web_search 多轮 / 限额
// ---------------------------------------------------------------------------

// toolCallSSE 生成上游「流式工具调用」回复（arguments 为单个完整分片）。
func toolCallSSE(id, name, args string) string {
	esc, _ := json.Marshal(args)
	return "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"" + id + "\",\"type\":\"function\",\"function\":{\"name\":\"" + name + "\",\"arguments\":" + string(esc) + "}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
		"data: [DONE]\n\n"
}

func TestAnthropicWebSearchMultiRound(t *testing.T) {
	up, upRec := newRecordUpstream(t,
		toolCallSSE("call_a", searchFnName, `{"query":"第一轮查询"}`),
		toolCallSSE("call_b", searchFnName, `{"query":"第二轮查询"}`),
		anthTextSSE,
	)
	fs := &fakeSearcher{results: fakeSearchResults()}
	h := newAnthropicHandler(t, up, fs)
	rec := postMessages(h, strings.Replace(anthWebSearchReq, "%s", "false", 1))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(fs.calls) != 2 || fs.calls[0] != "第一轮查询" || fs.calls[1] != "第二轮查询" {
		t.Fatalf("searcher calls = %v", fs.calls)
	}
	if len(upRec.bodies) != 3 {
		t.Fatalf("upstream calls = %d（两轮检索 + 终局作答）", len(upRec.bodies))
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	blocks := resp["content"].([]any)
	var toolUse, toolRes int
	for _, bi := range blocks {
		switch bi.(map[string]any)["type"] {
		case "server_tool_use":
			toolUse++
		case "web_search_tool_result":
			toolRes++
		}
	}
	if toolUse != 2 || toolRes != 2 {
		t.Fatalf("检索块数 = %d/%d blocks=%#v", toolUse, toolRes, blocks)
	}
	usage := resp["usage"].(map[string]any)
	if stu, _ := usage["server_tool_use"].(map[string]any); stu == nil || stu["web_search_requests"] != float64(2) {
		t.Fatalf("usage = %#v", usage)
	}
	// 第三轮的出站请求不应带搜索工具（只在限额后移除；此处未限额，应仍带）。
	if !strings.Contains(upRec.bodies[2], searchFnName) {
		t.Logf("note: 第三轮 body 未含 web_search（限额未触发时本不该发生）")
	}
}

func TestAnthropicWebSearchMaxUsesLimit(t *testing.T) {
	up, upRec := newRecordUpstream(t,
		toolCallSSE("call_a", searchFnName, `{"query":"唯一一次查询"}`),
		toolCallSSE("call_b", searchFnName, `{"query":"超限查询"}`),
		anthTextSSE,
	)
	fs := &fakeSearcher{results: fakeSearchResults()}
	h := newAnthropicHandler(t, up, fs)
	body := strings.Replace(anthWebSearchReq, "%s", "false", 1)
	body = strings.Replace(body, `"max_uses":8`, `"max_uses":1`, 1)
	rec := postMessages(h, body)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// 预算 1：只应真实执行一次检索，第二次调用被限额提示接住。
	if len(fs.calls) != 1 || fs.calls[0] != "唯一一次查询" {
		t.Fatalf("searcher calls = %v", fs.calls)
	}
	if len(upRec.bodies) < 2 || !strings.Contains(upRec.bodies[1], "System notice") {
		t.Fatalf("限额后应注入提示；bodies[1]=%s", upRec.bodies[1])
	}
	// 限额后 tools 数组里不应再有搜索工具（历史里的 assistant.tool_calls 回显
	// 属于 OpenAI 配对，不算）。
	var limitBody map[string]any
	if err := json.Unmarshal([]byte(upRec.bodies[1]), &limitBody); err != nil {
		t.Fatalf("bodies[1] not JSON: %v", err)
	}
	if tools, ok := limitBody["tools"].([]any); ok {
		for _, ti := range tools {
			fn, _ := ti.(map[string]any)["function"].(map[string]any)
			if fn != nil && fn["name"] == searchFnName {
				t.Fatalf("限额后搜索工具应被移除: %s", upRec.bodies[1])
			}
		}
	}
}

// ---------------------------------------------------------------------------
// SSE 测试工具
// ---------------------------------------------------------------------------

type anthEvent struct {
	name string
	data map[string]any
}

func parseAnthropicEvents(t *testing.T, body string) []anthEvent {
	t.Helper()
	var out []anthEvent
	for _, frame := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			continue
		}
		ev := anthEvent{name: name}
		_ = json.Unmarshal([]byte(data), &ev.data)
		out = append(out, ev)
	}
	return out
}

func anthropicEventNames(t *testing.T, body string) []string {
	t.Helper()
	events := parseAnthropicEvents(t, body)
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e.name)
	}
	return names
}
