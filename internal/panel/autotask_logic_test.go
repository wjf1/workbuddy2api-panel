package panel

import (
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestAutoActionFor 已知任务码必须能查到动作，未知码返回 nil（= 不可自动化）。
//
// 这个函数决定「任务中心里哪些行显示一键完成」，返回错等于按钮点了没反应。
func TestAutoActionFor(t *testing.T) {
	act := autoActionFor("chat_5")
	if act == nil {
		t.Fatal("chat_5 应登记在 autoActions 中")
	}
	if act.TaskCode != "chat_5" {
		t.Fatalf("返回的动作用于 %q，want chat_5", act.TaskCode)
	}
	if act.run == nil {
		t.Fatal("chat_5 的动作没有 run 实现（点了会 panic）")
	}

	// 上游 task_code 偶尔带空白，autoActionFor 内部 TrimSpace 后仍应命中。
	if got := autoActionFor("  chat_5\t"); got == nil || got.TaskCode != "chat_5" {
		t.Fatalf("带空白前缀的 chat_5 应仍命中，got %v", got)
	}

	for _, code := range []string{"", "no_such_task", "CHAT_5"} {
		if got := autoActionFor(code); got != nil {
			t.Errorf("未登记的任务码 %q 不应返回动作，got %q", code, got.TaskCode)
		}
	}
}

// TestAutoActionIndex autoActionIndex 是队列执行顺序的依据：越小越先跑。
func TestAutoActionIndex(t *testing.T) {
	// 顺序即依赖序：chat_5 必须先于 first_buddy（后者依赖活跃上报解锁）。
	if got := autoActionIndex("chat_5"); got != 0 {
		t.Fatalf("chat_5 的执行序 = %d，want 0（表首项）", got)
	}
	if !(autoActionIndex("chat_5") < autoActionIndex("first_buddy")) {
		t.Fatalf("chat_5 必须先于 first_buddy 执行：%d vs %d",
			autoActionIndex("chat_5"), autoActionIndex("first_buddy"))
	}

	// 表内每个登记项都必须能被查到自己的真实下标，否则排序会把它当成"未知"排到最后。
	for i := range autoActions {
		if got := autoActionIndex(autoActions[i].TaskCode); got != i {
			t.Errorf("%q 的执行序 = %d，want %d", autoActions[i].TaskCode, got, i)
		}
	}

	// 未知任务排到最后，但不能与真实下标撞车（真实下标恒 < len(autoActions)）。
	const unknown = 1 << 20
	if got := autoActionIndex("no_such_task"); got != unknown {
		t.Fatalf("未知任务的执行序 = %d，want %d", got, unknown)
	}
	if len(autoActions) >= unknown {
		t.Fatalf("动作表长度 %d 已达到未知哨兵值，排序将失效", len(autoActions))
	}

	// 与 autoActionFor 同口径：同一个上游 task_code 不能一个认、一个不认，
	// 否则按钮可点但排序把它当未知任务甩到最后。
	for i := range autoActions {
		code := autoActions[i].TaskCode
		if autoActionFor(" "+code+" ") == nil {
			t.Fatalf("%q 带空白时 autoActionFor 应命中", code)
		}
		if got := autoActionIndex(" " + code + " "); got != i {
			t.Errorf("%q 带空白时执行序 = %d，want %d（应与 autoActionFor 同口径）", code, got, i)
		}
	}
}

// TestIsMPTaskCode 只有登记过的 mp 专属码才走小程序口径（会多打一次 mp 列表接口）。
func TestIsMPTaskCode(t *testing.T) {
	for _, code := range []string{"school_season", "Sequential_Tasks_1", "Sequential_Tasks_7"} {
		if !isMPTaskCode(code) {
			t.Errorf("%q 应登记为 mp 专属任务", code)
		}
	}
	// 普通 PC 任务走默认口径：误判会让每次回读都多打一次上游请求。
	for _, code := range []string{"", "chat_5", "first_buddy", "Sequential_Tasks_8", "sequential_tasks_1"} {
		if isMPTaskCode(code) {
			t.Errorf("%q 不应被当作 mp 专属任务", code)
		}
	}
}

// TestAcceptStatusOr 回读状态的安全读取：nil 任务不能 panic。
func TestAcceptStatusOr(t *testing.T) {
	if got := acceptStatusOr(nil); got != "?" {
		t.Fatalf("nil 任务的状态 = %q，want \"?\"", got)
	}
	if got := acceptStatusOr(&upstream.Task{}); got != "?" {
		t.Fatalf("空状态 = %q，want \"?\"", got)
	}
	if got := acceptStatusOr(&upstream.Task{AcceptStatus: "accepted"}); got != "accepted" {
		t.Fatalf("状态 = %q，want accepted", got)
	}
}

// TestTaskProgressText 进度文本是「上报前 vs 上报后」对比的唯一凭据。
func TestTaskProgressText(t *testing.T) {
	if got := taskProgressText(nil); got != "?" {
		t.Fatalf("nil 任务进度 = %q，want \"?\"", got)
	}
	// 有目标：显示 当前/目标，即使 current=0 也要显示（0 是有效进度）。
	if got := taskProgressText(&upstream.Task{Current: 0, Target: 5}); got != "0/5" {
		t.Fatalf("进度 = %q，want 0/5", got)
	}
	if got := taskProgressText(&upstream.Task{Current: 3, Target: 5}); got != "3/5" {
		t.Fatalf("进度 = %q，want 3/5", got)
	}
	// 有目标且已领取：仍要显示 当前/目标（回读对比要看得见进度，claimed 会把 4/5 这类信息丢掉）。
	if got := taskProgressText(&upstream.Task{Current: 4, Target: 5, Claimed: true}); got != "4/5" {
		t.Fatalf("已领取但有目标时的进度 = %q，want 4/5（target 分支优先于 claimed）", got)
	}
	// 无目标且已领：claimed 优先于 accept_status。
	got := taskProgressText(&upstream.Task{Target: 0, Claimed: true, AcceptStatus: "accepted"})
	if got != "claimed" {
		t.Fatalf("已领取的进度文本 = %q，want claimed", got)
	}
	// 无目标未领：回落 accept_status。
	got = taskProgressText(&upstream.Task{Target: 0, AcceptStatus: "accepted"})
	if got != "accepted" {
		t.Fatalf("无目标未领取的进度文本 = %q，want accepted", got)
	}
}

// TestTruncateStr 上游错误文本截断：短串原样，长串加省略号
// （中文昵称/中文报错的字符边界见 TestTruncateStrKeepsValidUTF8）。
func TestTruncateStr(t *testing.T) {
	if got := truncateStr("boom", 10); got != "boom" {
		t.Fatalf("短串应原样返回，got %q", got)
	}
	if got := truncateStr("", 10); got != "" {
		t.Fatalf("空串应原样返回，got %q", got)
	}
	// 恰好等于上限：不截断、不加省略号。
	if got := truncateStr("0123456789", 10); got != "0123456789" {
		t.Fatalf("恰好等于上限时应原样返回，got %q", got)
	}
	if got := truncateStr("0123456789abc", 10); got != "0123456789…" {
		t.Fatalf("超长应截断并加省略号，got %q", got)
	}
	if got := truncateStr("0123456789abc", 10); !strings.HasSuffix(got, "…") {
		t.Fatalf("截断结果应带省略号，got %q", got)
	}
}
