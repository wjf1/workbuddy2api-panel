package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 本文件补 autotask.go 剩余的动作：简单事件上报（automation / library / template /
// playbook / canvas / appearance）、真实对话类（runModelChat / runBlackCat /
// runSkillFresh / runExpertLighthouse）、批量专家链（runExpertBatch）与全量入口
// （runAutoAll / accountTaskAuto / accountTaskAutoAll）。
//
// 桩架复用 autotask_read_test.go 的 newTaskFakePanel（按调用序号回放响应）。

// expertListJSON 专家市场列表响应（两个真实形态的 ex_ id）。
const expertListJSON = `{"code":0,"data":{"experts":[
	{"expert_id":"ex_aaa111","expert_type":"agent","display_name_zh":"数据分析师","profession_zh":"数据分析","version":"1.2.0"},
	{"expert_id":"ex_bbb222","expert_type":"agent","display_name_zh":"文案专家","profession_zh":"文案","version":"1.0.1"}
]}}`

// sseStream 通用 SSE 响应（含服务端 requestId）。
func sseStream(id string) string {
	return "data: {\"id\":\"" + id + "\",\"choices\":[]}\n\ndata: [DONE]\n\n"
}

// freezeActionGaps 把动作里的全部等待压到 0（含达标回读轮询：runSequentialEventTask
// 与 runMiniExpert 都按 claimPollGap 轮两轮，默认 3s×2 会让用例白等）。
func freezeActionGaps(t *testing.T) {
	t.Helper()
	oldReport, oldAppearance, oldTemplate, oldExpert, oldPoll :=
		reportGap, appearanceApplyGap, templateReportGap, expertSummonGap, claimPollGap
	reportGap, appearanceApplyGap, templateReportGap, expertSummonGap, claimPollGap = 0, 0, 0, 0, 0
	t.Cleanup(func() {
		reportGap, appearanceApplyGap, templateReportGap, expertSummonGap, claimPollGap =
			oldReport, oldAppearance, oldTemplate, oldExpert, oldPoll
	})
}

// TestRunSimpleEventActions 六个「一次事件上报即点亮」的动作：只验证判据事件码与
// 请求形状（各自的事件码就是它们唯一的存在理由，写错就静默不计数）。
func TestRunSimpleEventActions(t *testing.T) {
	freezeActionGaps(t)

	okResponder := func(int, taskCall) (int, string) {
		return http.StatusOK, `{"code":0,"data":{}}`
	}

	t.Run("runAutomationCreate 上报 automated_task_create_suc", func(t *testing.T) {
		p, calls := newTaskFakePanel(okResponder)
		if _, err := runAutomationCreate(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1", len(*calls))
		}
		if !strings.Contains((*calls)[0].Body, "automated_task_create_suc") {
			t.Fatalf("事件码应为 automated_task_create_suc：%q", (*calls)[0].Body)
		}
		if (*calls)[0].Host != chatFakeHost {
			t.Fatalf("桌面事件应走 chat 域，got %s", (*calls)[0].Host)
		}
	})

	t.Run("runLibraryRead 上报 web_element_click + library_doc_intro_click", func(t *testing.T) {
		p, calls := newTaskFakePanel(okResponder)
		if _, err := runLibraryRead(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1", len(*calls))
		}
		body := (*calls)[0].Body
		if !strings.Contains(body, "library_doc_intro_click") || !strings.Contains(body, "web_element_click") {
			t.Fatalf("判据是 library_doc_intro_click 点击事件：%q", body)
		}
		// Library_read 走 web 端 /v2/report（workbuddy.cn），不是桌面 copilot 域。
		if strings.Contains(body, "workbuddy-desktop") {
			t.Fatalf("资料库任务应走 web 指纹而非桌面指纹：%q", body)
		}
	})

	t.Run("runTemplateUse 上报 5 组模板事件", func(t *testing.T) {
		p, calls := newTaskFakePanel(okResponder)
		msg, err := runTemplateUse(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(msg, "×5") {
			t.Fatalf("got %q，want ×5", msg)
		}
		if len(*calls) != 5 {
			t.Fatalf("调用次数 = %d，want 5（每组一次上报）", len(*calls))
		}
		for i, c := range *calls {
			if !strings.Contains(c.Body, "template_used") {
				t.Fatalf("第 %d 组缺 template_used：%q", i+1, c.Body)
			}
		}
	})

	t.Run("runTemplateUse 第 3 组失败时停在 3 且不返回 error", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if seq == 3 {
				return http.StatusInternalServerError, errEnvelope(50001, "boom")
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		msg, err := runTemplateUse(p, testAuth())
		if err != nil {
			t.Fatalf("部分成功不该作为 error，got %v", err)
		}
		if !strings.Contains(msg, "第 3 组") {
			t.Fatalf("got %q，want 第 3 组失败提示", msg)
		}
		if len(*calls) != 3 {
			t.Fatalf("调用次数 = %d，want 3（失败即停）", len(*calls))
		}
	})

	t.Run("runPlaybookPrompt 上报 playbook_prompt_send", func(t *testing.T) {
		p, calls := newTaskFakePanel(okResponder)
		if _, err := runPlaybookPrompt(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(*calls) != 1 || !strings.Contains((*calls)[0].Body, "playbook_prompt_send") {
			t.Fatalf("应一次上报 playbook_prompt_send：%+v", *calls)
		}
	})

	t.Run("runCreateCanvas 上报 wbx_design_canvas 事件组", func(t *testing.T) {
		p, calls := newTaskFakePanel(okResponder)
		if _, err := runCreateCanvas(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1", len(*calls))
		}
		if !strings.Contains((*calls)[0].Body, "wbx_design_canvas_task_create") {
			t.Fatalf("缺画布创建判据事件：%q", (*calls)[0].Body)
		}
	})

	t.Run("runAppearance 先 set 主题再上报 skin_apply", func(t *testing.T) {
		p, calls := newTaskFakePanel(okResponder)
		if _, err := runAppearance(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(*calls) != 2 {
			t.Fatalf("调用次数 = %d，want 2（set + 事件）", len(*calls))
		}
		if !strings.Contains((*calls)[0].Path, "appearance/set") {
			t.Fatalf("第 1 步应为 appearance/set，got %s", (*calls)[0].Path)
		}
		if !strings.Contains((*calls)[0].Body, "theme-tkmw7j") {
			t.Fatalf("set 应带判据主题 resource_key：%q", (*calls)[0].Body)
		}
		if !strings.Contains((*calls)[1].Body, "appearance_skin_apply") {
			t.Fatalf("第 2 步应为 appearance_skin_apply 事件：%q", (*calls)[1].Body)
		}
	})

	t.Run("runAppearance set 失败则不发事件", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusInternalServerError, errEnvelope(50001, "boom")
		})
		if _, err := runAppearance(p, testAuth()); err == nil {
			t.Fatal("set 失败应返回错误")
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（set 失败不该继续上报）", len(*calls))
		}
	})
}

// TestRunModelChat accept → 真实对话 → 对齐模型上报。accept 失败不阻塞（行为事件
// 才是判据），但对话失败必须报错且不上报。
func TestRunModelChat(t *testing.T) {
	freezeActionGaps(t)

	t.Run("正常链路：accept → 对话 → 上报 glm-5.2", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Path == chatCompletionsPathVal {
				return http.StatusOK, sseStream("0123456789abcdef0123456789abcdef")
			}
			return http.StatusOK, tasksEnvelope()
		})
		msg, err := runModelChat(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "glm-5.2") {
			t.Fatalf("got %q，want 提到 glm-5.2", msg)
		}
		var sawAccept, sawChat, sawReport bool
		for _, c := range *calls {
			switch {
			case c.Path == tasksAcceptPathVal:
				sawAccept = true
				if !strings.Contains(c.Body, "Model_chat_GLM5.2") {
					t.Fatalf("accept 应含目标任务码：%q", c.Body)
				}
			case c.Path == chatCompletionsPathVal:
				sawChat = true
				if !strings.Contains(c.Body, "glm-5.2") {
					t.Fatalf("对话请求应用 glm-5.2：%q", c.Body)
				}
			case c.Path == reportPathVal:
				sawReport = true
				if !strings.Contains(c.Body, `"requestModelId":"glm-5.2"`) {
					t.Fatalf("上报必须对齐模型字段：%q", c.Body)
				}
			}
		}
		if !sawAccept || !sawChat || !sawReport {
			t.Fatalf("accept=%v chat=%v report=%v，want 全为 true", sawAccept, sawChat, sawReport)
		}
	})

	t.Run("accept 失败不阻塞后续行为链路", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Path == tasksAcceptPathVal {
				return http.StatusInternalServerError, errEnvelope(50001, "accept boom")
			}
			if c.Path == chatCompletionsPathVal {
				return http.StatusOK, sseStream("0123456789abcdef0123456789abcdef")
			}
			return http.StatusOK, tasksEnvelope()
		})
		msg, err := runModelChat(p, testAuth())
		if err != nil {
			t.Fatalf("accept 失败不该导致整体失败，got %v", err)
		}
		if !strings.Contains(msg, "glm-5.2") {
			t.Fatalf("got %q", msg)
		}
		var sawReport bool
		for _, c := range *calls {
			if c.Path == reportPathVal {
				sawReport = true
			}
		}
		if !sawReport {
			t.Fatal("accept 失败后仍应走完对话与上报（行为事件才是判据）")
		}
	})

	t.Run("对话 HTTP 400 时报错且不落上报", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Path == chatCompletionsPathVal {
				return http.StatusBadRequest, `{"error":{"message":"model not available"}}`
			}
			return http.StatusOK, tasksEnvelope()
		})
		_, err := runModelChat(p, testAuth())
		if err == nil {
			t.Fatal("对话失败应返回错误")
		}
		// ChatStream 把非 2xx 归类后随 err 返回，包装文案是「对话请求」。
		if !strings.Contains(err.Error(), "对话请求") || !strings.Contains(err.Error(), "400") {
			t.Fatalf("错误应带状态码与文案，got %v", err)
		}
		for _, c := range *calls {
			if c.Path == reportPathVal {
				t.Fatal("对话失败不该继续上报（会污染进度判据）")
			}
		}
	})

	t.Run("上报失败时提示对话已完成", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Path == chatCompletionsPathVal {
				return http.StatusOK, sseStream("0123456789abcdef0123456789abcdef")
			}
			if c.Path == reportPathVal {
				return http.StatusInternalServerError, errEnvelope(50001, "report boom")
			}
			return http.StatusOK, tasksEnvelope()
		})
		msg, err := runModelChat(p, testAuth())
		if err != nil {
			t.Fatalf("上报失败不该作为 error 返回（对话已消耗），got %v", err)
		}
		if !strings.Contains(msg, "对话已完成") {
			t.Fatalf("got %q，want 对话已完成提示", msg)
		}
	})
}

// TestRunBlackCat 夜猫子：窗口判定 → 差额 → 真实对话。窗口用注入时钟固定。
func TestRunBlackCat(t *testing.T) {
	freezeActionGaps(t)
	restore := upstreamSetNightChatGap(t)
	defer restore()

	oldNow := nightWindowNow
	t.Cleanup(func() { nightWindowNow = oldNow })

	// 固定到 CST 凌晨 2 点（窗口内）与 CST 下午 2 点（窗口外）。
	nightTime := time.Date(2026, 10, 26, 2, 0, 0, 0, cst.Loc)
	dayTime := time.Date(2026, 10, 26, 14, 0, 0, 0, cst.Loc)

	t.Run("窗口外直接返回提示且不打上游", func(t *testing.T) {
		nightWindowNow = func() time.Time { return dayTime }
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope()
		})
		msg, err := runBlackCat(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(msg, "不在 23:00–08:00") {
			t.Fatalf("got %q，want 窗口外提示", msg)
		}
		if len(*calls) != 0 {
			t.Fatalf("窗口外不该打上游，got %d 次调用", len(*calls))
		}
	})

	t.Run("窗口内已达标则提示无需补足", func(t *testing.T) {
		nightWindowNow = func() time.Time { return nightTime }
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "black_cat", Target: 3, Current: 3})
		})
		msg, err := runBlackCat(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(msg, "无需补足") {
			t.Fatalf("got %q，want 无需补足", msg)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（只查列表）", len(*calls))
		}
	})

	t.Run("窗口内按差额跑夜间对话并上报", func(t *testing.T) {
		nightWindowNow = func() time.Time { return nightTime }
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Path == chatCompletionsPathVal {
				return http.StatusOK, sseStream("0123456789abcdef0123456789abcdef")
			}
			if c.Path == reportPathVal {
				return http.StatusOK, `{"code":0,"data":{}}`
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "black_cat", Target: 2, Current: 1})
		})
		msg, err := runBlackCat(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(msg, "已完成 1 次") {
			t.Fatalf("got %q，want 完成 1 次", msg)
		}
		var chats, reports int
		for _, c := range *calls {
			switch c.Path {
			case chatCompletionsPathVal:
				chats++
				if !strings.Contains(c.Body, "glm-5.2") {
					t.Fatalf("夜猫子对话必须用 glm-5.2：%q", c.Body)
				}
			case reportPathVal:
				reports++
				if !strings.Contains(c.Body, `"requestModelId":"glm-5.2"`) {
					t.Fatalf("上报应对齐 glm-5.2：%q", c.Body)
				}
			}
		}
		if chats != 1 || reports != 1 {
			t.Fatalf("对话 %d 次 / 上报 %d 次，want 各 1（差额=1）", chats, reports)
		}
	})

	t.Run("对话失败时报告中断次数且不返回 error", func(t *testing.T) {
		nightWindowNow = func() time.Time { return nightTime }
		p, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Path == chatCompletionsPathVal {
				return http.StatusInternalServerError, `{"error":"boom"}`
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "black_cat", Target: 3, Current: 0})
		})
		msg, err := runBlackCat(p, testAuth())
		if err != nil {
			t.Fatalf("中断不该作为 error 返回，got %v", err)
		}
		if !strings.Contains(msg, "完成 0/3 次后中断") {
			t.Fatalf("got %q，want 中断提示", msg)
		}
	})
}

// TestRunSkillFresh skill_1：真实对话拿服务端 requestId → 事件链补 skill_info。
func TestRunSkillFresh(t *testing.T) {
	const reqID = "0123456789abcdef0123456789abcdef"

	t.Run("正常链路：对话 + 补 skill_info 事件", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Path == chatCompletionsPathVal {
				return http.StatusOK, sseStream(reqID)
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		msg, err := runSkillFresh(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(msg, "skill_info") {
			t.Fatalf("got %q，want 提到 skill_info", msg)
		}
		if len(*calls) != 2 {
			t.Fatalf("调用次数 = %d，want 2（对话 + 上报）", len(*calls))
		}
		body := (*calls)[1].Body
		for _, want := range []string{"skill_info", "toolStatus", reqID} {
			if !strings.Contains(body, want) {
				t.Fatalf("skill_info 事件缺 %q：%q", want, body)
			}
		}
		// finishReason=tool_calls 是「模型发起工具调用」的语义标记。
		if !strings.Contains(body, `"finishReason":"tool_calls"`) {
			t.Fatalf("chat_message_response 应带 finishReason=tool_calls：%q", body)
		}
	})

	t.Run("对话失败直接报错且不上报", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			return http.StatusInternalServerError, `{"error":"boom"}`
		})
		if _, err := runSkillFresh(p, testAuth()); err == nil {
			t.Fatal("对话失败应返回错误")
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（对话失败即停）", len(*calls))
		}
	})

	t.Run("SSE 里没有合法 requestId 时报错", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Path == chatCompletionsPathVal {
				return http.StatusOK, "data: {\"id\":\"not-a-real-id\"}\n\n"
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		_, err := runSkillFresh(p, testAuth())
		if err == nil {
			t.Fatal("拿不到服务端 requestId 应报错（自造 id 不计数）")
		}
		if !strings.Contains(err.Error(), "requestId") {
			t.Fatalf("错误应说明缺 requestId，got %v", err)
		}
	})
}

// TestRunExpertLighthouse 轻量云专家：市场命中则用真实条目，召唤链 + chat 链带
// has_expert，actual_use 用 LOCAL 且 type 为空/cost=0（对齐真实样本）。
func TestRunExpertLighthouse(t *testing.T) {
	const lhID = "ex_2cvvUZQhDyeJ"
	const reqID = "abcdef0123456789abcdef0123456789"

	lhList := `{"code":0,"data":{"experts":[
		{"expert_id":"` + lhID + `","expert_type":"agent","display_name_zh":"腾讯轻量云专家","profession_zh":"腾讯轻量云专家","version":"1.0.9"}
	]}}`

	t.Run("市场命中时用服务端条目", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			switch {
			case strings.Contains(c.Path, "market/expert/list"):
				return http.StatusOK, lhList
			case c.Path == chatCompletionsPathVal:
				return http.StatusOK, sseStream(reqID)
			default:
				return http.StatusOK, `{"code":0,"data":{}}`
			}
		})
		if _, err := runExpertLighthouse(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		var sawChat, sawReport bool
		var hasExpert, localMode, zeroCost, marketVersion bool
		for _, c := range *calls {
			if c.Path == chatCompletionsPathVal {
				sawChat = true
				if c.Expert != lhID {
					t.Fatalf("对话应带 X-Expert-Id=%s，got %q", lhID, c.Expert)
				}
			}
			if c.Path == reportPathVal {
				sawReport = true
				// 召唤链与 chat 链分两次上报：判据字段不一定落在同一条请求里。
				body := c.Body
				hasExpert = hasExpert || strings.Contains(body, `"has_expert":true`)
				localMode = localMode || strings.Contains(body, `"mode":"LOCAL"`)
				zeroCost = zeroCost || strings.Contains(body, `"cost":0`)
				marketVersion = marketVersion || strings.Contains(body, "1.0.9")
			}
		}
		if !sawChat || !sawReport {
			t.Fatalf("chat=%v report=%v，want 均为 true", sawChat, sawReport)
		}
		if !hasExpert {
			t.Fatal("chat 链 agent_task_created 必须带 has_expert:true")
		}
		if !localMode {
			t.Fatal("actual_use 必须是 LOCAL 形态（轻量云判据要求）")
		}
		if !zeroCost {
			t.Fatal("actual_use 应带 cost=0（对齐真实样本）")
		}
		if !marketVersion {
			t.Fatal("应使用市场返回的 version 1.0.9（服务端为准）")
		}
	})

	t.Run("市场不可用时回落内置专家信息继续", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			switch {
			case strings.Contains(c.Path, "market/expert/list"):
				return http.StatusInternalServerError, errEnvelope(50001, "market boom")
			case c.Path == chatCompletionsPathVal:
				return http.StatusOK, sseStream(reqID)
			default:
				return http.StatusOK, `{"code":0,"data":{}}`
			}
		})
		if _, err := runExpertLighthouse(p, testAuth()); err != nil {
			t.Fatalf("市场失败应回落到内置信息，got %v", err)
		}
		var sawReport bool
		for _, c := range *calls {
			if c.Path == reportPathVal {
				sawReport = true
				if !strings.Contains(c.Body, lhID) {
					t.Fatalf("回落时仍应用固定轻量云 id：%q", c.Body)
				}
			}
		}
		if !sawReport {
			t.Fatal("市场失败后仍应继续上报")
		}
	})

	t.Run("召唤链失败直接报错", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if strings.Contains(c.Path, "market/expert/list") {
				return http.StatusOK, `{"code":0,"data":{"experts":[]}}`
			}
			return http.StatusInternalServerError, errEnvelope(50001, "summon boom")
		})
		_, err := runExpertLighthouse(p, testAuth())
		if err == nil {
			t.Fatal("召唤链失败应返回错误")
		}
		if !strings.Contains(err.Error(), "召唤链") {
			t.Fatalf("错误应指明召唤链，got %v", err)
		}
	})
}

// TestRunExpertBatch 专家召唤+使用链：列表为空/报错、逐个继续、计数上限、
// 每轮都要真实 chat 拿服务端 requestId。
func TestRunExpertBatch(t *testing.T) {
	freezeActionGaps(t)
	const reqID = "aaaabbbbccccddddeeeeffff00001111"

	t.Run("按 count 上限只做前 N 个", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			switch {
			case strings.Contains(c.Path, "market/expert/list"):
				return http.StatusOK, expertListJSON
			case c.Path == chatCompletionsPathVal:
				return http.StatusOK, sseStream(reqID)
			default:
				return http.StatusOK, `{"code":0,"data":{}}`
			}
		})
		msg, err := runExpertUse(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(msg, "2 位") {
			t.Fatalf("市场只有 2 个专家，got %q", msg)
		}
		chats := 0
		for _, c := range *calls {
			if c.Path == chatCompletionsPathVal {
				chats++
				if c.Expert == "" {
					t.Fatal("专家对话必须带 X-Expert-Id（自造/缺失 id 不计数）")
				}
			}
		}
		if chats != 2 {
			t.Fatalf("对话次数 = %d，want 2", chats)
		}
	})

	t.Run("列表为空时报错", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			return http.StatusOK, `{"code":0,"data":{"experts":[]}}`
		})
		if _, err := runExpertUse(p, testAuth()); err == nil {
			t.Fatal("专家列表为空应返回错误")
		}
	})

	t.Run("列表接口报错时报错", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			return http.StatusInternalServerError, errEnvelope(50001, "boom")
		})
		_, err := runExpertUse(p, testAuth())
		if err == nil {
			t.Fatal("列表失败应返回错误")
		}
		if !strings.Contains(err.Error(), "拉取专家列表") {
			t.Fatalf("错误应指明拉列表，got %v", err)
		}
	})

	t.Run("单个专家失败不影响其余（不返回 error）", func(t *testing.T) {
		// 第 1 个专家的对话失败，第 2 个成功 → 整体仍是 nil error。
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			switch {
			case strings.Contains(c.Path, "market/expert/list"):
				return http.StatusOK, expertListJSON
			case c.Path == chatCompletionsPathVal && c.Expert == "ex_aaa111":
				return http.StatusInternalServerError, `{"error":"chat boom"}`
			case c.Path == chatCompletionsPathVal:
				return http.StatusOK, sseStream(reqID)
			default:
				return http.StatusOK, `{"code":0,"data":{}}`
			}
		})
		msg, err := runExpertUse(p, testAuth())
		if err != nil {
			t.Fatalf("单个失败不该让整链失败，got %v", err)
		}
		if !strings.Contains(msg, "1 位") {
			t.Fatalf("只有 1 个成功，got %q", msg)
		}
		// 校验失败的那个专家确实尝试过（不是被跳过）。
		var tried []string
		for _, c := range *calls {
			if c.Path == chatCompletionsPathVal {
				tried = append(tried, c.Expert)
			}
		}
		if len(tried) != 2 {
			t.Fatalf("应尝试两位专家，got %v", tried)
		}
	})

	t.Run("team 类型走 expertType=team 且 count=3", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			switch {
			case strings.Contains(c.Path, "market/expert/list"):
				return http.StatusOK, expertListJSON
			case c.Path == chatCompletionsPathVal:
				return http.StatusOK, sseStream(reqID)
			default:
				return http.StatusOK, `{"code":0,"data":{}}`
			}
		})
		if _, err := runExpertTeamUse(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		for _, c := range *calls {
			if strings.Contains(c.Path, "market/expert/list") {
				if !strings.Contains(c.Body, `"expert_type":"team"`) {
					t.Fatalf("专家团应请求 expert_type=team：%q", c.Body)
				}
			}
		}
	})
}

// TestRunAutoAll 全量入口：批量 accept（默认 + mp 两口径）→ 逐项执行 → 跳过已完成
// → 达标即领奖。用 autoActions 全集驱动，验证「单项失败不阻塞后续」。
func TestRunAutoAll(t *testing.T) {
	freezeActionGaps(t)

	t.Run("已接受/已领取的任务不再批量 accept，其余执行", func(t *testing.T) {
		// 列表里只有两个已 claimed 的任务：阶段 0 应判定「无事可接受」，
		// 逐项阶段则全部 skipped（这两条路径都要走到）。
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodGet && c.Path == tasksListPathVal {
				return http.StatusOK, preClaimedEnvelope()
			}
			return http.StatusOK, tasksEnvelope()
		})
		out := p.runAutoAll(testAuth())
		if len(out) == 0 {
			t.Fatal("runAutoAll 应返回逐项结果")
		}
		// 两项都已 claimed → 不该产生任何 accept 请求。
		for _, c := range *calls {
			if c.Path == tasksAcceptPathVal {
				t.Fatalf("全部已领取时不该 accept：%+v", c)
			}
		}
		// 「无此任务」与「已完成」都是 skipped，靠 message 区分：
		// 已领取的两项应报「已完成」，其余应是「该账号无此任务」。
		done, missing := 0, 0
		for _, item := range out {
			msg, _ := item["message"].(string)
			switch {
			case strings.Contains(msg, "已完成"):
				done++
			case strings.Contains(msg, "无此任务"):
				missing++
			}
		}
		if done != 2 {
			t.Fatalf("报「已完成」的项 = %d，want 2（chat_5 与 first_buddy）", done)
		}
		if missing == 0 {
			t.Fatal("其余任务应报「该账号无此任务」")
		}
		if done+missing != len(out) {
			t.Fatalf("结果项 %d 未被完全归类（done=%d missing=%d）", len(out), done, missing)
		}
	})

	t.Run("有未接受任务时先批量 accept", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodGet && c.Path == tasksListPathVal {
				return http.StatusOK, tasksEnvelope(
					taskSpec{Code: "chat_5", Target: 5, Current: 5, AcceptStatus: "not_accepted"},
				)
			}
			return http.StatusOK, tasksEnvelope()
		})
		_ = p.runAutoAll(testAuth())
		var acceptBody string
		for _, c := range *calls {
			if c.Path == tasksAcceptPathVal {
				acceptBody = c.Body
			}
		}
		if !strings.Contains(acceptBody, "chat_5") {
			t.Fatalf("批量 accept 应包含 chat_5，got %q", acceptBody)
		}
	})

	t.Run("列表查询失败时该项标记 error 但流程继续", func(t *testing.T) {
		var n int
		p, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodGet && c.Path == tasksListPathVal {
				return http.StatusInternalServerError, errEnvelope(50001, "list boom")
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		out := p.runAutoAll(testAuth())
		if len(out) == 0 {
			t.Fatal("应返回逐项结果")
		}
		errored := 0
		for _, item := range out {
			if item["status"] == "error" {
				errored++
			}
			n++
		}
		if errored == 0 {
			t.Fatal("列表失败时各项应标记 error")
		}
		if n != len(out) {
			t.Fatalf("结果项数 %d 与遍历数 %d 不一致", len(out), n)
		}
	})
}

// preClaimedEnvelope 列表里只有 chat_5 与 first_buddy，且都已 claimed
// （批量 accept 阶段应有「无事可做」，逐项阶段应全部 skipped）。
func preClaimedEnvelope() string {
	return tasksEnvelope(
		taskSpec{Code: "chat_5", Target: 5, Current: 5, AcceptStatus: "claimed"},
		taskSpec{Code: "first_buddy", Target: 1, Current: 1, AcceptStatus: "claimed"},
	)
}

// TestAccountTaskAutoHandler 单任务一键完成 handler：路径分派、404、501、
// 409 互斥、跳过已完成、达标自动领奖。走真实 ServeHTTP（含鉴权与 PathValue）。
func TestAccountTaskAutoHandler(t *testing.T) {
	freezeActionGaps(t)

	newPanelWithBackend := func(t *testing.T, respond func(seq int, c taskCall) (int, string)) (*Panel, *[]taskCall) {
		t.Helper()
		base, calls := newTaskFakePanel(respond)
		p := New(Config{Version: "test", APIKey: "k", Pool: newTestPool(t), Upstream: base.cfg.Upstream})
		p.cfg.Pool.Add(testAuth())
		return p, calls
	}

	t.Run("未知任务码返回 501 并说明需客户端交互", func(t *testing.T) {
		p, _ := newPanelWithBackend(t, func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope()
		})
		rec := postAuto(p, "u1", `{"task_code":"no_such_task"}`)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d，want 501", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "客户端内交互") {
			t.Fatalf("应说明需客户端交互：%s", rec.Body.String())
		}
	})

	t.Run("账号不存在返回 404", func(t *testing.T) {
		p, _ := newPanelWithBackend(t, func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope()
		})
		rec := postAuto(p, "nobody", `{"task_code":"chat_5"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d，want 404", rec.Code)
		}
	})

	t.Run("缺 task_code 返回 400", func(t *testing.T) {
		p, _ := newPanelWithBackend(t, func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope()
		})
		rec := postAuto(p, "u1", `{}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d，want 400", rec.Code)
		}
	})

	t.Run("账号锁被占时返回 409（不并发重跑）", func(t *testing.T) {
		p, calls := newPanelWithBackend(t, func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 0})
		})
		if !p.tryLockAccount("u1") {
			t.Fatal("测试前应能加锁")
		}
		rec := postAuto(p, "u1", `{"task_code":"chat_5"}`)
		p.unlockAccount("u1")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d，want 409", rec.Code)
		}
		if len(*calls) != 0 {
			t.Fatalf("409 时不该打上游，got %d 次", len(*calls))
		}
	})

	t.Run("已领取时返回 skipped 且不执行动作", func(t *testing.T) {
		p, calls := newPanelWithBackend(t, func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 5, AcceptStatus: "claimed"})
		})
		rec := postAuto(p, "u1", `{"task_code":"chat_5"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d，want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "已领取过") {
			t.Fatalf("应提示已领取：%s", rec.Body.String())
		}
		for _, c := range *calls {
			if c.Method == http.MethodPost && c.Path == reportPathVal {
				t.Fatal("已领取不该再上报")
			}
		}
	})

	t.Run("该账号没有此任务返回 404", func(t *testing.T) {
		p, _ := newPanelWithBackend(t, func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "first_buddy"})
		})
		rec := postAuto(p, "u1", `{"task_code":"chat_5"}`)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d，want 404，body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("列表失败返回 502", func(t *testing.T) {
		p, _ := newPanelWithBackend(t, func(int, taskCall) (int, string) {
			return http.StatusInternalServerError, errEnvelope(50001, "boom")
		})
		rec := postAuto(p, "u1", `{"task_code":"chat_5"}`)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d，want 502", rec.Code)
		}
	})

	t.Run("动作成功且达标时自动领奖并回报数额", func(t *testing.T) {
		// chat_5：首次查 2/5 → 补 3 条 → 回读 5/5 → 自动领奖。
		var listed int
		p, calls := newPanelWithBackend(t, func(seq int, c taskCall) (int, string) {
			switch {
			case c.Method == http.MethodGet && c.Path == tasksListPathVal:
				listed++
				if listed == 1 {
					return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 2, AcceptStatus: "accepted"})
				}
				return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 5, AcceptStatus: "accepted"})
			case strings.HasSuffix(c.Path, "/claim"):
				return http.StatusOK, `{"code":0,"data":{"credit":100,"energy":5}}`
			default:
				return http.StatusOK, `{"code":0,"data":{}}`
			}
		})
		rec := postAuto(p, "u1", `{"task_code":"chat_5"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{`"claimed":true`, `"credit":100`, `"energy":5`, "+100 分"} {
			if !strings.Contains(body, want) {
				t.Fatalf("响应缺 %q：%s", want, body)
			}
		}
		var sawClaim bool
		for _, c := range *calls {
			if strings.HasSuffix(c.Path, "/claim") {
				sawClaim = true
			}
		}
		if !sawClaim {
			t.Fatal("达标后应调用领奖接口")
		}
	})

	t.Run("动作返回 error 时 502 且带原因", func(t *testing.T) {
		p, _ := newPanelWithBackend(t, func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodGet && c.Path == tasksListPathVal {
				return http.StatusOK, tasksEnvelope(taskSpec{Code: "first_buddy"})
			}
			return http.StatusInternalServerError, errEnvelope(50001, "report boom")
		})
		rec := postAuto(p, "u1", `{"task_code":"chat_5"}`)
		// chat_5 的任务不存在 → 404；换一个会执行的动作需要列表里有它。
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d，want 404（列表里没有 chat_5）", rec.Code)
		}
	})
}

// postAuto 发一次单任务 auto 请求（带正确 api_key）。
func postAuto(p *Panel, uid, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/panel/api/accounts/"+uid+"/tasks/auto", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// postEmpty 发一次无 body 的 POST（带正确 api_key）。
func postEmpty(p *Panel, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// newTestPool 返回无落盘的账号池（AuthByUID 需要真池子）。
func newTestPool(t *testing.T) *pool.Pool {
	t.Helper()
	return pool.New("")
}

// upstreamSetNightChatGap 把夜猫子的 4s 轮间隔压到 0，返回恢复函数。
func upstreamSetNightChatGap(t *testing.T) func() {
	t.Helper()
	restore := upstream.SetNightChatGapForTest(0)
	t.Cleanup(restore)
	return restore
}

// TestAccountTaskAutoAllHandler 全量入口：并发点击 409、正常返回 results。
func TestAccountTaskAutoAllHandler(t *testing.T) {
	freezeActionGaps(t)

	t.Run("锁被占时返回 409", func(t *testing.T) {
		base, _ := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope()
		})
		p := New(Config{Version: "test", APIKey: "k", Pool: newTestPool(t), Upstream: base.cfg.Upstream})
		p.cfg.Pool.Add(testAuth())
		if !p.tryLockAccount("u1") {
			t.Fatal("测试前应能加锁")
		}
		rec := postEmpty(p, "/panel/api/accounts/u1/tasks/auto_all")
		p.unlockAccount("u1")
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d，want 409", rec.Code)
		}
	})

	t.Run("正常路径返回逐项结果并自动释放锁", func(t *testing.T) {
		base, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodGet && c.Path == tasksListPathVal {
				return http.StatusOK, preClaimedEnvelope()
			}
			return http.StatusOK, `{"code":0,"data":{}}`
		})
		p := New(Config{Version: "test", APIKey: "k", Pool: newTestPool(t), Upstream: base.cfg.Upstream})
		p.cfg.Pool.Add(testAuth())
		rec := postEmpty(p, "/panel/api/accounts/u1/tasks/auto_all")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"results"`) {
			t.Fatalf("响应应含 results：%s", rec.Body.String())
		}
		// 流水线结束后锁必须已释放，否则后续点击永远 409。
		if !p.tryLockAccount("u1") {
			t.Fatal("auto_all 结束后应释放账号锁")
		}
		p.unlockAccount("u1")
	})
}

// TestRunSequentialWrappers 三个 Sequential 包装各自只差「任务码 + 判据事件形态」。
// 光验证「打到对的码」不够——判据事件的形状才是各自的全部价值（发错事件等于白报），
// 所以这里让任务从 accepted/0 起步，真正跑一遍 primary 与 fallback。
func TestRunSequentialWrappers(t *testing.T) {
	freezeActionGaps(t)

	cases := []struct {
		name    string
		code    string
		run     func(p *Panel) (string, error)
		primary string // 期望出现的判据事件码
	}{
		{"Sequential_Tasks_4 用 PC 定时任务事件", "Sequential_Tasks_4", func(p *Panel) (string, error) {
			return runSequentialAutomation(p, testAuth())
		}, "automated_task_create_suc"},
		{"Sequential_Tasks_5 用带模型字段的 mini 对话事件", "Sequential_Tasks_5", func(p *Panel) (string, error) {
			return runSequentialModelChat(p, testAuth())
		}, "glm-5.2"},
		{"Sequential_Tasks_7 用灵感事件组", "Sequential_Tasks_7", func(p *Panel) (string, error) {
			return runSequentialPlaybook(p, testAuth())
		}, "playbook_prompt_send"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, calls := newTaskFakePanel(func(seq int, call taskCall) (int, string) {
				if call.Method == http.MethodGet && call.Path == tasksListPathVal {
					if !call.MP {
						return http.StatusOK, tasksEnvelope()
					}
					// mp 列表：已 accept、未达标 —— 强制走完 accept→判据→回读→领奖。
					return http.StatusOK, tasksEnvelope(taskSpec{
						Code: c.code, Target: 1, Current: 0, AcceptStatus: "accepted",
					})
				}
				return http.StatusOK, `{"code":0,"data":{}}`
			})
			msg, err := c.run(p)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if msg == "" {
				t.Fatal("应返回可读结论")
			}
			// 判据事件必须真的发出去过（这是这三个包装唯一的存在理由）。
			var bodies []string
			for _, call := range *calls {
				if call.Path == reportPathVal {
					bodies = append(bodies, call.Body)
				}
			}
			if len(bodies) == 0 {
				t.Fatalf("%s 应至少上报一条判据事件", c.code)
			}
			joined := strings.Join(bodies, "\n")
			if !strings.Contains(joined, c.primary) {
				t.Fatalf("%s 的判据事件应含 %q，实际上报：%s", c.code, c.primary, joined)
			}
		})
	}
}

// TestRunSequentialChat10 Sequential_Tasks_6 与 Tasks_1/3 同形状，只是任务码不同。
func TestRunSequentialChat10(t *testing.T) {
	freezeMPGaps(t)
	b := &mpTaskBackend{
		code: "Sequential_Tasks_6", acceptStatus: "accepted", target: 2, current: 1,
		scoreOnReport: true,
	}
	p, calls := newMPBackendPanel(t, b)
	if _, err := runSequentialChat10(p, testAuth()); err != nil {
		t.Fatalf("err = %v", err)
	}
	// 差额 = 2-1 = 1 条
	reports := mpEventReports(*calls)
	if len(reports) != 1 {
		t.Fatalf("上报次数 = %d，want 1（按差额补报）", len(reports))
	}
	if b.claims != 1 {
		t.Fatalf("达标后应领奖，claims=%d", b.claims)
	}
}
