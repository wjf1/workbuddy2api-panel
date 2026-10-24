package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestAnthropicStreamWriterPingEvents（hub 对标）：Anthropic 流式在空闲期应发送
// 官方 ping 事件保活（长思考模型推理期客户端/中间代理会读超时断连）；finish 后
// 心跳必须停止（不再有 ping）。
func TestAnthropicStreamWriterPingEvents(t *testing.T) {
	old := anthropicPingInterval
	anthropicPingInterval = 40 * time.Millisecond
	defer func() { anthropicPingInterval = old }()

	w := httptest.NewRecorder()
	tr := newAnthropicStreamWriter(w, "glm-5.3")

	// 首帧写出（触发心跳启动），随后静默 200ms（≈5 个心跳间隔）。
	tr.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1"}})
	time.Sleep(200 * time.Millisecond)
	tr.finish() // 生命周期终点：心跳停止

	body := w.Body.String()
	pings := strings.Count(body, "event: ping\n")
	if pings == 0 {
		t.Fatalf("空闲期应出现 ping 事件, body=%s", body)
	}
	if !strings.Contains(body, `"type":"ping"`) {
		t.Fatalf("ping 载荷应为 {\"type\":\"ping\"}, body=%s", body)
	}
	// ping 帧原子完整（event + data + 双换行成对）。
	if !strings.Contains(body, "event: ping\ndata: {\"type\":\"ping\"}\n\n") {
		t.Fatalf("ping 帧形态异常, body=%s", body)
	}
	// ping 不得打断事件序列：message_start 的 data 仍完整可解析。
	if !strings.Contains(body, "message_start") {
		t.Fatal("正常事件应照常写出")
	}

	// finish 后不再有 ping：清计数再静默一段，断言无新增。
	before := strings.Count(body, "event: ping\n")
	time.Sleep(150 * time.Millisecond)
	after := strings.Count(w.Body.String(), "event: ping\n")
	if after != before {
		t.Fatalf("finish 后仍出现 ping（%d → %d）", before, after)
	}
}

// TestAnthropicPingDoesNotBreakFrame 穿插 ping 不撕裂相邻事件（写侧互斥）。
func TestAnthropicPingDoesNotBreakFrame(t *testing.T) {
	old := anthropicPingInterval
	anthropicPingInterval = 5 * time.Millisecond
	defer func() { anthropicPingInterval = old }()

	w := httptest.NewRecorder()
	tr := newAnthropicStreamWriter(w, "glm-5.3")
	tr.event("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_1"}})
	// 高频写正常事件与心跳并发，最后校验所有 data 行均为合法 JSON。
	for i := 0; i < 30; i++ {
		tr.event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "x"},
		})
		time.Sleep(3 * time.Millisecond)
	}
	tr.finish()

	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &obj); err != nil {
			t.Fatalf("撕裂/非法事件帧: %q err=%v", line, err)
		}
	}
}
