package scheduler

// 启动补跑（missedToday / catchUpMissed）与补跑标记（runstamps.go）的行为钉子。
//
// 这组用例保护的核心语义：
//  1. 进程停机跨过今天的时点后重启 → 补跑一轮（迟到的请求不能静默消失）；
//  2. 同一天反复重启 → 不重复补跑（避免把上游打成刷量，账号被风控盯上）；
//  3. 未启用的任务族永不进补跑候选；
//  4. 标记文件损坏 / 缺失 → 当空表（宁可多补一轮，不因为写盘问题让任务停摆）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/safefile"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// stampTestScheduler 构造一个只用于补跑判定的调度器（不触网、不建池）。
func stampTestScheduler(t *testing.T, path string, checkinHours []int) *Scheduler {
	t.Helper()
	return New(Config{
		CheckinHours:   checkinHours,
		TravelHours:    []int{}, // 空 = New 里补默认 [9,21]
		ActivityHours:  []int{},
		KeepaliveHours: []int{},
		BlackcatHours:  []int{},
		GrowthHours:    []int{},
		RunStampFile:   path,
	})
}

// TestMissedTodayAfterAllSlotsPassed 时点全过 → 该任务族进补跑候选。
func TestMissedTodayAfterAllSlotsPassed(t *testing.T) {
	s := stampTestScheduler(t, filepath.Join(t.TempDir(), "runstamps.json"), []int{9, 21})
	// 22:00：当天 9 点与 21 点都已过去，下次触发落到明天。
	now := time.Date(2026, 11, 16, 22, 0, 0, 0, cst.Loc)
	got := s.missedToday(now)
	if !containsKind(got, taskCheckin) {
		t.Fatalf("checkin 应进补跑候选（时点全过），got=%v", got)
	}
	// 10:00：21 点还没到，今天仍有排程 → 不补。
	now = time.Date(2026, 11, 16, 10, 0, 0, 0, cst.Loc)
	if got := s.missedToday(now); containsKind(got, taskCheckin) {
		t.Fatalf("checkin 今天还有时点未到，不该补跑，got=%v", got)
	}
}

// TestMissedTodaySkipsAlreadyStamped 今天已补跑过 → 重启不重复。
func TestMissedTodaySkipsAlreadyStamped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstamps.json")
	s := stampTestScheduler(t, path, []int{9})
	now := time.Date(2026, 11, 16, 22, 0, 0, 0, cst.Loc)

	if s.ranToday(taskCheckin, now) {
		t.Fatal("初始状态不该有补跑标记")
	}
	s.markRan([]taskKind{taskCheckin}, now)
	if !s.ranToday(taskCheckin, now) {
		t.Fatal("markRan 之后 ranToday 应为 true")
	}
	if containsKind(s.missedToday(now), taskCheckin) {
		t.Fatal("已补跑过的任务不该再进补跑候选")
	}
	// 换一天：标记里只有旧日期，新日期应再次成为候选。
	nextDay := time.Date(2026, 11, 17, 22, 0, 0, 0, cst.Loc)
	if !containsKind(s.missedToday(nextDay), taskCheckin) {
		t.Fatal("跨天后应重新进补跑候选（标记只记当天）")
	}
}

// TestMissedTodayIgnoresDisabled 显式禁用的任务族永不进补跑候选。
func TestMissedTodayIgnoresDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:    []int{9},
		CheckinDisabled: true,
		RunStampFile:    filepath.Join(t.TempDir(), "runstamps.json"),
	})
	now := time.Date(2026, 11, 16, 22, 0, 0, 0, cst.Loc)
	if got := s.missedToday(now); containsKind(got, taskCheckin) {
		t.Fatalf("disabled 的签到不该进补跑候选，got=%v", got)
	}
}

// TestRunStampsCorruptTreatedAsEmpty 标记文件损坏 → 当空表（宁可多补一轮）。
func TestRunStampsCorruptTreatedAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstamps.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := stampTestScheduler(t, path, []int{9})
	now := time.Date(2026, 11, 16, 22, 0, 0, 0, cst.Loc)
	if s.ranToday(taskCheckin, now) {
		t.Fatal("损坏的标记文件应读成空表（ranToday=false）")
	}
	if !containsKind(s.missedToday(now), taskCheckin) {
		t.Fatal("损坏的标记文件下仍应补跑（不能因为文件坏了就不跑）")
	}
}

// TestRunStampsKeepsOnlyToday 标记文件只保留当天条目（不会越长越大）。
func TestRunStampsKeepsOnlyToday(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstamps.json")
	old := `{"checkin":"2026-11-15","travel":"2026-11-15"}`
	if err := safefile.Write(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	s := stampTestScheduler(t, path, []int{9})
	now := time.Date(2026, 11, 16, 22, 0, 0, 0, cst.Loc)
	s.markRan([]taskKind{taskCheckin}, now)

	raw, err := safefile.Read(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := s.loadStamps(path)
	if st["checkin"] != cst.Day(now) {
		t.Errorf("checkin 应记成今天，got=%q", st["checkin"])
	}
	if _, ok := st["travel"]; ok {
		t.Errorf("旧日期的 travel 条目应被丢弃，文件内容=%s（raw=%s）", st, raw)
	}
}

// TestRunStampsEmptyPathIsNoop 未配置路径时零行为（不建文件、ranToday 恒 false）。
func TestRunStampsEmptyPathIsNoop(t *testing.T) {
	s := stampTestScheduler(t, "", []int{9})
	now := time.Date(2026, 11, 16, 22, 0, 0, 0, cst.Loc)
	if s.ranToday(taskCheckin, now) {
		t.Fatal("无路径时 ranToday 应为 false")
	}
	s.markRan([]taskKind{taskCheckin}, now) // 不应 panic
	if !containsKind(s.missedToday(now), taskCheckin) {
		t.Fatal("无路径时每次重启都应补跑")
	}
}

// TestCatchUpMissedRunsAndStamps 补跑真跑一轮且落盘标记。
// 空账号池 + 不发请求的假上游：签到任务对零账号是空转，但 markRan 必须落盘——
// 这里断言「补跑后 ranToday 变 true」，覆盖 catchUpMissed 的接线。
func TestCatchUpMissedRunsAndStamps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runstamps.json")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("空账号池不该有任何上游请求: %s", r.URL.Path)
		http.Error(w, "no upstream call expected", 500)
	}))
	defer srv.Close()

	p := pool.New("") // 空池：签到遍历零账号
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool: p, Upstream: up,
		CheckinHours:   []int{9},
		TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true,
		BlackcatDisabled: true, GrowthDisabled: true,
		RunStampFile: path,
	})
	now := time.Now().In(cst.Loc)
	if now.Hour() < 10 {
		t.Skip("当前 CST 时刻早于 10 点，签到 9 点时才点未过，无法构造补跑场景")
	}
	s.catchUpMissed(context.Background())
	if !s.ranToday(taskCheckin, now) {
		t.Fatal("catchUpMissed 之后应留下当天的补跑标记")
	}
	// 再调一次不该重复补跑（ranToday 已 true，missedToday 返回空）。
	if got := s.missedToday(now); containsKind(got, taskCheckin) {
		t.Fatalf("第二次补跑应被标记挡掉，got=%v", got)
	}
}

func containsKind(kinds []taskKind, want taskKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}
