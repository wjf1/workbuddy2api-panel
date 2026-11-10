package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// SSE 粘帧拆分（sse_frame_split.go，对齐参照上游 afb5f22 / issue #197）的行为钉子。
//
// 核心：`data: {a} {b}` 这种一行两个 JSON 的帧，原样透传会让客户端解析失败、
// 整轮对话作废。拆分器必须把首尾相接的多个 JSON 值切开；切不动时返回单元素
// 走原样透传（宁可少切，不产出半截对象）。

func TestSplitConcatenatedJSON(t *testing.T) {
	a := `{"id":"1","choices":[]}`
	b := `{"id":"2","choices":[]}`

	cases := []struct {
		name  string
		in    string
		count int // 期望份数
	}{
		{"空格相接", a + " " + b, 2},
		{"直接相接", a + b, 2},
		{"换行相接", a + "\n" + b, 2},
		{"单对象不拆", a, 1},
		{"含转义引号的字符串", `{"text":"he said \"hi\" {fake}"}`, 1},
		{"字符串里的花括号不干扰", `{"a":"{","b":"}"}`, 1},
		{"嵌套对象", `{"a":{"b":1},"c":[1,2]}`, 1},
		{"数组值", `[1,2,3] [4,5]`, 2},
		{"三连帧", a + b + a, 3},
		{"空白包裹", "  " + a + "   " + b + "  ", 2},
		{"裸字符串值相接", `"hello" "world"`, 2},
		// 数字值无括号边界切不开：真实 SSE data 帧都是对象/数组，纯数字粘帧
		// 不存在于上游实际形态；「宁可少切」语义下保持单元素即可。
		{"数字值相接（切不开保持原样）", `1 2 3`, 1},
	}
	for _, c := range cases {
		parts := splitConcatenatedJSON(c.in)
		if len(parts) != c.count {
			t.Errorf("%s: want %d 份, got %d (%q)", c.name, c.count, len(parts), parts)
			continue
		}
		// 拆开时（count>1）每一份都必须是合法 JSON——宁可少切，不产出半截对象。
		// 未拆开时（count==1）原文原样返回，不校验（输入本就可能是垃圾）。
		if c.count > 1 {
			for i, p := range parts {
				if !json.Valid([]byte(p)) {
					t.Errorf("%s: 第 %d 份不是合法 JSON: %q", c.name, i, p)
				}
			}
		}
	}
}

func TestSplitConcatenatedJSONRejects(t *testing.T) {
	// 这些形态必须「切不动 → 返回单元素原样透传」：半截对象比粘帧更糟。
	a := `{"id":"1"}`
	cases := map[string]string{
		"括号不配平":      `{"a":1}} {"b":2}`,
		"字符串未闭合":     `{"a":"unclosed`,
		"末尾半截":       a + ` {"b":`,
		"非 JSON 垃圾":  `hello world`,
		"对象后跟垃圾":     a + ` garbage`,
		"深度为负":       `} {`,
		"空串":         ``,
		"引号内嵌套不闭合多帧": `{"a":"x"} {"b":"y`,
	}
	for name, in := range cases {
		parts := splitConcatenatedJSON(in)
		if len(parts) != 1 {
			t.Errorf("%s: 应保持单元素不拆, got %d 份 (%q)", name, len(parts), parts)
		}
	}
}

// TestLooksLikeJSON 只用来决定 WARN 与否：{ / [ 开头算「打算 JSON」。
func TestLooksLikeJSON(t *testing.T) {
	yes := []string{`{}`, ` [1]`, "\t{\"a\":1}", `{"a"`}
	for _, in := range yes {
		if !looksLikeJSON(in) {
			t.Errorf("应判定为 JSON 形态: %q", in)
		}
	}
	// 注：[DONE] 以 [ 开头会判 true，但它在上游 readLoop 被 sseDoneFrame 先行
	// 拦截，到不了本函数——不列为 no（与参照上游实现一致）。
	no := []string{"", "   ", ": heartbeat", "data: x"}
	for _, in := range no {
		if looksLikeJSON(in) {
			t.Errorf("不应判定为 JSON 形态: %q", in)
		}
	}
}

// TestSplitPartsCapped 极端畸形输入拆分数有上限（不把几百 KB 坏帧切成上千次写）。
func TestSplitPartsCapped(t *testing.T) {
	// 100 个合法小对象 → 超过 maxSplitParts 时整份放弃（返回单元素）
	var sb strings.Builder
	for i := 0; i < 100; i++ {
		sb.WriteString(`{"n":`)
		sb.WriteString(string(rune('0' + i%10)))
		sb.WriteString(`}`)
	}
	parts := splitConcatenatedJSON(sb.String())
	if len(parts) != 1 {
		t.Fatalf("超上限应整份放弃返回单元素, got %d 份", len(parts))
	}
}

// TestSplitPreservesContent 拆分不丢内容：拆出的份数重新拼接（去空白）应覆盖全部原文。
func TestSplitPreservesContent(t *testing.T) {
	in := `{"a":1}{"b":2}{"c":3}`
	parts := splitConcatenatedJSON(in)
	if len(parts) != 3 {
		t.Fatalf("want 3 份, got %d", len(parts))
	}
	// 每份内容逐对象核对
	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	for i, p := range parts {
		if p != want[i] {
			t.Errorf("第 %d 份 = %q, want %q", i, p, want[i])
		}
	}
}

// TestStreamSplitsConcatenatedFrame 端到端：粘帧进 → 拆分帧出。
//
// 上游把两个 chunk 塞进同一个 data 行时，客户端应收到两个各自合法的独立帧，
// 而不是一份解析必失败的文本（issue #197 的实际故障形态）。
func TestStreamSplitsConcatenatedFrame(t *testing.T) {
	f1 := `{"id":"x1","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{"content":"He"}}]}`
	f2 := `{"id":"x1","object":"chat.completion.chunk","created":1,"model":"glm-5.2","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}]}`
	raw := "data: " + f1 + " " + f2 + "\n\n"

	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()

	// 两个 id 相同的帧应各出现一次（拆开各自走了规范化路径）
	if got := strings.Count(body, `"delta":{"content":"He"}`); got != 1 {
		t.Errorf("第一帧应恰好出现 1 次, got %d\nbody=%s", got, body)
	}
	if got := strings.Count(body, `"delta":{"content":"llo"}`); got != 1 {
		t.Errorf("第二帧应恰好出现 1 次, got %d\nbody=%s", got, body)
	}
	// 输出里的每个 data 帧都必须是单独合法的 JSON（无粘帧残留）
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			t.Errorf("输出帧不是合法 JSON: %q (%v)", payload, err)
		}
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("缺 [DONE] 收尾")
	}
}
