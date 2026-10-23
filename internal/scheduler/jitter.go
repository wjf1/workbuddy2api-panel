// 保号任务时点抖动（v1.16.0 M5）：消除「整点齐射」风控特征。
//
// 背景：2026-10-23 六个账号 11:41 一次批量刷新后 RT 链被上游批量吊销，且 17 个
// 账号每日 22:00:00 同一秒齐射 refresh、9/21 点齐射签到——批量动作特征醒目。
// 抖动把每号打散到 jitter_minutes 窗口内执行（默认 20 分钟），偏移由
// hash(uid|日期) 决定：同号同日稳定（重跑不换位）、跨号分散、跨日轮换。
//
// 语义边界：
//   - 只作用于**定时排程**触发（runBatch 派发的五类任务：签到/旅行/活跃/保活/
//     夜猫子）；面板手动按钮仍走 RunXxxNow（运维语义是"现在就做"，不抖）。
//   - schedule.jitter_minutes=0 关闭（回到现状齐射）。
//   - 抖动等待可被 ctx 取消（优雅停机不必等睡满）。
//   - 成长任务（GrowthHook 队列）不抖：已是串行队列消费。
package scheduler

import (
	"context"
	"hash/fnv"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// jitterOffset 计算账号在 day（CST YYYY-MM-DD）的抖动偏移，范围 [0, maxMin*60s)。
// fnv32(uid|day) 稳定可复现：同号同日恒等，跨号均匀分散（fnv 雪崩足够）。
func jitterOffset(uid, day string, maxMin int) time.Duration {
	if maxMin <= 0 || uid == "" {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(uid))
	_, _ = h.Write([]byte("|"))
	_, _ = h.Write([]byte(day))
	return time.Duration(h.Sum32()%uint32(maxMin*60)) * time.Second
}

// jitterMin 快照读取抖动窗口（schedMu 下）。
func (s *Scheduler) jitterMin() int {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.jitterMinutes
}

// jitterWaitFor 返回 uid 从 start 起应等待的时长：offset - elapsed（保证每号
// 在任务起点的第 offset 秒执行，避免顺序累计漂移）；不为负。jitter 关闭返回 0。
func (s *Scheduler) jitterWaitFor(uid string, start time.Time) time.Duration {
	j := s.jitterMin()
	if j <= 0 {
		return 0
	}
	wait := jitterOffset(uid, cst.Now().Format("2006-01-02"), j) - time.Since(start)
	if wait <= 0 {
		return 0
	}
	return wait
}

// sleepJitter 等到 uid 的抖动偏移点（相对 start）；ctx 取消返回 false。
// 偏移 > 0 时打一行派发日志（排查「任务几点跑的」时对得上号）。
func (s *Scheduler) sleepJitter(ctx context.Context, uid, nickname string, start time.Time) bool {
	wait := s.jitterWaitFor(uid, start)
	if wait > 30*time.Second {
		log.Printf("jitter %s: 延迟 %s 后执行（打散整点齐射）", logfmt.Label(uid, nickname), wait.Round(time.Second))
	}
	return sleepCtx(ctx, wait)
}
