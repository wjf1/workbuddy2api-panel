// Anthropic 响应侧：OpenAI chat.completion → Anthropic message 的非流式转换，
// 以及流式路径的 SSE 逐帧翻译器（OpenAI chunk → Anthropic 事件序列）。
//
// 事件序列对齐 Anthropic 官方流的形态（客户端 zod/schema 按此校验）：
//
//	message_start
//	content_block_start(text)          … content_block_delta(text_delta)*  content_block_stop
//	content_block_start(tool_use)      … content_block_delta(input_json_delta)*  content_block_stop
//	content_block_start(server_tool_use / web_search_tool_result) … content_block_stop
//	message_delta(stop_reason, usage)  message_stop
//
// 错误帧（上游 200 + error 帧的既有形态）翻译为 Anthropic `error` 事件并终止流；
// 非 200 的 JSON 错误体由 finish() 统一转换为 Anthropic 错误体。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 非流式：OpenAI chat.completion → Anthropic message
// ---------------------------------------------------------------------------

// chatRespBlocks 从聚合后的 OpenAI 响应提取 Anthropic content 块与 stop_reason。
// skipToolName 非空时，该名字的 tool_calls 不产出 tool_use 块（web_search 由
// 服务端工具块承接）。text 返回拼接后的正文（供调用方做来源链接兜底）。
func chatRespBlocks(resp map[string]any, skipToolName string) (blocks []any, stopReason string, text string) {
	stopReason = "end_turn"
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil, stopReason, ""
	}
	c0, _ := choices[0].(map[string]any)
	if c0 == nil {
		return nil, stopReason, ""
	}
	if fr, _ := c0["finish_reason"].(string); fr != "" {
		stopReason = anthropicStopReason(fr)
	}
	msg, _ := c0["message"].(map[string]any)
	if msg == nil {
		return nil, stopReason, ""
	}
	if s, _ := msg["content"].(string); s != "" {
		// DSML 伪工具调用标记清洗（见 dsml.go：模型对未注册工具会把调用语法
		// 当正文吐出）。
		s = StripDSMLText(s)
		if s != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": s})
			text = s
		}
	}
	if tcs, ok := msg["tool_calls"].([]any); ok {
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if tc == nil {
				continue
			}
			fn, _ := tc["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if skipToolName != "" && name == skipToolName {
				continue
			}
			id, _ := tc["id"].(string)
			if id == "" {
				id = newAnthropicID("toolu_")
			}
			args, _ := fn["arguments"].(string)
			input := map[string]any{}
			if strings.TrimSpace(args) != "" {
				if json.Unmarshal([]byte(args), &input) != nil {
					input = map[string]any{"_raw": args}
				}
			}
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": id, "name": name, "input": input,
			})
		}
	}
	return blocks, stopReason, text
}

// anthropicUsageFromOpenAI OpenAI usage → Anthropic usage（cache 字段补零，
// 客户端按标准 schema 读取）。
func anthropicUsageFromOpenAI(usage map[string]any) map[string]any {
	u := map[string]any{
		"input_tokens":                intFromAny(usage["prompt_tokens"]),
		"output_tokens":               intFromAny(usage["completion_tokens"]),
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
	}
	return u
}

func usageOf(resp map[string]any) map[string]any {
	u, _ := resp["usage"].(map[string]any)
	if u == nil {
		return map[string]any{}
	}
	return u
}

// writeAnthropicMessage 写非流式 Anthropic message JSON。
func writeAnthropicMessage(w http.ResponseWriter, model string, blocks []any, stopReason string, usage map[string]any) {
	if blocks == nil {
		blocks = []any{}
	}
	if usage == nil {
		usage = map[string]any{
			"input_tokens": 0, "output_tokens": 0,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            newAnthropicID("msg_"),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       blocks,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         usage,
	})
}

// writeAnthropicFromPass 单 pass 结果 → Anthropic 响应（非流式客户端）。
func writeAnthropicFromPass(w http.ResponseWriter, model string, pr *anthropicPassResult) {
	if pr.status != http.StatusOK || pr.resp == nil {
		writeAnthropicPassError(w, pr)
		return
	}
	blocks, stopReason, _ := chatRespBlocks(pr.resp, "")
	writeAnthropicMessage(w, model, blocks, stopReason, anthropicUsageFromOpenAI(usageOf(pr.resp)))
}

// ---------------------------------------------------------------------------
// 流式：OpenAI SSE → Anthropic 事件序列
// ---------------------------------------------------------------------------

// anthropicStreamWriter 实现 http.ResponseWriter：作为 chatCompletions 的输出
// 目标，把上游 OpenAI 帧逐帧翻译成 Anthropic 事件；或（静态模式）直接把已
// 组装好的 content 块按事件序列写出（服务端搜索的多 pass 合成路径）。
type anthropicStreamWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	model   string
	msgID   string

	status int // 捕获的 HTTP 状态（0 = 未写）
	buf    []byte

	headerWritten bool
	started       bool
	ended         bool
	sawErr        bool

	nextIndex int
	openKind  string // "" / "text" / "tool"：当前已打开块的类型
	openIndex int

	toolStates   map[int]*anthropicToolState // OpenAI tool_call index → 状态
	finishReason string
	usage        map[string]any
	// dsml 正文伪工具标记的流式过滤器（标记可能跨帧，holdback 处理）。
	dsml *dsmlStreamFilter

	// SSE 心跳（对齐 hub）：长思考模型推理期无输出时客户端/中间代理会读超时断连。
	// 旁路 goroutine 定期发 Anthropic 官方 ping 事件（SDK 规范忽略），写侧经 wmu
	// 与 event() 互斥。上游 OpenAI 帧经 StreamHint 写给本 writer 时夹带的 `: ping`
	// 注释帧会被 handleFrame 丢弃（不带 data: 前缀，到不了客户端），故心跳由本层发。
	wmu       sync.Mutex
	hbStop    chan struct{}
	hbStarted bool
}

type anthropicToolState struct {
	blockIndex int
	id         string
	name       string
	open       bool
}

func newAnthropicStreamWriter(w http.ResponseWriter, model string) *anthropicStreamWriter {
	fl, _ := w.(http.Flusher)
	return &anthropicStreamWriter{
		w:          w,
		flusher:    fl,
		model:      model,
		msgID:      newAnthropicID("msg_"),
		toolStates: map[int]*anthropicToolState{},
		dsml:       newDSMLStreamFilter(),
	}
}

// ---- http.ResponseWriter 实现 ----

func (s *anthropicStreamWriter) Header() http.Header { return s.w.Header() }

func (s *anthropicStreamWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
}

func (s *anthropicStreamWriter) Write(p []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	s.buf = append(s.buf, p...)
	if s.status == http.StatusOK {
		s.drain()
	}
	return len(p), nil
}

// drain 处理缓冲区里所有完整的 SSE 帧（以空行分隔）。
func (s *anthropicStreamWriter) drain() {
	for {
		i := bytes.Index(s.buf, []byte("\n\n"))
		if i < 0 {
			return
		}
		frame := string(s.buf[:i])
		s.buf = s.buf[i+2:]
		s.handleFrame(frame)
	}
}

func (s *anthropicStreamWriter) handleFrame(frame string) {
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue // 注释/空行/其他字段：Anthropic 流无对应形态，忽略
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			if !s.sawErr {
				s.endWith("", nil)
			}
			return
		}
		s.handlePayload(payload)
	}
}

func (s *anthropicStreamWriter) handlePayload(payload string) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return
	}
	// 上游 error 帧（200 + error）：翻译为 Anthropic error 事件并终止。
	if e, ok := obj["error"]; ok {
		s.sawErr = true
		msg := "upstream stream error"
		if em, ok := e.(map[string]any); ok {
			if m, _ := em["message"].(string); m != "" {
				msg = m
			}
			if h, _ := em["gateway_hint"].(string); h != "" {
				msg += " (gateway_hint: " + h + ")"
			}
		}
		s.event("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": msg},
		})
		return
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		s.usage = u
	}
	s.startWith(nil)
	choices, _ := obj["choices"].([]any)
	for _, ci := range choices {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		if fr, _ := c["finish_reason"].(string); fr != "" {
			s.finishReason = fr
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if txt, _ := delta["content"].(string); txt != "" {
			// DSML 过滤器：只下发"确定与伪工具标记无关"的增量。
			if safe := s.dsml.Write(txt); safe != "" {
				s.openText()
				s.event("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": s.openIndex,
					"delta": map[string]any{"type": "text_delta", "text": safe},
				})
			}
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, tci := range tcs {
				tc, _ := tci.(map[string]any)
				if tc == nil {
					continue
				}
				idx := 0
				if v, ok := tc["index"].(float64); ok {
					idx = int(v)
				}
				s.handleToolCallDelta(idx, tc)
			}
		}
		// 旧版 function_call 分片（无 index/id）：按单调用形态并入 index 0。
		if fc, _ := delta["function_call"].(map[string]any); fc != nil {
			s.handleToolCallDelta(0, map[string]any{"function": fc})
		}
	}
}

// handleToolCallDelta 处理某 index 的 tool_call 分片：首片开块（tool_use），
// 后续片把 arguments 以 input_json_delta 续传。
func (s *anthropicStreamWriter) handleToolCallDelta(idx int, tc map[string]any) {
	st := s.toolStates[idx]
	if st == nil || !st.open {
		s.closeOpen()
		if st == nil {
			id, _ := tc["id"].(string)
			if id == "" {
				id = newAnthropicID("toolu_")
			}
			name := ""
			if fn, _ := tc["function"].(map[string]any); fn != nil {
				name, _ = fn["name"].(string)
			}
			st = &anthropicToolState{blockIndex: s.nextIndex, id: id, name: name}
			s.nextIndex++
			s.toolStates[idx] = st
		}
		st.open = true
		s.openKind, s.openIndex = "tool", st.blockIndex
		s.event("content_block_start", map[string]any{
			"type": "content_block_start", "index": st.blockIndex,
			"content_block": map[string]any{
				"type": "tool_use", "id": st.id, "name": st.name, "input": map[string]any{},
			},
		})
	}
	if fn, _ := tc["function"].(map[string]any); fn != nil {
		if args, _ := fn["arguments"].(string); args != "" {
			s.event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": st.blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			})
		}
	}
}

// openText 确保有一个打开的 text 块（必要时先关掉其它打开块）。
func (s *anthropicStreamWriter) openText() {
	if s.openKind == "text" {
		return
	}
	s.closeOpen()
	idx := s.nextIndex
	s.nextIndex++
	s.openKind, s.openIndex = "text", idx
	s.startWith(nil)
	s.event("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// closeOpen 关闭当前打开的 content 块（若有）。
func (s *anthropicStreamWriter) closeOpen() {
	if s.openKind == "" {
		return
	}
	if s.openKind == "tool" {
		for _, st := range s.toolStates {
			if st.blockIndex == s.openIndex {
				st.open = false
			}
		}
	}
	s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.openIndex})
	s.openKind = ""
}

// startWith 发 message_start（幂等）。usageOverride 非空时用作 message.usage
// （静态合成路径已知完整 usage；实时路径先发零值、末尾 message_delta 补齐）。
func (s *anthropicStreamWriter) startWith(usageOverride map[string]any) {
	if s.started {
		return
	}
	s.started = true
	// 不再在此处直接调 ensureSSEHeaders：它必须在 wmu 内执行，紧随其后的
	// s.event("message_start", ...) 会在锁内完成同样的头设置（幂等）。
	usage := usageOverride
	if usage == nil {
		usage = map[string]any{
			"input_tokens": 0, "output_tokens": 0,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		}
	}
	s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.msgID, "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": usage,
		},
	})
}

// endWith 发 message_delta + message_stop（幂等）。reason 为空时按已见
// finish_reason 推定；usage 为空时按已见上游 usage 组装。
func (s *anthropicStreamWriter) endWith(reason string, usage map[string]any) {
	if s.ended {
		return
	}
	s.ended = true
	if !s.started {
		s.startWith(nil)
	}
	// DSML holdback 收尾：放行被扣住的尾巴（吞弃态则丢弃）。必须在 closeOpen 前。
	if s.dsml != nil {
		if rest := s.dsml.Flush(); rest != "" {
			s.openText()
			s.event("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": s.openIndex,
				"delta": map[string]any{"type": "text_delta", "text": rest},
			})
		}
	}
	s.closeOpen()
	if reason == "" {
		if s.finishReason != "" {
			reason = anthropicStopReason(s.finishReason)
		} else {
			reason = "end_turn"
		}
	}
	if usage == nil {
		usage = map[string]any{
			"output_tokens":               intFromAny(s.usage["completion_tokens"]),
			"input_tokens":                intFromAny(s.usage["prompt_tokens"]),
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		}
	}
	s.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": usage,
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})
}

// emitBlocks 静态模式：把已组装的 Anthropic content 块按标准事件序列写出
// （服务端搜索路径的响应合成；实时路径不用本方法）。
func (s *anthropicStreamWriter) emitBlocks(blocks []any) {
	for _, bi := range blocks {
		b, _ := bi.(map[string]any)
		if b == nil {
			continue
		}
		switch b["type"] {
		case "text":
			if t, _ := b["text"].(string); t != "" {
				s.openText()
				s.event("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": s.openIndex,
					"delta": map[string]any{"type": "text_delta", "text": t},
				})
				s.closeOpen()
			}
		case "tool_use", "server_tool_use":
			typ, _ := b["type"].(string)
			id, _ := b["id"].(string)
			name, _ := b["name"].(string)
			if id == "" {
				id = newAnthropicID("toolu_")
			}
			s.startWith(nil)
			s.closeOpen()
			idx := s.nextIndex
			s.nextIndex++
			s.event("content_block_start", map[string]any{
				"type": "content_block_start", "index": idx,
				"content_block": map[string]any{"type": typ, "id": id, "name": name, "input": map[string]any{}},
			})
			if input, err := json.Marshal(b["input"]); err == nil && len(input) > 2 && string(input) != "{}" {
				s.event("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": idx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)},
				})
			}
			s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		case "web_search_tool_result":
			s.startWith(nil)
			s.closeOpen()
			idx := s.nextIndex
			s.nextIndex++
			s.event("content_block_start", map[string]any{
				"type": "content_block_start", "index": idx,
				"content_block": map[string]any{
					"type":        "web_search_tool_result",
					"tool_use_id": b["tool_use_id"],
					"content":     b["content"],
				},
			})
			s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
		}
	}
}

// finish 在 chatCompletions 返回后调用：
//   - 200 流正常收尾（无论 [DONE] 是否到达）：补发 message_delta/stop；
//   - 非 200（上游/分类错误 JSON）：整体转换为 Anthropic 错误体。
func (s *anthropicStreamWriter) finish() {
	s.stopHeartbeat() // writer 生命周期终点：先停心跳再收尾（endWith 还要写帧，走 event 无碍）
	if s.status != 0 && s.status != http.StatusOK {
		s.writeConvertedError()
		return
	}
	if !s.ended && !s.sawErr {
		s.endWith("", nil)
	}
}

// writeConvertedError 非 200 路径：OpenAI 错误 JSON → Anthropic 错误体。
func (s *anthropicStreamWriter) writeConvertedError() {
	var e struct {
		Error struct {
			Message     string `json:"message"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	_ = json.Unmarshal(s.buf, &e)
	msg := e.Error.Message
	if strings.TrimSpace(msg) == "" {
		msg = strings.TrimSpace(string(s.buf))
	}
	if msg == "" {
		msg = "upstream request failed"
	}
	if e.Error.GatewayHint != "" {
		msg += " (gateway_hint: " + e.Error.GatewayHint + ")"
	}
	status := s.status
	if status < 400 {
		status = http.StatusBadGateway
	}
	writeAnthropicError(s.w, status, anthropicErrorType(status), msg)
}

// ensureSSEHeaders 设置 Anthropic SSE 响应头（幂等；静态路径首调，
// 实时路径 StreamHint 已设同名头，这里只补 charset 与缺失项）。
//
// 首帧写出后必须彻底停止改头，理由有两条：
//  1. 语义上：s.w.WriteHeader(200) 已把响应头刷给客户端，此后再 Set 不会生效；
//  2. 并发上：本 writer 同时被当作 ResponseWriter 下传给 chatCompletions，其错误
//     分支走 writeJSON→w.Header().Set，而本函数经心跳 goroutine 在 wmu 内执行。
//     两边写同一个 header map，其中一个不持锁 → Go runtime 直接
//     "fatal error: concurrent map writes"（进程级 abort，不可 recover）。
//     只需保证「头只在首帧前改一次」，就不再存在两个并发写者。
func (s *anthropicStreamWriter) ensureSSEHeaders() {
	if s.headerWritten {
		return
	}
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// event 写一条 Anthropic SSE 事件（event: name + data: json）并 flush。
// 经 wmu 与心跳 goroutine 互斥（客户端写只有这两个来源，原子帧不撕裂）。
func (s *anthropicStreamWriter) event(name string, payload map[string]any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.eventLocked(name, raw)
}

// eventLocked 实际写出（调用方必须持有 s.wmu）。
func (s *anthropicStreamWriter) eventLocked(name string, raw []byte) {
	s.ensureSSEHeaders()
	if !s.headerWritten {
		s.headerWritten = true
		s.w.WriteHeader(http.StatusOK)
	}
	_, _ = io.WriteString(s.w, "event: "+name+"\ndata: "+string(raw)+"\n\n")
	if s.flusher != nil {
		s.flusher.Flush()
	}
	// 首帧已写出（200 头已落）→ 启动空闲心跳（幂等）。
	s.startHeartbeat()
}

// anthropicPingInterval 流式空闲心跳间隔（与 upstream 侧 sseHeartbeatInterval 同语义同值）。
// 包级变量仅供测试缩短间隔。
var anthropicPingInterval = 15 * time.Second

// startHeartbeat 启动空闲心跳：每 15s 发一条官方 ping 事件（Anthropic SDK 规范忽略）。
// 首帧写出后启动（此时 200 头已发，ping 只走数据面）。幂等；finish 时停止。
// 注意上游 OpenAI 帧经 StreamHint 写给本 writer 时若夹带 `: ping` 注释帧会被
// handleFrame 丢弃（不带 data: 前缀，到不了客户端），故心跳必须由本层自己发。
func (s *anthropicStreamWriter) startHeartbeat() {
	if s.hbStarted {
		return
	}
	s.hbStarted = true
	// stop 取局部变量：goroutine 只在启动时抓一次 channel。若每轮 select 重读
	// s.hbStop，stopHeartbeat 把字段置 nil 后这里会永久阻塞在 nil channel 上，
	// 心跳 goroutine 既退不出、又继续按 tick 尝试写已结束的响应（泄漏 + 噪音）。
	stop := make(chan struct{})
	s.hbStop = stop
	go func() {
		t := time.NewTicker(anthropicPingInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				raw, err := json.Marshal(map[string]any{"type": "ping"})
				if err != nil {
					continue
				}
				s.wmu.Lock()
				if !s.ended {
					s.eventLocked("ping", raw)
				}
				s.wmu.Unlock()
			}
		}
	}()
}

// heartbeatSuppressed 实现 upstream.heartbeatSuppressor 约定接口：本层自带官方
// ping 事件心跳，StreamHint 层的 `: ping` 注释帧会被 handleFrame 丢弃——抑制其
// 白跑（消除双层心跳冗余）。
func (s *anthropicStreamWriter) heartbeatSuppressed() bool { return true }

// stopHeartbeat 停止心跳 goroutine（finish 时调用；幂等）。
func (s *anthropicStreamWriter) stopHeartbeat() {
	if s.hbStop != nil {
		close(s.hbStop)
		s.hbStop = nil
	}
}

// intFromAny JSON number/其他 → int（缺省 0）。
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}
