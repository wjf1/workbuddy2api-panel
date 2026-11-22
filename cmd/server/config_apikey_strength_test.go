package main

import (
	"strings"
	"testing"
)

// TestWeakAPIKeyReason 弱密钥判定的行为钉子。
//
// 空 key 不算弱（交给 insecure_write_guard 兜底：非回环拒写）；样例值与过短值算弱——
// config.example.json 的 "test_key" 照抄后能完全旁路写守卫，是「有鉴权之名、无鉴权之实」
// 的典型暴露面。
func TestWeakAPIKeyReason(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		wantOK bool // true = 强度可接受（reason 为空）
	}{
		{"空 key 交给写守卫兜底", "", true},
		{"纯空白等价空 key", "   ", true},
		{"样例值 test_key", "test_key", false},
		{"样例值带空白", " test_key ", false},
		{"过短随机值", "abc123", false},
		{"12 字符随机值", "sk-0123456789", true},
		{"WriteDefault 生成的形态", "sk-abcdefghijklmnopqrstuvwxyz-_0", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reason := weakAPIKeyReason(c.key)
			if (reason == "") != c.wantOK {
				t.Fatalf("key=%q wantOK=%v, got reason=%q", c.key, c.wantOK, reason)
			}
		})
	}
}

// TestCheckAPIKeyStrength 启动校验的暴露面分级：非回环 + 弱密钥必须拒绝启动
// （除非显式 WB2A_ALLOW_WEAK_KEY=1），回环自用只 WARN 不拒绝——不破坏现状。
func TestCheckAPIKeyStrength(t *testing.T) {
	cases := []struct {
		name      string
		listen    string
		apiKey    string
		allowEnv  string
		wantFatal bool
	}{
		{"非回环 + 样例值 → 拒绝", ":7863", "test_key", "", true},
		{"非回环 0.0.0.0 + 过短 → 拒绝", "0.0.0.0:8787", "short", "", true},
		{"非回环 + 样例值 + 豁免 env → 放行", ":7863", "test_key", "1", false},
		{"回环 + 样例值 → 仅 WARN 放行", "127.0.0.1:7863", "test_key", "", false},
		{"回环 localhost + 过短 → 放行", "localhost:7863", "short", "", false},
		{"非回环 + 强 key → 放行", ":7863", "sk-0123456789abcdef", "", false},
		{"非回环 + 空 key → 写守卫兜底，不拒绝", ":7863", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.allowEnv != "" {
				t.Setenv("WB2A_ALLOW_WEAK_KEY", c.allowEnv)
			} else {
				t.Setenv("WB2A_ALLOW_WEAK_KEY", "")
			}
			err := checkAPIKeyStrength(c.listen, c.apiKey)
			if (err != nil) != c.wantFatal {
				t.Fatalf("listen=%q key=%q wantFatal=%v, got err=%v", c.listen, c.apiKey, c.wantFatal, err)
			}
			if err != nil && !strings.Contains(err.Error(), "WB2A_ALLOW_WEAK_KEY") {
				t.Fatalf("拒绝消息必须给出修复路径（含豁免说明），got: %v", err)
			}
		})
	}
}
