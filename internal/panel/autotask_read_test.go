package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 本文件给 autotask.go 的「任务读取 / accept 回读」路径打桩：upstream.Client.HTTP
// 换成假 Transport，一次真实 HTTP 都不发（仓库铁律：测试不许打真实网络）。
// 等待间隔用包级变量注入（reportGap / mpActionGap / claimPollGap），不许加 Sleep。

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// taskCall 记下一次上游调用的关键特征（够区分 mp 口径与读/写动作即可）。
type taskCall struct {
	Method string
	Path   string
	MP     bool // 是否带 X-Client-Platform: miniprogram
	Body   string
}

// newTaskFakePanel 返回 cfg.Upstream 走假 Transport 的 Panel，以及调用记录指针。
// respond 按「第几次调用」返回 (状态码, 响应体)。
func newTaskFakePanel(respond func(seq int, c taskCall) (int, string)) (*Panel, *[]taskCall) {
	var calls []taskCall
	c := &upstream.Client{
		ChatBaseCN: "https://fake.example",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			var body string
			if r.Body != nil {
				raw, _ := io.ReadAll(r.Body)
				body = strings.TrimSpace(string(raw))
			}
			call := taskCall{
				Method: r.Method,
				Path:   r.URL.Path,
				MP:     r.Header.Get("X-Client-Platform") == "miniprogram",
				Body:   body,
			}
			calls = append(calls, call)
			status, out := respond(len(calls), call)
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(out)),
			}, nil
		})},
	}
	return &Panel{cfg: Config{Upstream: c}}, &calls
}

func testAuth() *auth.Auth { return &auth.Auth{UID: "u1", Nickname: "测试号", AccessToken: "tok"} }

const (
	tasksListPathVal   = "/v2/activity/growth/tasks"
	tasksAcceptPathVal = "/v2/activity/growth/tasks/accept"
)

// taskSpec 构造列表响应用的任务行：target/current/accept_status 是判定的全部输入
// （upstream.Task.Claimable 不由上游给，而是 parseGrowthTasks 用 target/current 推算）。
type taskSpec struct {
	Code         string
	Target       int64
	Current      int64
	AcceptStatus string
}

func tasksEnvelope(specs ...taskSpec) string {
	type row struct {
		TaskCode     string `json:"task_code"`
		Target       int64  `json:"target"`
		Current      int64  `json:"current"`
		AcceptStatus string `json:"accept_status,omitempty"`
	}
	list := make([]row, 0, len(specs))
	for _, s := range specs {
		list = append(list, row{TaskCode: s.Code, Target: s.Target, Current: s.Current, AcceptStatus: s.AcceptStatus})
	}
	raw, err := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"tasks": list}})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func errEnvelope(code int, msg string) string {
	raw, _ := json.Marshal(map[string]any{"code": code, "msg": msg})
	return string(raw)
}

// TestTaskByCode 默认口径命中/未命中，以及 mp 专属码的回落。
func TestTaskByCode(t *testing.T) {
	t.Run("默认列表命中就不多打请求", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 1})
		})
		got, err := p.taskByCode(testAuth(), "chat_5")
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if got == nil || got.TaskCode != "chat_5" {
			t.Fatalf("taskByCode = %v，want chat_5 任务", got)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（命中后不该再查 mp 列表）", len(*calls))
		}
		if (*calls)[0].MP {
			t.Fatal("默认口径不应带 X-Client-Platform 头")
		}
		if (*calls)[0].Path != tasksListPathVal {
			t.Fatalf("请求路径 = %q，want %q", (*calls)[0].Path, tasksListPathVal)
		}
	})

	t.Run("未登记的任务码不多打第二次上游", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5"})
		})
		got, err := p.taskByCode(testAuth(), "no_such_task")
		if err != nil || got != nil {
			t.Fatalf("taskByCode = (%v,%v)，want (nil,nil)", got, err)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（未知码不该额外查 mp 列表）", len(*calls))
		}
	})

	t.Run("mp 专属码在默认列表找不到时回落到 mp 列表", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if seq == 1 {
				// 默认口径列表里没有 school_season（小程序限定下发的真实形态）
				return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5"})
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "school_season", Target: 1, AcceptStatus: "accepted"})
		})
		got, err := p.taskByCode(testAuth(), "school_season")
		if err != nil {
			t.Fatalf("err = %v，want nil", err)
		}
		if got == nil || got.TaskCode != "school_season" {
			t.Fatalf("taskByCode = %v，want school_season 任务", got)
		}
		if len(*calls) != 2 {
			t.Fatalf("调用次数 = %d，want 2（默认列表 + mp 列表）", len(*calls))
		}
		if !(*calls)[1].MP {
			t.Fatalf("第二次应在 mp 口径下请求：%+v", (*calls)[1])
		}
		if (*calls)[1].Path != tasksListPathVal {
			t.Fatalf("mp 回落路径 = %q，want %q", (*calls)[1].Path, tasksListPathVal)
		}
	})

	t.Run("mp 列表也找不到就返回 nil", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5"})
		})
		got, err := p.taskByCode(testAuth(), "Sequential_Tasks_1")
		if err != nil || got != nil {
			t.Fatalf("taskByCode = (%v,%v)，want (nil,nil)", got, err)
		}
		if len(*calls) != 2 {
			t.Fatalf("调用次数 = %d，want 2", len(*calls))
		}
	})

	t.Run("列表失败直接报错且不再回落 mp", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusInternalServerError, errEnvelope(50001, "boom")
		})
		got, err := p.taskByCode(testAuth(), "school_season")
		if err == nil {
			t.Fatal("列表 500 时应返回错误")
		}
		if got != nil {
			t.Fatalf("出错时任务应为空，got %+v", got)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（列表报错不该再打 mp 列表）", len(*calls))
		}
	})
}

// TestTaskByCodeMP mp 口径列表：命中、未命中、报错。
func TestTaskByCodeMP(t *testing.T) {
	t.Run("命中且强制带 mp 头", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "Sequential_Tasks_1", Target: 1})
		})
		got, err := p.taskByCodeMP(testAuth(), "Sequential_Tasks_1")
		if err != nil || got == nil || got.TaskCode != "Sequential_Tasks_1" {
			t.Fatalf("taskByCodeMP = (%+v,%v)", got, err)
		}
		if len(*calls) != 1 || !(*calls)[0].MP {
			t.Fatalf("调用记录 = %+v，want 1 次且带 mp 头", *calls)
		}
		if (*calls)[0].Method != http.MethodGet {
			t.Fatalf("读取应用 GET，got %s", (*calls)[0].Method)
		}
	})

	t.Run("未命中返回 nil 不报错", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope()
		})
		got, err := p.taskByCodeMP(testAuth(), "whatever")
		if err != nil || got != nil {
			t.Fatalf("taskByCodeMP = (%+v,%v)，want (nil,nil)", got, err)
		}
	})

	t.Run("报错透传", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusTooManyRequests, errEnvelope(42901, "rate limited")
		})
		if _, err := p.taskByCodeMP(testAuth(), "whatever"); err == nil {
			t.Fatal("429 时应返回错误")
		}
	})
}

// TestTaskByCodeWaiting 异步计分的轮询：达标即返回，否则在预算内轮询，失败不覆盖已有结果。
func TestTaskByCodeWaiting(t *testing.T) {
	oldGap := claimPollGap
	claimPollGap = 0
	t.Cleanup(func() { claimPollGap = oldGap })

	t.Run("已达标时一次就返回不再轮询", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			// target=1 current=1 → parseGrowthTasks 推算 Claimable=true
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 1, Current: 1})
		})
		got, err := p.taskByCodeWaiting(testAuth(), "chat_5")
		if err != nil || got == nil {
			t.Fatalf("taskByCodeWaiting = (%+v,%v)", got, err)
		}
		if !got.Claimable {
			t.Fatalf("应判定为可领取：%+v", got)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（已达标不该轮询）", len(*calls))
		}
	})

	t.Run("已领取同样立即返回", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", AcceptStatus: "claimed"})
		})
		if got, err := p.taskByCodeWaiting(testAuth(), "chat_5"); err != nil || got == nil || !got.Claimed {
			t.Fatalf("taskByCodeWaiting = (%+v,%v)", got, err)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1", len(*calls))
		}
	})

	t.Run("轮询到第 2 次达标就提前返回", func(t *testing.T) {
		var n atomic.Int64
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			n.Add(1)
			if seq == 1 {
				return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 1})
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 5})
		})
		got, err := p.taskByCodeWaiting(testAuth(), "chat_5")
		if err != nil || got == nil {
			t.Fatalf("taskByCodeWaiting = (%+v,%v)", got, err)
		}
		if got.Current != 5 || !got.Claimable {
			t.Fatalf("应拿到第二次的进度并判定达标：%+v", got)
		}
		if len(*calls) != 2 {
			t.Fatalf("调用次数 = %d，want 2（第 2 次达标后立即停）", len(*calls))
		}
	})

	t.Run("预算耗尽返回最后一次结果且不报错", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 1})
		})
		got, err := p.taskByCodeWaiting(testAuth(), "chat_5")
		if err != nil {
			t.Fatalf("轮询失败不应报错，got %v", err)
		}
		if got == nil || got.Current != 1 || got.Claimable {
			t.Fatalf("预算耗尽应返回最后一次结果（1/5，未达标），got %+v", got)
		}
		if len(*calls) != claimPollAttempts {
			t.Fatalf("调用次数 = %d，want claimPollAttempts=%d", len(*calls), claimPollAttempts)
		}
	})

	t.Run("轮询中查询失败保留上一次结果", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if seq == 2 {
				return http.StatusInternalServerError, errEnvelope(50001, "boom")
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5", Target: 5, Current: 1})
		})
		got, err := p.taskByCodeWaiting(testAuth(), "chat_5")
		if err != nil {
			t.Fatalf("轮询期间的失败不应覆盖结果，got %v", err)
		}
		if got == nil || got.Current != 1 {
			t.Fatalf("应保留第一次拿到的 1/5，got %+v", got)
		}
		if len(*calls) != 2 {
			t.Fatalf("调用次数 = %d，want 2（第 2 次失败即返回）", len(*calls))
		}
	})

	t.Run("任务不存在时不轮询", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(int, taskCall) (int, string) {
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5"})
		})
		got, err := p.taskByCodeWaiting(testAuth(), "no_such_task")
		if err != nil || got != nil {
			t.Fatalf("taskByCodeWaiting = (%+v,%v)，want (nil,nil)", got, err)
		}
		if len(*calls) != 1 {
			t.Fatalf("调用次数 = %d，want 1（不存在不该轮询）", len(*calls))
		}
	})
}

// TestAcceptWithVerifyMP accept 必须靠回读确认登记生效（上游存在 200+OK 但未登记）。
func TestAcceptWithVerifyMP(t *testing.T) {
	oldGap := mpActionGap
	mpActionGap = 0
	t.Cleanup(func() { mpActionGap = oldGap })

	countAccepts := func(calls []taskCall) int {
		n := 0
		for _, c := range calls {
			if c.Method == http.MethodPost && c.Path == tasksAcceptPathVal {
				n++
			}
		}
		return n
	}

	t.Run("登记生效一次通过", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodPost {
				return http.StatusOK, tasksEnvelope() // accept 响应体不参与判定
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "Sequential_Tasks_1", Target: 1, AcceptStatus: "accepted"})
		})
		if !p.acceptWithVerifyMP(testAuth(), "Sequential_Tasks_1") {
			t.Fatal("回读 accepted 应判定成功")
		}
		if n := countAccepts(*calls); n != 1 {
			t.Fatalf("accept 调用次数 = %d，want 1（一次通过不该重试）", n)
		}
		if !(*calls)[0].MP {
			t.Fatal("accept 必须带 mp 头（缺头上游返回 task not found）")
		}
		if body := (*calls)[0].Body; !strings.Contains(body, "Sequential_Tasks_1") {
			t.Fatalf("accept 请求体应带上 task_codes：%q", body)
		}
	})

	t.Run("回读未登记最多重试一次", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodPost {
				return http.StatusOK, tasksEnvelope()
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "Sequential_Tasks_1", AcceptStatus: "not_accepted"})
		})
		if p.acceptWithVerifyMP(testAuth(), "Sequential_Tasks_1") {
			t.Fatal("两次回读都没登记应判定失败")
		}
		if n := countAccepts(*calls); n != 2 {
			t.Fatalf("accept 调用次数 = %d，want 2（最多重试一次）", n)
		}
	})

	t.Run("accept 报错同样重试一次后放弃", func(t *testing.T) {
		p, calls := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodPost {
				return http.StatusInternalServerError, errEnvelope(50001, "accept failed")
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "Sequential_Tasks_1"})
		})
		if p.acceptWithVerifyMP(testAuth(), "Sequential_Tasks_1") {
			t.Fatal("accept 一直失败应返回 false")
		}
		if n := countAccepts(*calls); n != 2 {
			t.Fatalf("accept 调用次数 = %d，want 2", n)
		}
	})

	t.Run("回读拿不到任务视为未登记", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodPost {
				return http.StatusOK, tasksEnvelope()
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "chat_5"}) // 列表里没有该任务
		})
		if p.acceptWithVerifyMP(testAuth(), "Sequential_Tasks_1") {
			t.Fatal("回读为空应判定未登记")
		}
	})

	t.Run("accept_status 为空串视为未登记", func(t *testing.T) {
		p, _ := newTaskFakePanel(func(seq int, c taskCall) (int, string) {
			if c.Method == http.MethodPost {
				return http.StatusOK, tasksEnvelope()
			}
			return http.StatusOK, tasksEnvelope(taskSpec{Code: "Sequential_Tasks_1"})
		})
		if p.acceptWithVerifyMP(testAuth(), "Sequential_Tasks_1") {
			t.Fatal("空 accept_status 不该算登记成功")
		}
	})
}
