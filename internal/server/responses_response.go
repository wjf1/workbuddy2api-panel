// Responses API 的响应侧：非流式聚合翻译、流式事件序列、错误体。
//
// 设计约束（与 anthropic_response.go 同）：产出必须是真实 Responses 协议形状，
// Codex CLI 会严格按字段解析；少字段可以（客户端有兜底），字段类型错会让整条
// 响应解析失败。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// writeResponsesError 写 Responses 形状的错误体。
//
// 协议形状：{"error":{"message":..,"type":..,"code":..}}
// （与 Chat 的 {"error":{"message","type","param","code"}} 略不同，少了 param。）
func writeResponsesError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    responsesErrType(status),
			"code":    code,
		},
	})
}

// responsesErrType 按 HTTP 状态给 Responses 的 error.type（客户端据此分类重试）。
func responsesErrType(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// newResponseID 生成 response id（对齐官方 `resp_` 前缀形态）。
func newResponseID() string { return newAnthropicID("resp_") }

// writeResponsesFromPass 把一次聚合 pass 的结果翻译成 Responses 非流式响应。
//
// chat.completion → response 对象的字段映射：
//
//	choices[0].message.content            → output 里的 message/output_text 项
//	choices[0].message.reasoning_content  → output 里的 reasoning 项（在前）
//	choices[0].message.tool_calls         → output 里的 function_call 项
//	usage.prompt_tokens                   → usage.input_tokens
//	usage.completion_tokens               → usage.output_tokens
func writeResponsesFromPass(w http.ResponseWriter, model string, pr *anthropicPassResult) {
	if pr == nil || pr.status != http.StatusOK || pr.resp == nil {
		status := http.StatusBadGateway
		msg := "upstream request failed"
		code := "upstream_error"
		if pr != nil {
			if pr.status >= 400 {
				status = pr.status
			}
			if pr.errMsg != "" {
				msg = pr.errMsg
			}
		}
		writeResponsesError(w, status, code, msg)
		return
	}

	writeJSON(w, http.StatusOK, buildResponseObject(model, pr.resp, newResponseID()))
}

// buildResponseObject 把 chat.completion 聚合成 Responses 的 response 对象。
//
// 抽成独立函数是为了流式的 response.completed 事件也能复用同一产物
// （Codex CLI 对 completed 事件的 response 字段与最终非流式体做同构校验）。
func buildResponseObject(model string, completion map[string]any, id string) map[string]any {
	output := make([]any, 0, 3)
	// reasoningItems 单独收集：正文项要插在它们之后、工具项之前（上游顺序）。
	var reasoningItems []any
	var text strings.Builder
	var finish string

	if choices, ok := completion["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if f, ok := c["finish_reason"].(string); ok {
				finish = f
			}
			if msg, ok := c["message"].(map[string]any); ok {
				// reasoning（DeepSeek/GLM 思维链）在前，与上游顺序一致。
				if r, ok := msg["reasoning_content"].(string); ok && r != "" {
					item := map[string]any{
						"id":   newAnthropicID("rs_"),
						"type": "reasoning",
						"summary": []any{
							map[string]any{"type": "summary_text", "text": r},
						},
					}
					reasoningItems = append(reasoningItems, item)
					output = append(output, item)
				}
				if ct, ok := msg["content"].(string); ok {
					text.WriteString(ct)
				}
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						tcm, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						callID, _ := tcm["id"].(string)
						var name, args string
						if fn, ok := tcm["function"].(map[string]any); ok {
							name, _ = fn["name"].(string)
							args, _ = fn["arguments"].(string)
						}
						output = append(output, map[string]any{
							"id":        newAnthropicID("fc_"),
							"type":      "function_call",
							"call_id":   callID,
							"name":      name,
							"arguments": args,
							"status":    "completed",
						})
					}
				}
			}
		}
	}

	// 正文作为 message 项（无正文则不产，避免空 content 数组）。
	// 顺序与上游一致：reasoning 在前、message 次之、function_call 在后。
	if text.Len() > 0 {
		msgItem := map[string]any{
			"id":     newAnthropicID("msg_"),
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{
				map[string]any{"type": "output_text", "text": text.String(), "annotations": []any{}},
			},
		}
		if len(reasoningItems) > 0 {
			// 已有 reasoning：插在 reasoning 之后、其余（tool_calls）之前。
			out2 := make([]any, 0, len(output)+1)
			out2 = append(out2, reasoningItems...)
			out2 = append(out2, msgItem)
			out2 = append(out2, output[len(reasoningItems):]...)
			output = out2
		} else {
			output = append(output, msgItem)
		}
	}

	// finish_reason 映射：length → incomplete（客户端据此知道被截断）。
	status := "completed"
	if finish == "length" {
		status = "incomplete"
	}

	resp := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     status,
		"model":      model,
		"output":     output,
	}
	if status == "incomplete" {
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if u, ok := completion["usage"].(map[string]any); ok {
		resp["usage"] = chatUsageToResponses(u)
	}
	return resp
}

// chatUsageToResponses Chat usage → Responses usage 字段名映射。
//
// Responses 用 input_tokens / output_tokens / total_tokens，并额外要求
// input_tokens_details.cached_tokens 与 output_tokens_details.reasoning_tokens。
func chatUsageToResponses(u map[string]any) map[string]any {
	in, _ := u["prompt_tokens"].(float64)
	out, _ := u["completion_tokens"].(float64)
	total, _ := u["total_tokens"].(float64)
	cached, _ := u["prompt_cache_hit_tokens"].(float64)
	reasoning := 0.0
	if d, ok := u["completion_tokens_details"].(map[string]any); ok {
		reasoning, _ = d["reasoning_tokens"].(float64)
	}
	return map[string]any{
		"input_tokens":          int(in),
		"input_tokens_details":  map[string]any{"cached_tokens": int(cached)},
		"output_tokens":         int(out),
		"output_tokens_details": map[string]any{"reasoning_tokens": int(reasoning)},
		"total_tokens":          int(total),
	}
}

// ---------------------------------------------------------------------------
// 流式：OpenAI SSE → Responses 具名事件
// ---------------------------------------------------------------------------

// responsesStreamWriter 把 chatCompletions 的 OpenAI SSE 帧翻译成 Responses 事件流。
//
// 事件序列（Codex CLI 依赖这些名称与顺序）：
//
//	response.created
//	response.output_item.added        （message 项）
//	response.content_part.added
//	response.output_text.delta        （增量文本，重复）
//	response.output_text.done
//	response.content_part.done
//	response.output_item.done
//	response.completed                （含完整 response 对象）
//
// 工具调用事件（出现 tool_calls 时）：
//
//	response.output_item.added (type=function_call)
//	response.function_call_arguments.delta
//	response.function_call_arguments.done
//	response.output_item.done
type responsesStreamWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	id      string

	started     bool // 已发 response.created
	headerSent  bool // 200 头已落
	textStarted bool // 已发 output_item.added + content_part.added

	text      strings.Builder
	reasoning strings.Builder
	// msgItemID message output 项 id（首次产出正文时生成，done/completed 复用）。
	msgItemID string

	// 工具调用按 index 累计（OpenAI 的 tool_calls delta 是分片的）
	toolCalls map[int]*streamToolCall

	finishReason string
	startedAt    time.Time
	wmu          sync.Mutex // 与心跳（无）和主写入互斥，防帧撕裂
}

// streamToolCall 流式工具调用的累计状态。
type streamToolCall struct {
	itemID    string
	callID    string
	name      string
	args      strings.Builder
	announced bool // 已发 output_item.added（首帧带 id/name 时才发）
}

func newResponsesStreamWriter(w http.ResponseWriter, model string) *responsesStreamWriter {
	fl, _ := w.(http.Flusher)
	return &responsesStreamWriter{
		w:         w,
		flusher:   fl,
		model:     model,
		id:        newResponseID(),
		toolCalls: map[int]*streamToolCall{},
		startedAt: time.Now(),
	}
}

// Header 让 chatCompletions 在开流前设置的 header（如 Content-Type）能生效。
func (s *responsesStreamWriter) Header() http.Header { return s.w.Header() }

// WriteHeader 截获状态码：成功的 200 不立即下发（SSE 由本类型自己起头），
// 错误状态原样透传（chatCompletions 的错误路径直接写非 SSE 体）。
func (s *responsesStreamWriter) WriteHeader(status int) {
	if status == http.StatusOK {
		return
	}
	s.w.WriteHeader(status)
}

// Write 逐帧解析 OpenAI SSE 并翻译成 Responses 事件。
func (s *responsesStreamWriter) Write(p []byte) (int, error) {
	n := len(p)
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue // 心跳注释等：不转发（Responses 客户端不认裸注释）
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue // 由 finish() 发 response.completed
		}
		s.handleChunk(payload)
	}
	return n, nil
}

// handleChunk 解析单个 data 帧并产出对应事件。
func (s *responsesStreamWriter) handleChunk(payload string) {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	if len(chunk.Choices) == 0 {
		return
	}
	s.ensureStarted()

	c := chunk.Choices[0]
	if c.Delta.ReasoningContent != "" {
		s.reasoning.WriteString(c.Delta.ReasoningContent)
	}
	if c.Delta.Content != "" {
		if !s.textStarted {
			s.startTextItem()
		}
		s.text.WriteString(c.Delta.Content)
		s.emit("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       s.msgItemID,
			"output_index":  0,
			"content_index": 0,
			"delta":         c.Delta.Content,
		})
	}
	for _, tc := range c.Delta.ToolCalls {
		s.handleToolDelta(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
	}
	if c.FinishReason != nil && *c.FinishReason != "" {
		s.finishReason = *c.FinishReason
	}
}

// startTextItem 挂出 message 项与 content_part。
func (s *responsesStreamWriter) startTextItem() {
	s.textStarted = true
	s.msgItemID = newAnthropicID("msg_")
	s.emit("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]any{
			"id": s.msgItemID, "type": "message", "role": "assistant",
			"status": "in_progress", "content": []any{},
		},
	})
	s.emit("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "item_id": s.msgItemID,
		"output_index": 0, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// handleToolDelta 累计工具调用分片并在首帧挂出 output_item.added。
func (s *responsesStreamWriter) handleToolDelta(idx int, id, name, args string) {
	st, ok := s.toolCalls[idx]
	if !ok {
		st = &streamToolCall{}
		s.toolCalls[idx] = st
	}
	if id != "" {
		st.callID = id
	}
	if name != "" && st.name == "" {
		st.name = name
	}
	if args != "" {
		st.args.WriteString(args)
	}
	if !st.announced && st.name != "" {
		st.announced = true
		st.itemID = newAnthropicID("fc_")
		s.emit("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": 1 + idx,
			"item": map[string]any{
				"id": st.itemID, "type": "function_call", "call_id": st.callID,
				"name": st.name, "arguments": "", "status": "in_progress",
			},
		})
	}
	if args != "" && st.announced {
		s.emit("response.function_call_arguments.delta", map[string]any{
			"type": "response.function_call_arguments.delta", "item_id": st.itemID,
			"output_index": 1 + idx, "delta": args,
		})
	}
}

// finish 收尾：各 .done 事件 + response.completed + [DONE]。
func (s *responsesStreamWriter) finish() {
	if !s.started {
		s.ensureStarted() // 上游未产出任何帧：兜底发 created+completed，别让客户端挂住
	}
	if s.textStarted {
		full := s.text.String()
		s.emit("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "item_id": s.msgItemID,
			"output_index": 0, "content_index": 0, "text": full,
		})
		s.emit("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "item_id": s.msgItemID,
			"output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
		})
		s.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 0,
			"item": map[string]any{
				"id": s.msgItemID, "type": "message", "role": "assistant",
				"status": "completed",
				"content": []any{map[string]any{
					"type": "output_text", "text": full, "annotations": []any{},
				}},
			},
		})
	}
	for _, idx := range sortedToolIndexes(s.toolCalls) {
		st := s.toolCalls[idx]
		if !st.announced {
			continue
		}
		s.emit("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "item_id": st.itemID,
			"output_index": 1 + idx, "arguments": st.args.String(),
		})
		s.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "output_index": 1 + idx,
			"item": map[string]any{
				"id": st.itemID, "type": "function_call", "call_id": st.callID,
				"name": st.name, "arguments": st.args.String(), "status": "completed",
			},
		})
	}

	s.emit("response.completed", map[string]any{
		"type": "response.completed", "response": s.buildFinalResponse(),
	})
	s.wmu.Lock()
	_, _ = io.WriteString(s.w, "data: [DONE]\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
	s.wmu.Unlock()
}

// buildFinalResponse 组装 completed 事件里的完整 response 对象。
func (s *responsesStreamWriter) buildFinalResponse() map[string]any {
	output := make([]any, 0, 3)
	if r := s.reasoning.String(); r != "" {
		output = append(output, map[string]any{
			"id": newAnthropicID("rs_"), "type": "reasoning",
			"summary": []any{map[string]any{"type": "summary_text", "text": r}},
		})
	}
	if s.textStarted {
		output = append(output, map[string]any{
			"id": s.msgItemID, "type": "message", "role": "assistant",
			"status": "completed",
			"content": []any{map[string]any{
				"type": "output_text", "text": s.text.String(), "annotations": []any{},
			}},
		})
	}
	for _, idx := range sortedToolIndexes(s.toolCalls) {
		st := s.toolCalls[idx]
		if !st.announced {
			continue
		}
		output = append(output, map[string]any{
			"id": st.itemID, "type": "function_call", "call_id": st.callID,
			"name": st.name, "arguments": st.args.String(), "status": "completed",
		})
	}

	status := "completed"
	if s.finishReason == "length" {
		status = "incomplete"
	}
	resp := map[string]any{
		"id":         s.id,
		"object":     "response",
		"created_at": s.startedAt.Unix(),
		"status":     status,
		"model":      s.model,
		"output":     output,
	}
	if status == "incomplete" {
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return resp
}

// ensureStarted 发 response.created（幂等，只发一次）。
func (s *responsesStreamWriter) ensureStarted() {
	if s.started {
		return
	}
	s.started = true
	s.emit("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         s.id,
			"object":     "response",
			"created_at": s.startedAt.Unix(),
			"status":     "in_progress",
			"model":      s.model,
			"output":     []any{},
		},
	})
}

// emit 写一条 Responses SSE 事件（event: name + data: json）并 flush。
//
// 与 anthropic 层同形态：Responses 客户端两种写法都认（event 行 + data.type），
// 同时给出可最大兼容。
func (s *responsesStreamWriter) emit(name string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if !s.headerSent {
		s.headerSent = true
		s.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("Connection", "keep-alive")
		s.w.WriteHeader(http.StatusOK)
	}
	_, _ = io.WriteString(s.w, "event: "+name+"\ndata: "+string(raw)+"\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// sortedToolIndexes 工具调用 index 升序（map 遍历无序，事件顺序必须稳定）。
func sortedToolIndexes(m map[int]*streamToolCall) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
