// debug.go 可选诊断监听：pprof 与最小 metrics。
//
// 为什么**单独开一个端口**、而不是挂在主 mux 的 /debug/pprof/ 下：
//
//	Go 的 net/http/pprof 默认没有任何鉴权，profile 可以直接导出 CPU/heap 采样，
//	heap 里含请求体、账号 token 等明文。主端口通常对外暴露（`listen: ":7863"`），
//	一旦把 pprof 挂上去，等于给公网开了一个"读内存"的后门。独立监听可以只绑
//	127.0.0.1，且默认关闭（server.debug_addr 为空 = 不启动这个监听）。
//
// metrics 只放**低基数**指标：状态枚举 + 计数。刻意不做 per-model / per-account
// 标签——那会把基数炸到几千，Prometheus 侧先崩。
package main

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"runtime"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// newDebugServer 构造诊断监听。addr 形如 "127.0.0.1:7864"。
func newDebugServer(addr string, p *pool.Pool) *http.Server {
	mux := http.NewServeMux()

	// 显式挂到私有 mux：import 副作用会往 http.DefaultServeMux 注册，
	// 这里显式转发一份，不依赖全局状态（主服务也不用 DefaultServeMux）。
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/metrics", metricsHandler(p))

	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 ReadTimeout/WriteTimeout：profile?seconds=30 与 trace 是长连接，
		// 掐断会让采样半途而废。
	}
}

// metricsHandler 输出 Prometheus 文本格式。只读聚合计数，单次 scrape 成本可忽略。
func metricsHandler(p *pool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		total, healthy, cooling, disabled, inFlightFull := p.CountsDetailed()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP wb2api_accounts 账号池按状态计数\n")
		fmt.Fprintf(w, "# TYPE wb2api_accounts gauge\n")
		// 固定顺序输出，便于 diff 与人工比对（map 遍历顺序随机会让每次抓取都"变化"）。
		for _, kv := range []struct {
			state string
			n     int
		}{
			{"total", total},
			{"healthy", healthy},
			{"cooling", cooling},
			{"disabled", disabled},
			{"in_flight_full", inFlightFull},
		} {
			fmt.Fprintf(w, "wb2api_accounts{state=%q} %d\n", kv.state, kv.n)
		}
		fmt.Fprintf(w, "# HELP wb2api_goroutines 当前 goroutine 数（排查泄漏用）\n")
		fmt.Fprintf(w, "# TYPE wb2api_goroutines gauge\n")
		fmt.Fprintf(w, "wb2api_goroutines %d\n", runtime.NumGoroutine())
	}
}
