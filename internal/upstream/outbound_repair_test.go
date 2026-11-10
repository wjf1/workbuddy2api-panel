package upstream

import (
	"encoding/json"
	"testing"
)

// 出站形状修复（empty_text_part.go + reasoning_mirror.go，对齐参照上游 f987bac）
// 的行为钉子。两条都是「同 body 换任何账号都必然失败」的形态：
//   - 空 text 段 → 上游 400/11151，客户端重试只会重放同一段历史；
//   - Kimi/GLM 等按 `reasoning` 读历史 → 只发 reasoning_content 时多轮一致性挂。

// TestGuardEmptyTextPartsArray 数组形态矩阵（上游实测逐形态对照）。
func TestGuardEmptyTextPartsArray(t *testing.T) {
	cases := []struct {
		name    string
		parts   string
		wantPad bool
	}{
		{"空串 -> 补", `[{"type":"text","text":""}]`, true},
		{"null -> 补", `[{"type":"text","text":null}]`, true},
		{"缺失 -> 补", `[{"type":"text"}]`, true},
		{"纯空格 -> 不动（上游只判零长度不 trim）", `[{"type":"text","text":"   "}]`, false},
		{"未知 type -> 不动（上游忽略不受校验的段）", `[{"type":"textarea","text":""}]`, false},
		{"非空 -> 不动", `[{"type":"text","text":"hi"}]`, false},
		{"图片段不受影响", `[{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":""}]`, true},
	}
	for _, c := range cases {
		msg := map[string]any{
			"role":    "user",
			"content": mustJSONRaw(t, c.parts),
		}
		_ = msg
		// 走完整 guard 入口（messages 数组包装）
		obj := map[string]any{
			"messages": []any{map[string]any{
				"role":    "user",
				"content": mustJSONSlice(t, c.parts),
			}},
		}
		guardEmptyTextParts(obj["messages"].([]any))
		got := obj["messages"].([]any)[0].(map[string]any)["content"].([]any)
		text0 := got[0].(map[string]any)["text"]
		if c.wantPad {
			// 找到被补的 text 段（可能在任意下标）
			padded := false
			for _, p := range got {
				pm := p.(map[string]any)
				if pm["type"] == "text" {
					if s, _ := pm["text"].(string); s == " " {
						padded = true
					}
				}
			}
			if !padded {
				t.Errorf("%s: 应补位为空格, got %v", c.name, got)
			}
			_ = text0
		} else {
			for _, p := range got {
				pm := p.(map[string]any)
				if pm["type"] == "text" && c.parts != `[{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":""}]` {
					if s, _ := pm["text"].(string); s == " " {
						t.Errorf("%s: 不应补位, got %v", c.name, got)
					}
				}
			}
		}
	}
}

// TestGuardEmptyTextPartsSniffedString 字符串信封被上游嗅探成 parts 的形态
// （工具结果里的表单/按钮数组恰好是这个形状）。
func TestGuardEmptyTextPartsSniffedString(t *testing.T) {
	inner := `[{"type":"text","text":""},{"type":"text","text":"data"}]`
	msgs := []any{map[string]any{"role": "tool", "content": inner}}
	guardEmptyTextParts(msgs)

	got := msgs[0].(map[string]any)["content"].(string)
	// 字符串形态重新序列化后，空段应已被补成空格
	var parts []map[string]any
	if err := json.Unmarshal([]byte(got), &parts); err != nil {
		t.Fatalf("重新序列化失败: %v (%q)", err, got)
	}
	if parts[0]["text"] != " " {
		t.Errorf("嗅探出的空段应补位, got %q", got)
	}
	if parts[1]["text"] != "data" {
		t.Errorf("非空段不应改动, got %q", got)
	}
}

// TestGuardEmptyTextPartsStringNotSniffed 嗅探反例：普通字符串一律不动。
func TestGuardEmptyTextPartsStringNotSniffed(t *testing.T) {
	cases := []string{
		"普通文本",
		`[1,2,3]`,         // 数组但元素非对象
		`[{"a":1}]`,       // 对象数组但没有 text 段（无改动也不重排）
		`[{"type":"text"`, // 以 [ 开头但解析失败
		``,                // 空串（解析出空数组，不算 parts）
		`数字开头`,            // 非 [ 开头
	}
	for _, in := range cases {
		orig := in
		msgs := []any{map[string]any{"role": "user", "content": in}}
		guardEmptyTextParts(msgs)
		if got := msgs[0].(map[string]any)["content"]; got != orig {
			t.Errorf("嗅探反例不应改动 content: in=%q got=%q", orig, got)
		}
	}
}

// TestMirrorUpstreamReasoning 镜像纪律：六家族命中、非家族不动、绝不新建字段。
func TestMirrorUpstreamReasoning(t *testing.T) {
	// 家族命中：reasoning_content 有值、reasoning 缺失 → 镜像
	obj := map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{"role": "assistant", "reasoning_content": "想了想"},
		},
	}
	mirrorUpstreamReasoning(obj)
	msg := obj["messages"].([]any)[0].(map[string]any)
	if msg["reasoning"] != "想了想" {
		t.Fatalf("glm 应镜像 reasoning, got %v", msg["reasoning"])
	}

	// 反例家族：gpt/qwen 不看 reasoning 字段，不镜像
	obj2 := map[string]any{
		"model": "gpt-5.4",
		"messages": []any{
			map[string]any{"role": "assistant", "reasoning_content": "x"},
		},
	}
	mirrorUpstreamReasoning(obj2)
	if _, exists := obj2["messages"].([]any)[0].(map[string]any)["reasoning"]; exists {
		t.Fatal("gpt 不应被镜像")
	}

	// 已有非空 reasoning 不覆盖
	obj3 := map[string]any{
		"model": "kimi-k3",
		"messages": []any{
			map[string]any{"role": "assistant", "reasoning_content": "新", "reasoning": "旧"},
		},
	}
	mirrorUpstreamReasoning(obj3)
	if got := obj3["messages"].([]any)[0].(map[string]any)["reasoning"]; got != "旧" {
		t.Fatalf("已有值不应被覆盖, got %v", got)
	}

	// 空 reasoning_content 不造字段
	obj4 := map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "hi"},
		},
	}
	mirrorUpstreamReasoning(obj4)
	if _, exists := obj4["messages"].([]any)[0].(map[string]any)["reasoning"]; exists {
		t.Fatal("无 reasoning_content 时绝不新建 reasoning 字段")
	}

	// 非 assistant 不镜像
	obj5 := map[string]any{
		"model": "glm-5.2",
		"messages": []any{
			map[string]any{"role": "user", "reasoning_content": "x"},
		},
	}
	mirrorUpstreamReasoning(obj5)
	if _, exists := obj5["messages"].([]any)[0].(map[string]any)["reasoning"]; exists {
		t.Fatal("user 消息不应镜像")
	}

	// 六家族前缀全命中
	for _, m := range []string{"kimi-k3", "glm-5.2", "minimax-x", "mimo-1", "step-3", "space-bunny-9"} {
		if !readsUpstreamReasoning(m) {
			t.Errorf("家族 %s 应命中", m)
		}
	}
}

// TestClassify11151 11151 归 ErrEmptyContent（fail-fast 请求级，不罚号）。
func TestClassify11151(t *testing.T) {
	body := `{"code":11151,"msg":"a message has empty content"}`
	if got := Classify(400, body); got != ErrEmptyContent {
		t.Fatalf("11151 应归 ErrEmptyContent, got %v", got)
	}
	// 非 400 状态不归类
	if got := Classify(500, body); got == ErrEmptyContent {
		t.Fatal("非 400 不应归类")
	}
	// String()
	if ErrEmptyContent.String() != "empty_content" {
		t.Fatalf("String() = %q", ErrEmptyContent.String())
	}
}

// --- 测试辅助 ---

func mustJSONRaw(t *testing.T, s string) json.RawMessage {
	t.Helper()
	return json.RawMessage(s)
}

func mustJSONSlice(t *testing.T, s string) []any {
	t.Helper()
	var v []any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("测试用例不是合法 JSON 数组: %v", err)
	}
	return v
}
