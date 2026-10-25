// handler_chat.go /v1/chat/completions 主链路：轮转换号、降级重试、模型改写与
// fallback，以及该链路专属的用量统计与错误文案辅助。
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/prompt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	// 客户端 IP 提取（按请求传递到 ChatStream，不透传时 upstream 侧忽略）；
	// 消除早年共享字段方案的并发交叉污染（issue：ClientIP 竞态）。
	clientIP := upstream.ExtractClientIP(r)
	// 请求体缺省无大小上限（max_body_mb 缺省 0，对齐上游）：完整读入，超限类问题
	// 交由上游自然返回错误（其响应经既有错误分类链路透出，信息量更大）。仅当
	// server.max_body_mb>0 显式开启时才套 MaxBytesReader 就地 413。#41 的截断
	// 防御语义保留在读错误路径——未开上限时截断只可能来自客户端自己断流，
	// 读 body 出错就地 400，不把半截 JSON 喂上游 unmarshal 报 unexpected EOF 冤枉罚号。
	body, err := io.ReadAll(requestBody(w, r, h.cfg.MaxBodyBytes))
	if err != nil {
		if isBodyTooLarge(err) {
			log.Printf("WARN: [server] reject oversized body: path=%s limit=%dMB ip=%s", r.URL.Path, h.cfg.MaxBodyBytes>>20, clientIP)
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", h.bodyTooLargeMsg())
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
		// Tools 只用来数「客户端声明了几个工具」（issue #44 静默回合判据之一），
		// 条目内容本身不用，声明了才解析、随请求结束即回收。
		Tools []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &peek)

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, bareModel := resolveModel(peek.Model)

	// models.aliases 映射：客户端名命中映射 → 出站/选号/effort 全部改用目标名，
	// realm 也按目标重解析（别名值可带前缀跨域指向）。
	// 放在 hidden 检查**之前**：被映射到的目标仍受黑名单约束，别名不能绕过隐藏。
	// 出站 body 同步重写顶层 model（下方的 rewriteModel 只剥前缀，不认识别名——
	// 名字必须在这里就换掉）。未命中 → body 一个字节不动（现状零回归）。
	// modelEcho 非空 = 本请求走了映射，响应侧（SSE 帧 / 聚合体）把 model 覆写回
	// 客户端原发名——严格校验「响应 model == 请求 model」的客户端不会当场报错。
	// routeModel = 选号/粘性/账本使用的模型名（映射后为真实名，可带 realm 前缀）。
	modelEcho := ""
	routeModel := peek.Model
	if aliasTarget, aliased := h.modelAlias(realm, bareModel); aliased && aliasTarget != "" {
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			if cur, isStr := obj["model"].(string); isStr && cur == peek.Model {
				obj["model"] = aliasTarget
				if nb, merr := json.Marshal(obj); merr == nil {
					body = nb
					realm, bareModel = resolveModel(aliasTarget)
					modelEcho = peek.Model
					routeModel = aliasTarget
					log.Printf("INFO: [server] model alias applied: %s -> %s", peek.Model, aliasTarget)
				}
			}
		}
	}

	modelRate := ""
	if h.cfg.Upstream != nil {
		modelRate = h.cfg.Upstream.ModelRate(realm, bareModel)
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	// 模型名从（已映射的）body 取：映射请求的流水行显示真实路由名，与账本/上游
	// 口径一致（别名 INFO 行记录 客户端名 -> 真实名，两行合看即全链路）。
	st := newChatStat(time.Now(), body, peek.Stream)
	if tr := requestTraceFrom(r); tr != nil {
		// web_search 多轮编排：同一 trace 的前几轮 stat 此前被直接覆盖，归档事件
		// 只反映最后一轮（前几轮 token/credit/attempts 丢失）。改为累加合并——
		// 用量与尝试数是多轮总和；最终状态（outcome/status/ttfb）以最后一轮为准。
		if prev := tr.stat; prev != nil {
			st.attempts += prev.attempts
			st.promptTokens += prev.promptTokens
			st.completionTokens += prev.completionTokens
			st.totalTokens += prev.totalTokens
			st.credit += prev.credit
			st.hasCredit = st.hasCredit || prev.hasCredit
		}
		tr.stat = st
		// 来源在 ServeHTTP 入口采集（此时才知道开关与请求头），此处转交给统计对象，
		// 让 stdout 流水行与归档事件共用同一份来源值，两处不会漂移。
		st.clientIP, st.userAgent = tr.clientIP, tr.userAgent
	}
	defer st.done()

	// models.hidden 黑名单：点名调用直接拒绝，不打上游、不占选号。
	// 与 /v1/models 的列表过滤对称（只藏列表不够，知道名字仍可硬调）。
	// realm/bareModel 此处已是**映射后**的真实名——别名不能绕过黑名单。
	if h.hiddenModel(realm, bareModel) {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "model_unavailable",
			"model is hidden by gateway configuration, try another model", "")
		st.status = http.StatusBadRequest
		st.outcome = reqlog.OutcomeHTTPError
		return
	}

	tried := map[string]bool{}
	var lastErr error
	// modelBlock 记录本次是否因「模型级冷却」而选不到号（下方 acct==nil 分支填充）。
	// 默认零值 Blocked=false = 按既有的"没有可用账号"口径报错。
	var modelBlock pool.ModelBlockStatus
	// queueBusy 记录本次是否被上游排队门控 fail-fast 挡住（门控激活期内不发上游请求，
	// 直接回 429 upstream_queue_busy）。零值 = 未被挡。
	var queueBusy bool
	var queueInfo upstream.QueueInfo

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// ExtractKey 与粘性开关解耦（issue #35 侧）：关闭粘性时会话头族的聚合主键仍按
	// 会话级（RequestIDForKey(sessKey)），不悄悄退化成轮级——提取本身与粘性无关。
	sessKey := session.ExtractKey(body)
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		// 按模型解析：绑定号在**当前模型**被 6004 限额时视为不可用 → 重新分配，
		// 而不是钉在限额号上反复失败（"限额后换不动号"的正解）。
		// 用 routeModel 而非 peek.Model：映射后必须按真实模型的可用性口径判绑定，
		// 别名名不在任何账本里。
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, routeModel); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，
	// 换 user 消息换键）。#170 起带会话键的客户端也统一走轮级（对齐官方桌面 CLI
	// 的 X-Conversation-Request-ID 轮级语义——TraceStartHook 每次 USER_PROMPT_SUBMIT
	// 清空重生成），故不再限 sessKey=="" 才计算；sessKey 由下方派生处以复合键方式
	// 入键（防不同会话同轮文本互撞）。
	// 必须在下方 prompt.Rewrite 之前取——改写会动 messages 内容，之后取会让键漂移。
	turnKey := session.TurnKey(body)

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。
	reqHasImage := hasImagePart(body)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(sessKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}
	// 模型降级（pool.model_fallback，misakano PR #52 同语义）：curModel 是**本轮
	// 实际使用**的模型，初值 = bareModel；某账号在本轮模型上被 6004 模型级限流、
	// 且配置了等价模型时，本账号换用等价模型重试（curModel 改指向等价模型）。
	//
	// ⚠️ 为什么必须与 bareModel 分开：降级后二者指向**不同模型**，而它们要喂给不同
	// 用途——实际用途（选号 / 出站 body / 模型感知冷却豁免）→ curModel；冷却记账
	// （applyErrorPolicy 的 6004 分支）→ bareModel（**限流的是原模型**！误记成等价
	// 模型会把好好的等价模型也冷却掉，降级就白做了）。
	//
	// fallbackTried 记录本请求已试过的降级目标，防 A→B→A 循环（与 tried 按账号
	// 去重正交：降级是「同账号换模型」）。降级不写持久状态，冷却到期自动回原模型。
	curModel := bareModel
	fallbackTried := map[string]bool{}
	applyFallback := func(to string) bool {
		if to == "" || fallbackTried[to] {
			return false
		}
		fallbackTried[to] = true
		log.Printf("INFO: [server] model fallback: %s -> %s (6004 model rate limit)", curModel, to)
		curModel = to
		// 出站 body 重写：上游必须收到等价模型名。与 models.aliases 同一手法
		// （解析 → 改顶层 model → 重序列化）。解析失败时不动 body——那说明请求体
		// 本就不是合法 JSON，后续会按原样失败。
		var obj map[string]any
		if json.Unmarshal(body, &obj) == nil {
			obj["model"] = to
			if nb, merr := json.Marshal(obj); merr == nil {
				body = nb
			}
		}
		return true
	}

	// ttfb 首 token 等待（仅流式有观测；非流式传 0 = 无观测，速率不扣减）。
	// 显式入参而不是读 st.ttfb：后者在流式分支里是**调用之后**才赋值的，
	// 靠顺序传递会让将来重排代码时静默把速率算回旧的错口径。
	recordAttempt := func(uid string, delta pool.TokenUsageDelta, credit float64, hasCredit bool, started time.Time, ttfb time.Duration) {
		st.attempts++
		if delta.HasPromptTokens {
			st.promptTokens = delta.PromptTokens
		}
		if delta.HasCompletionTokens {
			st.completionTokens = delta.CompletionTokens
		}
		if delta.HasTotalTokens {
			st.totalTokens = delta.TotalTokens
		} else if delta.HasPromptTokens || delta.HasCompletionTokens {
			st.totalTokens = st.promptTokens + st.completionTokens
		}
		delta.Model = bareModel
		if delta.Model == "" {
			delta.Model = peek.Model
		}
		latency := time.Since(started)
		latencyMs := latency.Milliseconds()
		if latencyMs < 1 {
			latencyMs = 1
		}
		delta.HasLatencyMs = true
		delta.LatencyMs = latencyMs

		// HasCompletionTokens 是「本次有没有 token 观测」的独立判断，与速率怎么算
		// 无关，所以留在调用点；速率本身的边界（TTFB 缺失/超界）收在 tokensPerSecond。
		if delta.HasCompletionTokens {
			if tps, ok := tokensPerSecond(delta.CompletionTokens,
				time.Duration(latencyMs)*time.Millisecond, ttfb); ok {
				delta.HasTokensPerSecond = true
				delta.TokensPerSecond = tps
			}
		}
		h.cfg.Pool.RecordTokenUsage(uid, delta)
		// 每日限额护栏记账（v1.17.0 ⑤）：token 总量 + 积分消耗（×1000 milli）。
		// 只在有 token 观测时计（失败尝试不喂限额，与 usage 口径一致）。
		if delta.HasTotalTokens || delta.HasPromptTokens || delta.HasCompletionTokens {
			tok := delta.TotalTokens
			if !delta.HasTotalTokens {
				tok = delta.PromptTokens + delta.CompletionTokens
			}
			creditMilli := int64(0)
			if hasCredit && credit > 0 {
				creditMilli = int64(credit * 1000)
			}
			h.cfg.Pool.RecordDailyUsage(uid, delta.Model, tok, creditMilli)
		}

		// 用量时序记录。ok 以「上游是否给了 usage」判定：空 delta 意味着这次尝试
		// 没拿到任何 token 统计（传输错误 / >=400 / 解析失败），计为失败尝试。
		// 失败也计入请求数——否则重试放大在「用量」视图里看不见。
		if h.cfg.Usage != nil {
			realm := "cn"
			if a, ok := h.cfg.Pool.Status(uid); ok && a.Realm != "" {
				realm = a.Realm
			}
			h.cfg.Usage.Add(time.Now(), realm, uid, delta.Model, usage.Delta{
				PromptTokens:     delta.PromptTokens,
				HasPromptTokens:  delta.HasPromptTokens,
				CompletionTokens: delta.CompletionTokens,
				HasCompletion:    delta.HasCompletionTokens,
				TotalTokens:      delta.TotalTokens,
				HasTotal:         delta.HasTotalTokens,
				Credit:           credit,
				HasCredit:        hasCredit,
				HasCacheTokens:   st.hasCache,
				CacheHitTokens:   st.cacheHit,
				CacheMissTokens:  st.cacheMiss,
				ModelRate:        modelRate,
				LatencyMs:        delta.LatencyMs,
				HasLatency:       delta.HasLatencyMs,
				TokensPerSecond:  delta.TokensPerSecond,
				HasTPS:           delta.HasTokensPerSecond,
			}, delta.HasTotalTokens || delta.HasCompletionTokens || delta.HasPromptTokens)
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动
	//     （客户端项目规范/工具约定与网关提示词并用，issue #129）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough / append 非降级期：透传客户端原始 system（append 则再插一条网关 system）。
	// 降级裁决：append 在降级期退化为 replace（Rewrite(Degraded)）——append 带
	// 指纹原文重试是确定性再撞墙，replace 是一次性最小抢救（issue #129 设计 §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35）：后台按 X-Conversation-Request-ID（对话轮级）聚合请求，
	// 官方客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。此处**轮转
	// 循环外**生成一次，循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再
	// 碎片化（此前网关一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个
	// RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先，否则按
	//     粘性 key 进程内稳定生成；粘性 key 也空时走轮级兜底（TurnKey/TurnRequestID），
	//     无 user 消息时退化成本请求级随机——轮转内捕获一次即共享；
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	// 入站透传头白名单校验：客户端可控的任意串原样出站既是非官方指纹，异常值
	// （超长/控制字符）还可能让每次出站变 transport error 触发无谓轮转罚号。
	// 合法形态：8-128 个可打印 ASCII 字符；不合法一律丢弃走网关生成路径。
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body)}
	if v := sanitizePassthroughHeader(r.Header.Get("X-Conversation-Request-ID")); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞（#170 统一轮级）。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底（无 user 消息/无可签名内容）：会话级聚合，好于请求级随机。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = sanitizePassthroughHeader(r.Header.Get("X-Trace-ID"))

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 上游排队门控（queuegate.go）：门控激活期内**不发上游请求**直接回 429——
		// 队列堵着时任何账号、任何模型都会被上游拒收，发出去只是把一个请求放大
		// MaxRotate 倍再喂给已过载的上游，并把健康号白白推进冷却。这是「排队期间
		// 零成本止损」的关键一步（仅在轮转的第一跳检查即可：门控是进程级全局状态，
		// 同一请求内不会自愈）。
		if i == 0 {
			if active, _, info := h.upstreamQueue.snapshot(); active {
				queueBusy = true
				queueInfo = info
				st.status = http.StatusTooManyRequests
				break
			}
		}
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, curModel)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑，
				// 本次回落普通轮换。
				unbindSticky()
				acct = nil
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			acct = h.cfg.Pool.PickExcludingForRealm(tried, curModel, realm)
		}
		if acct == nil {
			// 模型降级兜底：全池该模型都处于 6004 冷却时 PickExcludingForRealm 返回
			// nil——若只在上游返回 6004 后降级，这里会**先报 503、根本走不到降级**，
			// 恰好在最需要降级的场景（全池被限流）失效。选不到号时也尝试换模型：
			// 锁定等价模型后重新进入下一轮选号（fallbackTried 防循环，MaxRotate 内）。
			if to := h.modelFallbackTo(curModel); to != "" {
				if applyFallback(to) {
					continue
				}
			}
			st.status = http.StatusServiceUnavailable
			// 记下"是不是模型级阻塞"。选号返回 nil 有两种完全不同的成因：
			//   (a) 池子真的没有可用号（账号级冷却/在途占满/积分保底）；
			//   (b) 号都在，但每个号都对这个模型处于模型级冷却（11102/6004）。
			// 两者此前都报 no_healthy_account，导致「模型不可用」被读成「号全挂了」。
			// 在这里取一次快照，供下方错误构造区分（issue #102 附带发现 1）。
			modelBlock = h.cfg.Pool.ModelBlocked(curModel)
			break
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.Nickname
		tried[acct.UID] = true

		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次 PickByUID 往返（语义与 fail()/PickByUID-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, r.Context()) {
				// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
				break
			}
			continue // 最后一个名额被并发抢走 → 换号
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshTokenIfNeeded(acct, h.cfg.RefreshSkew, "chat"); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（refresh 失败换号退避）
				}
				continue
			}
			if err := acct.SaveAtomicRetry(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		// 客户端 IP 按请求传递（PassthroughIP 开启时注入；消除共享字段竞态）。
		attemptStarted := time.Now()
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析）。传输层错误（非 *Error）走抖动换号分支；防御分支
		// （terr 为 nil 但 status>=400，如 ErrNone 兜底）回落本地 Classify，双保险。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 上游超时 / 停滞：**不换号、不罚号**。
			//
			// 超时不是账号的问题：同一份请求换到别的号，撞上的是同一个慢上游，
			// 只会把客户端拖到 MaxRotate × header_timeout（部署值 600s 时最坏
			// 约半小时），期间还给一串健康号喂连败计数。此前全仓没有任何超时
			// 识别，超时和"网络抖动"共用同一条换号路径。
			//
			// 判定三态：net.Error.Timeout()（ResponseHeaderTimeout / Client.Timeout）、
			// 显式 deadline（DeadlineExceeded / os.ErrDeadlineExceeded）、以及
			// **客户端仍在但 ctx 被取消**——那只能是我们自己的空闲看门狗掐的流，
			// 也就是上游停滞。客户端主动断连时 r.Context() 已取消，走下面的抖动分支。
			if isUpstreamTimeout(terr, r.Context().Err() != nil) {
				recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
				st.status = http.StatusServiceUnavailable
				lastErr = fmt.Errorf("%w: %v", errUpstreamTimeout, terr)
				log.Printf("WARN: [server] upstream timeout acct=%s: %v (rotation stopped, account not penalized)",
					logfmt.Label(acct.UID, acct.Nickname), terr)
				break
			}
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避）
			}
			continue
		}
		if status >= 400 {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append
			// 降级重试同样退化为 replace——原文在场只会确定性再撞 400）。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样），
				// 不再改写成网关固定文案——客户端必须看到真实错误才能排查。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				writeOpenAIErrorHint(w, httpStatusForKind(kind), "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = httpStatusForKind(kind)
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作，fail 只释放租约。
			// message 装上游 body 原文（含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				writeOpenAIErrorHint(w, httpStatusForKind(kind), "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = httpStatusForKind(kind)
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				writeOpenAIErrorHint(w, httpStatusForKind(kind), "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = httpStatusForKind(kind)
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 请求体解析失败（11101）：与 11115 / 图片无效同一哲学——同一 body 换任何
			// 账号都是同样的解析结果，轮转只会放大无效请求（每号一次上游调用 +
			// rotateBackoff 占用在途名额）。更要紧的是：继续轮转后末端会落到
			// 「其余保持 503」，把确定失败的请求伪装成"账号不可用、稍后再试"，
			// 客户端于是对必然失败的请求无限重试。立即透传上游原文回 400。
			if kind == upstream.ErrBadParams {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "chat request body was rejected by upstream"
				}
				writeOpenAIErrorHint(w, httpStatusForKind(kind), "bad_params", msg,
					h.hintOf(upstream.ErrBadParams, string(respBody), bareModel, reqHasImage, uerr))
				st.status = httpStatusForKind(kind)
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// 11133「model_param_invalid」：与 11101 / 11135 同为**请求级**错误——上游
			// 模型提供方拒绝了请求参数，同一 body 换任何账号都被同样拒绝（payload.go 已
			// 记录「schema 级拒绝换账号无用」）。轮转只会烧健康号配额，末端再落「其余
			// 保持 503」让客户端对必然失败的请求无限重试（2026-10-14 ZCode + cn:kimi-k3-1
			// 实案：连续换 3 号全撞 11133 → 末端 503 → ZCode 退避 11 次、单次 28s，表现为
			// 会话长时间无响应）。立即透传上游原文回 400 终止轮转。
			if kind == upstream.ErrModelParamInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "chat request parameters were rejected by upstream"
				}
				writeOpenAIErrorHint(w, httpStatusForKind(kind), "model_param_invalid", msg,
					h.hintOf(upstream.ErrModelParamInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = httpStatusForKind(kind)
				st.outcome = reqlog.OutcomeHTTPError
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			// 模型降级（仅 6004 模型级限流）：等价模型是同一底层能力的不同计价档，
			// 贵/便宜的那个被限流时改用另一个继续服务。**同账号**换模型（不换号，
			// 换模型比换号代价小）；冷却已按 bareModel（原模型）记上——误记成等价
			// 模型会把好好的等价模型也冷却掉，降级白做。11102 换模型救不了（等价
			// 模型可能同样没有）、账号级 429/5xx 换模型无意义，均不走此分支。
			if kind == upstream.ErrSoftRate && upstream.IsModelRateLimit(string(respBody)) {
				if to := h.modelFallbackTo(curModel); to != "" {
					if applyFallback(to) {
						// 同账号重试：把该账号从 tried 移除（降级是「换模型」不是
						// 「换号」，同一账号仍应有机会）；sticky 保持（本请求内不重绑）。
						delete(tried, acct.UID)
						releaseHeld()
						continue
					}
				}
			}
			fail(acct.UID)
			// 上游排队 fail-fast（优先于 WAF/rotateBackoff）：6020/6021 是上游服务端
			// 队列水位，换账号、换模型都不会绕过（实测两模型回同一排队码）——继续轮转
			// 只会把一个请求放大 MaxRotate 倍打已经堵住的上游。账号侧零惩罚
			// （applyErrorPolicy 的 ErrUpstreamQueue 分支不冷却/不熔断/不喂连败）。
			if kind == upstream.ErrUpstreamQueue {
				info, _ := upstream.ParseQueueInfo(string(respBody))
				h.upstreamQueue.noteQueue(acct.UID, info)
				break
			}
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义）：
			// 该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，IP 被拦而非账号）
			// 则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍打同一出口 IP，
			// 加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避）
			}
			continue
		}
		// 成功判定与粘性绑定一律**延后到这一跳真正成功之后**（见下方流式/非流式分支）：
		// 上游「200 已开流 + 一帧 error」是真实形态（6004 限流、内容拦截、审核），
		// 此前在读第一帧之前就 NoteSuccess + 清 11102 负缓存 + 绑粘性 → 被限流的号
		// 记成健康、粘性把会话钉死在它身上，后续每一轮都打同一个限流号。
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			// errFrame：上游 error 帧原文（观察者旁路采集），用于流尾的账号处置。
			var errFrame string
			sErr := upstream.StreamHint(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}), upstream.WithErrorFrameObserver(func(payload string) { errFrame = payload }),
				// 模型名回显（models.aliases）：映射请求的帧内 model 覆写回客户端
				// 原发名；modelEcho 为空（未映射）时 StreamHint 侧零改动。
				upstream.WithModelEcho(modelEcho))
			switch {
			case upstream.IsEmptyStreamError(sErr):
				// 上游 200 但空流（0 有效帧）：StreamHint 已写 error 帧 + [DONE]
				// 兜底（HTTP 头已发出只能 200），但这是上游缺陷不是成功——日志/
				// 状态收敛到 502 观测，与非流式 Aggregate 空流→502 upstream_parse
				// 同语义（此前 `_ =` 吞错把失败流记成 200，运维看到假成功）。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			case errFrame != "":
				// 上游以 error 帧报错（6004 限流 / 内容拦截 / 审核）：按帧内容分类并
				// 处置账号——**不记成功、不清 11102 负缓存、不绑粘性**。此前这些动作
				// 在流开始前就做了，于是一个正在限流的号被当成健康号，粘性还会把
				// 整个会话钉在它身上，后续每轮都失败。
				kind := upstream.FrameKind(errFrame)
				h.applyErrorPolicy(acct.UID, kind, errFrame, bareModel, nil)
				st.status = http.StatusServiceUnavailable
				st.outcome = reqlog.OutcomeStreamError
				log.Printf("WARN: [server] stream acct=%s model=%s: upstream error frame kind=%s payload=%s",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, kind, logfmt.Truncate(errFrame, 200))
			case sErr != nil:
				// 客户端写失败（断连）：上游帧无恙，账号健康——账号侧照常记成功
				// （与 default 同语义），请求日志归为 Interrupted（人已走，未完成）。
				st.outcome = reqlog.OutcomeInterrupted
				h.cfg.Pool.NoteSuccess(acct.UID)
				h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
				if sessKey != "" && h.cfg.Session != nil {
					h.cfg.Session.Bind(sessKey, acct.UID)
				}
			default:
				// 真成功：这一跳读完且上游没有报错，才记成功并让粘性跟上。
				st.outcome = reqlog.OutcomeSuccess
				// 上游能出完整流 → 队列已疏通，清除排队门控（不在恢复后继续拦请求）。
				h.upstreamQueue.clear()
				h.cfg.Pool.NoteSuccess(acct.UID)
				// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
				// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
				h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
				// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
				// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
				if sessKey != "" && h.cfg.Session != nil {
					h.cfg.Session.Bind(sessKey, acct.UID)
				}
			}
			// issue #44：声明了 tools 却拿到 finish_reason=stop 且全流零 tool_calls——
			// 上游把这个形态当正常收尾，客户端据此认为回合完成、工具一次没跑，用户只能
			// 手动催一句。此前网关侧完全无痕（这是 #149 / #44 定位困难的主因），这里
			// **只记日志、不改流**：模型「判断不需要工具、直接回答」是合法行为，强行
			// 改写成可重试错误会让这类正常回合反复重试，每次都真实计费。
			if isSilentToolStop(len(peek.Tools), stats.SawToolCalls(), stats.FinishReason()) {
				log.Printf("WARN: [server] stream acct=%s model=%s: finish_reason=stop with zero tool_calls despite %d declared tool(s) (silent stop; client may treat the turn as complete)",
					logfmt.Label(acct.UID, acct.Nickname), bareModel, len(peek.Tools))
			}
			credit, hasCredit := stats.Credit()
			if hit, miss, ok := stats.CacheTokens(); ok {
				st.cacheHit, st.cacheMiss, st.hasCache = hit, miss, true
			}
			recordAttempt(acct.UID, stats.Usage(), credit, hasCredit, attemptStarted, stats.TTFB())
			// WARN 信号放 recordAttempt 之后：st.promptTokens 此时才是本次的观测值。
			if st.hasCache {
				cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
			}
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			if toks, hasUsage := stats.Tokens(); hasUsage {
				st.toks = toks
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if hasCredit {
				st.credit = credit
				st.hasCredit = true
				if total, tok := stats.TotalTokens(); tok && total > 0 {
					h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
				}
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			recordAttempt(acct.UID, pool.TokenUsageDelta{}, 0, false, attemptStarted, 0)
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			st.outcome = reqlog.OutcomeHTTPError
			return
		}
		credit, total, hasCredit := usageCreditTotal(resp)
		if usage, ok := resp["usage"].(map[string]any); ok {
			if hit, okH := upstream.UsageCacheHitTokens(usage); okH {
				miss, _ := upstream.UsageCacheMissTokens(usage)
				st.cacheHit, st.cacheMiss, st.hasCache = int64(hit), int64(miss), true
			}
		}
		recordAttempt(acct.UID, usageDeltaFromResponse(resp), credit, hasCredit, attemptStarted, 0)
		if st.hasCache {
			cacheMissWarn.noteCacheTokens(bareModel, st.promptTokens, st.cacheHit, st.cacheMiss)
		}
		// 模型名回显（与流式 WithModelEcho 同语义）：聚合体的 model 覆写回客户端
		// 原发名，严格校验「响应 model == 请求 model」的客户端不当场报错。
		if modelEcho != "" {
			resp["model"] = modelEcho
		}
		writeJSON(w, http.StatusOK, resp)
		st.status = http.StatusOK
		st.outcome = reqlog.OutcomeSuccess
		st.toks = completionTokens(resp)
		// 非流式同理：聚合成功（无 error 帧、非空流）才算这一跳成功，事后才记成功/绑粘性。
		h.upstreamQueue.clear() // 上游已出完整响应 → 队列疏通，清除排队门控
		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		if sessKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if hasCredit {
			st.credit = credit
			st.hasCredit = true
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	// 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	// requestId 原样保留）。HTTP 状态码按 OpenAI 兼容口径映射类别：ErrSoftRate → 429
	// （限流语义、客户端应等待重试），其余保持 503。本地调度类错误（无可用账号/
	// 传输层抖动/非上游返回的 lastErr）→ 保留自有文案 no_healthy_account（本地错误
	// 没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// 上游排队门控挡下（本次根本没发上游请求）：回 429 + 独立 code，明确「上游队列忙」
	// 而非「账号全挂了」——面板与客户端据此不做换号/换模型的无谓动作（换了也一样堵）。
	// 若门控已被其他请求清除（上游已恢复），则不报排队，走下面的常规分支。
	if queueBusy {
		if _, _, info := h.upstreamQueue.snapshot(); info.Code == "" {
			queueBusy = false
		} else {
			queueInfo = info
		}
	}
	if queueBusy {
		// 状态码同样取自映射表（ERRUpstreamQueue → 429），与末端 UpstreamQueue 分支一致。
		qStatus := httpStatusForKind(upstream.ErrUpstreamQueue)
		writeOpenAIErrorHint(w, qStatus, "upstream_queue_busy",
			queueBusyMessage(queueInfo),
			upstream.GatewayHint(upstream.ErrUpstreamQueue, "", upstream.HintContext{}))
		st.status = qStatus
		st.outcome = reqlog.OutcomeHTTPError
		return
	}
	// 上游超时：轮转已在传输层分支止损（见 isUpstreamTimeout），这里给一条**能区分**
	// 的文案，别混进"没有可用账号"——两者的排查方向完全不同。
	if errors.Is(lastErr, errUpstreamTimeout) {
		code = "upstream_timeout"
		msg = "upstream timed out: rotation stopped (another account would hit the same slow upstream), please retry later"
	}
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定；本地调度类
	// 错误（无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	// upstreamMsgPassed 记录 error.message 是否已被上游原文占据：模型级阻塞分支
	// 据此决定要不要覆盖 msg（上游原文优先，含 requestId）。
	upstreamMsgPassed := false
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		// 状态码统一由 errmap.go 的 httpStatusForKind 决定（单一事实来源）；
		// 下面只组装 code 与文案——同类别在不同场景文案不同，但状态码必须一致。
		status = httpStatusForKind(ue.Kind)
		// 上游 requestId 与本地 rid 同时落台账 + 一行日志：用户报障给的是上游 ID，
		// 运维手里是 rid，这两者对上号才能让上游查（成功响应不含该字段，仅错误有）。
		if ue.RequestID != "" {
			st.upstreamRequestID = ue.RequestID
			rid := ""
			if tr := requestTraceFrom(r); tr != nil {
				rid = tr.id
			}
			log.Printf("WARN: [server] upstream requestId=%s rid=%s acct=%s model=%s kind=%s",
				ue.RequestID, rid, logfmt.Label(st.uid, st.nick), bareModel, ue.Kind)
		}
		switch ue.Kind {
		case upstream.ErrSoftRate:
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrUpstreamQueue:
			// 6020/6021 免费档排队：与账号无关（换号/换模型都不绕过），轮转已在命中处
			// fail-fast 终止。回 429（客户端"等一会儿再试"的语义与限流一致），但 code
			// 与 hint 明确区分「上游队列忙」而非「你的账号被限流」——否则面板与客户端
			// 都会读成账号问题（2026-10-15 全池误冷却的认知根源）。
			code = "upstream_queue_busy"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：网关出口 IP 被 WAF 拦截、
				// 轮转已止损、窗口过后自动解除。客户端提前重试无意义（换号不换 IP）；
				// 有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		case upstream.ErrModelBlocked:
			// 11102「该后端无此模型」：上游原文（下方统一透传）已经写清了原因，但
			// code 此前停在默认的 no_healthy_account —— 那是"服务端过载"的语义，
			// 客户端据此会不断重试（#81 里就是 503 → 客户端自动重试 5 次），而真正
			// 该做的是换个模型。用 400 + 明确的 code 把不可重试的性质讲清楚。
			code = "model_unavailable"
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
			upstreamMsgPassed = true
		}
	}
	// 模型级阻塞覆盖上面的通用文案（放在最后 = 优先级最高）。
	//
	// 为什么能覆盖 code 而不丢信息：modelBlock.Reason 就是 BlockModelBackoff 存下的
	// 上游原因，已在 hint 里原样带出，所以覆盖 message 不会丢掉上游信息，反而补上
	// 了上游不会告诉客户端的两件事——「有几个号被挡」和「最早什么时候解封」。
	//
	// 用 400 而非 503：这是"你选的这个模型当前不可用"，不是"服务端暂时过载"。
	// 换号重试必然同样失败，客户端不该按可重试错误处理。
	if modelBlock.Blocked {
		code = "model_unavailable"
		// 有上游原文时保留它（含 requestId，用户要拿去向上游反馈），只把"有几个号
		// 被挡、最早何时解封"这类本地调度信息放进 gateway_hint。
		if !upstreamMsgPassed {
			msg = "model is unavailable on every account (per-model cooldown), try another model"
		}
		hint = fmt.Sprintf("model_blocked: %d account(s) cooling down this model", modelBlock.Count)
		if !modelBlock.Until.IsZero() {
			hint += "; earliest unblock at " + modelBlock.Until.Format(time.RFC3339)
		}
		if s := strings.TrimSpace(modelBlock.Reason); s != "" {
			hint += "; upstream: " + s
		}
		status = http.StatusBadRequest
	}
	writeOpenAIErrorHint(w, status, code, msg, hint)
	st.status = status
	st.outcome = reqlog.OutcomeHTTPError
}

// queueBusyMessage 排队门控的客户端文案：把上游给的队列表态摊开（队前人数 /
// 队列规模 / 建议重试间隔），并明确「不是账号问题」——用户看到 429 的第一反应是
// 换号/换模型，而这两者对队列水位都无效（实测两模型回同一排队码）。
func queueBusyMessage(info upstream.QueueInfo) string {
	var b strings.Builder
	b.WriteString("upstream free-tier queue is busy (not an account problem;")
	b.WriteString(" switching accounts or models will not help)")
	if info.Position > 0 {
		fmt.Fprintf(&b, "; queue_position=%d", info.Position)
	}
	if info.Size != "" {
		fmt.Fprintf(&b, "; queue_size=%s", info.Size)
	}
	if info.RetryAfter > 0 {
		fmt.Fprintf(&b, "; retry_after=%ds", int(info.RetryAfter/time.Second))
	}
	return b.String()
}

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// usageCreditTotal 从聚合响应取 usage.credit 与 total_tokens（成本台账非流式入口）。
// 任一字段缺失/非法 → ok=false（不记录）。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	usage, _ := resp["usage"].(map[string]any)
	if usage == nil {
		return 0, 0, false
	}
	c, _ := usage["credit"].(float64)
	t, _ := usage["total_tokens"].(float64)
	if t <= 0 {
		return 0, 0, false
	}
	return c, int(t), true
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2）：第 i 次轮转失败
// （continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，±25% 抖动），
// ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转（客户端已走，
// 换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；正常单号请求
// （首次成功）不经过本函数，零开销。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// errUpstreamTimeout 上游超时的哨兵：末端出口据此给出与「没号可用」可区分的文案。
var errUpstreamTimeout = errors.New("upstream timeout")

// isUpstreamTimeout 判断这一跳的失败是否属于「上游超时 / 停滞」。超时不是账号的
// 问题，换号注定白换（同一份请求撞同一个慢上游），必须止损：不轮转、不罚号。
// 三态判定见传输层错误分支的注释。
func isUpstreamTimeout(err error, clientGone bool) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	return !clientGone && errors.Is(err, context.Canceled)
}
