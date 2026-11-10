// OpenAI Responses API 兼容层（POST /v1/responses）。
//
// 目标客户端：Codex CLI 等只认 Responses 协议的客户端。此前本仓只支持
// Chat Completions 与 Anthropic Messages，Codex CLI 这条路径整条走不通——
// 这是与头部同类项目最实质的一处能力缺口。
//
// 实现策略（与 anthropic.go 完全同构，刻意不引入第二套调度逻辑）：
//
//   - **不复制轮转与错误处置**：chatCompletions 是全仓最复杂的函数，Responses
//     适配层通过「转换型 ResponseWriter」把它的输出逐帧改写为 Responses 事件流，
//     行为与 /v1/chat/completions 完全同源。
//   - 请求侧：responses → chat 的字段映射见 responsesToChat；只映射网关真正
//     消费/透传的部分，未识别字段一律不猜（宁缺勿错）。
//   - 响应侧：非流式把 chat.completion 聚合体翻译成 response 对象；流式把
//     OpenAI SSE 的 delta 翻译成 Responses 的具名事件序列
//     （response.created / response.output_text.delta / response.completed ...）。
//
// 已知边界（明确不做，避免承诺过度）：
//   - `previous_response_id`：网关无状态，不保存历史 response。收到该字段时
//     忽略并继续（客户端已在 input 里带全量上下文，这是 Codex CLI 的实际形态）。
//   - 内置工具（web_search_preview / file_search / code_interpreter）：不实现，
//     出站前剥离，模型照常回答（能力降级而非报错）。
//     自定义 function 工具正常透传。
//   - `input` 既支持字符串（单轮）也支持消息数组（多轮）。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ---------------------------------------------------------------------------
// 请求侧类型（Responses API 的小集）
// ---------------------------------------------------------------------------

// responsesRequest POST /v1/responses 的入参（只声明网关消费的字段）。
type responsesRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`        // string 或 message 数组
	Instructions      string          `json:"instructions"` // 等价 system
	MaxOutputTokens   *int            `json:"max_output_tokens"`
	Temperature       *float64        `json:"temperature"`
	TopP              *float64        `json:"top_p"`
	Stream            bool            `json:"stream"`
	Tools             []responsesTool `json:"tools"`
	ToolChoice        json.RawMessage `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
	Reasoning         *struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	// PreviousResponseID 显式声明但不使用：网关无状态，忽略即可（见文件头）。
	PreviousResponseID string         `json:"previous_response_id"`
	Metadata           map[string]any `json:"metadata"`
	User               string         `json:"user"`
}

// responsesTool Responses 的 tools[] 条目。
//
// Responses 与 Chat 的工具形状不同：Responses 是扁平对象
// {"type":"function","name":..,"parameters":..}，Chat 是
// {"type":"function","function":{...}} 的嵌套。这里两种都收，映射时统一。
type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// Chat 嵌套形态兼容（部分客户端混用）
	Function *struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ---------------------------------------------------------------------------
// Responses → Chat 映射
// ---------------------------------------------------------------------------

// responsesToChat 把 Responses 请求翻译成 chatCompletions 能吃的 body。
//
// 返回的 body 直接交给 chatCompletions（与 anthropicToOpenAI 同契约）：
// 只放 Chat Completions 认识的键，多一个都可能被上游判 11101。
func responsesToChat(req *responsesRequest) (map[string]any, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("model: field required")
	}

	msgs, err := responsesInputToMessages(req.Input, req.Instructions)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input: at least one message is required")
	}

	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	// max_output_tokens → max_tokens。Responses 允许缺省，此时不设键
	// （上游按自身默认走）。
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		body["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.Stream {
		body["stream"] = true
	}
	if req.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *req.ParallelToolCalls
	}

	// tools：只保留自定义 function（内置工具剥离，见文件头）。
	if tools := responsesToolsToChat(req.Tools); len(tools) > 0 {
		body["tools"] = tools
		if tc := normalizeResponsesToolChoice(req.ToolChoice); tc != nil {
			body["tool_choice"] = tc
		}
	}

	// reasoning.effort → 上游思考档位（本仓既有键名 reasoning_effort）。
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		body["reasoning_effort"] = req.Reasoning.Effort
	}

	return body, nil
}

// responsesInputToMessages 把 Responses 的 input 归一成 chat messages。
//
// 三种形态：
//  1. input 是字符串 → 单条 user；
//  2. input 是 [{role, content}] 数组 → 逐条映射（content 支持字符串与内容块数组）；
//  3. instructions 非空 → 作为首条 system 前置。
func responsesInputToMessages(raw json.RawMessage, instructions string) ([]map[string]any, error) {
	var msgs []map[string]any
	if instructions != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": instructions})
	}
	if len(raw) == 0 {
		return msgs, nil
	}

	// 形态 1：纯字符串
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return append(msgs, map[string]any{"role": "user", "content": s}), nil
	}

	// 形态 2：消息数组
	var arr []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		// Responses 的 function_call / function_call_output 历史项（Codex CLI 多轮工具
		// 会话会带），映射成 chat 的 tool_calls / tool 消息。
		Type      string `json:"type"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Output    string `json:"output"`
	}
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("input: must be a string or an array of messages")
	}

	for _, item := range arr {
		switch item.Type {
		case "function_call":
			// 折叠成 assistant.tool_calls 的一条
			msgs = append(msgs, map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"id":   item.CallID,
					"type": "function",
					"function": map[string]any{
						"name":      item.Name,
						"arguments": item.Arguments,
					},
				}},
			})
			continue
		case "function_call_output":
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": item.CallID,
				"content":      item.Output,
			})
			continue
		}

		role := item.Role
		if role == "" {
			role = "user"
		}
		content, err := responsesContentToChat(item.Content)
		if err != nil {
			return nil, err
		}
		if content == nil {
			continue // 空内容跳过（不发空消息给上游）
		}
		msgs = append(msgs, map[string]any{"role": role, "content": content})
	}
	return msgs, nil
}

// responsesContentToChat 把 Responses 的 content 转成 Chat 的 content。
//
// 形态：纯字符串 → 原样；内容块数组 → 只保留 text 块拼成的 Chat 内容块数组
// （input_text → text；input_image 保留 image_url 形态）。其余块类型忽略。
func responsesContentToChat(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return s, nil
	}

	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, fmt.Errorf("input content: unsupported shape")
	}
	if len(blocks) == 0 {
		return nil, nil
	}

	// 纯文本块可以折叠成一个字符串（更省 token，且上游对字符串形态最稳）。
	allText := true
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type != "input_text" && b.Type != "text" && b.Type != "output_text" {
			allText = false
			break
		}
		sb.WriteString(b.Text)
	}
	if allText {
		if sb.Len() == 0 {
			return nil, nil
		}
		return sb.String(), nil
	}

	// 含图片：走 Chat 的多段内容块形态。
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "input_text", "text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		case "input_image":
			if b.ImageURL != "" {
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": b.ImageURL},
				})
			}
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// responsesToolsToChat 把 Responses 工具数组翻译成 Chat 的 tools 形态。
//
// 只保留自定义 function：内置工具（web_search_preview / file_search /
// code_interpreter / computer_use_preview 等）网关不实现，剥离后模型照常回答
// （能力降级而非报错）。
func responsesToolsToChat(tools []responsesTool) []map[string]any {
	var out []map[string]any
	for _, t := range tools {
		name, desc, params := t.Name, t.Description, t.Parameters
		if t.Function != nil { // 嵌套形态兼容
			if name == "" {
				name = t.Function.Name
			}
			if desc == "" {
				desc = t.Function.Description
			}
			if len(params) == 0 {
				params = t.Function.Parameters
			}
		}
		if name == "" {
			continue // 内置工具（无 name）或畸形条目 → 剥离
		}
		fn := map[string]any{"name": name}
		if desc != "" {
			fn["description"] = desc
		}
		if len(params) > 0 {
			var schema any
			if err := json.Unmarshal(params, &schema); err == nil {
				fn["parameters"] = schema
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// normalizeResponsesToolChoice 把 Responses 的 tool_choice 归一成 Chat 形态。
//
// Responses："auto" / "required" / "none" / {"type":"function","name":"x"}
// Chat：      "auto" / "required" / "none" / {"type":"function","function":{"name":"x"}}
func normalizeResponsesToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "required", "none":
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Name != "" {
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": obj.Name},
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP 入口
// ---------------------------------------------------------------------------

// responses POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(requestBody(w, r, h.cfg.MaxBodyBytes))
	if err != nil {
		if isBodyTooLarge(err) {
			logx.Warnf("[server] reject oversized body: path=%s limit=%dMB ip=%s", r.URL.Path, h.cfg.MaxBodyBytes>>20, upstream.ExtractClientIP(r))
			writeResponsesError(w, http.StatusRequestEntityTooLarge, "request_too_large", h.bodyTooLargeMsg())
			return
		}
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	body, err := responsesToChat(&req)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if req.Stream {
		tr := newResponsesStreamWriter(w, req.Model)
		h.delegateChat(r, body, tr)
		tr.finish()
		return
	}

	pr := h.runOpenAIPass(r.Context(), r, body)
	writeResponsesFromPass(w, req.Model, pr)
}
