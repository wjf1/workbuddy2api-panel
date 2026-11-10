package server

import (
	"encoding/json"
	"testing"
)

// 「思考吃光预算」救援（starved.go）的行为钉子。
//
// 背景：上游把 reasoning 与正文算在同一 max_tokens 预算里，预算偏小时思考独吞全部，
// 客户端看到正文 0 字节 + finish=length（即「空回复」）。救援 = 同额度关思考重发一次。
//
// 最容易出错、也最危险的两点必须钉死：
//  1. 触发条件不能放宽（否则会重发正常请求，凭空多打一次上游、多耗一次额度）；
//  2. 改写后的 body 必须是合法请求（thinking=disabled 且不带 effort）。

func TestAggregateStarvedLengthEmpty(t *testing.T) {
	// 典型形态：finish=length + 正文空 + 无工具
	resp := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "length",
			"message": map[string]any{
				"role":              "assistant",
				"content":           "",
				"reasoning_content": "思考思考思考",
			},
		}},
	}
	if !aggregateStarved(resp) {
		t.Fatal("finish=length + 正文空 应触发救援")
	}
}

func TestAggregateStarvedStopWithReasoning(t *testing.T) {
	// 第二种成因：上游思考完直接 stop，正文 0B 但有思考内容
	resp := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message": map[string]any{
				"content":           "",
				"reasoning_content": "想了想，决定不说了",
			},
		}},
	}
	if !aggregateStarved(resp) {
		t.Fatal("finish=stop + 正文空 + 有思考 应触发救援")
	}
}

// TestAggregateStarvedNegatives 不该救援的形态：一个都不能误判。
//
// 误判的代价是「凭空重发一次」，会多耗一份上游额度、也让同一请求打两次上游。
func TestAggregateStarvedNegatives(t *testing.T) {
	cases := map[string]map[string]any{
		"有正文的 length 截断（额度语义正常结果）": {
			"choices": []any{map[string]any{
				"finish_reason": "length",
				"message":       map[string]any{"content": "被截断的正文"},
			}},
		},
		"有工具调用（合法工具轮）": {
			"choices": []any{map[string]any{
				"finish_reason": "length",
				"message": map[string]any{
					"content": "",
					"tool_calls": []any{
						map[string]any{"id": "c1", "type": "function"},
					},
				},
			}},
		},
		"finish=stop 且无思考（上游真给了空回复，不是预算问题）": {
			"choices": []any{map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"content": "", "reasoning_content": ""},
			}},
		},
		"正常完成": {
			"choices": []any{map[string]any{
				"finish_reason": "stop",
				"message":       map[string]any{"content": "正常回答"},
			}},
		},
		"choices 为空": {},
	}
	for name, resp := range cases {
		if aggregateStarved(resp) {
			t.Errorf("不应触发救援: %s", name)
		}
	}
}

// TestStarvedRetryBodyDisablesThinking 改写后的 body：thinking=disabled 且不带档位。
//
// 关键：disabled 与 effort 并存时上游语义未取证，必须删掉档位。
func TestStarvedRetryBodyDisablesThinking(t *testing.T) {
	in := []byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":300,"messages":[],"reasoning_effort":"high"}`)
	out, ok := starvedRetryBody(in)
	if !ok {
		t.Fatal("deepseek 模型应可改写")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("改写结果不是合法 JSON: %v", err)
	}
	th, ok := obj["thinking"].(map[string]any)
	if !ok {
		t.Fatalf("应注入 thinking 对象, got %#v", obj["thinking"])
	}
	if th["type"] != "disabled" {
		t.Fatalf("thinking.type 应为 disabled, got %v", th["type"])
	}
	if _, exists := obj["reasoning_effort"]; exists {
		t.Fatal("disabled 时必须删掉 reasoning_effort（组合语义未取证）")
	}
	// 额度不能被放大（超发是对客户端的语义违约）
	if obj["max_tokens"] != float64(300) {
		t.Fatalf("max_tokens 不应被改动, got %v", obj["max_tokens"])
	}
}

// TestStarvedRetryBodyAlreadyDisabled 已经关着思考 → 不改写（成因不在思考）。
func TestStarvedRetryBodyAlreadyDisabled(t *testing.T) {
	in := []byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":300,"thinking":{"type":"disabled"}}`)
	if _, ok := starvedRetryBody(in); ok {
		t.Fatal("已 disabled 时不应再改写（会无限重发）")
	}
}

// TestStarvedRetryBodyTinyBudget 极小额度不救援（key 探活场景）。
func TestStarvedRetryBodyTinyBudget(t *testing.T) {
	in := []byte(`{"model":"cn:deepseek-v4.1-flash","max_tokens":1,"messages":[]}`)
	if _, ok := starvedRetryBody(in); ok {
		t.Fatal("max_tokens=1 是 key 探活，不应救援")
	}
}

// TestStarvedRetryBodyNonDeepSeek 非 deepseek 模型不认 thinking 开关 → 不改写。
func TestStarvedRetryBodyNonDeepSeek(t *testing.T) {
	in := []byte(`{"model":"cn:glm-5.2","max_tokens":300,"messages":[]}`)
	if _, ok := starvedRetryBody(in); ok {
		t.Fatal("glm 模型不支持 thinking 开关，不应改写")
	}
}

// TestStarvedRetryBodyMalformed 畸形 body → 不改写（不猜）。
func TestStarvedRetryBodyMalformed(t *testing.T) {
	if _, ok := starvedRetryBody([]byte(`{bad json`)); ok {
		t.Fatal("畸形 body 不应改写")
	}
}

// TestOutputBudgetAliases 三个额度别名都能读到（含 float64 / int 两种数值形态）。
func TestOutputBudgetAliases(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"max_tokens":300}`, 300},
		{`{"max_completion_tokens":512}`, 512},
		{`{"max_output_tokens":1024}`, 1024},
		{`{}`, 0},
	}
	for _, c := range cases {
		var obj map[string]any
		if err := json.Unmarshal([]byte(c.body), &obj); err != nil {
			t.Fatal(err)
		}
		if got := outputBudget(obj); got != c.want {
			t.Errorf("%s: got %d want %d", c.body, got, c.want)
		}
	}
}

// TestThinkingSwitchSupported 只有 deepseek 系可关思考（与 upstream/thinking.go 注入范围一致）。
func TestThinkingSwitchSupported(t *testing.T) {
	yes := []string{"cn:deepseek-v4.1-flash", "deepseek-reasoner", "DeepSeek-V4"}
	for _, m := range yes {
		if !thinkingSwitchSupported(m) {
			t.Errorf("应支持: %s", m)
		}
	}
	no := []string{"cn:glm-5.2", "kimi-k2", "qwen-max", ""}
	for _, m := range no {
		if thinkingSwitchSupported(m) {
			t.Errorf("不应支持: %s", m)
		}
	}
}
