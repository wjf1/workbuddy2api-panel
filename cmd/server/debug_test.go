package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestDebugServerExposesPprofAndMetrics 诊断监听要同时提供 pprof 与 /metrics。
func TestDebugServerExposesPprofAndMetrics(t *testing.T) {
	p := pool.New("")
	srv := newDebugServer("127.0.0.1:0", p)

	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	if rec := get("/debug/pprof/"); rec.Code != http.StatusOK {
		t.Fatalf("/debug/pprof/ 应返回 200，got %d", rec.Code)
	}

	rec := get("/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics 应返回 200，got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("/metrics Content-Type 应是 Prometheus 文本格式，got %q", ct)
	}
	body := rec.Body.String()
	// 固定五档 + goroutine 数：少一档意味着有人在 CountsDetailed 的语义上动了手，
	// 面板与监控会静默对不上。
	for _, want := range []string{
		"wb2api_accounts{state=\"total\"}",
		"wb2api_accounts{state=\"healthy\"}",
		"wb2api_accounts{state=\"cooling\"}",
		"wb2api_accounts{state=\"disabled\"}",
		"wb2api_accounts{state=\"in_flight_full\"}",
		"wb2api_goroutines ",
		"# TYPE wb2api_goroutines gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics 缺少 %q\n实际输出:\n%s", want, body)
		}
	}
}
