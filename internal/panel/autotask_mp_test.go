package panel

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件给小程序（mp）口径的任务闭环打桩：runMPMiniChatTask / runSequentialEventTask
// / runMiniExpert。桩架沿用 autotask_read_test.go 的 newTaskFakePanel，但换成**带状态**
// 的假上游——列表里 target/current/accept_status 会随 accept 与事件上报变化。
//
// 为什么必须带状态：这三个函数的正确性判据全在"读回来的进度"上（按差额补报、
// accept 后重取 target、异步未归账不领奖）。用固定响应只能验证分支是否走到，
// 验证不了这些语义。

// mpTaskBackend 假上游的可变状态 + 调用计数。
type mpTaskBackend struct {
	mu sync.Mutex

	code         string
	target       int64
	current      int64
	acceptStatus string // "" / "not_accepted" / "accepted" / "completed" / "claimed"
	locked       bool

	// acceptBeforeTarget accept 之前的 target（0 表示上游对未 accept 的任务不下发
	// 真实 target——Tasks_6 实测形态）。
	acceptBeforeTarget int64

	accepts       int
	acceptWorks   bool // accept 是否真的落账（false = 上游 200+OK 但未登记形态）
	reports       int
	reportFailAt  int  // 第 N 次上报返回 500（0 = 不失败）
	scoreOnReport bool // 上报后 current 是否 +1（false = 异步未归账）
	claimOnReport bool // 上报后 accept_status 直接变 claimed（另一端先领了）
	claims        int
	claimErr      bool

	// 专家市场（runMiniExpert / runExpertBatch）
	experts    string // 专家列表响应体；"" = 返回空列表
	expertErr  bool   // true = 市场接口 500
	chatID     string // 专家对话 SSE 里返回的服务端 requestId
	chatErr    bool   // true = 对话返回 500
	desktopOK  bool   // 桌面上报是否成功（false = 500）
	webBase    string // 领奖降级域，需与 chat 域区分
	eventsSeen []taskCall
}

func (b *mpTaskBackend) snapshot() (target, current int64, status string, claims int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.target, b.current, b.acceptStatus, b.claims
}

// newMPBackendPanel 构造走 mpTaskBackend 的 Panel。返回调用记录便于断言请求形状。
func newMPBackendPanel(t *testing.T, b *mpTaskBackend) (*Panel, *[]taskCall) {
	t.Helper()
	b.mu.Lock()
	if b.code == "" {
		b.code = "Sequential_Tasks_1"
	}
	if b.chatID == "" {
		b.chatID = "0123456789abcdef0123456789abcdef"
	}
	if b.webBase == "" {
		b.webBase = "https://web.fake.example"
	}
	b.mu.Unlock()

	return newTaskFakePanel(func(seq int, c taskCall) (int, string) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.eventsSeen = append(b.eventsSeen, c)

		switch {
		// ---- 任务列表：mp 口径下发任务，默认口径不下发（mp 专属码的真实形态） ----
		case c.Method == http.MethodGet && c.Path == tasksListPathVal:
			if !c.MP {
				return http.StatusOK, tasksEnvelope()
			}
			target, current := b.target, b.current
			if b.acceptStatus == "" || b.acceptStatus == "not_accepted" {
				target = b.acceptBeforeTarget
			}
			spec := taskSpec{Code: b.code, Target: target, Current: current, AcceptStatus: b.acceptStatus}
			if b.locked {
				spec.AcceptStatus = "not_accepted"
			}
			return http.StatusOK, tasksEnvelope(spec)

		// ---- accept：只有带 mp 头才落账 ----
		case c.Method == http.MethodPost && c.Path == tasksAcceptPathVal:
			b.accepts++
			if b.acceptWorks && c.MP {
				b.acceptStatus = "accepted"
			}
			return http.StatusOK, tasksEnvelope()

		// ---- mp 事件上报（billing 域 + workbuddy-mp 指纹） ----
		case c.Method == http.MethodPost && c.Path == reportPathVal && c.Product == "workbuddy-mp":
			b.reports++
			if b.reportFailAt > 0 && b.reports == b.reportFailAt {
				return http.StatusInternalServerError, errEnvelope(50001, "report boom")
			}
			if b.scoreOnReport {
				b.current++
				if b.target > 0 && b.current >= b.target {
					b.acceptStatus = "completed"
				}
			}
			if b.claimOnReport {
				b.acceptStatus = "claimed"
			}
			return http.StatusOK, tasksEnvelope()

		// ---- 领奖：chat 域 /activity/growth/tasks/{code}/claim（mp 头） ----
		case c.Method == http.MethodPost && strings.Contains(c.Path, "/activity/growth/tasks/") &&
			strings.HasSuffix(c.Path, "/claim"):
			b.claims++
			if b.claimErr {
				return http.StatusInternalServerError, errEnvelope(50001, "claim boom")
			}
			b.acceptStatus = "claimed"
			return http.StatusOK, `{"code":0,"data":{"credit":100,"energy":5}}`

		// ---- 专家市场列表 ----
		case c.Method == http.MethodPost && strings.Contains(c.Path, "market/expert/list"):
			if b.expertErr {
				return http.StatusInternalServerError, errEnvelope(50001, "market boom")
			}
			if b.experts == "" {
				return http.StatusOK, `{"code":0,"data":{"experts":[]}}`
			}
			return http.StatusOK, b.experts

		// ---- 专家真实对话：从 SSE 里被抓的服务端 requestId ----
		case c.Method == http.MethodPost && c.Path == chatCompletionsPathVal:
			if b.chatErr {
				return http.StatusInternalServerError, `{"error":"chat boom"}`
			}
			return http.StatusOK, sseWithID(b.chatID)

		// ---- 其余（桌面/活跃/web 上报） ----
		default:
			if !b.desktopOK && c.Path == reportPathVal {
				return http.StatusInternalServerError, errEnvelope(50001, "report boom")
			}
			return http.StatusOK, tasksEnvelope()
		}
	})
}

const chatCompletionsPathVal = "/v2/chat/completions"

// sseWithID 造一段含服务端 requestId 的 SSE：DesktopChatWithExpert 只认
// `"id":"<32hex>"` 这一形态（idRegex），其余字段不影响解析。
func sseWithID(id string) string {
	return "data: {\"id\":\"" + id + "\",\"object\":\"chat.completion.chunk\"}\n\n" +
		"data: [DONE]\n\n"
}

// freezeMPGaps 把 mp 闭环里的全部等待压到 0（accept 间隔 / 回读轮询 / 真人节奏）。
func freezeMPGaps(t *testing.T) {
	t.Helper()
	oldAction, oldPoll, oldChat, oldJitter := mpActionGap, claimPollGap, mpChatEventGap, mpChatEventJitter
	mpActionGap, claimPollGap, mpChatEventGap, mpChatEventJitter = 0, 0, 0, 0
	t.Cleanup(func() {
		mpActionGap, claimPollGap, mpChatEventGap, mpChatEventJitter = oldAction, oldPoll, oldChat, oldJitter
	})
}

// mpEventReports 只取 mp 事件上报（排除 accept / 领奖 / 列表）。
func mpEventReports(calls []taskCall) []taskCall {
	var out []taskCall
	for _, c := range calls {
		if c.Method == http.MethodPost && c.Path == reportPathVal && c.Product == "workbuddy-mp" {
			out = append(out, c)
		}
	}
	return out
}

// TestRunMPMiniChatTask 小程序对话任务闭环：查询 → accept 验证 → 按差额补报 → 回读 → 领奖。
func TestRunMPMiniChatTask(t *testing.T) {
	freezeMPGaps(t)

	t.Run("已领取时直接返回且一个写请求都不发", func(t *testing.T) {
		b := &mpTaskBackend{code: "school_season", acceptStatus: "claimed", target: 1, current: 1}
		p, calls := newMPBackendPanel(t, b)
		msg, err := runSchoolSeason(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "已领取") {
			t.Fatalf("got %q，want 已领取提示", msg)
		}
		for _, c := range *calls {
			if c.Method == http.MethodPost {
				t.Fatalf("已领取不该有任何写请求：%+v", c)
			}
		}
	})

	t.Run("mp 口径未下发任务时说明原因且不动作", func(t *testing.T) {
		// 上游只下发别的任务码（活动结束/未开放的形态）
		b := &mpTaskBackend{code: "other_task", acceptWorks: true}
		p, calls := newMPBackendPanel(t, b)
		msg, err := runSchoolSeason(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "未下发") {
			t.Fatalf("got %q，want 未下发提示", msg)
		}
		if b.accepts != 0 || b.reports != 0 {
			t.Fatalf("未下发时不该 accept/上报：accepts=%d reports=%d", b.accepts, b.reports)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（只读一次 mp 列表）", len(*calls))
		}
	})

	t.Run("accept 未登记生效时不补报（防半程态）", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_1", acceptStatus: "not_accepted", acceptWorks: false, target: 1}
		p, _ := newMPBackendPanel(t, b)
		msg, err := runSequentialChat(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "accept 未登记生效") {
			t.Fatalf("got %q，want accept 未登记提示", msg)
		}
		if b.reports != 0 {
			t.Fatalf("accept 未生效就上报会白跑：reports=%d", b.reports)
		}
		if b.accepts != 2 {
			t.Fatalf("accept 应重试一次（共 2 次），got %d", b.accepts)
		}
	})

	t.Run("accept 后重取真实 target，按差额补报而非兜底 1 条", func(t *testing.T) {
		// 场景来自 Tasks_6 实测：accept 前 target 下发 0，accept 后真实 target=4。
		// 若按兜底 target=1 只报 1 条，就会误判达标去领奖（claim 400）。
		b := &mpTaskBackend{
			code: "Sequential_Tasks_3", acceptStatus: "not_accepted", acceptWorks: true,
			acceptBeforeTarget: 0, target: 4, current: 0, scoreOnReport: true,
		}
		p, calls := newMPBackendPanel(t, b)
		msg, err := runSequentialChat5(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "领取") {
			t.Fatalf("补满后应领奖，got %q", msg)
		}
		if b.reports != 4 {
			t.Fatalf("上报次数 = %d，want 4（accept 后真实 target）", b.reports)
		}
		reports := mpEventReports(*calls)
		for i, c := range reports {
			if !strings.Contains(c.Body, "chat_request_send") {
				t.Fatalf("第 %d 条上报缺 chat_request_send：%q", i+1, c.Body)
			}
		}
		if b.claims != 1 {
			t.Fatalf("达标后应领奖一次，got %d", b.claims)
		}
	})

	t.Run("已达标（completed 未领）直接领奖不补报", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_1", acceptStatus: "completed", target: 1, current: 1}
		p, _ := newMPBackendPanel(t, b)
		msg, err := runSequentialChat(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if b.reports != 0 {
			t.Fatalf("已完成不该再上报：reports=%d", b.reports)
		}
		if b.claims != 1 || !strings.Contains(msg, "+100c +5e") {
			t.Fatalf("应领奖并回报到账数额，got msg=%q claims=%d", msg, b.claims)
		}
	})

	t.Run("上报中途失败：停在上一条且不领奖", func(t *testing.T) {
		b := &mpTaskBackend{
			code: "Sequential_Tasks_3", acceptStatus: "accepted", target: 3, current: 0,
			scoreOnReport: true, reportFailAt: 2,
		}
		p, _ := newMPBackendPanel(t, b)
		msg, err := runSequentialChat5(p, testAuth())
		if err != nil {
			t.Fatalf("部分成功不该作为 error 返回，got %v", err)
		}
		if !strings.Contains(msg, "完成 1/3 次上报后中断") {
			t.Fatalf("got %q，want 中断在 1/3", msg)
		}
		if b.reports != 2 {
			t.Fatalf("上报次数 = %d，want 2（第 2 次失败后立即停）", b.reports)
		}
		if b.claims != 0 {
			t.Fatalf("未达标不该领奖：claims=%d", b.claims)
		}
	})

	t.Run("上报成功但异步未归账：提示下次重试且不领奖", func(t *testing.T) {
		b := &mpTaskBackend{
			code: "Sequential_Tasks_1", acceptStatus: "accepted", target: 1, current: 0,
			scoreOnReport: false,
		}
		p, _ := newMPBackendPanel(t, b)
		msg, err := runSequentialChat(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "进度未达") {
			t.Fatalf("got %q，want 未达提示", msg)
		}
		if b.claims != 0 {
			t.Fatalf("未归账不该领奖：claims=%d", b.claims)
		}
	})

	t.Run("回读已是 claimed 时不重复领奖", func(t *testing.T) {
		// 上报后上游标记 claimed（另一端的领奖先到账）：此时再 claim 是重复请求。
		b := &mpTaskBackend{
			code: "Sequential_Tasks_1", acceptStatus: "accepted", target: 1,
			scoreOnReport: false, claimOnReport: true,
		}
		p, _ := newMPBackendPanel(t, b)
		msg, err := runSequentialChat(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if b.claims != 0 {
			t.Fatalf("claimed 状态不该再领奖：claims=%d", b.claims)
		}
		if !strings.Contains(msg, "claimed") {
			t.Fatalf("应说明本轮已入账，got %q", msg)
		}
	})
}

// TestRunMPMiniChatTaskActivityID 开学季事件必须带 activityId，普通对话任务不带。
// 这两个任务走同一 runMPMiniChatTask，差异全在 withActivityId 上，写错就点不亮。
func TestRunMPMiniChatTaskActivityID(t *testing.T) {
	freezeMPGaps(t)

	t.Run("school_season 带 activityId=school_open_day_2026", func(t *testing.T) {
		b := &mpTaskBackend{code: "school_season", acceptStatus: "accepted", target: 1, scoreOnReport: true}
		p, _ := newMPBackendPanel(t, b)
		if _, err := runSchoolSeason(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		reports := mpEventReports(b.eventsSeen)
		if len(reports) != 1 {
			t.Fatalf("上报次数 = %d，want 1", len(reports))
		}
		if !strings.Contains(reports[0].Body, "school_open_day_2026") {
			t.Fatalf("开学季事件必须带 activityId：%q", reports[0].Body)
		}
	})

	t.Run("Sequential_Tasks_1 不带 activityId", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_1", acceptStatus: "accepted", target: 1, scoreOnReport: true}
		p, _ := newMPBackendPanel(t, b)
		if _, err := runSequentialChat(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		reports := mpEventReports(b.eventsSeen)
		if len(reports) != 1 {
			t.Fatalf("上报次数 = %d，want 1", len(reports))
		}
		if strings.Contains(reports[0].Body, "activityId") {
			t.Fatalf("普通对话任务不该带 activityId（带了不计数）：%q", reports[0].Body)
		}
	})
}

// TestRunSequentialEventTask Sequential 链预留任务的通用骨架：accept → primary
// 判据 → 回读（未点亮且给了 fallback 时补一轮）→ 领奖。
func TestRunSequentialEventTask(t *testing.T) {
	freezeMPGaps(t)

	t.Run("primary 上报即点亮 → 领奖", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_4", acceptStatus: "accepted", target: 1}
		p, _ := newMPBackendPanel(t, b)
		primary := func() error {
			b.mu.Lock()
			b.current = 1
			b.acceptStatus = "completed"
			b.mu.Unlock()
			return nil
		}
		msg, err := p.runSequentialEventTask(testAuth(), "Sequential_Tasks_4", primary, nil)
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "领取") {
			t.Fatalf("got %q，want 领奖结论", msg)
		}
		if b.claims != 1 {
			t.Fatalf("claims = %d，want 1", b.claims)
		}
	})

	t.Run("accept 未生效提示每日锁定，不跑判据", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_4", acceptStatus: "not_accepted", acceptWorks: false, target: 1}
		p, _ := newMPBackendPanel(t, b)
		ran := false
		primary := func() error { ran = true; return nil }
		msg, err := p.runSequentialEventTask(testAuth(), "Sequential_Tasks_4", primary, nil)
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "锁定") {
			t.Fatalf("got %q，want 锁定提示", msg)
		}
		if ran {
			t.Fatal("accept 未生效不该跑判据（白报会污染进度）")
		}
	})

	t.Run("primary 失败透出且不领奖", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_4", acceptStatus: "accepted", target: 1}
		p, _ := newMPBackendPanel(t, b)
		primary := func() error { return errEnvelopeErr("primary boom") }
		msg, err := p.runSequentialEventTask(testAuth(), "Sequential_Tasks_4", primary, nil)
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "判据上报失败") || !strings.Contains(msg, "primary boom") {
			t.Fatalf("got %q，want 判据失败提示", msg)
		}
		if b.claims != 0 {
			t.Fatalf("判据失败不该领奖：claims=%d", b.claims)
		}
	})

	t.Run("未点亮时补报 fallback 一轮", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_7", acceptStatus: "accepted", target: 1}
		p, _ := newMPBackendPanel(t, b)
		primaryCalls, fallbackCalls := 0, 0
		primary := func() error { primaryCalls++; return nil }
		// fallback 生效：上报后进度达标。
		fallback := func() error {
			fallbackCalls++
			b.mu.Lock()
			b.current = 1
			b.acceptStatus = "completed"
			b.mu.Unlock()
			return nil
		}
		msg, err := p.runSequentialEventTask(testAuth(), "Sequential_Tasks_7", primary, fallback)
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if primaryCalls != 1 || fallbackCalls != 1 {
			t.Fatalf("primary=%d fallback=%d，want 各 1 次", primaryCalls, fallbackCalls)
		}
		if !strings.Contains(msg, "领取") {
			t.Fatalf("got %q，want 领奖结论", msg)
		}
	})

	t.Run("fallback 也失败透出且不领奖", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_7", acceptStatus: "accepted", target: 1}
		p, _ := newMPBackendPanel(t, b)
		primary := func() error { return nil }
		fallback := func() error { return errEnvelopeErr("fallback boom") }
		msg, err := p.runSequentialEventTask(testAuth(), "Sequential_Tasks_7", primary, fallback)
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "备选判据上报失败") {
			t.Fatalf("got %q，want 备选失败提示", msg)
		}
		if b.claims != 0 {
			t.Fatalf("claims = %d，want 0", b.claims)
		}
	})

	t.Run("mp 口径未下发时说明原因", func(t *testing.T) {
		b := &mpTaskBackend{code: "other_task", target: 1}
		p, _ := newMPBackendPanel(t, b)
		msg, err := p.runSequentialEventTask(testAuth(), "Sequential_Tasks_4", func() error { return nil }, nil)
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "未下发") {
			t.Fatalf("got %q，want 未下发提示", msg)
		}
	})
}

// errEnvelopeErr 造一个普通 error（判据回调只需返回 error，不需要上游信封）。
func errEnvelopeErr(msg string) error { return &fakeErr{msg} }

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }

// TestRunMiniExpert 小程序选专家对话：判据载体（市场真实 ex_ id）必须 accept 之前
// 就备好，拉不到就整任务不动作（避免「已登记未上报」的半程态）。
func TestRunMiniExpert(t *testing.T) {
	freezeMPGaps(t)

	const expertList = `{"code":0,"data":{"experts":[
		{"expert_id":"ex_real01","expert_type":"agent","display_name_zh":"数据分析师","profession_zh":"数据分析","version":"1.2.0"}
	]}}`

	t.Run("正常链路：先解析专家再 accept，上报真实 id 后领奖", func(t *testing.T) {
		b := &mpTaskBackend{
			code: "Sequential_Tasks_2", acceptStatus: "not_accepted", acceptWorks: true,
			target: 1, experts: expertList, scoreOnReport: true,
		}
		p, calls := newMPBackendPanel(t, b)
		msg, err := runMiniExpert(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "领取") {
			t.Fatalf("got %q，want 领奖结论", msg)
		}
		// 专家列表必须在 accept 之前（顺序即契约）。
		firstAccept, firstMarket := -1, -1
		for i, c := range *calls {
			if strings.Contains(c.Path, "market/expert/list") && firstMarket < 0 {
				firstMarket = i
			}
			if c.Path == tasksAcceptPathVal && firstAccept < 0 {
				firstAccept = i
			}
		}
		if firstMarket < 0 || firstAccept < 0 {
			t.Fatalf("缺少专家列表或 accept 调用：market=%d accept=%d", firstMarket, firstAccept)
		}
		if firstMarket > firstAccept {
			t.Fatalf("专家列表必须在 accept 之前（市场不可用时不留下半程态），market=%d accept=%d",
				firstMarket, firstAccept)
		}
		// 判据事件必须带真实 ex_ id 与 mp 指纹。
		reports := mpEventReports(*calls)
		if len(reports) != 1 {
			t.Fatalf("上报次数 = %d，want 1", len(reports))
		}
		if !strings.Contains(reports[0].Body, "ex_real01") {
			t.Fatalf("expert_actual_use 必须带市场真实 id：%q", reports[0].Body)
		}
		if !strings.Contains(reports[0].Body, "expert_actual_use") {
			t.Fatalf("判据事件码应为 expert_actual_use：%q", reports[0].Body)
		}
	})

	t.Run("专家市场不可用时跳过，不 accept", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_2", acceptStatus: "not_accepted", acceptWorks: true, target: 1}
		p, _ := newMPBackendPanel(t, b)
		msg, err := runMiniExpert(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "专家市场不可用") {
			t.Fatalf("got %q，want 市场不可用提示", msg)
		}
		if b.accepts != 0 {
			t.Fatalf("市场不可用时不该 accept（会留下半程态）：accepts=%d", b.accepts)
		}
	})

	t.Run("市场接口报错同样跳过", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_2", acceptStatus: "not_accepted", target: 1, expertErr: true}
		p, _ := newMPBackendPanel(t, b)
		if _, err := runMiniExpert(p, testAuth()); err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if b.accepts != 0 {
			t.Fatalf("accepts = %d，want 0", b.accepts)
		}
	})

	t.Run("已 completed 直接领奖", func(t *testing.T) {
		b := &mpTaskBackend{code: "Sequential_Tasks_2", acceptStatus: "completed", target: 1, current: 1}
		p, _ := newMPBackendPanel(t, b)
		if _, err := runMiniExpert(p, testAuth()); err != nil {
			t.Fatalf("err = %v", err)
		}
		if b.claims != 1 || b.reports != 0 {
			t.Fatalf("claims=%d reports=%d，want 1/0（已完成只领奖）", b.claims, b.reports)
		}
	})

	t.Run("上报失败不领奖", func(t *testing.T) {
		b := &mpTaskBackend{
			code: "Sequential_Tasks_2", acceptStatus: "accepted", target: 1,
			experts: expertList, scoreOnReport: true,
		}
		p, _ := newMPBackendPanel(t, b)
		b.mu.Lock()
		b.desktopOK = true
		b.mu.Unlock()
		// 让 mp 上报失败
		b.mu.Lock()
		b.reportFailAt = 1
		b.mu.Unlock()
		msg, err := runMiniExpert(p, testAuth())
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if !strings.Contains(msg, "上报 expert_actual_use 失败") {
			t.Fatalf("got %q，want 上报失败提示", msg)
		}
		if b.claims != 0 {
			t.Fatalf("claims = %d，want 0", b.claims)
		}
	})
}

// TestRunMPBackendReportsAreMPFingerprint 判据上报必须走 mp 指纹（workbuddy-mp +
// billing 域）：这条链一旦退回桌面指纹，任务会静默不计数且没有任何报错。
func TestRunMPBackendReportsAreMPFingerprint(t *testing.T) {
	freezeMPGaps(t)
	// 从 not_accepted 起步：accept 与领奖都要覆盖到（两者缺 mp 头都会被上游拒绝）。
	b := &mpTaskBackend{
		code: "Sequential_Tasks_1", acceptStatus: "not_accepted", acceptWorks: true,
		target: 1, scoreOnReport: true,
	}
	p, calls := newMPBackendPanel(t, b)
	if _, err := runSequentialChat(p, testAuth()); err != nil {
		t.Fatalf("err = %v", err)
	}
	reports := mpEventReports(*calls)
	if len(reports) != 1 {
		t.Fatalf("上报次数 = %d，want 1", len(reports))
	}
	if reports[0].Host != billingHost {
		t.Fatalf("mp 上报应走 billing 域，got host=%s", reports[0].Host)
	}
	if !strings.Contains(reports[0].Body, `"platform":"mini_program"`) {
		t.Fatalf("事件体应带 platform=mini_program：%q", reports[0].Body)
	}
	// accept 与领奖必须带 mp 头：缺头上游返回 task not found（实测）。
	var acceptMP, claimMP bool
	for _, c := range *calls {
		if c.Path == tasksAcceptPathVal {
			acceptMP = c.MP
		}
		if strings.HasSuffix(c.Path, "/claim") {
			claimMP = c.MP
		}
	}
	if !acceptMP {
		t.Fatal("accept 必须带 X-Client-Platform: miniprogram")
	}
	if !claimMP {
		t.Fatal("领奖必须带 X-Client-Platform: miniprogram")
	}
}

// TestMPChatGapIsInjectable 校验测试钩子本身：mpChatEventJitter 归零后 randDurationN
// 不得 panic（rand.Int64N(0) 会 panic），也不得返回负值。
func TestMPChatGapIsInjectable(t *testing.T) {
	if d := randDurationN(0); d != 0 {
		t.Fatalf("randDurationN(0) = %v，want 0", d)
	}
	if d := randDurationN(-time.Second); d != 0 {
		t.Fatalf("randDurationN(负值) = %v，want 0", d)
	}
	if d := randDurationN(2 * time.Millisecond); d < 0 || d >= 2*time.Millisecond {
		t.Fatalf("randDurationN 应落在 [0, n)，got %v", d)
	}
}
