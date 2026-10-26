package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// headerTouchCounter 统计 Header() 被调用次数。
//
// 为什么需要它：anthropicStreamWriter 同时被当作 ResponseWriter 下传给
// chatCompletions，其错误分支走 writeJSON → w.Header().Set；而心跳 goroutine 曾在
// wmu 内每次重设 SSE 头。两个写者写同一个 header map、其中一个不持锁 → Go runtime
// 直接 "fatal error: concurrent map writes"，进程级 abort，测试连 --- FAIL 都来不及
// 打印（曾表现为「偶发 0.2s 失败且无任何输出」）。
//
// 修复后的不变量：SSE 头只在首帧刷出前设置一次，之后 Header() 调用次数必须不再增长。
// 这个计数器让该不变量可被确定性验证——不依赖 -race（本机无 cgo/gcc 时无法启用）。
type headerTouchCounter struct {
	http.ResponseWriter
	n int32
}

func (h *headerTouchCounter) Header() http.Header {
	atomic.AddInt32(&h.n, 1)
	return h.ResponseWriter.Header()
}

func (h *headerTouchCounter) touches() int32 { return atomic.LoadInt32(&h.n) }

// TestAnthropicHeartbeatNeverTouchesHeadersAfterFirstFrame 锁死「首帧后不再改头」。
//
// 用极短 ping 间隔放大心跳次数（120ms 内 ≥ 20 次），断言：
//  1. 心跳确实跑起来了（响应体里出现多条 event: ping），否则本用例会假通过；
//  2. Header() 调用次数自首帧后保持不变。
func TestAnthropicHeartbeatNeverTouchesHeadersAfterFirstFrame(t *testing.T) {
	old := anthropicPingInterval
	anthropicPingInterval = 5 * time.Millisecond
	t.Cleanup(func() { anthropicPingInterval = old })

	rec := httptest.NewRecorder()
	counter := &headerTouchCounter{ResponseWriter: rec}
	s := newAnthropicStreamWriter(counter, "glm-5.2")
	s.startWith(nil) // 首帧：设头 + 200 + 启动心跳

	base := counter.touches()
	if base == 0 {
		t.Fatal("首帧应至少设置一次 SSE 头（Header() 未被调用，用例前提不成立）")
	}

	time.Sleep(120 * time.Millisecond) // 约 20 个 tick

	if got := counter.touches(); got != base {
		t.Fatalf("首帧后心跳仍在改响应头：Header() 调用 %d → %d。"+
			"这与下传的错误路径 writeJSON 并发写同一 header map，会触发 fatal error: concurrent map writes",
			base, got)
	}
	// 读 Body 之前必须先停心跳：httptest.ResponseRecorder 的 Body 不是并发安全的，
	// 心跳 goroutine 仍在写时读它会报 DATA RACE（这是用例自身的竞态，不是生产缺陷）。
	// stopHeartbeat 现在会等到 goroutine 退出，故这里读到的 ping 数是最终的。
	s.stopHeartbeat()
	if n := strings.Count(rec.Body.String(), "event: ping"); n < 2 {
		t.Fatalf("心跳未按预期运行（ping 帧 %d 条），本用例无法证明不变量，需检查 startHeartbeat", n)
	}
}
