// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
	"github.com/linguo2625469/workbuddy2api-panel/internal/websearch"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权（静态值；与 Live 同时给出时 Live 优先）
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// GetKeepaliveStats 返回保活可观测快照（供 /status；nil 时该键为 null）。
	// 闭包注入而非直接依赖 scheduler 包：server 与 scheduler 保持解耦（与
	// StickyCount 同模式）。
	GetKeepaliveStats func() any

	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m

	// Panel 管理面板 handler（可选；nil = 不挂载）。挂载在 /panel/ 前缀下，
	// 面板自带 Bearer 鉴权（同一 api_key）与内嵌静态资源，主路由只做转发。
	Panel http.Handler

	// Live 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏开关时立即生效）。
	// nil 时回退静态字段（测试与裸用场景）。
	Live *livecfg.Holder

	// PromptMode "custom"（网关用自有提示词替换 system）/ "passthrough"（透传）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// ModelsHidden 模型黑名单原始条目（config models.hidden；Live 为 nil 的
	// 裸用/测试路径经 loadLive() 归一化后使用）。
	ModelsHidden []string

	// ModelsAliases 模型映射原始条目（config models.aliases，"客户端名=真实名"；
	// Live 为 nil 的裸用/测试路径经 loadLive() 归一化后使用）。
	ModelsAliases []string

	// Usage 逐请求用量记录器（可选；nil = 不记录）。
	// 在 recordAttempt 这一唯一汇聚点调用，因此流式/非流式、成功/失败都会计入，
	// 且与 pool 的每账号累计器同源，两条口径不会漂移。
	Usage *usage.Recorder

	// RequestLog 请求指标与脱敏 JSONL 归档（可选；nil = 不记录）。
	RequestLog *reqlog.Recorder

	// RecordClientInfo 是否在请求日志里记录调用来源（客户端 IP / User-Agent）。
	// 来自 logging.request_client_info（缺省 true）；关闭时 reqlog 事件的来源字段
	// 保持为空，归档与面板都不出现来源信息。
	RecordClientInfo bool

	// Search 服务端 web_search 工具（/v1/messages）的检索后端（可选；nil =
	// 该服务端工具被剥离、模型照常作答）。
	Search websearch.Searcher
	// SearchMaxResults 单次检索返回条数上限（缺省 5）。
	SearchMaxResults int
	// SearchTimeout 单次检索超时（缺省 10s）。
	SearchTimeout time.Duration

	// MaxBodyBytes 入站请求体上限（字节，来自 config server.max_body_mb）。
	// 0 = 不限制（缺省：与 max_body_mb 移除后的现状完全一致，读 body 路径零差异）。
	// >0 时仅在 /v1/chat/completions 与 /v1/messages（含 count_tokens）读 body 前
	// 套 http.MaxBytesReader，超限就地 413——是纯 opt-in 的防护旋钮，不是默认行为。
	MaxBodyBytes int64
}

// loadLive 返回当前运行期快照；Live 为 nil 时用静态字段合成。
func (h *Handler) loadLive() livecfg.Snapshot {
	if h.cfg.Live != nil {
		return h.cfg.Live.Load()
	}
	return livecfg.Snapshot{
		APIKey:           h.cfg.APIKey,
		SoftCooldown:     h.cfg.SoftCooldown,
		RecordClientInfo: h.cfg.RecordClientInfo,
		HiddenModels:     livecfg.NormalizeHiddenModels(h.cfg.ModelsHidden),
		ModelAliases:     livecfg.NormalizeModelAliases(h.cfg.ModelsAliases),
	}
}

// hiddenModel 报告 (realm, 裸模型名) 是否被 config models.hidden 隐藏。
//
// 黑名单语义是「这个模型对本网关不存在」：列表不列、点名调用直接拒绝。生效点
// （cn 对话列表 / global 对话列表 / chat 点名）共用本判据，避免各写一份 map 查表
// 而漏掉某一处——漏一处就等于黑名单可被绕过（只藏列表不够，知道名字仍可硬调）。
func (h *Handler) hiddenModel(realm, bare string) bool {
	if bare == "" {
		return false
	}
	_, hide := h.loadLive().HiddenModels[realm+":"+bare]
	return hide
}

// modelAlias 查 aliases 映射：客户端名（剥 realm 前缀后的裸名，大小写不敏感）
// 命中映射 → 返回真实模型名（原样，可再带 realm 前缀跨域指向）；
// 未命中 → ("", false)。查找键与 livecfg.NormalizeModelAliases 的归一化口径配对：
// realm+":"+小写裸名。
func (h *Handler) modelAlias(realm, bare string) (string, bool) {
	if bare == "" {
		return "", false
	}
	target, ok := h.loadLive().ModelAliases[strings.ToLower(realm)+":"+strings.ToLower(bare)]
	return target, ok
}

// modelFallbackTo 查模型降级路由表（pool.model_fallback，misakano PR #52 同语义）：
// cur 模型命中等价模型 → 返回目标；未配置 / 目标与当前相同 → 空串。
// 查找键先原样再小写（配置键容忍大小写书写差异），只对裸名（无前缀）生效。
func (h *Handler) modelFallbackTo(cur string) string {
	if cur == "" {
		return ""
	}
	m := h.loadLive().ModelFallback
	if len(m) == 0 {
		return ""
	}
	if to, ok := m[cur]; ok && to != "" && to != cur {
		return to
	}
	if to, ok := m[strings.ToLower(cur)]; ok && to != "" && to != cur {
		return to
	}
	return ""
}

// softCooldown 返回当前生效的软冷却基数（热改优先，<=0 回退默认）。
func (h *Handler) softCooldown() time.Duration {
	if d := h.loadLive().SoftCooldown; d > 0 {
		return d
	}
	if h.cfg.SoftCooldown > 0 {
		return h.cfg.SoftCooldown
	}
	return 600 * time.Second
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "workbuddy2api"

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP wafIPGate
	// upstreamQueue 上游排队门控（fail-fast，queuegate.go）：6020/6021 免费档排队
	// 是上游服务端队列水位（与账号、模型都无关）→ 首次命中即 fail-fast 终止轮转，
	// 账号侧零惩罚。进程内状态、重启清零；任一请求成功即清除。
	upstreamQueue queueGate
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "custom" // 缺省 custom：网关自有提示词
	}
	if cfg.SearchMaxResults <= 0 {
		cfg.SearchMaxResults = 5
	}
	if cfg.SearchTimeout <= 0 {
		cfg.SearchTimeout = 10 * time.Second
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	// Anthropic Messages API 兼容层（ZCode/Claude Code 等 anthropic 协议客户端；
	// web_search 服务端工具见 anthropic_websearch.go）。count_tokens 为估算实现。
	h.mux.HandleFunc("POST /v1/messages", h.withAuth(h.messagesAnthropic))
	h.mux.HandleFunc("POST /v1/messages/count_tokens", h.withAuth(h.countTokensAnthropic))
	if cfg.Panel != nil {
		h.mux.Handle("/panel/", cfg.Panel) // /panel → /panel/ 由 ServeMux 自动重定向
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.cfg.RequestLog != nil && r.Method == http.MethodPost &&
		(r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/messages") {
		trace := &requestTrace{id: reqlog.NewRequestID(), start: time.Now()}
		if h.loadLive().RecordClientInfo {
			trace.captureClientInfo(r)
		}
		r = r.WithContext(context.WithValue(r.Context(), requestTraceKey{}, trace))
		obs := &responseObserver{ResponseWriter: w}
		w.Header().Set("X-Request-Id", trace.id)
		h.cfg.RequestLog.Begin()
		defer func() {
			status := obs.status
			if status == 0 {
				status = http.StatusOK
			}
			h.cfg.RequestLog.Record(trace.event(status))
		}()
		h.mux.ServeHTTP(obs, r)
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyAny(r, h.loadLive().APIKey) {
			// Anthropic 面用 Anthropic 错误体（Claude Code / ZCode 按其协议解析）。
			if strings.HasPrefix(r.URL.Path, "/v1/messages") {
				writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
					"missing or invalid API key (send x-api-key or Authorization: Bearer)")
				return
			}
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// sanitizePassthroughHeader 入站透传头白名单：8-128 个可打印 ASCII（0x21-0x7E）。
// 空串/超短/超长/含空白控制字符 → 返回空（调用方走生成/兜底路径）。
func sanitizePassthroughHeader(v string) string {
	if len(v) < 8 || len(v) > 128 {
		return ""
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x21 || v[i] > 0x7E {
			return ""
		}
	}
	return v
}

// requestBody 返回入站 body 的读取源：limit<=0（缺省）时原样返回 r.Body，
// 与「网关侧无上限」的既有读法完全一致；limit>0 时才套 http.MaxBytesReader，
// 读超限返回 *http.MaxBytesError（调用方据此回 413）。
func requestBody(w http.ResponseWriter, r *http.Request, limit int64) io.Reader {
	if limit <= 0 {
		return r.Body
	}
	return http.MaxBytesReader(w, r.Body, limit)
}

// isBodyTooLarge 报告读 body 的错误是否来自 requestBody 的上限（区别于客户端断流）。
func isBodyTooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// bodyTooLargeMsg 413 文案：带配置项名与生效的 MB 值，便于用户定位到旋钮。
func (h *Handler) bodyTooLargeMsg() string {
	return fmt.Sprintf("request body too large: limit %d MB (server.max_body_mb)", h.cfg.MaxBodyBytes>>20)
}

// tokensPerSecond 计算吐字速率（token/s），返回 (速率, 是否有意义)。
//
// 分母用「生成耗时」= 端到端耗时 - 首 token 等待（TTFB），不是端到端耗时。
//
// 为什么要减：不减的话，首 token 等待越长、报告速率被压得越低。同一模型换个
// 上游或网络，TTFB 从 0.3s 涨到 2s，速率能凭空掉一半——读起来像"模型变慢了"，
// 其实只是排队久了。issue #34 报的正是这个，维护者也确认「速率计算时并没有减去
// 首token到达时间」。
//
// ttfb<=0 表示没有观测：非流式回复天然没有「首个 data 帧」（日志里那一列记的是
// "-"）。此时不猜、不扣——凭空假定一个 TTFB 会把分母推向零、把速率抬成虚高，
// 比不扣更糟。只有真测到才扣。
//
// 同理，ttfb 不小于总耗时时（时钟粒度、或 TTFB 落在计时终点之后）退回端到端耗时，
// 避免零/负分母。token 数为负哨兵值（-1 = 观测缺失）时返回 false。
//
// 用量账本（handler）与控制台流水行（logging.go）都走这一个函数：两处各算一遍时
// 口径漂移过一次（流水行漏扣 TTFB、与面板数字对不上），共用是防再次分叉的唯一办法。
func tokensPerSecond(completionTokens int64, total, ttfb time.Duration) (float64, bool) {
	if completionTokens < 0 || total <= 0 {
		return 0, false
	}
	gen := total
	if ttfb > 0 {
		if g := total - ttfb; g > 0 {
			gen = g
		}
	}
	return float64(completionTokens) / gen.Seconds(), true
}
