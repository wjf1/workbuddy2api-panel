package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 11140 风控连坐保护（restrict.go）的行为钉子。
//
// 背景：上游把「账号存在异常行为」判在设备/出口上，不是判在单个账号上。
// 换号 = 把下一个号也送进去（参照上游实测：一次任务 7 个号全封）。
// 两道闸必须都钉住：
//  1. 命中 11140 → 就地停手，不再换号；
//  2. 窗内 ≥2 个不同账号被限 → 全池暂停，期间请求 503 快速失败。

// TestIsRestrictionMarker 词表口径：11140 两种文案命中，14017 不命中。
//
// 关键：14017（trial not activated）**不能**触发连坐——它是 register 未完成，
// 补完即自愈，属单账号状态，不是设备级证据。误判会让一次试用号未激活
// 把全池停 30 分钟。
func TestIsRestrictionMarker(t *testing.T) {
	yes := []string{
		`{"code":11140,"msg":"request illegal"}`,
		`{"code":11140,"msg":"Request Illegal"}`,           // 大小写不敏感
		`{"code":11140,"msg":"unusual activity detected"}`, // 第二种风控文案
		`{"code":11140,"msg":"account has unusual activity"}`,
	}
	for _, b := range yes {
		if !upstream.IsRestrictionMarker(b) {
			t.Errorf("应判定为连坐信号: %s", b)
		}
	}

	no := []string{
		`{"code":14017,"msg":"trial not activated"}`,                           // 试用未激活，非设备级
		`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, // 模型级限流
		`{"code":6004,"msg":"model busy"}`,
		``,
		`plain text`,
	}
	for _, b := range no {
		if upstream.IsRestrictionMarker(b) {
			t.Errorf("不应判定为连坐信号: %s", b)
		}
	}
}

// TestRestrictionGateSingleAccountNoHold 单个账号反复命中不触发全池暂停。
//
// 判定口径是「不同账号数」：一个号被反复重试不能把暂停骗出来，
// 否则单号故障会误伤整个池子。
func TestRestrictionGateSingleAccountNoHold(t *testing.T) {
	var g restrictionGate
	hold := 30 * time.Minute

	for i := 0; i < 10; i++ {
		if g.note("uid-A", hold) {
			t.Fatalf("单账号重复命中第 %d 次就触发了全池暂停，口径应为「不同账号数」", i+1)
		}
	}
	if d := g.remaining(); d > 0 {
		t.Fatalf("单账号不应暂停全池，remaining=%s", d)
	}
}

// TestRestrictionGateTwoAccountsTriggersHold 窗内两个不同账号 → 全池暂停。
func TestRestrictionGateTwoAccountsTriggersHold(t *testing.T) {
	var g restrictionGate
	hold := 30 * time.Minute

	if g.note("uid-A", hold) {
		t.Fatal("第一个账号不应触发暂停")
	}
	if !g.note("uid-B", hold) {
		t.Fatal("第二个不同账号应触发全池暂停")
	}
	if d := g.remaining(); d <= 0 {
		t.Fatal("触发后 remaining 应大于 0")
	}
	// 触发后窗口清空：需要一个全新的一轮才再触发（不叠旧账）。
	if g.note("uid-C", hold) {
		t.Fatal("触发后窗口已清空，单个新账号不应立即再次触发")
	}
}

// TestRestrictionGateHoldDisabled hold<=0 关闭连坐保护：只保留「命中即停手」。
//
// 这是给「不想要全池暂停」的部署留的逃生口（upstream.restriction_hold: 0）。
func TestRestrictionGateHoldDisabled(t *testing.T) {
	var g restrictionGate
	for i := 0; i < 5; i++ {
		if g.note("uid-X", 0) {
			t.Fatal("hold=0 时不应触发暂停")
		}
	}
	if g.note("uid-Y", -time.Second) {
		t.Fatal("hold<0 时不应触发暂停")
	}
	if d := g.remaining(); d > 0 {
		t.Fatal("关闭时 remaining 应为 0")
	}
}

// TestRestrictionGateExpires 暂停到期后自动解除（无需人工干预）。
func TestRestrictionGateExpires(t *testing.T) {
	var g restrictionGate
	g.note("uid-A", 30*time.Millisecond)
	g.note("uid-B", 30*time.Millisecond)
	if g.remaining() <= 0 {
		t.Fatal("应已暂停")
	}
	time.Sleep(60 * time.Millisecond)
	if d := g.remaining(); d > 0 {
		t.Fatalf("应已自然解除，remaining=%s", d)
	}
}

// TestRestrictionGateWindowPruning 窗外旧命中被剪掉，不会永久累积。
func TestRestrictionGateWindowPruning(t *testing.T) {
	old := restrictionWaveWindow
	restrictionWaveWindow = 30 * time.Millisecond
	defer func() { restrictionWaveWindow = old }()

	var g restrictionGate
	hold := time.Minute
	g.note("uid-A", hold)             // 窗内第 1 个
	time.Sleep(60 * time.Millisecond) // A 滑出窗口
	if g.note("uid-B", hold) {
		t.Fatal("A 已滑出窗口，B 是窗内唯一账号，不应触发暂停")
	}
}

// TestChatCompletionsBlockedByRestrictionHold 暂停期内请求 503 + Retry-After，
// 且**不消耗任何账号**（不派发上游请求）。
//
// 这是「不再拿账号去试」的最终验收：暂停期必须是快速失败，而不是继续轮转。
func TestChatCompletionsBlockedByRestrictionHold(t *testing.T) {
	p := pool.New("")
	h := NewHandler(Config{
		Pool:            p,
		Upstream:        &upstream.Client{},
		RestrictionHold: 30 * time.Minute,
	})

	// 制造连坐：两个不同账号在窗内被限制。
	h.restrict.note("uid-A", h.cfg.RestrictionHold)
	h.restrict.note("uid-B", h.cfg.RestrictionHold)
	if h.restrict.remaining() <= 0 {
		t.Fatal("前置条件失败：应已进入全池暂停")
	}

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.chatCompletions(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("暂停期应回 503，got %d", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("503 应带 Retry-After 头")
	}
	// 错误体要让客户端知道「别重试循环」，而不是一个裸 503。
	bodyText := rec.Body.String()
	if !strings.Contains(bodyText, "11140") && !strings.Contains(bodyText, "restricted") {
		t.Fatalf("503 正文应说明原因，got %q", bodyText)
	}
}

// TestRestrictionHoldDefaults 缺省配置应为 30m（不是 0）。
//
// 0 在本模块语义里是「关闭连坐保护」，所以默认值必须在 NewHandler 里
// 显式填成 30m——否则不配这个键的部署会静默失去保护。
func TestRestrictionHoldDefaults(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: &upstream.Client{}})
	if h.cfg.RestrictionHold != 30*time.Minute {
		t.Fatalf("RestrictionHold 缺省应为 30m，got %s", h.cfg.RestrictionHold)
	}
}
