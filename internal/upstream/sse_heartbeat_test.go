package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStreamHintUniformChunkID 流式 id 整流（hub #607 对齐）：上游流中 id 漂移
// （部分帧带不同 id、部分帧缺 id）时，客户端看到的每一帧 id 必须与首帧一致——
// 客户端按 chunk id 管理流状态，id 变化会卡在「生成中」。issue #35 只兜了
// 「缺 id 补哨兵」，本用例钉住「带不同 id 的帧也强制统一」。
func TestStreamHintUniformChunkID(t *testing.T) {
	upstreamBody := "data: {\"id\":\"chatcmpl-real-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-DRIFTED\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"!\"}}]}\n\n" +
		"data: [DONE]\n\n"
	up := httptest.NewServer(slowFrameHandler(strings.Split(strings.TrimSuffix(upstreamBody, "\n\n"), "\n\n"), 0))
	defer up.Close()
	resp, err := up.Client().Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	if err := StreamHint(w, resp.Body, nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	ids := collectChunkIDs(t, w.Body.String())
	if len(ids) == 0 {
		t.Fatal("无数据帧")
	}
	for _, id := range ids {
		if id != "chatcmpl-real-1" {
			t.Fatalf("流内出现非统一 id: %q（全帧=%v）", id, ids)
		}
	}
}

// TestStreamHintHeartbeat 慢上游（> 心跳间隔无输出）时客户端侧应收到 `: ping`
// 注释帧，且数据帧照常透传。
func TestStreamHintHeartbeat(t *testing.T) {
	old := sseHeartbeatInterval
	sseHeartbeatInterval = 60 * time.Millisecond
	defer func() { sseHeartbeatInterval = old }()

	// 首帧立即到，之后沉默 300ms（> 5 个心跳间隔），再收尾。
	frames := []string{
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}",
		"data: [DONE]",
	}
	up := httptest.NewServer(slowFrameHandler(frames, 300*time.Millisecond))
	defer up.Close()
	resp, err := up.Client().Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	start := time.Now()
	if err := StreamHint(w, resp.Body, nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("上游沉默期未生效（%v），测试前提不成立", elapsed)
	}
	body := w.Body.String()
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Fatalf("数据帧应照常透传, body=%s", body)
	}
	// 注释帧必须原子完整（不撕裂、带双换行收尾）。
	if strings.Count(body, ": ping\n\n") == 0 {
		t.Fatalf("慢流应包含完整 `: ping` 注释帧, body=%s", body)
	}
}

// TestStreamHintNoHeartbeatOnActiveStream 帧密集流（帧间隔 < 心跳间隔）不插心跳。
func TestStreamHintNoHeartbeatOnActiveStream(t *testing.T) {
	old := sseHeartbeatInterval
	sseHeartbeatInterval = 200 * time.Millisecond
	defer func() { sseHeartbeatInterval = old }()

	var frames []string
	for i := 0; i < 6; i++ {
		frames = append(frames, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}")
	}
	frames = append(frames, "data: [DONE]")
	up := httptest.NewServer(slowFrameHandler(frames, 40*time.Millisecond))
	defer up.Close()
	resp, err := up.Client().Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	w := httptest.NewRecorder()
	if err := StreamHint(w, resp.Body, nil); err != nil {
		t.Fatalf("StreamHint: %v", err)
	}
	if strings.Contains(w.Body.String(), ": ping") {
		t.Fatal("活跃流不应插入心跳")
	}
}

// slowFrameHandler 慢速分帧 handler：每帧延迟 delay 后写出并 flush。
func slowFrameHandler(frames []string, delay time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, f := range frames {
			_, _ = io.WriteString(w, f+"\n\n")
			if fl != nil {
				fl.Flush()
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}
	})
}

// collectChunkIDs 从 SSE 文本中抽取每个数据帧的 id 字段。
func collectChunkIDs(t *testing.T, body string) []string {
	t.Helper()
	var ids []string
	for _, line := range strings.Split(body, "\n") {
		payload := strings.TrimPrefix(line, "data: ")
		if line == payload || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		if id, ok := obj["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids
}
