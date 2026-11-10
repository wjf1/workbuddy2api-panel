package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Responses API 适配层的行为钉子。
//
// 重点不在覆盖率，而在锁死三件容易静默错的事：
//  1. 请求侧映射：input 字符串/数组两种形态、instructions→system、
//     max_output_tokens→max_tokens、内置工具剥离；
//  2. 响应侧形状：output 项类型与 usage 字段名（Codex CLI 严格解析）；
//  3. 流式事件序列与顺序（客户端按事件名驱动状态机）。

func TestResponsesInputStringToUser(t *testing.T) {
	req := &responsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hello"`),
	}
	body, err := responsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := body["messages"].([]map[string]any)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if msgs[0]["role"] != "user" || msgs[0]["content"] != "hello" {
		t.Fatalf("unexpected message: %+v", msgs[0])
	}
}

func TestResponsesInstructionsBecomeSystem(t *testing.T) {
	req := &responsesRequest{
		Model:        "glm-5.2",
		Instructions: "you are terse",
		Input:        json.RawMessage(`"hi"`),
	}
	body, err := responsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (system+user), got %d", len(msgs))
	}
	// system 必须前置（上游对位置敏感）
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "you are terse" {
		t.Fatalf("system 未前置: %+v", msgs[0])
	}
}

func TestResponsesMaxOutputTokensMapsToMaxTokens(t *testing.T) {
	n := 256
	req := &responsesRequest{
		Model:           "glm-5.2",
		Input:           json.RawMessage(`"hi"`),
		MaxOutputTokens: &n,
	}
	body, err := responsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := body["max_tokens"]; got != 256 {
		t.Fatalf("max_output_tokens 应映射为 max_tokens=256, got %v", got)
	}
	// 反向断言：不能把 Responses 的键名漏给上游（会被判 11101）
	if _, ok := body["max_output_tokens"]; ok {
		t.Fatal("不应把 max_output_tokens 原样传给上游")
	}
}

func TestResponsesInputArrayWithMultiturn(t *testing.T) {
	req := &responsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`[
			{"role":"user","content":"q1"},
			{"role":"assistant","content":"a1"},
			{"role":"user","content":[{"type":"input_text","text":"q2"}]}
		]`),
	}
	body, err := responsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	// 第三条的 input_text 块应折叠成字符串
	if msgs[2]["content"] != "q2" {
		t.Fatalf("input_text 块应折叠为字符串, got %#v", msgs[2]["content"])
	}
}

func TestResponsesFunctionCallHistory(t *testing.T) {
	// Codex CLI 多轮工具会话会把 function_call / function_call_output 历史
	// 塞回 input；必须映射成 chat 的 tool_calls / tool 消息，否则上游不认。
	req := &responsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`[
			{"role":"user","content":"read file"},
			{"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"p\":\"a\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"content"}
		]`),
	}
	body, err := responsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	msgs := body["messages"].([]map[string]any)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	if msgs[1]["role"] != "assistant" {
		t.Fatalf("function_call 应映射为 assistant, got %v", msgs[1]["role"])
	}
	if _, ok := msgs[1]["tool_calls"]; !ok {
		t.Fatal("function_call 应带 tool_calls")
	}
	if msgs[2]["role"] != "tool" || msgs[2]["tool_call_id"] != "call_1" {
		t.Fatalf("function_call_output 应映射为 tool 消息, got %+v", msgs[2])
	}
}

func TestResponsesBuiltinToolsStripped(t *testing.T) {
	// 内置工具（web_search_preview 等）网关不实现，必须剥离而不是报错；
	// 同时自定义 function 必须保留。
	req := &responsesRequest{
		Model: "glm-5.2",
		Input: json.RawMessage(`"hi"`),
		Tools: []responsesTool{
			{Type: "web_search_preview"}, // 内置：无 name
			{Type: "function", Name: "read", Description: "read a file",
				Parameters: json.RawMessage(`{"type":"object","properties":{"p":{"type":"string"}}}`)},
		},
	}
	body, err := responsesToChat(req)
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := body["tools"].([]map[string]any)
	if len(tools) != 1 {
		t.Fatalf("内置工具应被剥离只剩 1 个自定义工具, got %d", len(tools))
	}
	fn := tools[0]["function"].(map[string]any)
	if fn["name"] != "read" {
		t.Fatalf("自定义工具名错误: %v", fn["name"])
	}
}

func TestResponsesToolChoiceNormalized(t *testing.T) {
	// Responses 的 {"type":"function","name":"x"} 与 Chat 的嵌套形态不同
	got := normalizeResponsesToolChoice(json.RawMessage(`{"type":"function","name":"read"}`))
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("应归一为对象, got %#v", got)
	}
	fn, ok := m["function"].(map[string]any)
	if !ok || fn["name"] != "read" {
		t.Fatalf("Chat 形态应为 {type:function, function:{name:read}}, got %#v", m)
	}
	// 字符串形态原样
	if v := normalizeResponsesToolChoice(json.RawMessage(`"auto"`)); v != "auto" {
		t.Fatalf("auto 应原样返回, got %v", v)
	}
}

func TestResponsesMissingModelRejected(t *testing.T) {
	if _, err := responsesToChat(&responsesRequest{Input: json.RawMessage(`"hi"`)}); err == nil {
		t.Fatal("缺 model 应报错")
	}
}

func TestResponsesEmptyInputRejected(t *testing.T) {
	if _, err := responsesToChat(&responsesRequest{Model: "glm-5.2", Input: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("空 input 应报错")
	}
}

// TestBuildResponseObjectShape 非流式响应形状：字段名与 output 项类型。
//
// Codex CLI 严格按 object/status/output[].type/usage 字段解析，
// 这里逐项钉住，防止将来「顺手改个字段名」把客户端打挂。
func TestBuildResponseObjectShape(t *testing.T) {
	completion := map[string]any{
		"choices": []any{
			map[string]any{
				"finish_reason": "stop",
				"message": map[string]any{
					"role":              "assistant",
					"content":           "hello world",
					"reasoning_content": "thinking...",
				},
			},
		},
		"usage": map[string]any{
			"prompt_tokens":     float64(10),
			"completion_tokens": float64(20),
			"total_tokens":      float64(30),
		},
	}

	resp := buildResponseObject("glm-5.2", completion, "resp_test")

	if resp["object"] != "response" {
		t.Fatalf("object 应为 response, got %v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Fatalf("status 应为 completed, got %v", resp["status"])
	}
	if resp["id"] != "resp_test" {
		t.Fatalf("id 透传失败: %v", resp["id"])
	}

	output := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("want 2 output items (reasoning + message), got %d", len(output))
	}
	// reasoning 在前
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("第一项应为 reasoning, got %v", output[0].(map[string]any)["type"])
	}
	msg := output[1].(map[string]any)
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Fatalf("第二项应为 message/assistant, got %+v", msg)
	}
	parts := msg["content"].([]any)
	part := parts[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "hello world" {
		t.Fatalf("content part 错误: %+v", part)
	}

	// usage 字段名必须换成 Responses 口径
	u := resp["usage"].(map[string]any)
	if u["input_tokens"] != 10 || u["output_tokens"] != 20 || u["total_tokens"] != 30 {
		t.Fatalf("usage 映射错误: %+v", u)
	}
	if _, ok := u["prompt_tokens"]; ok {
		t.Fatal("不应残留 Chat 的 prompt_tokens 字段名")
	}
	if _, ok := u["input_tokens_details"]; !ok {
		t.Fatal("应带 input_tokens_details")
	}
}

// TestBuildResponseObjectLengthIncomplete 截断（finish=length）必须标 incomplete。
//
// 客户端据此决定续读；标成 completed 会让它以为回答完整而截断在半句。
func TestBuildResponseObjectLengthIncomplete(t *testing.T) {
	completion := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "length",
			"message":       map[string]any{"content": "half a sen"},
		}},
	}
	resp := buildResponseObject("glm-5.2", completion, "r")
	if resp["status"] != "incomplete" {
		t.Fatalf("finish=length 应为 incomplete, got %v", resp["status"])
	}
	details, ok := resp["incomplete_details"].(map[string]any)
	if !ok || details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete_details 错误: %+v", resp["incomplete_details"])
	}
}

// TestResponsesStreamEventSequence 流式事件序列与顺序。
//
// 这是客户端状态机的驱动源：事件名错/顺序错会让 Codex CLI 挂住或丢内容。
func TestResponsesStreamEventSequence(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newResponsesStreamWriter(rec, "glm-5.2")

	// 模拟 chatCompletions 产出的 OpenAI SSE
	frames := []string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"content":"lo"}}]}` + "\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}
	for _, f := range frames {
		sw.Write([]byte(f))
	}
	sw.finish()

	body := rec.Body.String()

	// 必须的事件，且顺序正确
	order := []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
	}
	last := -1
	for _, ev := range order {
		idx := strings.Index(body, ev)
		if idx < 0 {
			t.Fatalf("缺事件: %s", ev)
		}
		if idx < last {
			t.Fatalf("事件顺序错: %s 出现在前一个事件之前", ev)
		}
		last = idx
	}

	// 增量内容必须拼成 "Hello"
	if !strings.Contains(body, `"delta":"Hel"`) {
		t.Fatal("缺第一个增量帧")
	}
	if !strings.Contains(body, `"text":"Hello"`) {
		t.Fatalf("output_text.done 应带完整文本 Hello，实际片段: %s", body[:min(400, len(body))])
	}
	// SSE 结束标记
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatal("缺 [DONE]")
	}
	// Content-Type 必须是 SSE
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应含 text/event-stream, got %q", ct)
	}
}

// TestResponsesStreamToolCallEvents 工具调用的流式事件（分片 arguments 累计）。
func TestResponsesStreamToolCallEvents(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newResponsesStreamWriter(rec, "glm-5.2")

	frames := []string{
		// 首帧带 id/name，arguments 为空
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":""}}]}}]}` + "\n\n",
		// 后续帧只带 arguments 片段
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
	}
	for _, f := range frames {
		sw.Write([]byte(f))
	}
	sw.finish()

	body := rec.Body.String()

	if !strings.Contains(body, "response.function_call_arguments.delta") {
		t.Fatal("缺 arguments delta 事件")
	}
	if !strings.Contains(body, "response.function_call_arguments.done") {
		t.Fatal("缺 arguments done 事件")
	}
	// arguments 必须累计完整
	if !strings.Contains(body, `{\"p\":\"a\"}`) {
		t.Fatalf("arguments 未累计完整，片段: %s", body[:min(600, len(body))])
	}
	// function_call item 的 type 与 call_id
	if !strings.Contains(body, `"type":"function_call"`) {
		t.Fatal("缺 function_call item")
	}
	if !strings.Contains(body, `"call_id":"call_1"`) {
		t.Fatal("call_id 丢失")
	}
}

// TestResponsesStreamEmptyFallback 上游一帧未出也要收尾（不能让客户端挂住）。
func TestResponsesStreamEmptyFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newResponsesStreamWriter(rec, "glm-5.2")
	sw.finish() // 一帧都没写

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.created") {
		t.Fatal("空流也应发 response.created")
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Fatal("空流也应发 response.completed（否则客户端挂住）")
	}
}

// TestWriteResponsesErrorShape 错误体形状（无 param 字段，type 按状态映射）。
func TestWriteResponsesErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeResponsesError(rec, http.StatusTooManyRequests, "rate_limited", "slow down")

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码透传失败: %d", rec.Code)
	}
	var out struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("错误体不是合法 JSON: %v", err)
	}
	if out.Error.Message != "slow down" || out.Error.Code != "rate_limited" {
		t.Fatalf("错误体内容错误: %+v", out.Error)
	}
	if out.Error.Type != "rate_limit_error" {
		t.Fatalf("429 应映射 rate_limit_error, got %q", out.Error.Type)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
