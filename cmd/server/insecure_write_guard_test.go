package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestInsecureWriteGuard 不安全绑定写保护的行为钉子。
//
// 守卫只在「监听非回环 && api_key 为空」时介入；任一条件不满足必须完全旁路，
// 否则会误伤回环自用与已配 key 的正常部署（后者是全部生产实例的现状）。
func TestInsecureWriteGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("passed"))
	})

	cases := []struct {
		name     string
		listen   string
		apiKey   string
		method   string
		wantCode int
	}{
		// 回环 + 空 key：零配置自用场景，写方法必须放行。
		{"loopback v4 empty key POST", "127.0.0.1:7863", "", http.MethodPost, http.StatusOK},
		{"loopback v4 empty key DELETE", "127.0.0.1:7863", "", http.MethodDelete, http.StatusOK},
		{"loopback localhost empty key POST", "localhost:7863", "", http.MethodPost, http.StatusOK},
		{"loopback v6 empty key POST", "[::1]:7863", "", http.MethodPost, http.StatusOK},
		{"loopback v6 bare empty key PUT", "::1:7863", "", http.MethodPut, http.StatusOK},

		// 非回环 + 空 key：暴露面，写方法一律 405，只读放行。
		{"any-iface empty key POST", ":7863", "", http.MethodPost, http.StatusMethodNotAllowed},
		{"any-iface empty key PUT", ":7863", "", http.MethodPut, http.StatusMethodNotAllowed},
		{"any-iface empty key PATCH", ":8787", "", http.MethodPatch, http.StatusMethodNotAllowed},
		{"any-iface empty key DELETE", "0.0.0.0:8787", "", http.MethodDelete, http.StatusMethodNotAllowed},
		{"any-iface empty key GET", ":8787", "", http.MethodGet, http.StatusOK},
		{"any-iface empty key HEAD", ":8787", "", http.MethodHead, http.StatusOK},
		{"any-iface empty key OPTIONS", ":8787", "", http.MethodOptions, http.StatusOK},

		// 空白 key 等价于未设置（避免 " " 被当成已启用鉴权）。
		{"any-iface blank key POST", ":8787", "   ", http.MethodPost, http.StatusMethodNotAllowed},

		// 已配 key：守卫完全旁路，鉴权交回 httpauth（含非回环绑定）。
		{"any-iface with key POST", ":8787", "sekret", http.MethodPost, http.StatusOK},
		{"any-iface with key DELETE", "0.0.0.0:8787", "sekret", http.MethodDelete, http.StatusOK},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := insecureWriteGuard(c.listen, c.apiKey, ok)
			req := httptest.NewRequest(c.method, "http://example.com/v1/chat/completions", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.wantCode {
				t.Fatalf("listen=%q key=%q %s: got %d want %d (body=%q)",
					c.listen, c.apiKey, c.method, rec.Code, c.wantCode, rec.Body.String())
			}
			if c.wantCode == http.StatusMethodNotAllowed {
				if got := rec.Header().Get("Allow"); !strings.Contains(got, "GET") {
					t.Fatalf("405 响应应带 Allow 头列出只读方法，got %q", got)
				}
				// 拒绝理由要能指导用户修复：提示设 api_key，而不是干巴巴的 405。
				if body := rec.Body.String(); !strings.Contains(body, "api_key") {
					t.Fatalf("405 正文应提示设置 api_key，got %q", body)
				}
			}
			if c.wantCode == http.StatusOK && rec.Body.String() != "passed" {
				t.Fatalf("放行时应命中下游 handler，got body=%q", rec.Body.String())
			}
		})
	}
}
