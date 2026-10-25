// errmap.go 上游错误类别 → 对外 HTTP 状态码的唯一映射点。
//
// 为什么要收在一处：这个映射是对外契约。客户端（尤其是 Claude Code / ZCode 这类
// 按状态码决定"重试 / 换模型 / 放弃"的工具）的全部行为都依赖它。此前它散在
// handler_chat.go 的 5 个早退点与末端 switch 里，改一个类别的状态码要翻 6 个地方，
// 而且没人能一眼说出「ErrImageInvalid 到底回什么」。
//
// 语义分组（改这里之前先想清楚客户端会怎么反应）：
//   - 400：请求本身或所选模型不可用。换号、重试、等待都无用——必须让客户端改请求。
//   - 429：限流 / 上游排队。等一会儿再来有意义，Retry-After 参与决策。
//   - 503：服务端（账号池）暂时不可用。换号或稍后重试有意义。
//
// 注意：这只是"错误类别 → 状态码"。同一类别下的**文案与 code** 仍按场景各自组装
// （例如 ErrWafBlock 的文案取决于本地 WAF 窗口是否激活、ErrModelBlocked 要带上
// 被挡账号数与最早解封时刻），那些不是查表能表达的，不要强行塞进来。
package server

import (
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// httpStatusForKind 上游错误类别 → 对外 HTTP 状态码。未列出的类别一律 503。
func httpStatusForKind(kind upstream.ErrKind) int {
	switch kind {
	// —— 请求/模型问题：不该重试 ——
	case upstream.ErrBadParams, // 11101 等请求体无法解析
		upstream.ErrPromptTooLong,     // 11115 上下文超限
		upstream.ErrImageInvalid,      // 图片格式/数据无效
		upstream.ErrModelParamInvalid, // 11133 参数被模型提供方拒绝
		upstream.ErrContentBlocked,    // 内容策略拦截（降级重试已在上游侧做完）
		upstream.ErrModelBlocked:      // 11102 该号无此模型（换号无用、换模型有用）
		return http.StatusBadRequest

	// —— 限流/排队：等一会儿有意义 ——
	case upstream.ErrSoftRate, // 429 软限流
		upstream.ErrUpstreamQueue: // 6020/6021 免费档排队，与账号无关
		return http.StatusTooManyRequests
	}

	// —— 其余：账号池/上游暂时不可用 ——
	// ErrHardCredit（余额不足）、ErrSessionDead（会话失效）、ErrNotFound、
	// ErrServer（5xx）、ErrAccountFault（授权/配额故障）、ErrWafBlock（IP 级拦截）、
	// ErrClient（其他 4xx）、ErrNone 走这里。
	return http.StatusServiceUnavailable
}
