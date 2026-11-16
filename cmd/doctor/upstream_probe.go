package main

// upstream_probe.go — doctor 的可选上游连通性探测。
//
// 只做一件事：用一个已知账号对上游发一次**只读**请求（模型目录 /v1/models 或
// 等价端点），测出「能不能连上、延迟多少」。不发对话请求、不改任何状态——
// 可以放心在生产部署上跑。
//
// 探测失败不算 error：上游抖动 / 本地网络不通都不代表网关接线错误。归 warning，
// 让人知道「现在打不通」但不必紧张。

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// checkUpstream 轻量连通性探测：优先用第一个账号的 realm 打该域的模型目录端点。
// 没有任何账号时跳过（无法判定，不是问题）。
func checkUpstream(cfg *doctorConfig) check {
	entries, err := loadAuths(cfg.AuthDir)
	if err != nil || len(entries) == 0 {
		return check{Name: "upstream", Level: levelOK, Detail: "无账号可探测（跳过）"}
	}
	a := entries[0]
	base := "https://www.codebuddy.cn"
	if isGlobalRealm(a) {
		base = "https://www.workbuddy.ai"
	}
	url := base + "/v1/models"

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return check{Name: "upstream", Level: levelWarn, Detail: fmt.Sprintf("构造探测请求失败: %v", err)}
	}
	// 只发鉴权头：模型目录端点对凭证的校验足够代表「链路通 + token 可用」。
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		return check{Name: "upstream", Level: levelWarn,
			Detail: fmt.Sprintf("探测 %s 失败（%s）: %v", url, elapsed.Round(time.Millisecond), err)}
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return check{Name: "upstream", Level: levelOK,
			Detail: fmt.Sprintf("%s → %d（%s）", url, resp.StatusCode, elapsed.Round(time.Millisecond))}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return check{Name: "upstream", Level: levelWarn,
			Detail: fmt.Sprintf("%s → %d：探测账号 token 可能已失效（保活会刷新；长期失效需重登）",
				url, resp.StatusCode)}
	default:
		return check{Name: "upstream", Level: levelWarn,
			Detail: fmt.Sprintf("%s → %d（%s）", url, resp.StatusCode, elapsed.Round(time.Millisecond))}
	}
}
