package main

// auth_source.go — doctor 读取账号目录的薄封装。
//
// 单独成文件是为了把「怎么读账号」与「怎么判定」分开：checkAuthDir 关心统计，
// checkUpstream 关心拿第一个账号探测；两者共用这里的加载口径。

import (
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// loadAuths 读账号目录；目录不存在时返回空表而非错误（首次部署的常态）。
func loadAuths(dir string) ([]*auth.Auth, error) {
	return auth.LoadDir(dir)
}

// isGlobalRealm 判定账号属于国际版（口径对齐 internal/auth.Realm()：
// 显式 realm=global 或 domain 落在 workbuddy.ai 家族）。
func isGlobalRealm(a *auth.Auth) bool {
	if a == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(a.Realm()), "global") {
		return true
	}
	d := strings.ToLower(strings.TrimSpace(a.Domain))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}
