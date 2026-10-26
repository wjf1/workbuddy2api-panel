package panel

import (
	"net/http"
	"strings"
	"testing"
)

// 本文件给任务动作（run*）打桩，桩架沿用 autotask_read_test.go 的 newTaskFakePanel。
// 判据来自代码注释里记录的实测结论：每条都对应一个「上游行为 vs 本网关行为」的契约。

const (
	reportPathVal = "/v2/report"
	chatFakeHost  = "chat.fake.example"
	billingHost   = "billing.fake.example"
)

// countHost 统计打到某个域的请求数（区分桌面上报与活跃上报）。
func countHost(calls []taskCall, host string) int {
	n := 0
	for _, c := range calls {
		if c.Host == host {
			n++
		}
	}
	return n
}

// TestRunChat5 补足 chat_5 进度：按差额上报，已达标一条都不发。
func TestRunChat5(t *testing.T) {
	oldGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldGap })

	t.Run("进度已达标时一条都不上报", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 5})
		})
		msg, err := runChat5(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "无需上报") {
			t.Fatalf("已达标应提示无需上报，got %q", msg)
		}
		if n := countHost(*calls, billingHost); n != 0 {
			t.Fatalf("活跃上报次数 = %d，want 0（已达标不该再消耗上游配额）", n)
		}
	})

	t.Run("按差额上报且走 billing 域", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if seq == 1 {
				return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 2})
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		msg, err := runChat5(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "3 条") {
			t.Fatalf("应补报 3 条（5-2），got %q", msg)
		}
		reports := []taskCall{}
		for _, c := range *calls {
			if c.Host == billingHost {
				reports = append(reports, c)
			}
		}
		if len(reports) != 3 {
			t.Fatalf("上报次数 = %d，want 3", len(reports))
		}
		for i, c := range reports {
			if c.Method != http.MethodPost || c.Path != reportPathVal {
				t.Fatalf("第 %d 次上报 = %s %s，want POST %s", i+1, c.Method, c.Path, reportPathVal)
			}
			if !strings.Contains(c.Body, "chat_request_send") {
				t.Fatalf("上报体应含 chat_request_send 事件：%q", c.Body)
			}
			// 桌面上报走 chat 域，活跃上报走 billing 域且不带桌面指纹。
			if strings.Contains(c.Body, "workbuddy-desktop") {
				t.Fatalf("活跃上报不该带桌面端指纹：%q", c.Body)
			}
		}
	})

	t.Run("上游没给 target 时回落到 5", func(t *testing.T) {
		// target=0 是有效进度值（Task 注释明示），但 runChat5 需要一条基准线，
		// 规则是：拿不到 target 就按 5 算。
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if seq == 1 {
				return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5"})
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		if _, err := runChat5(p, testAuth()); err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if n := countHost(*calls, billingHost); n != 5 {
			t.Fatalf("上报次数 = %d，want 5（target 缺失时按 5 补）", n)
		}
	})

	t.Run("任务不存在时报错且不上报", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "first_buddy"})
		})
		_, err := runChat5(p, testAuth())
		if err == nil {
			t.Fatal("任务不存在应返回错误")
		}
		if n := countHost(*calls, billingHost); n != 0 {
			t.Fatalf("上报次数 = %d，want 0", n)
		}
	})

	t.Run("上报中途失败：停止后续上报且不返回 error", func(t *testing.T) {
		// 语义：部分成功也算力争到进度，前端展示"上报第 N 条失败"而不是整任务失败。
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if seq == 1 {
				return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 3, Current: 0})
			}
			if seq == 3 { // 第 2 次上报（第 1 次是列表）
				return http.StatusInternalServerError, errEnvelope(50001, "report boom")
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		msg, err := runChat5(p, testAuth())
		if err != nil {
			t.Fatalf("上报失败不应作为 error 返回，got %v", err)
		}
		if !strings.Contains(msg, "上报第 2/3 条失败") {
			t.Fatalf("应报告第 2/3 条失败，got %q", msg)
		}
		if n := countHost(*calls, billingHost); n != 2 {
			t.Fatalf("上报次数 = %d，want 2（失败后立即停止，不再打第 3 条）", n)
		}
	})
}

// TestRunRichMeow 桌面端事件链点亮 RichMeow_Chat（纯 API 可点亮，三账号实测）。
func TestRunRichMeow(t *testing.T) {
	t.Run("成功时按桌面指纹上报整条事件链", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		msg, err := runRichMeow(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if msg == "" {
			t.Fatal("成功时应返回说明文案")
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1", len(*calls))
		}
		c := (*calls)[0]
		if c.Host != chatFakeHost {
			t.Fatalf("桌面上报应走 chatBase，got host=%s", c.Host)
		}
		if c.Method != http.MethodPost || c.Path != reportPathVal {
			t.Fatalf("桌面上报 = %s %s，want POST %s", c.Method, c.Path, reportPathVal)
		}
		if !strings.Contains(c.Body, "workbuddy-desktop") {
			t.Fatalf("应带桌面端指纹 extName=workbuddy-desktop：%q", c.Body)
		}
		// 事件链含六类事件，缺一环都不点亮点不亮。
		for _, ev := range []string{"agent_task_created", "chat_message_send", "chat_request_send", "chat_message_response"} {
			if !strings.Contains(c.Body, ev) {
				t.Fatalf("事件链缺少 %q：%q", ev, c.Body)
			}
		}
	})

	t.Run("上报失败必须透出错误", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusInternalServerError, errEnvelope(50001, "report boom")
		})
		if _, err := runRichMeow(p, testAuth()); err == nil {
			t.Fatal("桌面上报失败应返回错误")
		}
	})
}

// TestRunBuddyApp Buddy_App / Buddy_App_QQ 共用同一 run：五连事件 + 桌面指纹。
func TestRunBuddyApp(t *testing.T) {
	t.Run("成功时上报 buddyapp 事件", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		if _, err := runBuddyApp(p, testAuth()); err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1", len(*calls))
		}
		c := (*calls)[0]
		if c.Host != chatFakeHost || c.Path != reportPathVal {
			t.Fatalf("桌面上报 = %s %s，want chat 域 POST %s", c.Host, c.Path, reportPathVal)
		}
		if !strings.Contains(c.Body, "workbuddy-desktop") {
			t.Fatalf("应带桌面端指纹：%q", c.Body)
		}
	})

	t.Run("上报失败必须透出错误", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusTooManyRequests, errEnvelope(42901, "rate limited")
		})
		if _, err := runBuddyApp(p, testAuth()); err == nil {
			t.Fatal("上报失败应返回错误")
		}
	})
}

// TestRunFirstBuddy 领养链路：前置上报 → 同意协议 → 领取；
// 只有「门槛未过」才能算成功台面（业务已完成，只是当日活跃不够）。
func TestRunFirstBuddy(t *testing.T) {
	oldGap := reportGap
	reportGap = 0
	t.Cleanup(func() { reportGap = oldGap })

	const buddyAgreePath = "/activity/growth/buddy/agreement"
	const buddyFirstPath = "/activity/growth/buddy/first"

	t.Run("三步全成功", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		msg, err := runFirstBuddy(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "Buddy") {
			t.Fatalf("应返回领取成功说明，got %q", msg)
		}
		if len(*calls) != 3 {
			t.Fatalf("调用次数 = %d，want 3（上报 → 协议 → 领取）", len(*calls))
		}
		if (*calls)[0].Host != billingHost || !strings.Contains((*calls)[0].Body, "chat_request_send") {
			t.Fatalf("第 1 步应是活跃上报：%+v", (*calls)[0])
		}
		if (*calls)[1].Path != buddyAgreePath || !strings.Contains((*calls)[1].Body, `"agree":true`) {
			t.Fatalf("第 2 步应是同意协议（POST %s, agree=true）：%+v", buddyAgreePath, (*calls)[1])
		}
		if (*calls)[2].Path != buddyFirstPath {
			t.Fatalf("第 3 步应是领取 Buddy（POST %s）：%+v", buddyFirstPath, (*calls)[2])
		}
	})

	t.Run("前置上报失败立即返回不再走后两步", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if seq == 1 {
				return http.StatusInternalServerError, errEnvelope(50001, "report boom")
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		if _, err := runFirstBuddy(p, testAuth()); err == nil {
			t.Fatal("前置上报失败应返回错误")
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（第一步就失败，不该继续）", len(*calls))
		}
	})

	t.Run("同意协议失败返回错误", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if seq == 2 { // 第二步：同意协议
				return http.StatusInternalServerError, errEnvelope(50001, "agreement boom")
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		if _, err := runFirstBuddy(p, testAuth()); err == nil {
			t.Fatal("协议失败应返回错误")
		}
		if len(*calls) != 2 {
			t.Fatalf("调用次数 = %d，want 2", len(*calls))
		}
	})

	t.Run("领取被判「门槛未过」时不算任务失败", func(t *testing.T) {
		// upstream.IsBuddyTaskIncomplete 识别的口径：业务链路走完了，只是当日活跃不够。
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if seq == 3 { // 第三步：领取 Buddy
				return http.StatusBadRequest, `{"code":400,"msg":"first_buddy task not completed yet"}`
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		msg, err := runFirstBuddy(p, testAuth())
		if err != nil {
			t.Fatalf("门槛未过不应作为 error，got %v", err)
		}
		if !strings.Contains(msg, "请稍后重试") {
			t.Fatalf("应提示稍后重试，got %q", msg)
		}
		if len(*calls) != 3 {
			t.Fatalf("调用次数 = %d，want 3", len(*calls))
		}
	})
}
