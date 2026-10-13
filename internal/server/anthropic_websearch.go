// /v1/messages 服务端 web_search 工具的编排与 Anthropic 块合成。
//
// Anthropic 语义：web_search 是「服务端工具」——模型决定搜什么（server_tool_use
// 块），服务端执行检索，把 web_search_tool_result 块回灌模型，模型基于结果作答；
// 一轮答复内可多轮检索（上限 max_uses，缺省 8）。上游模型供应商不实现任何
// 服务端工具，所以这一整套由网关承担：
//
//	循环（每轮一次聚合 pass，恒带注入的 web_search 函数工具）：
//	  模型要搜索？ ── 否 ──→ 终局（作答）
//	                └─ 是 ──→ 网关执行检索（internal/websearch）
//	                          → assistant.tool_calls + 各 tool 结果回灌历史
//	                          → 预算耗尽时：撤掉工具 + 追加限额提示 → 下一轮作答
//
// 为什么预算耗尽要「撤工具 + 提示」而不是原地静默：模型想调用一个不在 tools
// 里的工具时，deepseek 系模型会把调用语法当正文吐出来（DSML 泄漏，见 dsml.go
// 的实测记录）——保留工具到最后一刻、用显式提示收束，是这一泄漏的主要防线；
// 清洗器只是兜底。
//
// 与 Claude Code 类客户端的边界：某轮里模型若同时调用了其它（客户端）工具，
// 该轮检索照常执行（模型想要的信息），但立即终局——把其它工具以 tool_use 块
// 交给客户端执行，不把网关内部搜索循环与客户端工具循环混跑。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/websearch"
)

// searchFnName 注入到上游请求里的函数工具名（OpenAI function 协议）。
const searchFnName = "web_search"

// errSearchLimit 检索预算耗尽（对模型回执用，不对外报错）。
var errSearchLimit = errors.New("max_uses limit reached")

// webSearchTool 请求里 web_search 服务端工具的网关侧执行参数。
type webSearchTool struct {
	maxUses int // 单轮最多执行的检索次数（钳制 1..8，缺省 8，对齐 Anthropic 默认）
}

// pickWebSearchTool 从服务端工具列表取 web_search（没有则 nil）。
func pickWebSearchTool(serverTools []anthropicTool) *webSearchTool {
	for _, t := range serverTools {
		if strings.HasPrefix(t.Type, "web_search") {
			mu := t.MaxUses
			if mu <= 0 {
				mu = 8
			}
			if mu > 8 {
				mu = 8
			}
			return &webSearchTool{maxUses: mu}
		}
	}
	return nil
}

// chatToolCall 聚合响应里的工具调用（OpenAI tool_calls 条目）。
type chatToolCall struct {
	ID   string
	Name string
	Args string
}

func extractChatToolCalls(resp map[string]any) []chatToolCall {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	if msg == nil {
		return nil
	}
	tcs, _ := msg["tool_calls"].([]any)
	var out []chatToolCall
	for _, tci := range tcs {
		tc, _ := tci.(map[string]any)
		if tc == nil {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		id, _ := tc["id"].(string)
		name, _ := fn["name"].(string)
		args, _ := fn["arguments"].(string)
		out = append(out, chatToolCall{ID: id, Name: name, Args: args})
	}
	return out
}

// searchFnToolDef 注入给上游模型的 web_search 函数工具定义。
func searchFnToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        searchFnName,
			"description": "Search the web for real-time or factual information. Returns result blocks with titles, URLs and snippets.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "The search query"},
				},
				"required": []string{"query"},
			},
		},
	}
}

// serveMessagesWithSearch web_search 服务端工具的完整编排（见文件头注释）。
func (h *Handler) serveMessagesWithSearch(w http.ResponseWriter, r *http.Request, req *anthropicRequest, body map[string]any, ws *webSearchTool) {
	ctx := r.Context()

	// 注入搜索函数工具（模型据此决定搜什么/是否再搜）。
	if cur, ok := body["tools"].([]any); ok {
		body["tools"] = append(cur, searchFnToolDef())
	} else {
		body["tools"] = []any{searchFnToolDef()}
	}

	var prelude []any // 全部轮次的检索块（server_tool_use / web_search_tool_result 对）
	var allResults []websearch.Result
	attempts, executed := 0, 0 // attempts=已发起的检索（含失败），预算 = ws.maxUses
	inputTokens, outputTokens := 0, 0
	limitNotified := false
	var final *anthropicPassResult

	// 轮次上限：预算耗尽后还会补跑一轮「限额提示 + 作答」，另留余量防异常循环。
	maxRounds := ws.maxUses + 2
	for round := 0; round < maxRounds; round++ {
		pr := h.runOpenAIPass(ctx, r, body)
		if pr.status != http.StatusOK || pr.resp == nil {
			writeAnthropicPassError(w, pr)
			return
		}
		inputTokens += intFromAny(usageOf(pr.resp)["prompt_tokens"])
		outputTokens += intFromAny(usageOf(pr.resp)["completion_tokens"])

		var searchCalls, otherCalls []chatToolCall
		for _, c := range extractChatToolCalls(pr.resp) {
			if c.Name == searchFnName {
				searchCalls = append(searchCalls, c)
			} else {
				otherCalls = append(otherCalls, c)
			}
		}
		// 终局条件：模型不再要搜索 / 还要客户端工具（交客户端）/ 轮次上限。
		if len(searchCalls) == 0 || len(otherCalls) > 0 || round == maxRounds-1 {
			final = pr
			break
		}

		// 执行本轮检索（预算内）；超预算的调用回显限额消息——不能置之不理，
		// OpenAI 历史里的孤儿 tool_calls 会让上游 400。
		var toolMsgs []any
		roundExecuted, limitSkips := 0, 0
		for _, c := range searchCalls {
			if attempts >= ws.maxUses {
				limitSkips++
				toolMsgs = append(toolMsgs, toolMsg(c.ID,
					"web search skipped: max_uses limit reached; answer with the results you already have"))
				continue
			}
			query := toolCallQuery(c)
			attempts++
			suID := newAnthropicID("srvtoolu_")
			prelude = append(prelude, serverToolUseBlock(suID, query))
			results, serr := h.searchOnce(ctx, query)
			if serr != nil {
				prelude = append(prelude, webSearchErrorBlock(suID))
				toolMsgs = append(toolMsgs, toolMsg(c.ID, "web search failed: "+serr.Error()))
				continue
			}
			executed++
			roundExecuted++
			allResults = append(allResults, results...)
			prelude = append(prelude, webSearchResultBlock(suID, results))
			toolMsgs = append(toolMsgs, toolMsg(c.ID, resultsForModel(results)))
		}
		// 历史回灌：assistant（携带 tool_calls）+ 各 tool 结果，保持 OpenAI 配对。
		appendAssistantToolExchange(body, assistantTextOf(pr.resp), searchCalls, toolMsgs)

		// 预算用尽：撤掉搜索工具并显式提示——既省一轮空转，也是 DSML 泄漏
		// （模型对"想要但不存在的工具"会吐调用语法）的主要防线。
		if attempts >= ws.maxUses && !limitNotified {
			removeSearchFnTool(body)
			appendSystemNote(body, "Web search request limit reached. Answer the user with the results you already have, and do not attempt further web searches.")
			limitNotified = true
		}
		// 本轮全部失败（检索后端异常）且非限额跳过：继续会同样失败，终局让模型
		// 基于错误回执作答。
		if roundExecuted == 0 && limitSkips == 0 {
			final = pr
			break
		}
	}
	if final == nil {
		// 防御：循环未产出终局（round 上限保证不应发生）。
		final = &anthropicPassResult{status: http.StatusBadGateway, errMsg: "web search orchestration produced no final pass"}
	}
	if final.status != http.StatusOK || final.resp == nil {
		writeAnthropicPassError(w, final)
		return
	}

	// 终局块：检索块 + 模型最终答复（本轮其它工具调用照常透出为 tool_use，
	// stop_reason 已由 chatRespBlocks 映射为 tool_use）。
	blocks, stopReason, text := chatRespBlocks(final.resp, searchFnName)
	finalBlocks := append(prelude, blocks...)

	// 来源链接兜底：正文完全没引用检索结果 URL 时补 "Sources:" 列表——
	// ZCode 的 WebSearch 工具从正文 Markdown 链接提取来源，没有链接等于白搜。
	// 模型已自行引用（含任意一条 URL）则不动它的表述。
	if executed > 0 {
		if merged := ensureSourceLinks(text, allResults); merged != text {
			finalBlocks = replaceTextBlocks(finalBlocks, merged)
		}
	}

	usage := map[string]any{
		"input_tokens":                inputTokens,
		"output_tokens":               outputTokens,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     0,
	}
	if executed > 0 {
		usage["server_tool_use"] = map[string]any{"web_search_requests": executed}
	}

	if req.Stream {
		tr := newAnthropicStreamWriter(w, req.Model)
		tr.startWith(map[string]any{
			"input_tokens": inputTokens, "output_tokens": 0,
			"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
		})
		tr.emitBlocks(finalBlocks)
		tr.endWith(stopReason, usage)
		return
	}
	writeAnthropicMessage(w, req.Model, finalBlocks, stopReason, usage)
}

// searchOnce 执行单次检索（按配置加超时与结果条数上限）。
func (h *Handler) searchOnce(ctx context.Context, query string) ([]websearch.Result, error) {
	if h.cfg.Search == nil {
		return nil, fmt.Errorf("web search backend is not configured")
	}
	max := h.cfg.SearchMaxResults
	if max <= 0 {
		max = 5
	}
	timeout := h.cfg.SearchTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return h.cfg.Search.Search(sctx, query, max)
}

// toolCallQuery 从工具调用参数取 query（非法 JSON 时退回原始参数文本）。
func toolCallQuery(c chatToolCall) string {
	var a struct {
		Query string `json:"query"`
	}
	if json.Unmarshal([]byte(c.Args), &a) == nil && strings.TrimSpace(a.Query) != "" {
		return strings.TrimSpace(a.Query)
	}
	return strings.TrimSpace(c.Args)
}

// serverToolUseBlock Anthropic server_tool_use 块（模型检索决策的呈现）。
func serverToolUseBlock(id, query string) map[string]any {
	return map[string]any{
		"type": "server_tool_use", "id": id, "name": searchFnName,
		"input": map[string]any{"query": query},
	}
}

// webSearchErrorBlock 检索失败的 tool_result（客户端 schema 允许的 error 形态）。
func webSearchErrorBlock(toolUseID string) map[string]any {
	return map[string]any{
		"type": "web_search_tool_result", "tool_use_id": toolUseID,
		"content": map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"},
	}
}

// webSearchResultBlock 检索成功的 tool_result（条目见 webSearchResultEntries）。
func webSearchResultBlock(toolUseID string, results []websearch.Result) map[string]any {
	return map[string]any{
		"type": "web_search_tool_result", "tool_use_id": toolUseID,
		"content": webSearchResultEntries(results),
	}
}

// toolMsg OpenAI role:"tool" 消息（检索结果/错误回灌模型）。
func toolMsg(toolCallID, content string) map[string]any {
	return map[string]any{"role": "tool", "tool_call_id": toolCallID, "content": content}
}

// appendAssistantToolExchange 追加一轮「assistant(tool_calls) + tool 结果」到
// 出站 messages（OpenAI 配对要求：assistant 在前、结果随后）。
func appendAssistantToolExchange(body map[string]any, text string, calls []chatToolCall, toolMsgs []any) {
	base, _ := body["messages"].([]any)
	base = append(base, map[string]any{
		"role": "assistant", "content": text, "tool_calls": callsToOpenAI(calls),
	})
	base = append(base, toolMsgs...)
	body["messages"] = base
}

// appendSystemNote 追加一条系统语义的 user 消息（不依赖上游对中途 system 的
// 支持——部分模型声明 supportsMidConversationSystem=false）。
func appendSystemNote(body map[string]any, note string) {
	base, _ := body["messages"].([]any)
	base = append(base, map[string]any{
		"role":    "user",
		"content": "(System notice: " + note + ")",
	})
	body["messages"] = base
}

// callsToOpenAI 工具调用 → OpenAI assistant.tool_calls（回灌历史用）。
func callsToOpenAI(calls []chatToolCall) []any {
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		args := c.Args
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, map[string]any{
			"id": c.ID, "type": "function",
			"function": map[string]any{"name": c.Name, "arguments": args},
		})
	}
	return out
}

// assistantTextOf OpenAI assistant 消息的正文（无则空串）。
func assistantTextOf(resp map[string]any) string {
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c0, _ := choices[0].(map[string]any)
	msg, _ := c0["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	s, _ := msg["content"].(string)
	return s
}

// removeSearchFnTool 从出站 tools 里剔除注入的搜索函数工具（预算耗尽后调用）。
func removeSearchFnTool(body map[string]any) {
	tools, _ := body["tools"].([]any)
	kept := make([]any, 0, len(tools))
	for _, ti := range tools {
		t, _ := ti.(map[string]any)
		if t != nil {
			if fn, _ := t["function"].(map[string]any); fn != nil {
				if name, _ := fn["name"].(string); name == searchFnName {
					continue
				}
			}
		}
		kept = append(kept, ti)
	}
	if len(kept) == 0 {
		delete(body, "tools")
	} else {
		body["tools"] = kept
	}
}

// webSearchResultEntries 检索结果 → Anthropic web_search_tool_result.content 条目。
// encrypted_content 必填（客户端 schema 要求 string）：网关没有真正的加密载荷
// （那是 Anthropic 服务端模型的内部凭据），用摘要填充——对客户端是无害透传字段。
func webSearchResultEntries(results []websearch.Result) []any {
	out := make([]any, 0, len(results))
	for _, r := range results {
		e := map[string]any{
			"type":              "web_search_result",
			"title":             r.Title,
			"url":               r.URL,
			"encrypted_content": r.Snippet,
		}
		if r.PageAge != "" {
			e["page_age"] = r.PageAge
		}
		out = append(out, e)
	}
	return out
}

// resultsForModel 检索结果 → 回灌模型的 tool 消息内容（紧凑 JSON）。
func resultsForModel(results []websearch.Result) string {
	items := make([]map[string]any, 0, len(results))
	for _, r := range results {
		it := map[string]any{"title": r.Title, "url": r.URL}
		if r.Snippet != "" {
			it["snippet"] = r.Snippet
		}
		if r.PageAge != "" {
			it["date"] = r.PageAge
		}
		items = append(items, it)
	}
	raw, _ := json.Marshal(items)
	return string(raw)
}

// ensureSourceLinks 兜底补来源列表：正文不含任何结果 URL 时追加 "Sources:"。
// 已引用（哪怕一条）则尊重模型自己的表述，不追加。
func ensureSourceLinks(text string, results []websearch.Result) string {
	if len(results) == 0 {
		return text
	}
	for _, r := range results {
		if r.URL != "" && strings.Contains(text, r.URL) {
			return text
		}
	}
	var b strings.Builder
	b.WriteString(text)
	if strings.TrimSpace(text) != "" {
		b.WriteString("\n\n")
	}
	b.WriteString("Sources:")
	seen := map[string]bool{}
	for _, r := range results {
		if r.URL == "" || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		b.WriteString("\n- [" + r.Title + "](" + r.URL + ")")
	}
	return b.String()
}

// replaceTextBlocks 用合并后的正文替换最终块序列里的文本块（保留 tool_use /
// 检索块顺序与位置）；无文本块时追加在末尾。
func replaceTextBlocks(blocks []any, text string) []any {
	out := make([]any, 0, len(blocks)+1)
	inserted := false
	for _, bi := range blocks {
		m, _ := bi.(map[string]any)
		if m != nil && m["type"] == "text" {
			if !inserted && text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
				inserted = true
			}
			continue
		}
		out = append(out, bi)
	}
	if !inserted && text != "" {
		out = append(out, map[string]any{"type": "text", "text": text})
	}
	return out
}
