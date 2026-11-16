package server

// narration / toolonly 两条救援的判据钉子。
//
// 这两条救援的共同点：**误命中会把正常回答续成废话**，所以判据必须窄。用例重点
// 不在「能救」，而在「不该救的时候一定不救」——那是线上投诉的真实来源。

import (
	"io"
	"strings"
	"testing"
)

// narrationBody 构造「声明了工具 + 历史里有工具轮」的请求体（narration 的必要条件）。
func narrationBody() string {
	return `{"model":"deepseek-v4.1-flash","tools":[{"type":"function","function":{"name":"read_file"}}],` +
		`"messages":[{"role":"user","content":"查一下"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"file body"}]}`
}

// TestNarrationTailSuspends 尾部形态判定：预告符 → true，句末标点 → false。
func TestNarrationTailSuspends(t *testing.T) {
	suspends := []string{
		"连接并测试下行：",
		"验证路由器状态：",
		"以下是配置",
		"步骤一、准备环境，",
		"代码块写了一半```go\nfunc main() {\n",
		"Reading failfirst.sh mutation script",
	}
	for _, s := range suspends {
		if !narrationTailSuspends(s) {
			t.Errorf("%q 应判为未说完", s)
		}
	}
	complete := []string{
		"已完成全部检查。",
		"结果是 42!",
		"He said \"done\"",
		"结束）",
		"",
	}
	for _, s := range complete {
		if narrationTailSuspends(s) {
			t.Errorf("%q 应判为已说完", s)
		}
	}
}

// TestNarrationSkipsToolMarkup 正文含工具调用标记 → 一律不救（否则会重复执行）。
func TestNarrationSkipsToolMarkup(t *testing.T) {
	cases := []string{
		"准备调用：</｜｜DSML｜｜ invoke>",
		"<function_calls>read_file</function_calls>",
		`<invoke name="read_file">`,
	}
	for _, s := range cases {
		if narrationTailSuspends(s) {
			t.Errorf("含工具标记的正文不该判为未说完: %q", s)
		}
	}
}

// TestNarrationRescuesSuspendedTail 预告句收尾且是工具会话 → 触发救援。
func TestNarrationRescuesSuspendedTail(t *testing.T) {
	seg1 := streamFrame("连接并测试下行：") + endFrame("stop", 30, 0) + doneFrame
	seg2 := streamFrame("已连上，延迟 12ms。") + endFrame("stop", 20, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) { return 200, seg2, true })

	r := newTestReader(t, up, narrationBody(), io.NopCloser(strings.NewReader(seg1)), 0)
	out := drain(t, r)

	if up.callCount() != 1 {
		t.Fatalf("预告句收尾应触发一次救援，实际 %d", up.callCount())
	}
	if !strings.Contains(out, "已连上") {
		t.Errorf("救援段内容应接进流，实际:\n%s", out)
	}
	// 救援指令必须点明「停在预告上」，否则模型会以为已说完。
	if !strings.Contains(up.body(0), "预告") {
		t.Errorf("救援请求应带预告式指令，实际 body:\n%s", up.body(0))
	}
}

// TestNarrationSkipsWhenNoToolTurn 没有工具轮（闲聊/角色扮演）→ 不救。
func TestNarrationSkipsWhenNoToolTurn(t *testing.T) {
	seg1 := streamFrame("那么，接下来：") + endFrame("stop", 30, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) { return 200, seg1, true })

	// 声明了 tools 但历史里没有工具轮。
	body := `{"model":"deepseek-v4.1-flash","tools":[{"type":"function","function":{"name":"x"}}],` +
		`"messages":[{"role":"user","content":"你好"}]}`
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(seg1)), 0)
	drain(t, r)

	if up.callCount() != 0 {
		t.Fatalf("无工具轮时不该救援（那是正常文风），实际 %d 次", up.callCount())
	}
}

// TestToolOnlyRescuesShortAnswerAfterToolResult 末条是工具结果 + 短正文 + 长 prompt
// → 试续一次，且只采纳工具调用。
func TestToolOnlyRescuesShortAnswerAfterToolResult(t *testing.T) {
	seg1 := streamFrame("看完了，需要走通验证。") + bigPromptEndFrame("stop", 30, 30000) + doneFrame
	// 救援段给工具调用：应被采纳。
	seg2 := toolFrame() + endFrame("stop", 20, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) { return 200, seg2, true })

	// prompt_tokens 需 >= toolOnlyMinPrompt：把 usage 的 prompt 提到 30000。
	body := narrativeToolBody(30000)
	r := newTestReader(t, up, body, io.NopCloser(strings.NewReader(seg1)), 0)
	out := drain(t, r)

	if up.callCount() != 1 {
		t.Fatalf("应触发一次安全续写，实际 %d 次", up.callCount())
	}
	if !strings.Contains(out, "tool_calls") {
		t.Errorf("救援段的工具调用应被采纳，实际:\n%s", out)
	}
	// 救援指令必须点明不要重复上一个工具调用（写操作会二次生效）。
	if !strings.Contains(up.body(0), "不要重复调用") {
		t.Errorf("救援请求应带防重复指令，实际 body:\n%s", up.body(0))
	}
}

// TestToolOnlyDiscardsTextOnlyRescue 救援段只有正文 → 整段丢弃，客户端看到原样。
func TestToolOnlyDiscardsTextOnlyRescue(t *testing.T) {
	seg1 := streamFrame("看完了，需要走通验证。") + bigPromptEndFrame("stop", 30, 30000) + doneFrame
	seg2 := streamFrame("这是救援段多写的废话。") + endFrame("stop", 20, 0) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) { return 200, seg2, true })

	r := newTestReader(t, up, narrativeToolBody(30000), io.NopCloser(strings.NewReader(seg1)), 0)
	out := drain(t, r)

	if up.callCount() != 1 {
		t.Fatalf("应尝试一次安全续写，实际 %d 次", up.callCount())
	}
	if strings.Contains(out, "救援段多写的废话") {
		t.Errorf("救援段的正文必须整段丢弃（否则把正常回答续成废话），实际:\n%s", out)
	}
	if !strings.Contains(out, "看完了") {
		t.Errorf("首段内容应保留，实际:\n%s", out)
	}
}

// TestToolOnlySkipsShortPrompt 短会话（prompt < 20000）→ 不救（一句问答很正常）。
func TestToolOnlySkipsShortPrompt(t *testing.T) {
	seg1 := streamFrame("看完了，需要走通验证。") + bigPromptEndFrame("stop", 30, 5000) + doneFrame
	up := newScriptedUpstream(t, func(call int) (int, string, bool) { return 200, seg1, true })

	r := newTestReader(t, up, narrativeToolBody(5000), io.NopCloser(strings.NewReader(seg1)), 0)
	drain(t, r)

	if up.callCount() != 0 {
		t.Fatalf("短会话不该触发安全续写，实际 %d 次", up.callCount())
	}
}

// narrativeToolBody 构造「声明工具 + 有工具轮 + 末条是工具结果」的请求体。
// prompt 规模由首段 usage 决定（见 bigPromptEndFrame）。
func narrativeToolBody(promptTokens float64) string {
	_ = promptTokens
	return `{"model":"deepseek-v4.1-flash","tools":[{"type":"function","function":{"name":"read_file"}}],` +
		`"messages":[{"role":"user","content":"查一下 a 文件"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"c1","content":"file body"}]}`
}
