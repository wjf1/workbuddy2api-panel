package upstream

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestNormalizeRoles 验证出站请求体把 developer 角色归一为 system。
// 上游 role 白名单不含 developer（OpenAI 新规范的 system 别名），
// 命中即 HTTP 400 code=11128；此处走 PrepareBodyOptWithEfforts 全链路断言。
func TestNormalizeRoles(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantRoles []string // 与输出 messages 逐条对应的期望 role；len 即消息数
	}{
		{"developer 改写为 system",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"Developer 首字母大写改写",
			`{"messages":[{"role":"Developer","content":"x"}]}`, []string{"system"}},
		{"DEVELOPER 全大写改写",
			`{"messages":[{"role":"DEVELOPER","content":"x"}]}`, []string{"system"}},
		{"前后空白 TrimSpace 后改写",
			`{"messages":[{"role":" developer ","content":"x"}]}`, []string{"system"}},
		{"system 原样保留",
			`{"messages":[{"role":"system","content":"x"}]}`, []string{"system"}},
		{"user 原样保留",
			`{"messages":[{"role":"user","content":"x"}]}`, []string{"user"}},
		{"assistant 原样保留",
			`{"messages":[{"role":"assistant","content":"x"}]}`, []string{"assistant"}},
		{"tool 原样保留（不因未知而改写）",
			`{"messages":[{"role":"tool","content":"x"}]}`, []string{"tool"}},
		{"messages 缺失不 panic 且其余字段不变",
			`{"model":"glm-5.2"}`, []string{}},
		{"messages 为空数组不 panic",
			`{"messages":[]}`, []string{}},
		{"混合消息仅 developer 被改写",
			`{"messages":[{"role":"developer","content":"a"},{"role":"user","content":"b"},{"role":"developer","content":"c"}]}`,
			[]string{"system", "user", "system"}},
		{"sanitize=false 时仍归一（与脱敏解耦）",
			`{"messages":[{"role":"developer","content":"x"}]}`, []string{"system"}},
		{"非对象消息元素跳过、其余正常处理",
			`{"messages":["str",{"role":"developer","content":"x"},42]}`, []string{"system"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 全程 sanitize=false：验证 role 归一与内容脱敏开关无关（D4）。
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("unmarshal: %v (out=%s)", err, out)
			}

			// 提取输出 messages 里的 role（非对象元素跳过，不 panic）。
			var got []string
			if msgs, ok := obj["messages"].([]any); ok {
				for _, m := range msgs {
					msg, ok := m.(map[string]any)
					if !ok {
						continue
					}
					if role, ok := msg["role"].(string); ok {
						got = append(got, role)
					}
				}
			}

			if len(got) != len(c.wantRoles) {
				t.Fatalf("role 数量不符: got %v (%d) want %v (%d)", got, len(got), c.wantRoles, len(c.wantRoles))
			}
			for i := range got {
				if got[i] != c.wantRoles[i] {
					t.Errorf("role[%d] = %q want %q", i, got[i], c.wantRoles[i])
				}
			}
		})
	}

	// messages 缺失时，其余字段必须原样保留（除强制 stream）。
	t.Run("messages 缺失时其余字段不变", func(t *testing.T) {
		out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","temperature":0.7}`), false, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if obj["model"] != "glm-5.2" || obj["temperature"] != 0.7 {
			t.Errorf("其余字段被改动: %v", obj)
		}
	})
}

func TestPrepareBodyOptWithEfforts(t *testing.T) {
	efforts := map[string][]string{
		"glm-5.2":      {"off", "low", "high"},
		"glm-5.2-mini": {"low", "medium"},
		"glm-5.2-max":  {"high", "xhigh"},
	}
	cases := []struct {
		name    string
		body    string
		efforts map[string][]string
		wantKey string // 输出应带有的 effort 字段名；空表示该字段应不存在
		wantVal string // 期望值
	}{
		{"downgrade to highest supported at or below request",
			`{"model":"glm-5.2-mini","reasoning_effort":"high"}`, efforts, "reasoning_effort", "medium"},
		{"floor to lowest when all supported above request",
			`{"model":"glm-5.2-max","reasoning_effort":"low"}`, efforts, "reasoning_effort", "high"},
		{"supported effort passes through unchanged",
			`{"model":"glm-5.2","reasoning_effort":"low"}`, efforts, "reasoning_effort", "low"},
		{"camelCase field name downgrades and keeps key",
			`{"model":"glm-5.2-mini","reasoningEffort":"high"}`, efforts, "reasoningEffort", "medium"},
		{"unknown model passes through",
			`{"model":"unknown","reasoning_effort":"max"}`, efforts, "reasoning_effort", "max"},
		{"unknown effort value passes through",
			`{"model":"glm-5.2","reasoning_effort":"ultra"}`, efforts, "reasoning_effort", "ultra"},
		{"empty cache passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, map[string][]string{}, "reasoning_effort", "max"},
		{"no effort field untouched",
			`{"model":"glm-5.2-mini","messages":[]}`, efforts, "", ""},
		{"nil efforts map passes through",
			`{"model":"glm-5.2","reasoning_effort":"max"}`, nil, "reasoning_effort", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, c.efforts)
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatalf("unmarshal: %v (body=%s)", err, out)
			}
			if c.wantKey == "" {
				if _, ok := m["reasoning_effort"]; ok {
					t.Errorf("reasoning_effort should be absent, got %v", m["reasoning_effort"])
				}
				if _, ok := m["reasoningEffort"]; ok {
					t.Errorf("reasoningEffort should be absent, got %v", m["reasoningEffort"])
				}
				return
			}
			got, ok := m[c.wantKey].(string)
			if !ok || got != c.wantVal {
				t.Errorf("%s: got %v (%T) want %q", c.wantKey, m[c.wantKey], m[c.wantKey], c.wantVal)
			}
		})
	}
}

// TestPrepareBodyStreamOptions body 未显式带 stream_options 时注入
// {include_usage: true}（D7，官方 CLI 流式必发）；body 已带则不覆盖。
func TestPrepareBodyStreamOptions(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"glm-5.2","messages":[]}`), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options not injected: %v", obj["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("stream_options.include_usage = %v want true", so["include_usage"])
	}

	out2 := PrepareBodyOptWithEffertsPreserve(t, `{"model":"glm-5.2","messages":[],"stream_options":{"include_usage":false}}`)
	obj2, err := decodeBody(out2)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so2, ok := obj2["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options lost: %v", obj2["stream_options"])
	}
	if so2["include_usage"] != false {
		t.Errorf("stream_options.include_usage = %v want false (not overwritten)", so2["include_usage"])
	}
}

// PrepareBodyOptWithEffertsPreserve helper：PrepareBodyOptWithEfforts 包装。
func PrepareBodyOptWithEffertsPreserve(t *testing.T, body string) []byte {
	t.Helper()
	return PrepareBodyOptWithEfforts([]byte(body), false, nil)
}

// decodeBody helper：解析 body JSON。
func decodeBody(b []byte) (map[string]any, error) {
	var obj map[string]any
	err := json.Unmarshal(b, &obj)
	return obj, err
}

// TestPrepareBodyDeterministic 序列化稳定性：同输入跑多遍出站字节级一致
// （prompt_cache_key 前缀命中的前提——链中不得注入时间/随机/ID 类不确定源）。
func TestPrepareBodyDeterministic(t *testing.T) {
	inputs := []string{
		`{"model":"glm-5.2","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"你好"}],"reasoning_effort":"high"}`,
		`{"model":"deepseek-v4","messages":[{"role":"user","content":"写个函数"}],"tool_choice":{"type":"auto"},"tools":[{"type":"function","function":{"name":"f"}}]}`,
		`{"model":"glm-5.3","messages":[{"role":"developer","content":"sys"},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
	}
	for i, in := range inputs {
		var first []byte
		for round := 0; round < 5; round++ {
			out := PrepareBodyOptWithEfforts([]byte(in), true, map[string][]string{"glm-5.2": {"off", "low", "high"}})
			if round == 0 {
				first = out
				continue
			}
			if string(out) != string(first) {
				t.Fatalf("input #%d round %d differs from round 0:\n%s\n%s", i, round, first, out)
			}
		}
	}
}

// TestNormalizeImageURL 覆盖 OpenAI chat 多模态内容的 image_url 兼容：
// 字符串形态必须转为上游需要的对象形态；对象形态及其中字段必须原样保留；
// 无效输入不补默认值，继续交给上游返回真实错误。
func TestNormalizeImageURL(t *testing.T) {
	tests := []struct {
		name string
		body string
		want any
	}{
		{
			name: "data url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD"},
		},
		{
			name: "http url string to object",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":"https://example.test/a.png"}]}]}`,
			want: map[string]any{"url": "https://example.test/a.png"},
		},
		{
			name: "object with detail preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD","detail":"low","mime_type":"image/png"}}]}]}`,
			want: map[string]any{"url": "data:image/png;base64,QUJD", "detail": "low", "mime_type": "image/png"},
		},
		{
			name: "invalid object url type preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":123}}]}]}`,
			want: map[string]any{"url": float64(123)},
		},
		{
			name: "missing image url preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`,
			want: nil,
		},
		{
			name: "empty string preserved",
			body: `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":""}]}]}`,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, sanitize := range []bool{false, true} {
				out := PrepareBodyOptWithEfforts([]byte(tc.body), sanitize, nil)
				obj, err := decodeBody(out)
				if err != nil {
					t.Fatalf("sanitize=%v unmarshal: %v (out=%s)", sanitize, err, out)
				}
				msgs := obj["messages"].([]any)
				content := msgs[0].(map[string]any)["content"].([]any)
				var part map[string]any
				for _, rawPart := range content {
					candidate, ok := rawPart.(map[string]any)
					if ok && candidate["type"] == "image_url" {
						part = candidate
						break
					}
				}
				if part == nil {
					t.Fatal("image_url part not found")
				}
				if tc.want == nil {
					if _, exists := part["image_url"]; exists {
						t.Fatalf("sanitize=%v: missing image_url should stay missing, got %#v", sanitize, part)
					}
					continue
				}
				if got := part["image_url"]; !reflect.DeepEqual(got, tc.want) {
					t.Errorf("sanitize=%v: image_url=%#v want %#v", sanitize, got, tc.want)
				}
			}
		})
	}
}

// TestNormalizeToolPatterns 工具 schema pattern 的 `\_` 转义归一（11129 实案：
// exa agent_run 的 `^agent\_run\_` 被 deepseek 系确定性拒收，归一后上游 200）。
// 覆盖：function.parameters 与裸 parameters 两形态、嵌套 schema、patternProperties
// 键、消息正文不碰、无 tools no-op、sanitize 开关两态一致。
func TestNormalizeToolPatterns(t *testing.T) {
	lookupPattern := func(t *testing.T, body string, path ...any) string {
		t.Helper()
		var obj map[string]any
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		cur := any(obj)
		for _, p := range path {
			switch k := p.(type) {
			case string:
				m, ok := cur.(map[string]any)
				if !ok {
					t.Fatalf("path %v: want object, got %T", path, cur)
				}
				cur = m[k]
			case int:
				l, ok := cur.([]any)
				if !ok || k >= len(l) {
					t.Fatalf("path %v: bad array at %T", path, cur)
				}
				cur = l[k]
			}
		}
		s, _ := cur.(string)
		return s
	}

	t.Run("exa agent_run pattern normalized", func(t *testing.T) {
		// 反斜杠，测试体经工具链多层转义易被吞，运行时拼装保真
		// 注释独立成行：放行尾会让 gofmt 按同行超长字符串的字面量列尾对齐，
		// 在 go1.22 下生成上千空格（gofmt 版本间行为不一致，CI 直接判红）。
		bs := string(byte(92))
		body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"agent_run","parameters":{"type":"object","properties":{"runId":{"type":"string","pattern":"^agent` + bs + bs + `_run` + bs + bs + `_"}` + `,"query":{"type":"string"}},"required":["query"]}}}]}` //nolint:lll // 实案 body 原样
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "runId", "pattern"); got != `^agent_run_` {
			t.Fatalf("pattern = %q, want %q", got, `^agent_run_`)
		}
		// 同 schema 的兄弟字段不受影响
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "query", "type"); got != "string" {
			t.Fatalf("sibling field disturbed: %q", got)
		}
	})

	t.Run("nested anyOf items pattern normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"anyOf":[{"properties":{"code":{"pattern":"x` + bs + bs + `_y"}}},{"type":"string"}]}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "anyOf", 0, "properties", "code", "pattern"); got != "x_y" {
			t.Fatalf("nested pattern = %q, want %q", got, "x_y")
		}
	})

	t.Run("bare tool parameters form normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"name":"t","parameters":{"properties":{"id":{"pattern":"p` + bs + bs + `_q"}}}}]}`
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "parameters", "properties", "id", "pattern"); got != "p_q" {
			t.Fatalf("bare parameters pattern = %q, want %q", got, "p_q")
		}
	})

	t.Run("patternProperties key normalized", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"patternProperties":{"^a` + bs + bs + `_b` + bs + bs + `_":{"type":"string"}}}}}]}` //nolint:lll
		var obj map[string]any
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		tool := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
		props := tool["patternProperties"].(map[string]any)
		if _, ok := props["^a_b_"]; !ok {
			t.Fatalf("normalized key missing: %v", props)
		}
		for k := range props {
			if strings.Contains(k, string(byte(92))) {
				t.Fatalf("patternProperties key not normalized: %q", k)
			}
		}
	})

	t.Run("message content backslash untouched", func(t *testing.T) {
		bs := string(byte(92))
		raw := `{"messages":[{"role":"user","content":"path C:` + bs + bs + `_dir and regex a` + bs + bs + `_b"}],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"q` + bs + bs + `_r"}}}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(raw), false, nil))
		var obj map[string]any
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		msg := obj["messages"].([]any)[0].(map[string]any)
		if got := msg["content"]; got != `path C:\_dir and regex a\_b` {
			t.Fatalf("message content mutated: %q", got)
		}
	})

	t.Run("clean pattern untouched", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"^[a-z]+` + bs + bs + `d_$"}}}}}]}` //nolint:lll
		out := string(PrepareBodyOptWithEfforts([]byte(body), false, nil))
		if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != `^[a-z]+\d_$` {
			t.Fatalf("clean pattern mutated: %q", got)
		}
	})

	t.Run("no tools is no-op", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"model":"m","messages":[{"role":"user","content":"a` + bs + bs + `_b"}]}`
		var inObj, outObj map[string]any
		if err := json.Unmarshal([]byte(body), &inObj); err != nil {
			t.Fatal(err)
		}
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		if err := json.Unmarshal(out, &outObj); err != nil {
			t.Fatalf("out not json: %v", err)
		}
		inMsg, _ := json.Marshal(inObj["messages"])
		outMsg, _ := json.Marshal(outObj["messages"])
		if string(inMsg) != string(outMsg) {
			t.Fatalf("messages changed without tools: %s -> %s", inMsg, outMsg)
		}
		if _, has := outObj["tools"]; has {
			t.Fatal("tools appeared out of nowhere")
		}
	})

	t.Run("sanitize on and off behave the same", func(t *testing.T) {
		bs := string(byte(92))
		body := `{"messages":[],"tools":[{"type":"function","function":{"name":"t","parameters":{"properties":{"p":{"pattern":"a` + bs + bs + `_b"}}}}}]}` //nolint:lll
		for _, sanitize := range []bool{false, true} {
			out := string(PrepareBodyOptWithEfforts([]byte(body), sanitize, nil))
			if got := lookupPattern(t, out, "tools", 0, "function", "parameters", "properties", "p", "pattern"); got != "a_b" {
				t.Fatalf("sanitize=%v: pattern = %q, want %q", sanitize, got, "a_b")
			}
		}
	})
}

// TestClampGPTMinMaxTokens GPT 系上游要求 max_tokens ≥ 16（实测 gpt-6-sol/gpt-6-luna/
// gpt-5.6-sol：15 → 400 code=11133 model_param_invalid，16 → 200）。Claude Code 切
// 模型时的探针请求 max_tokens 极小，全号轮转同样被拒 → 客户端 503。非 GPT 模型不动。
func TestClampGPTMinMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		body string
		want any // nil 表示字段不存在
	}{
		{"gpt below floor", `{"model":"gpt-6-sol","max_tokens":1,"messages":[]}`, float64(16)},
		{"gpt global prefix", `{"model":"global:gpt-5.6-sol","max_tokens":15,"messages":[]}`, float64(16)},
		{"gpt at floor", `{"model":"gpt-6-luna","max_tokens":16,"messages":[]}`, float64(16)},
		{"gpt above floor", `{"model":"gpt-6-sol","max_tokens":32000,"messages":[]}`, float64(32000)},
		{"gpt alias translated then clamped", `{"model":"gpt-6-sol","max_completion_tokens":1,"messages":[]}`, float64(16)},
		{"gpt absent untouched", `{"model":"gpt-6-sol","messages":[]}`, nil},
		{"non-gpt untouched", `{"model":"hy4-preview-f","max_tokens":1,"messages":[]}`, float64(1)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obj, err := decodeBody(PrepareBodyOptWithEffertsPreserve(t, c.body))
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, has := obj["max_tokens"]
			if c.want == nil {
				if has {
					t.Fatalf("max_tokens = %v, want absent", got)
				}
				return
			}
			if got != c.want {
				t.Fatalf("max_tokens = %v, want %v", got, c.want)
			}
		})
	}
}

// TestDerefToolSchemas 覆盖 tools schema 的 `$ref`/`$defs` 解引用（修 11133）。
//
// 回归背景（2026-10-14 ZCode 实案）：MCP 类客户端把重复 schema 抽进 `$defs`、用
// `{"$ref":"#/$defs/X"}` 引用以压缩体积。WorkBuddy 上游对 tools 做扁平化校验、
// 不解析 `$ref`，命中即 HTTP 400 code=11133 model_param_invalid。实测对照：
// 保留 `$ref` 返 400、内联展开后 200，且与具体模型无关。
//
// 这些用例同时钉住几条**安全边界**——解引用做错会改坏合法请求或让进程挂掉：
//   - 循环引用必须放弃展开（否则无限递归 / 请求体撑爆）；
//   - 远程 `$ref` 不解析（网关不能为一次改写去拉第三方）；
//   - 无 `$defs` 时出站报文必须逐字节不变。
func TestDerefToolSchemas(t *testing.T) {
	run := func(t *testing.T, body string) map[string]any {
		t.Helper()
		out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("out not json: %v (%s)", err, string(out))
		}
		return obj
	}
	params := func(t *testing.T, obj map[string]any, toolIdx int) map[string]any {
		t.Helper()
		tools, ok := obj["tools"].([]any)
		if !ok || toolIdx >= len(tools) {
			t.Fatalf("tools[%d] missing", toolIdx)
		}
		tool := tools[toolIdx].(map[string]any)
		// OpenAI 形态优先
		if fn, ok := tool["function"].(map[string]any); ok {
			p, _ := fn["parameters"].(map[string]any)
			return p
		}
		p, _ := tool["parameters"].(map[string]any)
		return p
	}

	t.Run("顶层 $ref 内联且 $defs 删除", func(t *testing.T) {
		body := `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],
"tools":[{"type":"function","function":{"name":"t","parameters":{"$ref":"#/$defs/Args",
"$defs":{"Args":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}}}]}`
		obj := run(t, body)
		p := params(t, obj, 0)
		if _, has := p["$ref"]; has {
			t.Fatalf("$ref 仍在: %v", p["$ref"])
		}
		if _, has := p["$defs"]; has {
			t.Fatalf("$defs 未删除: %v", p["$defs"])
		}
		props, ok := p["properties"].(map[string]any)
		if !ok || props["q"] == nil {
			t.Fatalf("内联后 properties 缺失: %v", p)
		}
		if p["type"] != "object" {
			t.Fatalf("内联后 type = %v, want object", p["type"])
		}
	})

	t.Run("嵌套属性里的 $ref 也展开", func(t *testing.T) {
		body := `{"messages":[{"role":"user","content":"hi"}],
"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object",
"properties":{"a":{"$ref":"#/$defs/A"}},
"$defs":{"A":{"type":"string","description":"desc-a"}}}}}]}`
		obj := run(t, body)
		p := params(t, obj, 0)
		props := p["properties"].(map[string]any)
		a, ok := props["a"].(map[string]any)
		if !ok {
			t.Fatalf("properties.a 不是对象: %v", props["a"])
		}
		if _, has := a["$ref"]; has {
			t.Fatalf("嵌套 $ref 未展开: %v", a)
		}
		if a["type"] != "string" || a["description"] != "desc-a" {
			t.Fatalf("嵌套展开内容不符: %v", a)
		}
	})

	t.Run("definitions 旧写法也支持", func(t *testing.T) {
		body := `{"messages":[{"role":"user","content":"hi"}],
"tools":[{"type":"function","function":{"name":"t","parameters":{"$ref":"#/definitions/B",
"definitions":{"B":{"type":"integer"}}}}}]}`
		obj := run(t, body)
		p := params(t, obj, 0)
		if p["type"] != "integer" {
			t.Fatalf("definitions 未展开: %v", p)
		}
		if _, has := p["definitions"]; has {
			t.Fatalf("definitions 未删除")
		}
	})

	t.Run("循环引用保留 $ref 且不挂起", func(t *testing.T) {
		// A -> B -> A：必须放弃展开而不是无限递归
		body := `{"messages":[{"role":"user","content":"hi"}],
"tools":[{"type":"function","function":{"name":"t","parameters":{"$ref":"#/$defs/A",
"$defs":{"A":{"type":"object","properties":{"b":{"$ref":"#/$defs/B"}}},
"B":{"type":"object","properties":{"a":{"$ref":"#/$defs/A"}}}}}}}]}`
		obj := run(t, body)
		// 关键断言：函数返回了（没死循环），且顶层 $ref 已被解开成 object
		p := params(t, obj, 0)
		if p["type"] != "object" {
			t.Fatalf("循环场景顶层应已展开: %v", p)
		}
		// 环上的引用必须**保留 $ref 原样**（交给上游判），不能展开成半截或空对象。
		props, ok := p["properties"].(map[string]any)
		if !ok {
			t.Fatalf("循环场景 properties 丢失: %v", p)
		}
		b, ok := props["b"].(map[string]any)
		if !ok {
			t.Fatalf("properties.b 不是对象: %v", props["b"])
		}
		// B 展开一层后，b.properties.a 指向 A，此处必须保留 $ref（环的闭合点）。
		bProps, ok := b["properties"].(map[string]any)
		if !ok {
			t.Fatalf("properties.b.properties 丢失: %v", b)
		}
		a, ok := bProps["a"].(map[string]any)
		if !ok {
			t.Fatalf("properties.b.properties.a 不是对象: %v", bProps["a"])
		}
		if a["$ref"] != "#/$defs/A" {
			t.Fatalf("环的闭合点应保留 $ref 原样，实得: %v", a)
		}
		// 且不得出现多层重复嵌套（首版实现会把环展开到深度上限）。
		if _, deeper := a["properties"]; deeper {
			t.Fatalf("环上出现了多余的重复展开: %v", a)
		}
	})

	t.Run("远程 $ref 不解析", func(t *testing.T) {
		body := `{"messages":[{"role":"user","content":"hi"}],
"tools":[{"type":"function","function":{"name":"t","parameters":{"$ref":"https://example.com/s.json"}}}]}`
		obj := run(t, body)
		p := params(t, obj, 0)
		if p["$ref"] != "https://example.com/s.json" {
			t.Fatalf("远程 $ref 不应被改动: %v", p)
		}
	})

	t.Run("找不到定义时保留 $ref 原样", func(t *testing.T) {
		body := `{"messages":[{"role":"user","content":"hi"}],
"tools":[{"type":"function","function":{"name":"t","parameters":{"$ref":"#/$defs/Missing",
"$defs":{"Other":{"type":"string"}}}}}]}`
		obj := run(t, body)
		p := params(t, obj, 0)
		if p["$ref"] != "#/$defs/Missing" {
			t.Fatalf("未命中定义时应保留原 $ref: %v", p)
		}
	})

	t.Run("同一定义多处引用互不影响", func(t *testing.T) {
		// 深拷贝校验：共享指针时第二次 delete $defs 会污染第一次的展开结果
		body := `{"messages":[{"role":"user","content":"hi"}],
"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object",
"properties":{"a":{"$ref":"#/$defs/S"},"b":{"$ref":"#/$defs/S"}},
"$defs":{"S":{"type":"string","enum":["x","y"]}}}}}]}`
		obj := run(t, body)
		p := params(t, obj, 0)
		props := p["properties"].(map[string]any)
		for _, k := range []string{"a", "b"} {
			m, ok := props[k].(map[string]any)
			if !ok || m["type"] != "string" {
				t.Fatalf("properties.%s 展开失败: %v", k, props[k])
			}
			if enum, ok := m["enum"].([]any); !ok || len(enum) != 2 {
				t.Fatalf("properties.%s enum 丢失: %v", k, m["enum"])
			}
		}
	})

	t.Run("无裸 parameters 形态也支持", func(t *testing.T) {
		body := `{"messages":[{"role":"user","content":"hi"}],
"tools":[{"type":"custom","parameters":{"$ref":"#/$defs/C","$defs":{"C":{"type":"boolean"}}}}]}`
		obj := run(t, body)
		p := params(t, obj, 0)
		if p["type"] != "boolean" {
			t.Fatalf("裸 parameters 未展开: %v", p)
		}
	})

	t.Run("无 tools 时不 panic", func(t *testing.T) {
		obj := run(t, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
		if _, has := obj["tools"]; has {
			t.Fatalf("不应凭空插入 tools")
		}
	})

	t.Run("messages 里的 $ref 字面量不碰", func(t *testing.T) {
		body := `{"messages":[{"role":"user","content":"see #/$defs/X"}],
"tools":[{"type":"function","function":{"name":"t","parameters":{"type":"object"}}}]}`
		obj := run(t, body)
		msgs := obj["messages"].([]any)
		m := msgs[0].(map[string]any)
		if m["content"] != "see #/$defs/X" {
			t.Fatalf("正文被改动: %v", m["content"])
		}
	})
}
