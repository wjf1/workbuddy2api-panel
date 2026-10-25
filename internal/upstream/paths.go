// paths.go 出站 URL/路径构建：CN/global 双域的 chat/billing/web base 选择与
// 各端点路径候选序列（billing 404 fallback 链）。
package upstream

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// defaultGlobalBase 缺省 global base（D5：config 未覆盖时默认 workbuddy.ai）。
const defaultGlobalBase = "https://www.workbuddy.ai"

// globalChatBase 生效的 global chat base：Client.ChatBaseGlobal 非空取之，否则默认。
func (c *Client) globalChatBase() string {
	if c.ChatBaseGlobal != "" {
		return c.ChatBaseGlobal
	}
	return defaultGlobalBase
}

// globalBillingBase 生效的 global billing base：Client.BillingBaseGlobal 非空取之，否则默认。
func (c *Client) globalBillingBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalOn 报告账号是否路由到 global 上游：GlobalEnabled 开且账号 Realm()==global。
// 双保险：config 开关是第一道闸（上游侧），auth.Realm() 的开关闸是第二道（账号侧）。
func (c *Client) globalOn(a *auth.Auth) bool {
	return c.GlobalEnabled && a != nil && a.Realm() == "global"
}

// 路径常量：CN 与 global 共用的 chat 出站路径（/v2 单路径）。
const chatCompletionsPath = "/v2/chat/completions"

// chatPaths 返回按 realm 的 chat 路径候选序列：
// global → [/v2]（#119 固定单路径：/console 挂腾讯云 WAF body 内容规则，反引号
// printf/whoami 等命令执行特征确定性 403；/v2 同 base 不挂该规则，实测等价端点。
// 已知取舍：若上游未来关闭 /v2，global chat 整体不可用——届时应重新启用 /console
// 路径，此注释即"坏了再说"的锚点）；cn → [/v2]（单元素，现状）。
func (c *Client) chatPaths(a *auth.Auth) []string {
	return []string{chatCompletionsPath}
}

// billing 域端点路径（billingBase + path）。balance/checkin 与 report（report.go）同域，
// 统一走 billingJSON 发请求。
const (
	billingMeterPath   = "/billing/meter/get-user-resource"    // global 首选（国际版无 /v2 前缀）
	dailyCheckinPath   = "/billing/meter/daily-checkin"        // global 首选
	billingMeterPathV2 = "/v2/billing/meter/get-user-resource" // CN 现状 / global fallback

	billingSummaryPath   = "/billing/meter/get-user-resource-summary"    // 会员档位摘要 global 首选
	billingSummaryPathV2 = "/v2/billing/meter/get-user-resource-summary" // CN 现状 / global fallback
	dailyCheckinPathV2   = "/v2/billing/meter/daily-checkin"
)

// billingMeterPaths 按 realm 返回 billing/meter 域路径候选序列：
// global → [无 /v2, 有 /v2]（404 时 fallback）；cn → [有 /v2]（现状逐字，零回归）。
// 仅作用于 get-user-resource / daily-checkin（/billing/meter/* 族）；report /v2/report 不参与，
// 其他 billing 端点（growth 等）路径不含 /billing/meter 前缀，走原常量不受影响。
func (c *Client) billingMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingMeterPath, billingMeterPathV2}
	}
	return []string{billingMeterPathV2}
}

// checkinMeterPaths 同上，针对 daily-checkin。
func (c *Client) checkinMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{dailyCheckinPath, dailyCheckinPathV2}
	}
	return []string{dailyCheckinPathV2}
}

// summaryMeterPaths 会员档位摘要端点的双路径（与 checkinMeterPaths 同形态）。
func (c *Client) summaryMeterPaths(a *auth.Auth) []string {
	if c.globalOn(a) {
		return []string{billingSummaryPath, billingSummaryPathV2}
	}
	return []string{billingSummaryPathV2}
}

func (c *Client) chatBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalChatBase()
	}
	return c.ChatBaseCN
}

func (c *Client) billingBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return c.globalBillingBase()
	}
	return c.BillingBaseCN
}

// webBase 返回官网域（任务领奖类接口；未注入时回落默认）。
// realm 感知：global 账号切国际站 workbuddy.ai，CN 用 workbuddy.cn。
func (c *Client) webBase(a *auth.Auth) string {
	if c.globalOn(a) {
		return defaultGlobalBase
	}
	if c.WebBaseCN != "" {
		return c.WebBaseCN
	}
	return "https://www.workbuddy.cn"
}
