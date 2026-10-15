// queuegate.go 上游排队（6020/6021）fail-fast 门控。
//
// 背景（2026-10-15 本机实案）：上游对免费档返回 HTTP 429 + 业务码排队语义——
// 6020 "queue.waiting.title"（已入队，带 queue_position/queue_size/retry_after）
// 与 6021 "queue.full.title"（队列已满）。响应还带 client_user_type:"free"。
// Classify 此前没有该码，落到 status==429 兜底 → 被当成「这个账号被限流了」逐号
// 软冷却：106 秒内 16/17 个账号全部进冷却（每个 600s/1200s/2400s 阶梯），而真凶
// 是上游队列积压，与账号毫无关系。
//
// 与 WAF IP 级门控（wafip.go）的差异及理由：WAF 403 单条响应无法区分「单号偶发」
// 与「出口 IP 被拦」，必须靠短窗多号计数取证；而排队码**单条响应就已自证**是队列
// 水位（上游显式给出 queue_position/queue_size，且与账号无关），故本门控首次命中
// 即激活，不做多号取证。账号侧零惩罚（applyErrorPolicy 不冷却、不熔断、不喂连败），
// 只有「是否继续轮转」这一个动作被改变。
//
// 自愈：门控期内 fail-fast；任一请求成功即清除门控（上游队列已疏通）——不在
// 恢复后继续拦着请求。进程内状态、重启清零（窗口短，重建成本极低）。
package server

import (
	"log"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// queueGateCooldownDefault 上游未给 retry_after 时的门控时长。
// 取 60s：与 wafIPWindow 同量级，短到不耽误恢复、长到足以让队列喘息。
const queueGateCooldownDefault = 60 * time.Second

// queueGateCooldownMax 门控时长上限：上游 retry_after 给得离谱（或 estimated_wait
// 极大）时钳到该值，避免一次异常响应把网关锁死几分钟。
const queueGateCooldownMax = 2 * time.Minute

// queueGateCooldownMin 门控时长下限：上游给的 retry_after 过小（如 0/1s）时不足
// 以避开队列窗口，抬到该值。
const queueGateCooldownMin = 15 * time.Second

// queueGate 上游排队门控状态机（Handler 内嵌，零值可用）。
type queueGate struct {
	mu    sync.Mutex
	until time.Time            // 门控截止；零值 = 未激活
	info  upstream.QueueInfo   // 最近一次排队响应携带的队列表态（展示用）
	hits  map[string]time.Time // uid → 最近一次命中时刻（观测/日志用，惰性剪枝）
}

// noteQueue 记一次上游排队响应，返回门控是否激活（调用方据此 fail-fast 终止轮转：
// 继续换号只会把同一请求再打一次已经堵住的上游）。
//
// uid 只用于观测（记录「哪些号撞到排队」，便于确认是整体性而非单号问题）。
// 首次命中即激活（理由见文件头）；激活期内再次命中按新信息刷新截止时刻。
func (g *queueGate) noteQueue(uid string, info upstream.QueueInfo) bool {
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	if uid != "" {
		g.hits[uid] = now
	}
	g.info = info
	d := g.durationLocked(info)
	next := now.Add(d)
	// 刷新语义：排队是滚动状态，每次命中都按最新 retry_after 顺延，但设总上限
	// （避免持续排队时门控无限延长——上限即「单次最长等待」）。
	if next.After(g.until) {
		g.until = next
	}
	if limit := now.Add(queueGateCooldownMax); g.until.After(limit) {
		g.until = limit
	}
	active := now.Before(g.until)
	if active {
		log.Printf("WARN: [server] upstream queue busy: code=%s position=%d size=%s retry_after=%s (account-level penalty skipped; rotation fail-fast until %s)",
			info.Code, info.Position, info.Size, info.RetryAfter, g.until.Format(time.RFC3339))
	}
	return active
}

// durationLocked 算本次门控时长：优先采信上游 retry_after（权威口径），
// 缺失时回落 estimated_wait，都没有才用默认值；结果钳到 [min, max]。
// 调用方必须已持有 g.mu。
func (g *queueGate) durationLocked(info upstream.QueueInfo) time.Duration {
	d := info.RetryAfter
	if d <= 0 {
		d = info.EstimatedWait
	}
	if d <= 0 {
		return queueGateCooldownDefault
	}
	if d < queueGateCooldownMin {
		return queueGateCooldownMin
	}
	if d > queueGateCooldownMax {
		return queueGateCooldownMax
	}
	return d
}

// clear 清除门控（任一请求成功即调用：上游队列已疏通，不再拦请求）。
func (g *queueGate) clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until.IsZero() && len(g.hits) == 0 {
		return
	}
	g.until = time.Time{}
	g.info = upstream.QueueInfo{}
	g.hits = nil
}

// snapshot 门控当前状态（面板/状态接口展示用）：是否激活、截止时刻、最近表态。
func (g *queueGate) snapshot() (active bool, until time.Time, info upstream.QueueInfo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Now().Before(g.until), g.until, g.info
}
