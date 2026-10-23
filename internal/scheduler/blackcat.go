// blackcat.go 夜猫子任务执行器：23:00–08:00 窗口内对池内账号补足 glm-5.2 对话
// 并上报事件链（black_cat 判据）。窗口外触发则直接跳过（只观测，不报错）。
package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// RunBlackcatNow 对所有可用账号执行夜猫子对话补足（窗口外跳过）。
// 由 blackcat_hours 排程（默认 [23]）触发；执行前二次校验 InNightWindow。
func (s *Scheduler) RunBlackcatNow() {
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	for _, st := range s.cfg.Pool.List() {
		s.blackcatOne(st)
		time.Sleep(activityAccountDelay)
	}
}

// blackcatOne 单账号夜猫子补足（从 RunBlackcatNow 抽出）。
func (s *Scheduler) blackcatOne(st pool.Status) {
	// 暂停选号（paused）账号跳过：夜猫子是全任务体系中唯一「整任务都是
	// 真实模型对话」的（RunNightChats 逐条 ChatStream 发 glm-5.2 短对话），
	// 与「让位防风控」正面冲突。旅行/成长任务都是纯上报或领奖 RPC，照常跑。
	if st.Disabled || st.Paused {
		return
	}
	a := s.cfg.Pool.AuthByUID(st.UID)
	if a == nil || a.AccessTokenValue() == "" {
		return
	}
	if a.IsGlobal() {
		return // D4 门控：global 无 CN 任务体系，不发起任何上游调用
	}
	need, err := s.cfg.Upstream.BlackcatNeed(a)
	if err != nil {
		log.Printf("blackcat %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if need <= 0 {
		return
	}
	ok, err := s.cfg.Upstream.RunNightChats(a, int(need))
	if err != nil {
		log.Printf("blackcat %s: %d/%d 完成，中断: %v", logfmt.Label(a.UID, a.Nickname), ok, need, err)
		return
	}
	log.Printf("blackcat %s: 完成 %d 次夜间对话", logfmt.Label(a.UID, a.Nickname), ok)
}

// runBlackcatScheduled 定时夜猫子（M5 抖动版）：每号在自己的偏移点执行；
// 夜猫子窗口 23:00–08:00 很宽，抖动不挤压窗口。执行前校验窗口（手动路径同口径）。
func (s *Scheduler) runBlackcatScheduled(ctx context.Context) {
	if !upstream.InNightWindow(time.Now()) {
		log.Printf("blackcat: 当前不在 23:00–08:00 计数窗口，跳过")
		return
	}
	start := time.Now()
	for _, st := range s.cfg.Pool.List() {
		if !s.sleepJitter(ctx, st.UID, st.Nickname, start) {
			return
		}
		s.blackcatOne(st)
	}
}
