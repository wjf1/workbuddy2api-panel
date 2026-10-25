// handler_status.go /status 与 /healthz：账号/队列/台账状态聚合与探活。
package server

import (
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

// UpstreamQueueStatus 暴露上游排队门控当前状态（面板/运维观测用）。
// active=true 表示门控期内请求会被直接回 429 upstream_queue_busy（不发上游请求）。
func (h *Handler) UpstreamQueueStatus() (active bool, until time.Time, info upstream.QueueInfo) {
	return h.upstreamQueue.snapshot()
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// cost_explore 探索台账（issue #136 §5 可观测性）：累计探索事件数 + 各
	// (域, 模型) 的最近探索时刻（键 "realm|model"）。与 accounts[].model_costs
	// 行对照即可读出「探索→毕业」全链路（单一事实来源，不做双表示）。零回归只增键。
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	// api_key 为空（未启用鉴权，公开监听风险面）时脱敏 accounts：全量账号列表
	// （uid/昵称/模型成本台账）对任何能连到端口的人可见。计数汇总保留（够
	// 负载均衡/探活用），逐账号明细只对配了 key 的部署透出。
	accounts := h.cfg.Pool.List()
	if h.loadLive().APIKey == "" {
		accounts = nil
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       accounts,
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
		// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
		// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// credit_floor 生效的积分保底值（0 = 关闭）。与 accounts[].credits +
		// model_costs 对照即可判定「某号为何对某模型不出票」。零值也显式写出
		// （运维口径：缺失会让人误以为没记录）。
		"credit_floor": h.cfg.Pool.CreditFloor(),
		// upstream_queue 上游排队门控快照（激活时给出 until/code/position/size/
		// retry_after，未激活为 null）。排队与账号无关，独立字段避免与冷却混淆。
		"upstream_queue": h.upstreamQueueStatusPayload(),
		// 护栏拦截计数（v1.18.1 可观测）：保底/每日限额在 pick 主路径此前完全静默。
		"guards": h.cfg.Pool.Guards(),
		// state.json 落盘健康：last_ok（Unix 秒，0=未写过）+ last_error（空=无错）。
		"persist_health": func() map[string]any {
			okAt, lastErr := h.cfg.Pool.PersistHealth()
			return map[string]any{"last_ok": okAt, "last_error": lastErr}
		}(),
		// keepalive 可观测：smart 开关/阈值/退避表规模（面板装配注入 GetKeepaliveStats；
		// nil 时省略该键——handler 不直接依赖 scheduler 包，避免分层穿透）。
		"keepalive": func() any {
			if h.cfg.GetKeepaliveStats == nil {
				return nil
			}
			return h.cfg.GetKeepaliveStats()
		}(),
		// cost_explore 事件与 per-model 时间戳（时间值由 encoding/json 写 RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
	})
}

// upstreamQueueStatusPayload /status 的上游排队门控载荷（未激活为 null）。
func (h *Handler) upstreamQueueStatusPayload() any {
	active, until, info := h.upstreamQueue.snapshot()
	if !active {
		return nil
	}
	out := map[string]any{"active": true, "until": until.Format(time.RFC3339)}
	if info.Code != "" {
		out["code"] = info.Code
	}
	if info.Position > 0 {
		out["position"] = info.Position
	}
	if info.Size != "" {
		out["size"] = info.Size
	}
	if info.RetryAfter > 0 {
		out["retry_after"] = int(info.RetryAfter.Seconds())
	}
	return out
}

// countsMapFrom 把 CountsDetailed 五元组打包成 /status 的域分组建模。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}
