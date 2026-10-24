package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestMemberTierLifecycle 会员档位：写入后 Status 透出 paid/pkg/时刻；节流
// 「未读 = 需拉取、刚读 = 不需要」；未读账号三字段全空（不猜）。
func TestMemberTierLifecycle(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 未读：不透出（不猜）。
	if st, _ := p.Status("u1"); st.MemberTier != "" || st.MemberPkg != "" {
		t.Fatalf("未读账号不应透出档位: %+v", st)
	}
	if !p.TierNeedsFetch("u1") {
		t.Fatal("从未读取应需要拉取")
	}

	p.SetTier("u1", true, "Pro 体验版")
	st, _ := p.Status("u1")
	if st.MemberTier != "paid" || st.MemberPkg != "Pro 体验版" || st.MemberTierFetchedAt.IsZero() {
		t.Fatalf("paid 档位透出异常: %+v", st)
	}
	if p.TierNeedsFetch("u1") {
		t.Fatal("刚读取不需要再拉取（CST 日内节流）")
	}

	// 改判 free（套餐可能变化）。
	p.SetTier("u1", false, "")
	if st, _ := p.Status("u1"); st.MemberTier != "free" || st.MemberPkg != "" {
		t.Fatalf("free 档位透出异常: %+v", st)
	}

	// 不存在账号：no-op 不 panic。
	p.SetTier("nope", true, "x")
	if p.TierNeedsFetch("nope") {
		t.Fatal("不存在账号不需要拉取")
	}
}
