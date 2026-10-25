// Anthropic Messages API 兼容层（/v1/messages）。
//
// 目标客户端：走 Anthropic 协议的工具——ZCode 的原生 WebSearch 内部请求
// （web_search_20260209 服务端工具）、Claude Code 等。实现策略：
//
//	Anthropic 请求 → 内部 OpenAI chat body → 复用既有 chatCompletions 管线
//	（挑号 / 轮转 / 超时与 WAF 处置 / 提示词改写 / 记账）→ 响应翻译回
//	Anthropic 形状。
//
// 关键取舍（与既有实现的关系）：
//   - 不复制轮转与错误处置：chatCompletions 是全仓最复杂的函数（1475 行、
//     十来条错误路径），适配层通过"转换型 ResponseWriter"把它的输出逐帧
//     改写为 Anthropic 事件流，行为完全同源，不产生第二套调度逻辑。
//   - 服务端工具（web_search_*）：Anthropic 语义是"服务端执行检索"，上游
//     模型供应商无法执行。网关自己执行检索（internal/websearch）并按
//     server_tool_use / web_search_tool_result 块合成（见
//     anthropic_websearch.go）。
//   - 非 web_search 的其它服务端工具（web_fetch_* / code_execution_* 等）：
//     网关不实现，出站前静默剥离，模型照常回答（能力降级而非报错）。
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 请求侧类型（Anthropic Messages API 的子集）
// ---------------------------------------------------------------------------

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system"` // string 或 [{type:"text",...}]
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     *int               `json:"max_tokens"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    json.RawMessage    `json:"tool_choice"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string 或 []block
}

// anthropicTool tools 数组条目。自定义工具（Type 空 / "custom"）用
// Name/Description/InputSchema；服务端工具（web_search_* 等）用 Type +
// MaxUses/AllowedDomains/BlockedDomains。
type anthropicTool struct {
	Type           string          `json:"type"`
	Name           string          `json:"name"`
	Description    string          `json:"description"`
	InputSchema    json.RawMessage `json:"input_schema"`
	MaxUses        int             `json:"max_uses"`
	AllowedDomains []string        `json:"allowed_domains"`
	BlockedDomains []string        `json:"blocked_domains"`
}

// anthropicContentBlock 消息内容块（只声明网关消费的子集；未列字段忽略）。
type anthropicContentBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text"`
	Source    *anthropicImageSource `json:"source"`
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	Input     json.RawMessage       `json:"input"`
	ToolUseID string                `json:"tool_use_id"`
	Content   json.RawMessage       `json:"content"`
	IsError   bool                  `json:"is_error"`
}

type anthropicImageSource struct {
	Type      string `json:"type"` // "base64" / "url"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// ---------------------------------------------------------------------------
// Anthropic → 内部 OpenAI 映射
// ---------------------------------------------------------------------------

// isServerToolType 判定 tools[] 条目是否为「服务端工具」。
// 判据：Type 非空且非 "custom" 即为服务端工具——Anthropic 自定义工具是
// Type==""（旧形态）或 Type=="custom"（新形态）；其余（web_search_20250305、
// web_fetch_20250910、code_execution_* 等）都是服务端执行的工具，上游 OpenAI
// 协议没有对应形态，必须从出站 tools 剥离。
func isServerToolType(t string) bool { return t != "" && t != "custom" }

// anthropicToOpenAI 把 Anthropic 请求翻译成内部 OpenAI chat body。
// 返回：(body, 服务端工具列表, error)。服务端工具由调用方决定处理策略
// （web_search → 网关执行；其余 → 忽略）。
func anthropicToOpenAI(req *anthropicRequest) (map[string]any, []anthropicTool, error) {
	out := make([]any, 0, len(req.Messages)+1)
	if sys := systemText(req.System); sys != "" {
		out = append(out, map[string]any{"role": "system", "content": sys})
	}
	for i, m := range req.Messages {
		msgs, err := convertAnthropicMessage(m)
		if err != nil {
			return nil, nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out = append(out, msgs...)
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": out,
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		// 上游只认 max_tokens（见 upstream/payload.go 的别名翻译注释）。
		body["max_tokens"] = *req.MaxTokens
	}
	if req.Stream {
		body["stream"] = true
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		body["stop"] = req.StopSequences
	}

	var serverTools []anthropicTool
	fns := make([]any, 0, len(req.Tools))
	for _, t := range req.Tools {
		if isServerToolType(t.Type) {
			serverTools = append(serverTools, t)
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		params := json.RawMessage(t.InputSchema)
		if len(params) == 0 || string(params) == "null" {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		fn["parameters"] = params
		fns = append(fns, map[string]any{"type": "function", "function": fn})
	}
	if len(fns) > 0 {
		body["tools"] = fns
	}
	if tc := convertAnthropicToolChoice(req.ToolChoice); tc != nil {
		body["tool_choice"] = tc
	}
	return body, serverTools, nil
}

// systemText 归一 system 字段（string 或 text 块数组 → 纯文本，多块换行拼接）。
func systemText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return str
		}
		return ""
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// convertAnthropicMessage 单条 Anthropic 消息 → 若干条 OpenAI 消息。
// 一条 Anthropic user 消息可能同时含 tool_result 块与文本块：tool_result
// 拆成独立 role:"tool" 消息（OpenAI 协议要求），文本/图片保持 user 消息，
// 保持块出现顺序；assistant 的 tool_use → tool_calls，与文本合并为一条。
func convertAnthropicMessage(m anthropicMessage) ([]any, error) {
	role := strings.TrimSpace(m.Role)
	switch role {
	case "user", "assistant":
	case "system":
		// Anthropic 的「对话中途 system」形态（role=system 而非顶层 system 字段）：
		// Claude Code / ZCode 等新客户端用它承载中途注入的系统提醒。上游 OpenAI
		// 协议容忍 messages 中段的 system，透传并保持位置，既不合并也不重排。
		if sys := systemText(m.Content); sys != "" {
			return []any{map[string]any{"role": "system", "content": sys}}, nil
		}
		// 空 system 不转发：上游对空 content 的 system 可能拒收，且无语义。
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported role %q", role)
	}
	raw := strings.TrimSpace(string(m.Content))
	if raw == "" || raw == "null" {
		return []any{map[string]any{"role": role, "content": ""}}, nil
	}
	if strings.HasPrefix(raw, `"`) {
		var s string
		if err := json.Unmarshal(m.Content, &s); err != nil {
			return nil, fmt.Errorf("invalid string content: %w", err)
		}
		return []any{map[string]any{"role": role, "content": s}}, nil
	}
	if !strings.HasPrefix(raw, "[") {
		return nil, fmt.Errorf("content must be a string or an array of blocks")
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("invalid content blocks: %w", err)
	}

	var out []any
	var parts []any // user/assistant 的 text/image 部件（OpenAI content parts）
	var toolCalls []any
	flushParts := func() {
		if len(parts) == 0 {
			return
		}
		out = append(out, map[string]any{"role": role, "content": partsToContent(parts)})
		parts = nil
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": b.Text})
			}
		case "image":
			if u := anthropicImageURL(b.Source); u != "" {
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
			}
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 && strings.TrimSpace(string(b.Input)) != "null" {
				args = string(b.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": b.ID, "type": "function",
				"function": map[string]any{"name": b.Name, "arguments": args},
			})
		case "tool_result":
			flushParts()
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      toolResultText(b.Content),
			})
		default:
			// thinking / redacted_thinking / server_tool_use / web_search_tool_result /
			// document 等：无 OpenAI 对应形态，静默丢弃。服务端工具的中间块由网关
			// 自产（客户端把历史原样回放时也不会再进模型上下文）。
		}
	}

	if role == "assistant" {
		msg := map[string]any{"role": "assistant", "content": partsToContent(parts)}
		if len(toolCalls) > 0 {
			msg["tool_calls"] = toolCalls
		}
		out = append(out, msg)
	} else {
		flushParts()
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"role": role, "content": ""})
	}
	return out, nil
}

// partsToContent OpenAI content 形态收敛：单 text 部件 → 纯字符串（上游兼容性
// 最好）；其余保持 parts 数组。
func partsToContent(parts []any) any {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		if p, ok := parts[0].(map[string]any); ok && p["type"] == "text" {
			return p["text"]
		}
	}
	return parts
}

// toolResultText tool_result 的 content（string 或 text 块数组）→ 纯文本。
func toolResultText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			return str
		}
		return ""
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw) // 无法解析的形态原样透传，宁缺勿造
	}
	var parts []string
	for _, b := range blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicImageURL image block → OpenAI image_url 值（base64 → data URI）。
func anthropicImageURL(src *anthropicImageSource) string {
	if src == nil {
		return ""
	}
	switch src.Type {
	case "base64":
		if src.Data == "" || src.MediaType == "" {
			return ""
		}
		return "data:" + src.MediaType + ";base64," + src.Data
	case "url":
		return src.URL
	}
	return ""
}

// convertAnthropicToolChoice → OpenAI tool_choice：
// auto→"auto"；any→"required"；tool→{type:function,function:{name}}；none→"none"。
// 缺省/未知 → nil（不写字段，上游按默认 auto 处理）。
func convertAnthropicToolChoice(raw json.RawMessage) any {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if tc.Name == "" {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 处理器
// ---------------------------------------------------------------------------

// messagesAnthropic POST /v1/messages 入口。
func (h *Handler) messagesAnthropic(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(requestBody(w, r, h.cfg.MaxBodyBytes))
	if err != nil {
		if isBodyTooLarge(err) {
			log.Printf("WARN: [server] reject oversized body: path=%s limit=%dMB ip=%s", r.URL.Path, h.cfg.MaxBodyBytes>>20, upstream.ExtractClientIP(r))
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large", h.bodyTooLargeMsg())
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model: field required")
		return
	}
	if req.MaxTokens == nil || *req.MaxTokens <= 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "max_tokens: must be a positive integer")
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages: at least one message is required")
		return
	}

	body, serverTools, err := anthropicToOpenAI(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// 服务端 web_search：编排检索 + 块合成（多 pass），与普通单 pass 分开走。
	if ws := pickWebSearchTool(serverTools); ws != nil && h.cfg.Search != nil {
		h.serveMessagesWithSearch(w, r, &req, body, ws)
		return
	}

	if req.Stream {
		// 流式：转换型 ResponseWriter 作为 chatCompletions 的输出目标，
		// OpenAI SSE 帧逐帧翻译成 Anthropic 事件序列。
		tr := newAnthropicStreamWriter(w, req.Model)
		h.delegateChat(r, body, tr)
		tr.finish()
		return
	}

	pr := h.runOpenAIPass(r.Context(), r, body)
	writeAnthropicFromPass(w, req.Model, pr)
}

// countTokensAnthropic POST /v1/messages/count_tokens。
// 估算实现（无 tokenizer）：按请求 JSON 字节数 / 3 折算——对中文（3 字节/字）
// 接近 1 token/字，对英文偏保守（约 4 字节/token，高估 ~30%）。仅用于客户端
// 上下文管理的近似判断，不承诺精确值。
func (h *Handler) countTokensAnthropic(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(requestBody(w, r, h.cfg.MaxBodyBytes))
	if err != nil {
		if isBodyTooLarge(err) {
			log.Printf("WARN: [server] reject oversized body: path=%s limit=%dMB ip=%s", r.URL.Path, h.cfg.MaxBodyBytes>>20, upstream.ExtractClientIP(r))
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large", h.bodyTooLargeMsg())
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req struct {
		Model    string          `json:"model"`
		System   json.RawMessage `json:"system"`
		Messages json.RawMessage `json:"messages"`
		Tools    json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	n := len(req.System) + len(req.Messages) + len(req.Tools)
	tokens := (n + 2) / 3
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokens})
}

// delegateChat 以给定 OpenAI body 调一次既有 chatCompletions 管线，输出写到 w。
// w 可以是转换型 writer（流式翻译）或 captureWriter（聚合回收）。
func (h *Handler) delegateChat(r *http.Request, body map[string]any, w http.ResponseWriter) {
	raw, err := json.Marshal(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "marshal body: "+err.Error())
		return
	}
	sub := r.Clone(r.Context())
	sub.Method = http.MethodPost
	sub.Body = io.NopCloser(bytes.NewReader(raw))
	sub.ContentLength = int64(len(raw))
	h.chatCompletions(w, sub)
}

// anthropicPassResult 一次内部 pass 的聚合产物（captureWriter 回收）。
type anthropicPassResult struct {
	status  int
	resp    map[string]any // status==200 时的 OpenAI chat.completion
	errMsg  string         // 失败文案（上游原文优先）
	errHint string         // gateway_hint（有则附在 Anthropic error message 尾部）
}

// runOpenAIPass 聚合执行一次内部 pass（强制非流式；多 pass 编排与单 pass
// 非流式客户端都走这里）。
func (h *Handler) runOpenAIPass(ctx context.Context, r *http.Request, body map[string]any) *anthropicPassResult {
	body["stream"] = false
	cw := &captureWriter{}
	h.delegateChat(r.WithContext(ctx), body, cw)
	res := &anthropicPassResult{status: cw.statusOrOK()}
	if res.status == http.StatusOK {
		_ = json.Unmarshal(cw.buf, &res.resp)
		if res.resp == nil {
			res.status = http.StatusBadGateway
			res.errMsg = "aggregated upstream response is not valid JSON"
		}
		return res
	}
	var e struct {
		Error struct {
			Message     string `json:"message"`
			Code        string `json:"code"`
			GatewayHint string `json:"gateway_hint"`
		} `json:"error"`
	}
	if json.Unmarshal(cw.buf, &e) == nil {
		res.errMsg = e.Error.Message
		res.errHint = e.Error.GatewayHint
	}
	if strings.TrimSpace(res.errMsg) == "" {
		res.errMsg = strings.TrimSpace(string(cw.buf))
	}
	if strings.TrimSpace(res.errMsg) == "" {
		res.errMsg = "upstream request failed"
	}
	return res
}

// captureWriter 捕获型 ResponseWriter：把 chatCompletions 的写出全部收进内存，
// 供多 pass 编排与响应格式转换使用（不改动 chatCompletions 本身）。
type captureWriter struct {
	hdr    http.Header
	status int
	buf    []byte
}

func (c *captureWriter) Header() http.Header {
	if c.hdr == nil {
		c.hdr = http.Header{}
	}
	return c.hdr
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *captureWriter) Write(p []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

func (c *captureWriter) statusOrOK() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

// ---------------------------------------------------------------------------
// 错误与 id 工具
// ---------------------------------------------------------------------------

// writeAnthropicError 写 Anthropic 标准错误体。
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": msg,
		},
	})
}

// writeAnthropicPassError 内部 pass 失败 → Anthropic 错误体（状态码原样映射；
// gateway_hint 附在 message 尾部——Anthropic error 没有并列扩展字段）。
func writeAnthropicPassError(w http.ResponseWriter, pr *anthropicPassResult) {
	msg := pr.errMsg
	if msg == "" {
		msg = "upstream request failed"
	}
	if pr.errHint != "" {
		msg += " (gateway_hint: " + pr.errHint + ")"
	}
	status := pr.status
	if status < 400 {
		status = http.StatusBadGateway
	}
	writeAnthropicError(w, status, anthropicErrorType(status), msg)
}

// anthropicErrorType HTTP 状态 → Anthropic error.type。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}

// anthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func anthropicStopReason(finishReason string) string {
	switch finishReason {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// newAnthropicID 生成 Anthropic 形态 id（msg_xxx / srvtoolu_xxx / toolu_xxx）。
func newAnthropicID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败属系统级异常；退化为固定后缀不阻断请求（id 只作关联）。
		return prefix + "000000000000000000000000"
	}
	return prefix + hex.EncodeToString(b[:])
}
