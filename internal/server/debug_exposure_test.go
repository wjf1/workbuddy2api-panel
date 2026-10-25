package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestMainMuxHasNoDebugEndpoints 安全边界回归：诊断端点（pprof / metrics）**不得**
// 出现在对外主服务上。
//
// pprof 无鉴权，profile 可导出 CPU/heap 采样，heap 里含请求体与账号 token；主端口
// 通常绑定 0.0.0.0。把它们挂上主 mux 等于给公网开一个"读内存"的后门。诊断监听是
// 独立端口（cmd/server/debug.go），这里钉住"主 mux 不认识这些路径"。
func TestMainMuxHasNoDebugEndpoints(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up})

	for _, path := range []string{"/debug/pprof/", "/debug/pprof/profile", "/metrics"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s 应返回 404（主服务不挂诊断端点），got %d", path, rec.Code)
		}
	}
}
