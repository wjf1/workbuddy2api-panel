package pool

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestCreditsTrendLifecycle 采样（同桶覆盖）→ Close 兜底落盘 → 重载恢复一致 →
// 聚合（全池总和按桶并集）。
func TestCreditsTrendLifecycle(t *testing.T) {
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")
	p := New(stateFp)
	p.Add(&auth.Auth{UID: "u1", Nickname: "甲"})
	p.Add(&auth.Auth{UID: "u2", Nickname: "乙"})

	p.SetCreditsDetailed("u1", 1000, 2000, 0, time.Time{}, 0) // 桶 H0
	p.SetCreditsDetailed("u1", 900, 2000, 0, time.Time{}, 0)  // 同桶覆盖 → 900
	p.SetCreditsDetailed("u2", 500, 800, 0, time.Time{}, 0)   // 同桶 → 总和 1400

	p.Close() // 兜底落盘（节流窗口内的尾部采样随退出强制写）

	p2 := New(stateFp)
	tr := p2.CreditsTrend()
	p2.Close()
	if len(tr.Total) != 1 || tr.Total[0].Credits != 1400 {
		t.Fatalf("重载后 total=%+v want 1 点 1400（Close 兜底落盘）", tr.Total)
	}
	if len(tr.Series) != 2 {
		t.Fatalf("series=%d want 2", len(tr.Series))
	}
}

// TestCreditsTrendWindowTrim 超过 30 天的旧桶被裁剪；节流后 Close 兜底不丢新桶。
func TestCreditsTrendWindowTrim(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})

	// 直接注入旧桶（绕过 time.Now）。
	old := time.Now().AddDate(0, 0, -40).Truncate(time.Hour)
	p.mu.Lock()
	if p.creditHist == nil { // lazy-init 与 recordCreditsPointLocked 同口径
		p.creditHist = map[string][]creditPoint{}
	}
	p.creditHist["u1"] = []creditPoint{{Unix: old.Unix(), Credits: 1}}
	p.mu.Unlock()

	p.SetCreditsDetailed("u1", 700, 1000, 0, time.Time{}, 0) // 新桶 + 裁剪
	p.Close()

	tr := p.CreditsTrend()
	for _, pt := range tr.Total {
		if pt.T == old.Unix() {
			t.Fatal("40 天前的旧桶应被裁剪")
		}
	}
	if len(tr.Total) != 1 || tr.Total[0].Credits != 700 {
		t.Fatalf("total=%+v want 1 点 700（只剩新桶）", tr.Total)
	}
}
