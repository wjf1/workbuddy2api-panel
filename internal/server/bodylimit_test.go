package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// bigChatBody 返回一个合法的 chat 请求体，pad 字段撑到 n 字节以上。
func bigChatBody(n int) []byte {
	return []byte(`{"model":"glm-5.2","messages":[],"pad":"` + strings.Repeat("a", n) + `"}`)
}

// TestMaxBodyBytesOptIn 请求体上限是纯 opt-in：MaxBodyBytes>0 时超限就地 413、
// 不打上游；上限内的请求照常放行。缺省 0 = 不限制由
// TestChatLargeBodyNoGatewayLimit 覆盖。
func TestMaxBodyBytesOptIn(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 1 << 20}) // 1MB

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bigChatBody(2<<20))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: code=%d body=%s, want 413", rec.Code, rec.Body)
	}
	if calls != 0 {
		t.Errorf("oversized body must not reach upstream: calls=%d", calls)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bigChatBody(1<<10))))
	if rec.Code != http.StatusOK {
		t.Fatalf("under limit: code=%d body=%s, want 200", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("under-limit body should reach upstream once: calls=%d", calls)
	}
}

// TestMaxBodyBytesAnthropic 上限对 /v1/messages 与 count_tokens 两个入口同源生效。
func TestMaxBodyBytesAnthropic(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, MaxBodyBytes: 1 << 20})

	body := bigChatBody(2 << 20)
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", path, bytes.NewReader(body)))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s: code=%d body=%s, want 413", path, rec.Code, rec.Body)
		}
	}
}
