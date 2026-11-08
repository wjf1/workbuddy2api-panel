// 不安全绑定下的写保护（判定：监听地址非回环 && 未启用 api_key）。
//
// 空密钥 = 「未启用鉴权」，此前恒放行——绑定 0.0.0.0 / docker-compose 暴露端口时，
// 局域网内任何人都可以打 /v1 消耗账号、经 /panel/api 增删账号。本守卫保留
// 「回环自用零配置」语义：仅当绑定面是非回环地址时，非幂等方法（POST/PUT/
// PATCH/DELETE）一律 405，只读方法放行（面板可见但改不了）；设置 api_key 后
// 守卫完全旁路，鉴权交回 httpauth。
package main

import (
	"net/http"
	"strings"
)

func insecureWriteGuard(listen, apiKey string, next http.Handler) http.Handler {
	if strings.TrimSpace(apiKey) != "" {
		return next
	}
	host := listen
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	// ":8787"（host 段为空 = 全接口）与 "0.0.0.0" 都视为非回环。
	switch host {
	case "127.0.0.1", "localhost", "::1", "[::1]":
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
