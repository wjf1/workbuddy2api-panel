package pool

import (
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDeadKindClassification Status.DeadKind 按 reason 分类：session_dead / banned / 其他。
// reason 是自由文本，分类收口在 deadKindOf，前端不猜字符串。
func TestDeadKindClassification(t *testing.T) {
	cases := []struct {
		reason string
		want   string
	}{
		{"12153 session dead", "session_dead"},                                                       // NoteSessionDead 满阈 / handler 信封 12153
		{"refresh session dead", "session_dead"},                                                     // handler.go chat 刷新失败单发
		{"account banned by upstream (11140 request illegal), re-login required", "banned"},          // handler.go 11140
		{"manual disable (panel)", ""},                                                               // 人工禁用：普通已禁用
		{"", ""},                                                                                     // 健康账号
	}
	for _, tc := range cases {
		if got := deadKindOf(tc.reason); got != tc.want {
			t.Errorf("deadKindOf(%q)=%q want %q", tc.reason, got, tc.want)
		}
	}
}

// TestDeadKindStatusFlow 状态流转中 DeadKind 的填充与清除：禁用时填充、复活时清空。
func TestDeadKindStatusFlow(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 健康账号恒空。
	if st, _ := p.Status("u1"); st.DeadKind != "" {
		t.Fatalf("健康账号 DeadKind=%q want \"\"", st.DeadKind)
	}

	// 连续 3 次 12153 禁用 → session_dead。
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	st, _ := p.Status("u1")
	if !st.Disabled || st.DeadKind != "session_dead" {
		t.Fatalf("12153 判死后 DeadKind=%q want session_dead (disabled=%v)", st.DeadKind, st.Disabled)
	}

	// Revive（重登成功路径）清禁用 + DeadKind。
	p.Revive("u1")
	if st, _ := p.Status("u1"); st.DeadKind != "" || st.Disabled {
		t.Fatalf("复活后 DeadKind=%q disabled=%v want 空/false", st.DeadKind, st.Disabled)
	}

	// 人工禁用 → DeadKind 空，不进「待重登」分组。
	p.Disable("u1", "manual disable (panel)")
	if st, _ := p.Status("u1"); st.DeadKind != "" {
		t.Fatalf("人工禁用 DeadKind=%q want \"\"", st.DeadKind)
	}
}

// TestSessionDeadReasonPredicate 判定函数与 reason 文案的契约（M2 告警复用同一判定）。
func TestSessionDeadReasonPredicate(t *testing.T) {
	if !IsSessionDeadReason("12153 session dead") || !IsSessionDeadReason("refresh session dead") {
		t.Error("两类 session dead 终态都应命中 IsSessionDeadReason")
	}
	if IsSessionDeadReason("account banned by upstream (11140 request illegal), re-login required") {
		t.Error("banned 不应命中 IsSessionDeadReason")
	}
	if !IsBannedReason("account banned by upstream (11140 request illegal), re-login required") {
		t.Error("banned reason 应命中 IsBannedReason")
	}
	if IsBannedReason("12153 session dead") {
		t.Error("session dead 不应命中 IsBannedReason")
	}
}
