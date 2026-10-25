package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// mkToolBody 造一个含 n 个 tool-call 块的历史消息（每个都挂 metaData），
// 用于验证剥除只删 metaData、不碰工具语义字段。
func mkToolBody(withMeta bool) []byte {
	meta := ""
	if withMeta {
		meta = `,"metaData":{"_meta":{"traceId":"t1","baggage":"b","rawResponse":{"exitCode":0}}}`
	}
	return []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"hi"}]},` +
		`{"role":"assistant","content":[` +
		`{"type":"text","text":"我来执行"},` +
		`{"type":"tool-call","tool":{"id":"call_1","name":"Bash","status":"executed",` +
		`"args":{"command":"ls"},"result":{"type":"text","text":"file.txt"}` + meta + `}},` +
		`{"type":"tool-call","tool":{"id":"call_2","name":"Read","args":{},"result":{"text":"x"}` + meta + `}}` +
		`]}]}`)
}

// TestStripToolMetaDataRemovesOnlyMetaData 剥除必须精确：metaData 消失，而
// id/name/args/result.text 一个不少——这些是模型真正要读的内容。
func TestStripToolMetaDataRemovesOnlyMetaData(t *testing.T) {
	in := mkToolBody(true)
	if !strings.Contains(string(in), "metaData") {
		t.Fatal("用例自检失败：输入里应含 metaData")
	}

	out := StripToolMetaData(in)

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if strings.Contains(string(out), "metaData") {
		t.Fatalf("metaData 未被完全剥除: %s", out)
	}

	msgs := got["messages"].([]any)
	blocks := msgs[1].(map[string]any)["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("content 块数变化: %d", len(blocks))
	}
	tc := blocks[1].(map[string]any)["tool"].(map[string]any)
	if tc["id"] != "call_1" || tc["name"] != "Bash" {
		t.Fatalf("工具标识被破坏: %v", tc)
	}
	if args, _ := tc["args"].(map[string]any); args["command"] != "ls" {
		t.Fatalf("args 被破坏: %v", tc["args"])
	}
	res, _ := tc["result"].(map[string]any)
	if res["text"] != "file.txt" {
		t.Fatalf("result.text 被破坏: %v", tc["result"])
	}
}

// TestStripToolMetaDataNoopKeepsBytes 开关打开但请求里没有 metaData 时，
// 必须原样返回入参（不重新序列化），保证出站报文与关闭时逐字节一致。
func TestStripToolMetaDataNoopKeepsBytes(t *testing.T) {
	in := mkToolBody(false)
	out := StripToolMetaData(in)
	if &out[0] != &in[0] {
		t.Fatal("无字段可删时不应重新序列化，应原样返回入参切片")
	}
	if string(out) != string(in) {
		t.Fatal("无字段可删时字节必须完全一致")
	}
}

// TestStripToolMetaDataRobustness 畸形输入一律透传，绝不把请求弄得更糟。
func TestStripToolMetaDataRobustness(t *testing.T) {
	cases := map[string]string{
		"非 JSON":       `not json at all`,
		"非对象":          `[1,2,3]`,
		"messages 非数组": `{"messages":"oops"}`,
		"content 非数组":  `{"messages":[{"content":"plain string"}]}`,
		"块不是对象":        `{"messages":[{"content":[1,2]}]}`,
		"tool 不是对象":    `{"messages":[{"content":[{"tool":"x"}]}]}`,
		"空对象":          `{}`,
	}
	for name, in := range cases {
		got := StripToolMetaData([]byte(in))
		if string(got) != in {
			t.Errorf("%s: 畸形输入应原样透传，got=%s", name, got)
		}
	}
}

// TestStripToolMetaDataLeavesReasoning reasoning 块（思考链）必须原样保留：
// 部分上游多轮要求回传，剥掉有破坏性风险。断言按结构比较，不依赖 JSON 键序。
func TestStripToolMetaDataLeavesReasoning(t *testing.T) {
	in := []byte(`{"messages":[{"role":"assistant","content":[` +
		`{"type":"reasoning","text":"想一下","metaData":{"x":1}},` +
		`{"type":"tool-call","tool":{"id":"c","name":"N","metaData":{"x":2}}}` +
		`]}]}`)
	out := StripToolMetaData(in)

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	blocks := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("块数变化: %d", len(blocks))
	}
	reasoning := blocks[0].(map[string]any)
	if reasoning["text"] != "想一下" {
		t.Fatalf("reasoning 正文被破坏: %v", reasoning)
	}
	if md, ok := reasoning["metaData"].(map[string]any); !ok || md["x"] != float64(1) {
		t.Fatalf("reasoning.metaData 被误删（本轮只处理 tool/result 的 metaData）: %v", reasoning)
	}
	tool := blocks[1].(map[string]any)["tool"].(map[string]any)
	if _, has := tool["metaData"]; has {
		t.Fatalf("tool.metaData 未被剥除: %v", tool)
	}
	if tool["name"] != "N" {
		t.Fatalf("tool.name 被破坏: %v", tool)
	}
}
