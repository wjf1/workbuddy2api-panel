// reasoning_mirror.go 给「上游读 reasoning 字段」的模型补一份镜像
// （对齐参照上游 misakano7545 f987bac，源头为 hub 2026-11-08 实测）。
//
// 背景：同一段思考内容，各家协议用的键名不一样——
//   - OpenAI / DeepSeek 系：`reasoning_content`（本仓 thinking.go 的
//     backfillReasoningContent 已经在给 deepseek 系做，含空值占位）
//   - Kimi / GLM / MiniMax / MiMo / Step / Space-Bunny 这类：上游按 `reasoning`
//     读历史里的思考段，客户端只发 `reasoning_content` 时，上游看到的是「assistant
//     消息没有思考」，多轮一致性校验就会挂。
//
// 为什么单独一份、不并进 backfillReasoningContent：两者纪律相反。
//   - deepseek 那份是**补齐**：每个 assistant 消息都必须有 reasoning_content，空的
//     历史也补占位（那边的上游对 len>0 有校验）。
//   - 这里是**镜像**：只把已有的非空 reasoning_content 抄进空的 reasoning，绝不
//     新建字段、绝不填占位。这六个家族没有「必须有该字段」的实测依据，凭空补字段
//     等于替上游臆造报文。
//
// 出站 model 已是裸名（handler 在出站前剥 realm 前缀），这里直接匹配。
package upstream

import "strings"

// upstreamReasoningModelPrefixes 上游按 `reasoning` 字段读历史的模型前缀。
//
// 前缀集合照抄参照上游（源头 workbuddy2api-hub 5546063）。加新家族前先确认它确实
// 读 `reasoning`：错误地加进来，会在原本不看该字段的模型上多出一个陌生键。
var upstreamReasoningModelPrefixes = []string{
	"kimi", "glm", "minimax", "mimo", "step", "space-bunny",
}

// readsUpstreamReasoning 该模型是否按 `reasoning` 字段读历史。
func readsUpstreamReasoning(model string) bool {
	lower := strings.ToLower(strings.TrimSpace(model))
	if lower == "" {
		return false
	}
	for _, p := range upstreamReasoningModelPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// mirrorUpstreamReasoning 把这些模型的 assistant 历史里非空 reasoning_content
// 镜像到空的 reasoning 字段；其余一律不动。
//
// 只处理 assistant（user/tool 没有思考段）；reasoning 已有非空字符串时不覆盖
// ——上游若自己写了更权威的版本，不能拿客户端的副本盖掉它。
func mirrorUpstreamReasoning(obj map[string]any) {
	model, _ := obj["model"].(string)
	if !readsUpstreamReasoning(model) {
		return
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	for _, rawMsg := range msgs {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		rc, _ := msg["reasoning_content"].(string)
		if strings.TrimSpace(rc) == "" {
			continue
		}
		if r, ok := msg["reasoning"].(string); ok && strings.TrimSpace(r) != "" {
			continue
		}
		msg["reasoning"] = rc
	}
}
