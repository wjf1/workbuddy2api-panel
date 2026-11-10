package jsonscan

import (
	"encoding/json"
	"testing"
)

// 等价性钉子：快路径（jsonscan）与整树解析（encoding/json）必须给出**相同**结论。
//
// 这两个调用点（session.ResolveConversationID / upstream.InjectPromptCacheKey）
// 都是「快路径命中就省一次整树解析、未命中回落原路径」，所以正确性的全部要求
// 就是：快路径 Ok=true 时，取到的值与整树解析取到的值一致。任何偏差都会让
// 会话粘性绑错号或缓存键算错，属于静默故障——必须由本测试守住。
func TestLookupMatchesEncodingJSON(t *testing.T) {
	cases := []string{
		`{"conversation_id":"abc"}`,
		`{"conversationId":"def"}`,
		`{"metadata":{"conversation_id":"nested-1"}}`,
		`{"metadata":{"conversationId":"nested-2"}}`,
		`{"metadata":{"conversation_id":"nested"},"conversation_id":"top"}`, // 嵌套优先
		`{"metadata":{},"conversation_id":"only-top"}`,
		`{"prompt_cache_key":"client-key","model":"x"}`,
		`{"model":"x","prompt_cache_key":""}`, // 空串不等于「已带」
		`{"conversation_id":"a","messages":[{"role":"user","content":"hi"}]}`,
		`{}`,
		`{"conversation_id":123}`,      // 非字符串 → 取不到
		`{"metadata":"not-an-object"}`, // metadata 非对象 → 不 panic
	}

	for _, body := range cases {
		// 整树解析基准
		var obj map[string]any
		if err := json.Unmarshal([]byte(body), &obj); err != nil {
			t.Fatalf("测试用例本身不是合法 JSON: %s", body)
		}
		wantTop, _ := obj["conversation_id"].(string)
		wantCamel, _ := obj["conversationId"].(string)
		wantKey, _ := obj["prompt_cache_key"].(string)
		wantNested := ""
		if meta, ok := obj["metadata"].(map[string]any); ok {
			v1, _ := meta["conversation_id"].(string)
			v2, _ := meta["conversationId"].(string)
			if v1 != "" {
				wantNested = v1
			} else {
				wantNested = v2
			}
		}

		res := Lookup([]byte(body), Query{
			Top:        []string{"conversation_id", "conversationId", "prompt_cache_key"},
			Nested:     "metadata",
			NestedKeys: []string{"conversation_id", "conversationId"},
		})
		if !res.Ok {
			t.Fatalf("应能完整走完: %s", body)
		}

		if got := res.Get("conversation_id"); got != wantTop {
			t.Errorf("%s conversation_id: got %q want %q", body, got, wantTop)
		}
		if got := res.Get("conversationId"); got != wantCamel {
			t.Errorf("%s conversationId: got %q want %q", body, got, wantCamel)
		}
		if got := res.Get("prompt_cache_key"); got != wantKey {
			t.Errorf("%s prompt_cache_key: got %q want %q", body, got, wantKey)
		}
		// 嵌套取值口径：conversation_id 优先于 conversationId
		gotNested := res.GetNested("conversation_id")
		if gotNested == "" {
			gotNested = res.GetNested("conversationId")
		}
		if gotNested != wantNested {
			t.Errorf("%s nested: got %q want %q", body, gotNested, wantNested)
		}
	}
}

// TestLookupMalformedFallsBack 畸形 JSON 必须 Ok=false（调用方据此回落整树解析）。
//
// 若这里误报 Ok=true，调用方会拿着不完整的结果当准的用——比慢更糟。
func TestLookupMalformedFallsBack(t *testing.T) {
	bad := []string{
		``,
		`{`,
		`{"a":}`,
		`{"a":"unterminated`,
		`not json at all`,
		`[1,2,3]`, // 顶层是数组不是对象
	}
	for _, body := range bad {
		if res := Lookup([]byte(body), Query{Top: []string{"a"}}); res.Ok {
			t.Errorf("畸形/非对象 body 不应 Ok=true: %q", body)
		}
	}
}

// BenchmarkLookupLargeBody 大 body 下快路径 vs 整树解析的差距。
//
// 线上代理的 body 常 100~500KB（长会话历史），而网关只需要其中一两个短串。
// 这里用 ~200KB 的近似形态做对照，用来防止将来有人「顺手」把快路径改回整树。
func BenchmarkLookupLargeBody(b *testing.B) {
	// 构造 ~200KB：200 条历史消息 + 少量顶层字段
	msgs := make([]map[string]any, 0, 200)
	for i := 0; i < 200; i++ {
		msgs = append(msgs, map[string]any{
			"role":    "user",
			"content": "这是一段用于压测的历史消息内容，重复填充以接近真实长会话体积。" + string(rune('a'+i%26)),
		})
	}
	body, err := json.Marshal(map[string]any{
		"model":           "glm-5.2",
		"conversation_id": "conv-bench-123",
		"messages":        msgs,
		"tools":           []any{},
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("body size = %d bytes", len(body))

	q := Query{Top: []string{"conversation_id", "prompt_cache_key"}}

	b.Run("jsonscan", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			if r := Lookup(body, q); !r.Ok {
				b.Fatal("should be ok")
			}
		}
	})

	b.Run("encoding/json", func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		for i := 0; i < b.N; i++ {
			var obj map[string]any
			if err := json.Unmarshal(body, &obj); err != nil {
				b.Fatal(err)
			}
		}
	})
}
