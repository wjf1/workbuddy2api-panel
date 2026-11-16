package upstream

// output_budget 的行为钉子：抬升的四种必要情形 + 四种「不猜」情形。
//
// 这组用例锁的是「出站前兜住预算被思考偷走」的语义，避免将来改动
// ApplyMinOutputBudget 时把探活请求也抬了、或把已达标/未给额度的请求误改。

import "testing"

// TestMinOutputBudgetTable 档位表：已知档返回下限，未知档返回 0。
func TestMinOutputBudgetTable(t *testing.T) {
	cases := []struct {
		effort string
		want   int64
	}{
		{"minimal", 4096},
		{"low", 4096},
		{"medium", 6144},
		{"high", 8192},
		{"xhigh", 12288},
		{"max", 16384},
		{"HIGH", 8192},   // 大小写不敏感
		{" high ", 8192}, // 空白容忍
		{"unknown", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := MinOutputBudget(c.effort); got != c.want {
			t.Errorf("MinOutputBudget(%q)=%d want %d", c.effort, got, c.want)
		}
	}
}

// TestApplyMinOutputBudgetRaises 低于下限的显式额度抬到下限。
func TestApplyMinOutputBudgetRaises(t *testing.T) {
	obj := map[string]any{"max_tokens": float64(2048)}
	if !ApplyMinOutputBudget(obj, "high") {
		t.Fatal("2048 低于 high 档下限 8192，应被抬升")
	}
	if got, _ := explicitOutputBudget(obj); got != 8192 {
		t.Errorf("max_tokens=%v want 8192", obj["max_tokens"])
	}
}

// TestApplyMinOutputBudgetLeavesAlone 四种不猜的情形都不动额度。
func TestApplyMinOutputBudgetLeavesAlone(t *testing.T) {
	cases := []struct {
		name   string
		obj    map[string]any
		effort string
	}{
		{"没给额度", map[string]any{}, "high"},
		{"探活级额度", map[string]any{"max_tokens": float64(1)}, "high"},
		{"已超下限", map[string]any{"max_tokens": float64(32768)}, "high"},
		{"档位未知", map[string]any{"max_tokens": float64(2048)}, "bogus"},
		{"关思考", map[string]any{"max_tokens": float64(2048), "thinking": map[string]any{"type": "disabled"}}, "high"},
		{"档位 off", map[string]any{"max_tokens": float64(2048)}, "off"},
	}
	for _, c := range cases {
		before := c.obj["max_tokens"]
		if ApplyMinOutputBudget(c.obj, c.effort) {
			t.Errorf("%s: 不该改动额度（changed=true）", c.name)
		}
		if c.obj["max_tokens"] != before {
			t.Errorf("%s: 额度被改成 %v（原 %v）", c.name, c.obj["max_tokens"], before)
		}
	}
}

// TestEnsureMinBudget 救援段补额度的直接入口（floor=0 或探活级时不动）。
func TestEnsureMinBudget(t *testing.T) {
	obj := map[string]any{"max_tokens": float64(1024)}
	if !EnsureMinBudget(obj, 8192) {
		t.Fatal("1024 < floor 8192 应被抬升")
	}
	if v, _ := explicitOutputBudget(obj); v != 8192 {
		t.Errorf("got %d want 8192", v)
	}

	probe := map[string]any{"max_tokens": float64(1)}
	if EnsureMinBudget(probe, 8192) {
		t.Error("探活级额度不该被抬")
	}
	none := map[string]any{}
	if EnsureMinBudget(none, 8192) {
		t.Error("没给额度时不该凭空写入")
	}
	if EnsureMinBudget(map[string]any{"max_tokens": float64(1024)}, 0) {
		t.Error("floor<=0 不该改动")
	}
}

// TestExplicitOutputBudgetAliases 三个额度别名都能读到（含字符串形态）。
func TestExplicitOutputBudgetAliases(t *testing.T) {
	cases := []struct {
		obj  map[string]any
		want int64
		ok   bool
	}{
		{map[string]any{"max_tokens": float64(100)}, 100, true},
		{map[string]any{"max_completion_tokens": int(200)}, 200, true},
		{map[string]any{"max_output_tokens": int64(300)}, 300, true},
		{map[string]any{"max_tokens": "400"}, 400, true},
		{map[string]any{"max_tokens": "abc"}, 0, false},
		{map[string]any{}, 0, false},
	}
	for i, c := range cases {
		got, ok := explicitOutputBudget(c.obj)
		if got != c.want || ok != c.ok {
			t.Errorf("case %d: got (%d,%v) want (%d,%v)", i, got, ok, c.want, c.ok)
		}
	}
}

// TestThinkingSwitchSupported 只有 DeepSeek 系支持显式关思考。
func TestThinkingSwitchSupported(t *testing.T) {
	for _, m := range []string{"deepseek-v4.1-flash", "DeepSeek-V4-Pro", " deepseek-r1 "} {
		if !ThinkingSwitchSupported(m) {
			t.Errorf("%q 应支持思考开关", m)
		}
	}
	for _, m := range []string{"glm-5.2", "kimi-k2", "qwen3-max", ""} {
		if ThinkingSwitchSupported(m) {
			t.Errorf("%q 不应判为支持思考开关", m)
		}
	}
}
