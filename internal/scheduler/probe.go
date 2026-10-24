// 死号探活（v1.16.0 M8）：对「会话失效」类禁用号低频 refresh 探测，捕捉上游
// 误吊销解除 / 会话恢复的窗口，命中即自动复活 + webhook 告知。默认关闭
// （schedule.probe_dead_hours 空）——对已判死账号的周期性探测请求本身是上游
// 可观测特征，由用户显式权衡开启。
//
// 范围与节奏：
//   - 只探 dead_kind == "session_dead" 的禁用号；banned（11140 风控封号）不探
//     ——封号不是会话态，重试只会撞墙。
//   - 号间随机 5–11s（避免探测自己又排成整齐队列）。
//   - 探测成功 = refresh 端点接受 = 会话已恢复 → ReviveDisabled（不动冷却/熔断，
//     仅清禁用与计数）+ 落盘 + WARN 日志 + onRecovered 回调（main 注入 → alert
//     webhook event=dead_recovered）。失败静默（debug 语义，次日再探）。
//   - 定时驱动复用 StartBalanceRefresh 的分钟级循环（driveProbeDead 按 CST 小时
//     命中 probe_dead_hours 触发，每号每日至多一轮）；面板手动触发走
//     POST /panel/api/probe_dead_all。
package scheduler

import (
	"context"
	"log"
	"math/rand/v2"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// probeAccountDelayMin/Max 探活号间随机间隔。
const (
	probeAccountDelayMin = 5 * time.Second
	probeAccountDelayMax = 11 * time.Second
)

// SetRecoveredHook 挂载死号复活回调（panel/main 装配：→ alert webhook）。
func (s *Scheduler) SetRecoveredHook(fn func(uid, nickname string)) {
	s.schedMu.Lock()
	s.cfg.RecoveredHook = fn
	s.schedMu.Unlock()
}

// RunProbeDeadNow 立即对全部 session dead 禁用号探测一轮（面板手动触发同口径）。
// 返回探测账号数。ctx 取消时立即放弃剩余账号（优雅停机不必等号间 sleep 睡满——
// 死号多时一探几分钟，此前不可取消还同步阻塞余额刷新循环里的 driveProbeDead）。
func (s *Scheduler) RunProbeDeadNow() int {
	return s.runProbeDead(context.Background())
}

func (s *Scheduler) runProbeDead(ctx context.Context) int {
	n := 0
	for _, st := range s.cfg.Pool.List() {
		if !st.Disabled || st.DeadKind != "session_dead" {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if n > 0 {
			if !sleepCtx(ctx, probeAccountDelayMin+time.Duration(rand.Int64N(int64(probeAccountDelayMax-probeAccountDelayMin)))) {
				return n
			}
		}
		n++
		s.probeOne(st, a)
	}
	return n
}

// probeOne 单号探测：refresh 成功即复活。
func (s *Scheduler) probeOne(st pool.Status, a *auth.Auth) {
	if err := s.cfg.Upstream.RefreshToken(a, "probe"); err != nil {
		// 失败静默：死号探测失败是预期态（长期全失效时每号每小时一行是固定噪声，
		// 收尾已有「本小时探测 N 个死号」汇总行，这里不再逐号打）。
		return
	}
	// 上游接受了刷新：会话已恢复（此前误吊销解除）。仅清禁用与 12153 计数，
	// 不动冷却/熔断观测（与 ReviveDisabled 口径一致）。
	s.cfg.Pool.ClearSessionDead(st.UID)
	s.cfg.Pool.ReviveDisabled(st.UID)
	if err := a.SaveAtomicRetry(); err != nil {
		log.Printf("probe %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
	}
	log.Printf("probe %s: 死号复活——上游已接受刷新，账号恢复可选（此前 reason=%q）",
		logfmt.Label(st.UID, st.Nickname), st.DisabledReason)
	s.schedMu.Lock()
	hook := s.cfg.RecoveredHook
	s.schedMu.Unlock()
	if hook != nil {
		hook(st.UID, st.Nickname)
	}
}

// driveProbeDead 定时驱动（StartBalanceRefresh 每轮调用）：当前 CST 小时命中
// probe_dead_hours 且该小时未跑过时执行一轮。
func (s *Scheduler) driveProbeDead() {
	hours := s.probeDeadHours()
	if len(hours) == 0 {
		return
	}
	now := cst.Now()
	hour, day := now.Hour(), cst.Day(now)
	hit := false
	for _, h := range hours {
		if h == hour {
			hit = true
			break
		}
	}
	if !hit {
		return
	}
	s.mu.Lock()
	if s.probeLastRun == day+"|"+itoa(hour) {
		s.mu.Unlock()
		return
	}
	s.probeLastRun = day + "|" + itoa(hour)
	s.mu.Unlock()

	// 异步执行：探活每号 5-11s 间隔，同步跑会阻塞余额刷新循环与本循环里的保活
	// 退避重试（死号多时一探几分钟）。
	go func(h int) {
		if n := s.runProbeDead(s.ctxRef()); n > 0 {
			log.Printf("probe: 本小时探测 %d 个死号（probe_dead_hours 命中 %d 点）", n, h)
		}
	}(hour)
}

// ctxRef 返回调度器生命周期 ctx（目前 driveProbeDead 由余额循环驱动、无直接 ctx
// 可用；进程退出即整体终止，此处用 Background 即可——sleepCtx 的可取消性主要
// 服务手动触发路径的演进，保持签名以备接线）。
func (s *Scheduler) ctxRef() context.Context { return context.Background() }

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
