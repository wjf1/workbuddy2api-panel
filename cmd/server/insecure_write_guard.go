// 不安全绑定下的写保护（判定：监听地址非回环 && 未启用 api_key）。
//
// 空密钥 = 「未启用鉴权」，此前恒放行——绑定 0.0.0.0 / docker-compose 暴露端口时，
// 局域网内任何人都可以打 /v1 消耗账号、经 /panel/api 增删账号。本守卫保留
// 「回环自用零配置」语义：仅当绑定面是非回环地址时，非幂等方法（POST/PUT/
// PATCH/DELETE）一律 405，只读方法放行（面板可见但改不了）；设置 api_key 后
// 守卫完全旁路，鉴权交回 httpauth——弱密钥（含样例值 test_key）照抄即可绕过本
// 守卫得到「有鉴权之名、无鉴权之实」的暴露面，由启动时的 checkAPIKeyStrength 兜住。
package main

import (
	"net/http"
	"strings"
)

// listenIsLoopback 判断监听地址是否回环。":8787"（host 段为空 = 全接口）
// 与 "0.0.0.0" 都视为非回环。insecure_write_guard 与 checkAPIKeyStrength 共用。
func listenIsLoopback(listen string) bool {
	host := listen
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}

func insecureWriteGuard(listen, apiKey string, next http.Handler) http.Handler {
	if strings.TrimSpace(apiKey) != "" {
		return next
	}
	if listenIsLoopback(listen) {
		return next // 回环自用：不干预
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		http.Error(w,
			"insecure mode: listening on a non-loopback address without api_key; write methods are refused. Set api_key in config.json to enable write access.",
			http.StatusMethodNotAllowed)
	})
}
