// admin_tasks.go 手动补跑排程。POST /admin/tasks/{name}/run，立刻 202，后台执行。
//
// 用途：排程是按整点跑的（见 schedule.*_hours），错过一次（进程重启、机器休眠、
// 上游临时故障）就得等下一个整点。这个端点让你能立刻补跑一次，不必改配置或重启。
//
// 与面板既有「任务中心手动触发」的关系：面板走的是 panel 包装层的接口；本端点挂在
// 主 mux 上，供运维脚本 / curl 直接调用（同一 api_key 鉴权，见 handler.go 的注册）。
//
// 并发保护：同一任务不允许并发跑（重复触发回 409）。任务本身跑在后台 goroutine，
// 调用方拿到 202 即返回，不会等任务跑完——补跑可能持续数十秒到数分钟。
package server

import (
	"net/http"
	"strings"
	"sync"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
)

// TaskRunner 手动补跑需要的能力。*scheduler.Scheduler 满足它。
//
// 用接口而非直接依赖 *scheduler.Scheduler：server 与 scheduler 保持解耦
// （与 GetKeepaliveStats / StickyCount 的闭包注入同模式），也便于测试注入假实现。
type TaskRunner interface {
	RunCheckinNow()
	RunActivityNow()
	RunKeepaliveNow()
	RunTravelNow()
	RunBlackcatNow()
	RunStreakBonusNow()
}

// adminTaskNames 可补跑的任务名（对外文档与错误提示用；顺序即提示顺序）。
var adminTaskNames = []string{"checkin", "activity", "keepalive", "travel", "blackcat", "streak"}

// adminTaskMu 与 adminTaskRunning 保护「同一任务不并发跑」。
//
// 放包级而非 Handler 字段：补跑是运维动作，与 handler 实例无关；
// 且进程内只有一份调度器，包级状态语义更直接。
var (
	adminTaskMu      sync.Mutex
	adminTaskRunning = map[string]bool{}
)

// taskRunners 任务名 → 触发函数。Tasks 未注入（测试/裸用）时返回 nil。
func (h *Handler) taskRunners() map[string]func() {
	t := h.cfg.Tasks
	if t == nil {
		return nil
	}
	return map[string]func(){
		"checkin":   t.RunCheckinNow,
		"activity":  t.RunActivityNow,
		"keepalive": t.RunKeepaliveNow,
		"travel":    t.RunTravelNow,
		"blackcat":  t.RunBlackcatNow,
		"streak":    t.RunStreakBonusNow,
	}
}

// adminTaskRun POST /admin/tasks/{name}/run。
func (h *Handler) adminTaskRun(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	runners := h.taskRunners()
	if runners == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "unavailable",
			"task runner not wired into this handler")
		return
	}
	run, ok := runners[name]
	if !ok {
		writeOpenAIError(w, http.StatusNotFound, "not_found",
			"unknown task: "+name+"; known: "+strings.Join(adminTaskNames, ", "))
		return
	}

	adminTaskMu.Lock()
	if adminTaskRunning[name] {
		adminTaskMu.Unlock()
		writeOpenAIError(w, http.StatusConflict, "busy", "task already running: "+name)
		return
	}
	adminTaskRunning[name] = true
	adminTaskMu.Unlock()

	go func() {
		defer func() {
			adminTaskMu.Lock()
			delete(adminTaskRunning, name)
			adminTaskMu.Unlock()
			// panic 隔离：补跑炸了不能带走整个进程（后台 goroutine panic 会整个进程退出）。
			if rec := recover(); rec != nil {
				logx.Errorf("[admin] 手动补跑 %s panic: %v", name, rec)
			}
		}()
		logx.Infof("[admin] 手动补跑任务 %s 开始", name)
		run()
		logx.Infof("[admin] 手动补跑任务 %s 结束", name)
	}()

	writeJSON(w, http.StatusAccepted, map[string]string{"task": name, "status": "started"})
}

// adminTasksList GET /admin/tasks —— 列出可补跑的任务与当前是否在跑。
//
// 让运维脚本能先查再打，不必猜任务名。
func (h *Handler) adminTasksList(w http.ResponseWriter, r *http.Request) {
	runners := h.taskRunners()
	out := make([]map[string]any, 0, len(adminTaskNames))
	adminTaskMu.Lock()
	for _, name := range adminTaskNames {
		out = append(out, map[string]any{
			"name":    name,
			"running": adminTaskRunning[name],
			"wired":   runners != nil,
		})
	}
	adminTaskMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
}
