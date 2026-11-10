package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 手动补跑排程（admin_tasks.go）的行为钉子。
//
// 三个必须守住的点：
//  1. 未知任务名 → 404 且提示已知任务（运维不该靠猜）；
//  2. 同一任务并发触发 → 409（补跑可能持续数分钟，并发跑会打架）；
//  3. 任务真在后台跑（202 立即返回，不阻塞调用方）。

// fakeTaskRunner 计数型假任务运行器。
type fakeTaskRunner struct {
	checkin, activity, keepalive, travel, blackcat, streak int64
	// delay 让任务跑一会儿，用来测并发保护
	delay time.Duration
}

func (f *fakeTaskRunner) bump(p *int64) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	atomic.AddInt64(p, 1)
}

func (f *fakeTaskRunner) RunCheckinNow()     { f.bump(&f.checkin) }
func (f *fakeTaskRunner) RunActivityNow()    { f.bump(&f.activity) }
func (f *fakeTaskRunner) RunKeepaliveNow()   { f.bump(&f.keepalive) }
func (f *fakeTaskRunner) RunTravelNow()      { f.bump(&f.travel) }
func (f *fakeTaskRunner) RunBlackcatNow()    { f.bump(&f.blackcat) }
func (f *fakeTaskRunner) RunStreakBonusNow() { f.bump(&f.streak) }

// newTaskTestHandler 构造只关心 Tasks 的最小 handler。
func newTaskTestHandler(tr TaskRunner) *Handler {
	return NewHandler(Config{Tasks: tr})
}

func TestAdminTaskRunUnknownTask(t *testing.T) {
	h := newTaskTestHandler(&fakeTaskRunner{})
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/nope/run", nil)
	req.SetPathValue("name", "nope")
	rec := httptest.NewRecorder()
	h.adminTaskRun(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知任务应 404, got %d", rec.Code)
	}
	// 错误提示必须列出已知任务名，否则运维只能翻源码
	body := rec.Body.String()
	if !strings.Contains(body, "checkin") {
		t.Fatalf("404 应提示已知任务名, got %s", body)
	}
}

func TestAdminTaskRunAcceptedAndExecutes(t *testing.T) {
	fr := &fakeTaskRunner{}
	h := newTaskTestHandler(fr)

	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
	req.SetPathValue("name", "checkin")
	rec := httptest.NewRecorder()
	h.adminTaskRun(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("应回 202 Accepted, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"started"`) {
		t.Fatalf("响应体应含 status=started, got %s", rec.Body.String())
	}

	// 等后台 goroutine 跑完（202 是立即返回的，任务在后台）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&fr.checkin) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&fr.checkin); got != 1 {
		t.Fatalf("任务应执行 1 次, got %d", got)
	}
}

// TestAdminTaskRunConcurrentRejected 同一任务并发触发 → 409。
//
// 补跑可能持续数分钟，并发跑会重复打上游（签到任务重复执行有副作用）。
func TestAdminTaskRunConcurrentRejected(t *testing.T) {
	fr := &fakeTaskRunner{delay: 200 * time.Millisecond}
	h := newTaskTestHandler(fr)

	// 第一次：占住
	req1 := httptest.NewRequest(http.MethodPost, "/admin/tasks/travel/run", nil)
	req1.SetPathValue("name", "travel")
	rec1 := httptest.NewRecorder()
	h.adminTaskRun(rec1, req1)
	if rec1.Code != http.StatusAccepted {
		t.Fatalf("首次应 202, got %d", rec1.Code)
	}

	// 第二次（任务仍在跑）：应 409
	req2 := httptest.NewRequest(http.MethodPost, "/admin/tasks/travel/run", nil)
	req2.SetPathValue("name", "travel")
	rec2 := httptest.NewRecorder()
	h.adminTaskRun(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("并发应 409 Conflict, got %d", rec2.Code)
	}

	// 等第一次跑完后，应能再次触发（锁被正确释放）
	time.Sleep(300 * time.Millisecond)
	req3 := httptest.NewRequest(http.MethodPost, "/admin/tasks/travel/run", nil)
	req3.SetPathValue("name", "travel")
	rec3 := httptest.NewRecorder()
	h.adminTaskRun(rec3, req3)
	if rec3.Code != http.StatusAccepted {
		t.Fatalf("任务结束后应能再次触发, got %d", rec3.Code)
	}
	time.Sleep(300 * time.Millisecond) // 收尾，避免影响后续测试的包级状态
}

// TestAdminTaskRunNotWired Tasks 未注入时回 503（而不是 panic）。
func TestAdminTaskRunNotWired(t *testing.T) {
	h := NewHandler(Config{}) // Tasks 为 nil
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
	req.SetPathValue("name", "checkin")
	rec := httptest.NewRecorder()
	h.adminTaskRun(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未注入 Tasks 应 503, got %d", rec.Code)
	}
}

// TestAdminTasksList 列出任务与运行态。
func TestAdminTasksList(t *testing.T) {
	h := newTaskTestHandler(&fakeTaskRunner{})
	rec := httptest.NewRecorder()
	h.adminTasksList(rec, httptest.NewRequest(http.MethodGet, "/admin/tasks", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("应 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	// 六个任务都应出现在列表里
	for _, name := range adminTaskNames {
		if !strings.Contains(body, `"`+name+`"`) {
			t.Errorf("列表缺任务: %s", name)
		}
	}
}

// TestAdminTaskPanicIsolated 补跑 panic 不能带走进程。
//
// 后台 goroutine 里的 panic 会让整个进程退出——对网关来说是致命的，
// 必须被 recover 兜住。
func TestAdminTaskPanicIsolated(t *testing.T) {
	h := newTaskTestHandler(&panicRunner{})
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
	req.SetPathValue("name", "checkin")
	rec := httptest.NewRecorder()
	h.adminTaskRun(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("应 202, got %d", rec.Code)
	}
	// 给 goroutine 时间 panic 并被 recover；若未 recover，测试进程会崩
	time.Sleep(150 * time.Millisecond)

	// 关键：进程还活着 → 能继续响应
	rec2 := httptest.NewRecorder()
	h.adminTasksList(rec2, httptest.NewRequest(http.MethodGet, "/admin/tasks", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("panic 后进程应仍可用, got %d", rec2.Code)
	}
	// 锁也应已释放
	adminTaskMu.Lock()
	still := adminTaskRunning["checkin"]
	adminTaskMu.Unlock()
	if still {
		t.Fatal("panic 后任务锁未释放（后续补跑会被永久 409）")
	}
}

type panicRunner struct{ fakeTaskRunner }

func (p *panicRunner) RunCheckinNow() { panic("boom") }
