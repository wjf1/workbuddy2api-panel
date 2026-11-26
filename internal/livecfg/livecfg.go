// Package livecfg 运行期可变配置的并发安全持有者。
//
// 背景：进程启动时读入的配置是普通字段（读多写零），但管理面板允许在线改配置，
// 于是少量"可热生效"的字段需要有并发安全的读写点。此处用不可变快照 + atomic 指针：
// 读方 Load 拿到一致视图，写方 Store 整体替换，无锁无数据竞争。
//
// 只承载**读路径深、热改需求强**的少数字段；池参数/排程参数等各有既有 setter
// （pool.SetBreaker、scheduler.Reconfigure 等），不重复收编到这里。
package livecfg

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// KeyGroup 一组密钥分发的配置单元。
type KeyGroup struct {
	Name string `json:"name"` // 组名（面板展示与限流分桶标识；空 = "group-N"）
	Key  string `json:"key"`  // 该组 API key（强度校验同主 api_key；空 = 该组禁用）
	// Models 组内模型白名单（口径同 Snapshot.ModelWhitelist 未归一化的原始条目：
	// 裸名两域生效、"cn:x"/"global:x" 单域）。空 = 不限制（全量模型，受全局
	// Whitelist/Hidden 约束）。
	Models []string `json:"models"`
	// RPM 组内每分钟请求数上限（<=0 = 不限）。命中组的请求按组名分桶计数，
	// 超限 429（独立于全局 RateLimit，两者可同时生效，先触发先生效）。
	RPM int `json:"rpm"`
}

// NormalizeKeyGroups 归一化分组条目：丢弃空 key 组、补齐组名缺省值、
// 预归一化各组模型白名单。返回空切片 = 单密钥模式。
func NormalizeKeyGroups(in []KeyGroup) []KeyGroup {
	if len(in) == 0 {
		return nil
	}
	out := make([]KeyGroup, 0, len(in))
	for i, g := range in {
		g.Key = strings.TrimSpace(g.Key)
		if g.Key == "" {
			continue
		}
		g.Name = strings.TrimSpace(g.Name)
		if g.Name == "" {
			g.Name = "group-" + strconv.Itoa(i+1)
		}
		out = append(out, g)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GroupModelSet 返回该组的模型白名单集合（已归一化）；空 = 不限制（nil）。
func (g KeyGroup) GroupModelSet() map[string]struct{} {
	return NormalizeHiddenModels(g.Models)
}

// Snapshot 一次读取的不可变配置视图。
type Snapshot struct {
	APIKey               string              // 网关/面板共同鉴权密钥；空 = 不鉴权
	SoftCooldown         time.Duration       // 429 软冷却基数（<=0 时调用方回退内置默认）
	SanitizeFingerprints bool                // 出站请求体指纹脱敏
	RecordClientInfo     bool                // 请求日志是否记录调用来源（客户端 IP / UA）
	HiddenModels         map[string]struct{} // 精确隐藏的模型 id（已归一化为 "cn:x"/"global:x" 全量形式）；nil = 不隐藏
	ModelAliases         map[string]string   // 客户端名 → 真实模型名（键已归一化小写；"cn:x"/"global:x" 前缀键只在该域生效，裸名键两域都生效）；nil = 不映射
	// ModelWhitelist 模型白名单（已归一化为 "cn:x"/"global:x" 全量形式）；
	// nil/空 = 不启用（全部可用，老行为）。启用后仅名单内可用，Hidden 在其后叠加。
	ModelWhitelist map[string]struct{}
	// KeyGroups 密钥分组分发（空 = 单密钥模式，与现状一致）。每组一个独立 key，
	// 可绑定模型白名单子集与 RPM 限流；命中组注入请求 ctx 供模型判定与限流分桶。
	// 主 APIKey 始终有效（管理面/兼容旧客户端）；KeyGroups 是增量分发通道。
	KeyGroups []KeyGroup
	// ModelFallback 模型降级路由表（pool.model_fallback）：key = 请求模型（裸名），
	// value = 6004 模型级限流时改用的等价模型。空/nil = 不降级（老行为）。
	// 语义：等价模型是同一底层能力的不同计价档（如 deepseek-v4.1-flash →
	// deepseek-v4-flash），贵/便宜的那个被限流时改用另一个继续服务。只对 6004 生效
	// （11102 换模型救不了、账号级 429/5xx 换模型无意义）；降级不写持久状态，
	// 冷却到期自动回原模型。热生效。
	ModelFallback map[string]string
}

// Holder 原子持有当前快照。
type Holder struct {
	p atomic.Pointer[Snapshot]
}

// New 以初始快照构建。
func New(s Snapshot) *Holder {
	h := &Holder{}
	h.Store(s)
	return h
}

// Load 返回当前快照（Holder 为 nil 或从未 Store 时返回零值快照，调用方无需判空）。
func (h *Holder) Load() Snapshot {
	if h == nil {
		return Snapshot{}
	}
	if s := h.p.Load(); s != nil {
		return *s
	}
	return Snapshot{}
}

// Store 整体替换快照。
func (h *Holder) Store(s Snapshot) { h.p.Store(&s) }

// NormalizeHiddenModels 把用户配置的 hidden 条目归一化为精确 id 集合。
//
// 条目带合法 "cn:"/"global:" 前缀 → 只隐藏该域；裸名 → 两域都隐藏。
// 大小写敏感（与 internal/server.resolveModel 的前缀规则对称：取第一个 ":",
// 前段恰为 "cn"/"global" 才剥离）。返回的 map 只读共享，调用方不得修改。
//
// 为什么裸名要展开成两个键：黑名单是「这个模型对本网关不存在」，忽略域就等于
// 客户端换个前缀（global:xxx）就绕过——列表里藏了，点名却照样放行。
func NormalizeHiddenModels(hidden []string) map[string]struct{} {
	if len(hidden) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(hidden)*2)
	for _, m := range hidden {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if prefix, bare, ok := strings.Cut(m, ":"); ok && (prefix == "cn" || prefix == "global") {
			out[prefix+":"+bare] = struct{}{}
		} else {
			out["cn:"+m] = struct{}{}
			out["global:"+m] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeModelWhitelist 与 NormalizeHiddenModels 同口径归一化白名单条目：
// 裸名 → 两域都生效；"cn:"/"global:" 前缀 → 只该域。返回 nil = 未启用。
// 语义与 Hidden 相反：集合非空 = 仅名单内可用（白名单模式），Hidden 再叠加。
func NormalizeModelWhitelist(whitelist []string) map[string]struct{} {
	return NormalizeHiddenModels(whitelist)
}

// NormalizeModelAliases 把用户配置的 aliases 条目（"客户端名=真实模型名"）归一化为查找表。
//
// 语义：
//   - 条目按第一个 "=" 拆键值；键/值都 Trim 空间，键小写存储（查找端同样小写——
//     OpenAI 生态的模型名事实大小写不敏感，而 WorkBuddy 上游目录 id 是小写，
//     归一化避免 "DeepSeek-V4.1-Flash" 这类拼法查空）。
//   - 键带合法 realm 前缀（"cn:"/"global:"）→ 键存 "cn:x" 全量形式，只在该域生效；
//     裸名 → 同时登记 "cn:x" 与 "global:x"，两域都生效（与 NormalizeHiddenModels 同口径）。
//   - 值原样保留（含值里的 realm 前缀——映射完再走一遍 resolveModel，
//     所以别名可以跨域指向："my-glm" → "global:glm-5.2" 合法）。
//   - 无 "="、空键、空值、键==值（大小写不敏感）→ 丢弃（自我映射还可能成环）。
//   - 同一键后写条目覆盖先写条目（配置里重复定义以最后一条为准，与 map 字面量一致）。
func NormalizeModelAliases(in []string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in)*2)
	for _, item := range in {
		k, v, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if k == "" || v == "" || strings.EqualFold(k, v) {
			continue
		}
		if prefix, bare, cut := strings.Cut(k, ":"); cut && (prefix == "cn" || prefix == "global") {
			out[prefix+":"+bare] = v
		} else {
			out["cn:"+k] = v
			out["global:"+k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
