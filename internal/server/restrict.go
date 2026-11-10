// restrict.go 11140 风控连坐保护。
//
// 背景：上游把「账号存在异常行为，已被限制使用」（403 / code 11140 的
// "request illegal" 与 "unusual activity" 两种文案）判在**设备与出口**上，
// 不是判在单个账号上。本仓原有处置是「禁用这个号、立刻换下一个号继续同一请求」，
// 于是一个客户端请求能顺着轮转把池子挨个撞一遍——参照上游实测事故：
// 一次任务 7 个号全封。本仓 MaxRotate 默认 3，最坏一次请求连撞 3 个号，
// 风险方向一致、同源。
//
// 两道闸：
//  1. 命中 11140 就地停手（不换号）——applyErrorPolicy 返回 restricted=true，
//     chatCompletions 的轮转循环据此 break；
//  2. 同一窗口内 ≥ restrictionWaveMin 个**不同**账号被限制 → 判定为连坐扫荡，
//     全池暂停派发 restrictionHold，期间请求快速失败（503 + Retry-After），
//     不再拿账号去试。
//
// 与 wafip.go 的关系：两者都是「跨账号的进程级 fail-fast 状态机」，但判定信号
// 不同（WAF 403 非信封体 vs 11140 授权风控信封）互不干涉，各自独立记账。
// 归属层判断与 wafip 同：唯一消费者是 chatCompletions 的「是否继续轮转」，
// 属 server 而非 pool（pool 是账号级记账层，跨 UID 语义不属于任何账号）。
// 进程内状态、重启清零——「暂停」是「现在先别打」的意思，重启后重新判定即可。
package server

import (
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
)

// restrictionWaveWindow 连坐判定窗口；restrictionWaveMin 窗口内不同账号数阈值。
// var 仅供测试注入短窗（生产恒 15min / 2——单号反复 11140 归账号自己的禁用，
// 不触发全池暂停）。
var (
	restrictionWaveWindow = 15 * time.Minute
	restrictionWaveMin    = 2
)

// restrictionGate 连坐状态机（Handler 内嵌，零值可用）。
type restrictionGate struct {
	mu    sync.Mutex
	hits  map[string]time.Time // uid → 最近一次被限制的时刻（窗内，惰性剪枝）
	until time.Time            // 全池暂停截止；零值 = 未暂停
}

// note 记一次账号级风控限制，返回本次是否把全池暂停。
//
// hold <= 0 表示配置关掉了连坐保护（只保留「命中即停手」那道闸）。
// 同一 uid 重复命中只更新时间、不增计数（判定口径是「不同账号数」），避免一个号
// 被反复重试把暂停骗长；判定成立后清空窗口，暂停解除后需要全新的一轮才再触发。
func (g *restrictionGate) note(uid string, hold time.Duration) bool {
	if hold <= 0 {
		return false
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hits == nil {
		g.hits = map[string]time.Time{}
	}
	for u, t := range g.hits {
		if now.Sub(t) > restrictionWaveWindow {
			delete(g.hits, u)
		}
	}
	g.hits[uid] = now
	if len(g.hits) < restrictionWaveMin {
		return false
	}
	g.until = now.Add(hold)
	g.hits = map[string]time.Time{}
	logx.Warnf("[server] upstream restriction wave: %d accounts restricted within %s, dispatch paused %s (until %s)",
		restrictionWaveMin, restrictionWaveWindow, hold, g.until.Format(time.RFC3339))
	return true
}

// remaining 全池暂停的剩余时长（0 = 未暂停，可正常派发）。
func (g *restrictionGate) remaining() time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if d := time.Until(g.until); d > 0 {
		return d
	}
	return 0
}

// reset 立即解除（测试与将来面板「一键解冻」用）。
func (g *restrictionGate) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hits = nil
	g.until = time.Time{}
}
