// strip_metadata.go 出站请求体里 tool-call 追踪元数据的按需剥除（默认关闭）。
//
// 背景：客户端回传历史消息时，每个 tool-call 块除 args/result 外还挂着一整块
// metaData（traceId / traceparent / baggage / requestId / requestModelId /
// rawResponse / 时间戳等 ~20 个字段，单个约 1.3–1.8KB）。实测一条含 39 个
// tool-call 的历史消息里，这类纯观测字段占 55–70KB，且对模型没有任何语义
// 价值——它们原样被转发给上游，直接挤占上下文与费用。
//
// 设计取舍：
//   - 默认关闭。是否可删取决于上游是否用这些字段做链路关联，只有真实 A/B 能
//     回答；先做成 opt-in 开关，量过再谈默认值。
//   - 只动 messages[].content[] 里的 tool 块，且只删 metaData 一个键。
//     id / name / args / result.text 一律保留（模型要读工具结果）。
//   - 不动 reasoning 块（部分上游多轮要求保留思考链，改动风险高）。
//   - 没命中任何 metaData 时**原样返回原字节**，不重新序列化——开关打开但
//     请求里本来就没有这类字段时，出站报文与关闭时逐字节相同。
package upstream

import (
	"encoding/json"
	"log"
)

// StripToolMetaData 剥除出站 body 中 tool-call 块的 metaData 字段。
//
// 返回值语义：无可剥除内容、或 body 不是合法 JSON 对象时，返回**入参原切片**
// （不复制、不重新序列化），保证"开关打开但无字段可删"与关闭时出站报文一致。
func StripToolMetaData(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		// 上游本来就会对非法 JSON 报错，这里不拦截、不改写，保持透传语义。
		return body
	}
	if !stripToolMetaDataObj(obj) {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		// 理论上不可达（obj 来自 json.Unmarshal）；真出错就透传原文，别把请求弄坏。
		log.Printf("WARN: [upstream] strip tool metaData: marshal failed, passing through: %v", err)
		return body
	}
	return out
}

// stripToolMetaDataObj 在已解析的对象上原地剥除，返回是否有改动。
//
// 兼容两种形态：content 块里的 tool 对象（tool.metaData），以及执行结果块里的
// result 对象（result.metaData）。结构不符（content 不是数组等）时静默跳过。
func stripToolMetaDataObj(obj map[string]any) bool {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		blocks, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			for _, key := range [...]string{"tool", "result"} {
				holder, ok := block[key].(map[string]any)
				if !ok {
					continue
				}
				if _, has := holder["metaData"]; has {
					delete(holder, "metaData")
					changed = true
				}
			}
		}
	}
	return changed
}
