// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client

	// refreshMu 保护 refreshing/renewed 两张刷新表：
	//   refreshing 同一个 refresh token 的在途刷新（并发调用共享结果）；
	//   renewed    刷新成功后 60 秒内的结果（拿着旧 RT 的后来者直接复用，不再花一次 RT）。
	// 网络 I/O 不在这把锁内；锁序固定为 refreshMu → a.mu，任何路径不得反向。
	refreshMu  sync.Mutex
	refreshing map[string]*refreshCall
	renewed    map[string]*refreshResult
	// rotationObs RT 轮换观察者（SetRotationObserver 注入，M6 审计；nil = 不观察）。
	// 在 refreshMu 下读写：refresh() 的 leader 路径调用时天然持有该锁序。
	rotationObs RotationObserver

	// ChatHTTP 聊天 SSE 专用 client：无总时长上限（Timeout=0），首字节由
	// Transport.ResponseHeaderTimeout 约束，流中空闲由 IdleTimeout 约束。
	// 与 HTTP 共享同一个 *http.Transport 实例，连接池不重复。
	ChatHTTP *http.Client

	// HeaderTimeout 聊天 SSE 首字节前（响应头）超时；<=0 表示未设置（回落 HTTP.Timeout）。
	HeaderTimeout time.Duration
	// IdleTimeout 聊天 SSE 流中空闲超时；<=0 表示禁用空闲监控。
	IdleTimeout time.Duration

	// DumpRequestMax 落盘文件里出站请求体片段的上限（字节）；0 = 默认 256 KB。
	// 见 dump.go。
	DumpRequestMax int
	// DumpDir 非空时把每次 chat 请求的出站报文与上游原始响应落盘（诊断用，
	// 见 dump.go）。启动后只读，空 = 不落盘。
	DumpDir string

	// effortsMu/efforts 缓存各模型 supportedEfforts（FetchModels 刷新），供请求体 effort 降级。
	// 按 realm 分层桶（cn/global）：同模型名跨域探测的 effort 集合可能不同，
	// 混桶会互相污染（C-2）。
	effortsMu sync.RWMutex
	efforts   map[string]map[string][]string
	// defaultEfforts 缓存各模型 reasoning.defaultEffort（FetchModels 刷新），供
	// thinking.go 补档：缺显式 effort 时优先用模型声明默认档，空串回退硬编码 high。
	// 与 efforts 同 realm 分层桶（同 C-2 隔离原则），共用 effortsMu。
	defaultEfforts map[string]map[string]string
	// modelRates 缓存各模型当前生效积分倍率（规范化数值，如 "0.5"）。
	// 与 efforts 共用 realm 分层和锁；每次成功刷新模型目录时整体替换对应域。
	modelRates map[string]map[string]string

	// globalModels 缓存 global 模型名目录探测结果（成功 ∩ 静态 overlay；
	// 1h TTL + 5min 负缓存），见 global_models.go。按实例持有，测试新建 Client 即隔离。
	globalModels fetchGlobalModelsCache

	// SanitizeFingerprints 出站请求体黑名单指纹脱敏开关（默认 true；false 完全还原）。
	// 面板保存配置热改 + chat 热路径并发读写，用 atomic.Bool 消除数据竞争。
	SanitizeFingerprints atomic.Bool

	// UserAgent 出站 User-Agent 显式覆盖（非空时全路径生效，优先于默认三段式）。
	// 空 = 默认官方形态：chat/refresh/FetchModels 走
	// `WorkBuddy/<ver> WorkBuddy/<ver> CLI/<cliVer>`；billing 走 `WorkBuddy/<ver>`
	// （仅当 client_name 非空）。
	UserAgent string

	// ClientVersion WorkBuddy 客户端版本段（出站 UA 的 `WorkBuddy/<ver>` + X-IDE-Version）。
	// 空 = 内置默认（对齐官方 5.5.4 分发包）。
	ClientVersion string

	// CliVersion 出站 UA 中 `CLI/<ver>` 段版本。空 = 内置默认（官方内置 CLI 2.137.1）。
	CliVersion string

	// ClientName 用量归属头取值（X-Product / X-IDE-Name / X-IDE-Type / X-IDE-Version）。
	// 空 = 旧行为：X-Product="SaaS"，不设 X-IDE-*（向后兼容，不突变归因）。
	ClientName string

	// PassthroughIP 是否透传客户端 IP 给上游（X-Forwarded-For/X-Real-IP 首段）。
	// 缺省 false（反代安全边界）；handler 在 chat 路径按请求把 clientIP 传入 ChatStream。
	PassthroughIP bool

	// DeviceToken 设备风控 Token（X-Device-Token 头）全局兜底来源：config upstream.device_token。
	// 解析优先级：auth.Auth.DeviceToken > DeviceToken（config）> DeviceTokenFile（文件）。
	DeviceToken string

	// DeviceTokenFile 设备 token 文件路径兜底（宿主落盘的桌面端 token，5 分钟读取缓存）。
	DeviceTokenFile string

	ChatBaseCN    string
	BillingBaseCN string
	// WebBaseCN 官网（workbuddy.cn）域：部分「任务领奖」类接口只在此域提供
	// （Web 成长中心用；CLI 域 copilot.tencent.com 的同名路径返回 400）。
	WebBaseCN string

	// ChatBaseGlobal / BillingBaseGlobal 国际版（global realm）上游 base。
	// 空 = 缺省默认 https://www.workbuddy.ai（D5）。
	ChatBaseGlobal    string
	BillingBaseGlobal string

	// GlobalEnabled 是否启用 global realm 路由（config global.enabled，缺省 true）。
	// false 时即便用户 auth 写了 realm=global 也**不**路由到 global base——
	// chatBase/billingBase 返回 CN base，路径也走 CN（双保险，与 auth.Realm() 的开关闸呼应）。
	GlobalEnabled bool
}

// New 生产默认值。Transport 由 newTransport() 集中构造（连接层加固：真正禁 h2 /
// TLS 握手超时 / 短 keepalive 探测 / 失败清池，参数见 transport.go——吸收上游
// kongjianguan 4 连击实测经验）。
func New() *Client {
	tr := newTransport()
	c := &Client{
		HTTP:          &http.Client{Timeout: 120 * time.Second, Transport: tr},
		ChatHTTP:      &http.Client{Timeout: 0, Transport: tr}, // 无总时长；首字节由 ResponseHeaderTimeout 管
		ChatBaseCN:    "https://copilot.tencent.com",
		BillingBaseCN: "https://www.codebuddy.cn",
		WebBaseCN:     "https://www.workbuddy.cn",
		// GlobalEnabled 缺省 true（与 config global.enabled 缺省 true 一致；纯 CN 部署行为不变：
		// CN 账号恒判 cn，global base 只在 realm=global 的账号上被使用）。
		GlobalEnabled: true,
	}
	c.SanitizeFingerprints.Store(true)
	return c
}

// chatHTTP 返回聊天专用 client；未设置（如测试只注入 HTTP）时回落 HTTP。
func (c *Client) chatHTTP() *http.Client {
	if c.ChatHTTP != nil {
		return c.ChatHTTP
	}
	return c.HTTP
}

// MemberTier 会员档位读出（hub #621 同语义）：paid = 计费摘要 IsPaidUser；
// pkg = 套餐名（付费包名，免费侧可能为空）。调用方节流一天一次——摘要端点
// 与余额同族，高频轮询是上游可观测特征。
type MemberTier struct {
	Paid bool
	Pkg  string
}

// UserResourceSummary 拉计费摘要（get-user-resource-summary）：IsPaidUser /
// SubscriptionPackageName。解析宽松（嵌套位置容错——上游信封层级调整不碎）。
// 供面板「会员类型」胶囊显示；失败返回错误由调用方决定保留旧值（不写结论）。
func (c *Client) UserResourceSummary(a *auth.Auth) (MemberTier, error) {
	data, err := c.billingMeterJSON(a, c.summaryMeterPaths(a), http.MethodPost, map[string]any{})
	if err != nil {
		return MemberTier{}, err
	}
	return parseMemberTier(data), nil
}

// parseMemberTier 在响应 JSON 里宽松查找 IsPaidUser / SubscriptionPackageName
// （任意嵌套层级，键名大小写不敏感）。
func parseMemberTier(data json.RawMessage) MemberTier {
	var out MemberTier
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, vv := range t {
				lk := strings.ToLower(k)
				if lk == "ispaiduser" {
					if b, ok := vv.(bool); ok {
						out.Paid = b
					}
				} else if lk == "subscriptionpackagename" {
					if sv, ok := vv.(string); ok && sv != "" {
						out.Pkg = sv
					}
				}
				walk(vv)
			}
		case []any:
			for _, vv := range t {
				walk(vv)
			}
		}
	}
	var root any
	if json.Unmarshal(data, &root) == nil {
		walk(root)
	}
	return out
}

// prepareBody 组装出站请求体（脱敏开关由 Client.SanitizeFingerprints 控制）。
// realm 为账号 Realm()（cn/global），供 efforts 缓存分桶（跨域 effort 集合不互相污染）。
func (c *Client) prepareBody(body []byte, realm, uid, conversationID string) []byte {
	efforts, defs := c.effortsSnapshot(realm), c.defaultEffortsSnapshot(realm)
	if realmKey(realm) == "global" {
		// global 域降级源 = 远端探测桶（权威）∪ 产品静态兜底表（全局 21 名内档位如
		// deepseek-v4.1-flash ['high']）。当前探测桶为空时也按静态表降级，不全程透传
		//（issue #84：往 WorkBuddy 上游发 low/max 非法，须降级到 high）。
		efforts, defs = globalEffortMap(efforts, defs)
	}
	body = PrepareBodyOptWithEffortsAndDefault(body, c.SanitizeFingerprints.Load(), efforts, defs)
	// prompt_cache_key 注入（P0 费用优化，费用降 ~17×）：按账号隔离的稳定缓存键，
	// 让同一客户端对同一账号的连续请求命中上游前缀缓存。
	body = InjectPromptCacheKey(body, uid, conversationID)
	return body
}

// effortsSnapshot 返回 effort 能力缓存副本；nil 表示未知（透传不降级）。
func (c *Client) effortsSnapshot(realm string) map[string][]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.efforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string][]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// defaultEffortsSnapshot 返回指定 realm 的模型 defaultEffort 缓存副本；
// 该域无探测或无声明默认档 → nil（thinking.go 回退硬编码 high）。
func (c *Client) defaultEffortsSnapshot(realm string) map[string]string {
	c.effortsMu.RLock()
	defer c.effortsMu.RUnlock()
	bucket, ok := c.defaultEfforts[realmKey(realm)]
	if !ok || len(bucket) == 0 {
		return nil
	}
	cp := make(map[string]string, len(bucket))
	for k, v := range bucket {
		cp[k] = v
	}
	return cp
}

// realmKey 归一化 efforts 缓存键：cn/global。空 realm 视为 cn（老调用/无前缀模型名）。
func realmKey(realm string) string {
	if realm == "" {
		return "cn"
	}
	return realm
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
// body 读失败（连接中断/空闲掐流/截断）返回普通错误（非 *Error）——半截 body 不进
// Classify，不参与账号惩罚（传输层故障不该喂熔断误罚号）。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 等价于 ChatStreamContext(context.Background(), ...)：不带调用方取消语义。
// 需要客户端断开联动的调用方用 ChatStreamContext 传入请求 ctx。
//
// global realm：先打 /console/chat/completions，404/405 时同一 base 二次换 /v2/chat/completions
// （上游新旧路径分叉，PLAN R9 fallback 顺序）。cn：/v2/chat/completions 现状不变。
func (c *Client) ChatStream(a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	return c.ChatStreamContext(context.Background(), a, body, clientIP, meta)
}

// ChatStreamContext 同 ChatStream，但从 ctx 派生请求 context：调用方（handler）传入
// r.Context() 后，客户端断连/请求取消会立即中断在途上游调用、释放连接与账号在途名额，
// 不再空转到 IdleTimeout。ctx 为 nil 时回落 Background。成功流的 cancel 仍由
// monitorBody 的 Close 接管（reqCtx 取消与显式 Close 任一触发即断）。
//
// 错误路径（≥400 且非 fallback 状态码）除 (status, respBody) 外还返回**已分类的**
// *Error（Kind 信封 + Retry-After 头解析）：客户端错误分类在此一次完成，handler
// 不再对 body 二次 Classify（消除「上游分类一次、网关再分类一次」的双路径漂移面），
// Retry-After 也随信封流动。respBody 仍原样返回（错误透传语义：message 透传上游
// 原文）。判定为 ErrNone 的响应（理论上不存在，防御）err 为 nil，handler 按
// respBody 自行兜底。
//
// global chat 自 #119 实测后固定走 /v2（chat 层无 fallback 链；billing 层的 404
// fallback 独立存在，语义不受影响）。ensureConsoleSystem 在 prepareBody 后统一套用
// 全局脚本：首条消息非 system 时前置兜底 system（防 console 域上游 code 11-128；
// #119 后 global 出站固定 /v2，该兜底保留——上游对 /v2 是否需要 system 无实测
// 反证，删了无回滚路径）。
func (c *Client) ChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, clientIP string, meta ChatMeta) (rc io.ReadCloser, status int, respBody []byte, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	prepared := c.prepareBody(body, a.Realm(), a.UID, cacheKeySource(meta))
	// body 侧会话字段（issue #51）：把本轮 ID 同时写进消息体，与出站头保持头体一致
	// （官方桌面端头与体都带 conversationRequestId）。见 conv_fields.go。
	prepared = injectConversationFields(prepared, meta.ConversationRequestID)
	if c.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	// reqCtx 的 cancel 在每个出口显式调用（Do 失败 / ≥400 / 成功分支移交 monitorBody），
	// 循环本身各分支必 return——无循环尾兜底代码（此前外层 var cancel 从未赋值 +
	// 尾部不可达 cancel() 是潜伏 nil-panic，已删；chatPaths 恒非空由构造保证）。
	for _, path := range c.chatPaths(a) {
		endpoint := c.chatBase(a) + path
		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(prepared))
		if err != nil {
			return nil, 0, nil, err
		}
		c.ChatHeaders(req, a, clientIP, meta)
		// 从调用方 ctx 派生：保留取消传播（父 ctx 取消 → 本 ctx 取消），
		// 同时 monitorBody.Close 仍能独立 cancel 本分支（空闲掐流）。
		reqCtx, cancel := context.WithCancel(ctx)
		req = req.WithContext(reqCtx)
		resp, err := c.chatHTTP().Do(req)
		if err != nil {
			cancel()
			log.Printf("ERR: [upstream] chat_stream acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
			// 传输层失败 → 清空共享连接池的空闲连接（连接层加固）：失败连接可能仍
			// 留在空闲池里，下一个请求会继续捡到它——仅靠 IdleConnTimeout 等过期
			// 不够，主动清池才断根。
			roundTripCloseIdle(c.chatHTTP().Transport)
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			cancel()
			// body 读失败（掐流/截断）→ 传输层错误：半截 raw 不交回调用方进 Classify，
			// 否则 handler 侧 applyErrorPolicy 会按误判分类罚号。
			if rerr != nil {
				log.Printf("ERR: [upstream] chat_stream acct=%s: read body: %v", logfmt.Label(a.UID, a.Nickname), rerr)
				return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
			}
			kind := Classify(resp.StatusCode, string(raw))
			log.Printf("WARN: [upstream] chat_stream acct=%s: upstream %d %s body=%s",
				logfmt.Label(a.UID, a.Nickname), resp.StatusCode, kind, truncate(string(raw), 200))
			// ≥400 直接返回（#119 后 global 单路径 /v2，chat 层无 fallback 链）。
			// 分类一次、随 Kind 信封返回（含 Retry-After 头解析）：
			// ErrNone 是防御分支（≥400 不应产生 None），返回原文让 handler 兜底。
			if kind == ErrNone {
				return nil, resp.StatusCode, raw, nil
			}
			ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
			if d, ok := ParseRetryAfter(resp.Header); ok {
				ue.RetryAfter = d
			}
			return nil, resp.StatusCode, raw, ue
		}
		// 成功分支：cancel 所有权交给 monitorBody（其 Close 会 cancel）；
		// IdleTimeout<=0 时 monitorBody 原样返回底流、无人调 cancel——可接受：
		// 取消传播由 http.Transport 在 body Close / 父 ctx 取消时处理，连接正常清理。
		// dumpUpstream 套在最外层：拿到的是 monitorBody 之后、未经任何改写的上游
		// 原始字节，DumpDir 为空时原样返回本 reader（零开销）。
		stream := c.dumpUpstream(monitorBody(resp.Body, c.IdleTimeout, cancel), prepared, a)
		return stream, resp.StatusCode, nil, nil
	}
	// for range 空集时编译器要求兜底 return。chatPaths 由构造保证非空，但这是生产
	// 请求路径——配置异常改空时不得 panic 断连，防御性错误让调用方走轮转处置。
	return nil, 0, nil, fmt.Errorf("chat paths empty (client construction bug or config corruption)")
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
// 偶发上游 5xx（code 10000）做有界重试（见 retryBillingTransient）——单次抖动不再
// 让该账号整天漏签；「已签到」等业务错误不重试。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return c.retryBillingTransient(func() error {
		_, err := c.billingMeterJSON(a, c.checkinMeterPaths(a), http.MethodPost, map[string]any{})
		return err
	})
}

// IsAlreadyCheckin 报告 err 是否表示"今天已签到"（上游幂等拒绝重复签到）。
// 只认带分类的 *Error（业务 code 或 HTTP 错误）：网络层/解析层错误不得当作幂等成功，
// 否则停机补签遇到抖动会误记为 already，账号当天实际未签到却被判定正常。
func IsAlreadyCheckin(err error) bool {
	var ue *Error
	if !errors.As(err, &ue) {
		return false
	}
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(ue.Msg, m) || strings.Contains(strings.ToLower(ue.Msg), strings.ToLower(m)) {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	return logfmt.Truncate(s, n)
}
