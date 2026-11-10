// starved.go 「思考吃光输出预算」的救援重发。
//
// 现象（本机实测，cn:deepseek-v4.1-flash，2026-11-10，即本轮上游对齐验收时复现）：
// 上游把 reasoning 与正文字节算在**同一个** max_tokens 预算里，思考没有独立上限。
// 客户端给一个偏小的预算时，思考可以独吞整份额度——结束于 finish_reason=length、
// 正文零字节、没有 tool_calls，客户端看到的就是「空回复」：
//
//	max_tokens=32  → 思考 32，  正文 0 字节   finish=length
//	max_tokens=300 → 思考 300， 正文 0 字节   finish=length
//	max_tokens=1200 → 思考数百，正文正常      finish=stop
//
// 救援动作 = 「**同一份额度、关掉思考**」重发一次：不放大客户端要的额度（超发是对
// 客户端的语义违约，也会形成新的可判别形态），只把预算从思考手里拿回来。
//
// 触发条件（三个同时成立，缺一不救）：
//  1. finish_reason == "length" 或「思考完直接 stop」；
//  2. 正文为空；
//  3. 没有 tool_calls。
//
// 有正文的截断是客户端额度语义的正常结果，不在此列。
//
// 作用范围：**非流式聚合路径**（/v1/messages 非流式、/v1/responses 非流式）。
// 流式路径的等价能力（多段拼接续写）涉及 continueReader 级别的改造，量大且风险高，
// 不在本轮范围（见 docs/UPSTREAM-ALIGNMENT.md 的 R2-2 说明）。
package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
)

// minStarvedBudget 低于此额度不救援：客户端常用 max_tokens=1 做 key 探活，
// 那种请求按原样失败即可，不值得为它多发一次上游请求。
const minStarvedBudget = 16

// outputBudget 读请求体里的输出限额（三个别名），未携带 → 0。
func outputBudget(obj map[string]any) int {
	for _, k := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		switch v := obj[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	return 0
}

// aggregateStarved 聚合体是否属于「预算被思考吃光、什么也没产出」。
//
// 两个成因，救援动作相同（关思考重发）：
//   - finish_reason=length 且正文空（额度真的用尽）；
//   - finish_reason=stop 且正文空但有思考（上游思考完就直接收尾）。
func aggregateStarved(resp map[string]any) bool {
	chs, _ := resp["choices"].([]any)
	if len(chs) == 0 {
		return false
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return false
	}
	fr, _ := c["finish_reason"].(string)
	if fr != "length" && fr != "stop" {
		return false
	}
	m, _ := c["message"].(map[string]any)
	if m == nil {
		return false
	}
	// 有正文 → 不是「什么都没产出」，不救（有正文的截断是客户端额度的正常结果）。
	if s, _ := m["content"].(string); strings.TrimSpace(s) != "" {
		return false
	}
	// 有工具调用 → 是合法的工具轮，不救。
	if tcs, _ := m["tool_calls"].([]any); len(tcs) > 0 {
		return false
	}
	// finish=stop 且无思考 → 上游真的给了空回复，不是预算问题，不救。
	if fr == "stop" {
		if r, _ := m["reasoning_content"].(string); strings.TrimSpace(r) == "" {
			return false
		}
	}
	return true
}

// starvedRetryBody 把请求体改写成救援形态：thinking 关掉、删掉思考档位，
// 其余（含 max_tokens / tools / messages）原样保留。
//
// 返回 false 的情形都表示「不猜」：body 不可解析、额度小到不值得重发、
// 已经关着思考（说明不是思考吃预算）。
func starvedRetryBody(body []byte) ([]byte, bool) {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	if bt := outputBudget(obj); bt > 0 && bt < minStarvedBudget {
		return nil, false
	}
	th, ok := obj["thinking"].(map[string]any)
	if ok {
		if t, _ := th["type"].(string); strings.EqualFold(strings.TrimSpace(t), "disabled") {
			return nil, false // 已经关着思考：成因不在思考，别重复发
		}
	} else {
		// 客户端没显式带 thinking（档位/开关由网关 payload 层注入）：按模型能力判断。
		// 拿不到开关的模型不猜别的手段。
		model, _ := obj["model"].(string)
		if !thinkingSwitchSupported(model) {
			return nil, false
		}
		th = map[string]any{}
	}
	th["type"] = "disabled"
	obj["thinking"] = th
	// 关思考就别再带档位：上游对 disabled + effort 的组合语义未取证，
	// 而思考管线的 disabled 分支本来就要求两者不共存。
	delete(obj, "reasoning_effort")
	delete(obj, "reasoningEffort")
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// thinkingSwitchSupported 该模型的思考是否可被 thinking.type=disabled 关掉。
//
// 与 upstream/thinking.go 的注入范围保持一致：只有 deepseek 系模型由网关注入
// thinking 开关（其余模型上游默认行为不同，关不掉）。这里只做「知不知道怎么关」
// 的判断，不判断当前是否开着。
func thinkingSwitchSupported(model string) bool {
	return strings.Contains(strings.ToLower(model), "deepseek")
}

// tryStarvedRescue 对一次非流式聚合结果做空截断救援。
//
// 调用方：runOpenAIPass 之后（/v1/messages 与 /v1/responses 的非流式分支）。
// 返回 true 表示已救援并替换了 pr（调用方无需再处理原结果）。
//
// 每请求最多一次；救援失败则原样返回原结果——**不救援时行为与接线前完全一致**，
// 这是本机制的安全性前提（不能因为救援逻辑把正常请求搞坏）。
func (h *Handler) tryStarvedRescue(r *http.Request, body map[string]any, pr *anthropicPassResult) (*anthropicPassResult, bool) {
	if pr == nil || pr.status != http.StatusOK || pr.resp == nil {
		return pr, false
	}
	if !aggregateStarved(pr.resp) {
		return pr, false
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return pr, false
	}
	retryBody, ok := starvedRetryBody(raw)
	if !ok {
		return pr, false
	}

	// 成因在重发前抓（重发会覆盖 pr）。
	cause := "预算被思考吃光"
	if fr := firstFinishReason(pr.resp); fr == "stop" {
		cause = "思考完直接收尾"
	}

	var retry map[string]any
	if err := json.Unmarshal(retryBody, &retry); err != nil {
		return pr, false
	}

	np := h.runOpenAIPass(r.Context(), r, retry)
	if np == nil || np.status != http.StatusOK || np.resp == nil {
		logx.Warnf("[starved] %s（正文 0B）→ 关思考重发失败（status=%d），降级为原结果", cause, np.status)
		return pr, false
	}
	// 救援段仍然空？不无限重试，按原结果返回。
	if aggregateStarved(np.resp) {
		logx.Warnf("[starved] %s（正文 0B）→ 关思考重发仍为空，降级为原结果", cause)
		return pr, false
	}
	logx.Infof("[starved] %s（正文 0B）→ 同额度关思考重发成功，正文 %d 字节", cause, contentLen(np.resp))
	return np, true
}

// firstFinishReason 取聚合体的 finish_reason（诊断用）。
func firstFinishReason(resp map[string]any) string {
	chs, _ := resp["choices"].([]any)
	if len(chs) == 0 {
		return ""
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return ""
	}
	fr, _ := c["finish_reason"].(string)
	return fr
}

// contentLen 取聚合体正文长度（诊断用）。
func contentLen(resp map[string]any) int {
	chs, _ := resp["choices"].([]any)
	if len(chs) == 0 {
		return 0
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return 0
	}
	m, _ := c["message"].(map[string]any)
	if m == nil {
		return 0
	}
	s, _ := m["content"].(string)
	return len(s)
}
