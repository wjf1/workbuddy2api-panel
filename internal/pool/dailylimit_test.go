package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDailyLimitToken 护栏①：每日 Token 限额达限 → 该号不接任何模型；跨日
// （日期键翻转）自动解封。
func TestDailyLimitToken(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	p.SetDailyLimits(1000, 0, 0) // 日限 1000 token

	p.RecordDailyUsage("u1", "glm-5.2", 600, 0)
	if p.Pick() == nil {
		t.Fatal("600/1000 token 应仍可选")
	}

	p.RecordDailyUsage("u1", "glm-5.2", 500, 0) // 累计 1100 ≥ 1000
	if got := p.Pick(); got != nil {
		t.Fatalf("达限后不应可选, got %v", got)
	}
	// Status 透出当日计数（面板徽章）。
	st, _ := p.Status("u1")
	if st.DailyTokens != 1100 {
		t.Fatalf("daily_tokens=%d want 1100", st.DailyTokens)
	}

	// 跨日解封：手动把日期键拨回昨天。
	p.mu.Lock()
	e := p.byUID["u1"]
	e.dailyDay = "" // 强制视为"非今天" → 读取侧归零
	p.mu.Unlock()
	if p.Pick() == nil {
		t.Fatal("跨日应自动解封")
	}
}

// TestDailyLimitCreditOnlyFree 护栏②：每日积分限额达限 → 只放免费模型
// （收费判定复用 floorBlocked 同判据：无实测台账 + 无目录倍率 = 未知 = 放行）。
func TestDailyLimitCreditOnlyFree(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	p.SetDailyLimits(0, 500*1000, 0) // 日限 500 积分

	p.RecordDailyUsage("u1", "glm-5.2", 100, 600*1000) // 消耗 600 ≥ 500
	// 无台账/倍率观测：未知模型按放行（保守选择，与保底同哲学）。
	if p.Pick() == nil {
		t.Fatal("未知收费模型应放行（未知不拦）")
	}
	// 注入实测台账（免费）→ 放行。
	p.NoteModelCost("u1", "free-model", 0, 1000) // cost=0 免费
	if p.Pick() == nil {
		t.Fatal("免费模型应放行")
	}
	// 注入实测台账（收费）→ 拦。
	p.NoteModelCost("u1", "paid-model", 5.0, 1000)
	if p.PickExcludingForRealm(nil, "paid-model", "") != nil {
		t.Fatal("达限后收费模型应被拦")
	}
	if p.PickExcludingForRealm(nil, "free-model", "") == nil {
		t.Fatal("达限后免费模型仍应可选")
	}
}

// TestDailyLimitPerModel 护栏③：按模型每日 Token 限额——达限只拦该模型，
// 同号其他模型照常。
func TestDailyLimitPerModel(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000, 0)
	p.SetDailyLimits(0, 0, 800) // 按模型日限 800

	p.RecordDailyUsage("u1", "glm-5.2", 900, 0)
	if p.PickExcludingForRealm(nil, "glm-5.2", "") != nil {
		t.Fatal("该模型达限应被拦")
	}
	if p.PickExcludingForRealm(nil, "glm-5.1", "") == nil {
		t.Fatal("其他模型不应受牵连")
	}
}
