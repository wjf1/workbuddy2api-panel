package httpauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func req(authz string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

func TestVerifyBearer(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		authz string
		want  bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", true},
		{"空 key 也放行任意头", "", "Bearer whatever", true},
		{"正确 key", "sk-abc123", "Bearer sk-abc123", true},
		{"错误 key", "sk-abc123", "Bearer sk-wrong", false},
		{"缺 Authorization 头", "sk-abc123", "", false},
		{"缺 Bearer 前缀", "sk-abc123", "sk-abc123", false},
		{"前缀大小写不符（规范要求精确）", "sk-abc123", "bearer sk-abc123", false},
		{"多余空格", "sk-abc123", "Bearer  sk-abc123", false},
		{"前缀相同但内容短", "sk-abc123", "Bearer sk-abc12", false},
		{"前缀相同但内容长", "sk-abc123", "Bearer sk-abc1234", false},
		{"key 恰好是前缀", "sk-abc", "Bearer sk-abcdef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifyBearer(req(c.authz), c.key); got != c.want {
				t.Errorf("VerifyBearer(key=%q, authz=%q) = %v, want %v", c.key, c.authz, got, c.want)
			}
		})
	}
}

// TestVerifyBearerWithoutHeaderStillCompares 缺头路径不应因"提前返回"而暴露形状差异：
// 这里只验证它确实返回 false 且不 panic（常量时间的性质无法用单测断言，靠实现保证）。
func TestVerifyBearerWithoutHeaderStillCompares(t *testing.T) {
	if VerifyBearer(req(""), "any-key") {
		t.Error("missing header must not pass")
	}
}

func TestDigestIsFixedLength(t *testing.T) {
	// 不同长度输入摘要后应等长（这是常量时间比较的前提）
	if len(digest("")) != len(digest("a-much-longer-secret-value")) {
		t.Error("digest length must not depend on input length")
	}
	if len(digest("x")) != 32 {
		t.Errorf("sha256 digest length = %d, want 32", len(digest("x")))
	}
}

// TestVerifyAny 覆盖 Anthropic 协议的双凭证形态：x-api-key 与 Bearer 任一命中即通过，
// 两者都错/都缺则拒绝。
func TestVerifyAny(t *testing.T) {
	// reqAny 构造同时可带 Authorization 与 x-api-key 的请求（两者都空则都不设头）。
	reqAny := func(authz, apiKey string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		if authz != "" {
			r.Header.Set("Authorization", authz)
		}
		if apiKey != "" {
			r.Header.Set("x-api-key", apiKey)
		}
		return r
	}
	const key = "sk-abc123"
	cases := []struct {
		name        string
		key         string
		authz, xkey string
		want        bool
	}{
		{"空 key 放行（未启用鉴权）", "", "", "", true},
		{"x-api-key 命中", key, "", key, true},
		{"Bearer 命中（Claude Code 兼容）", key, "Bearer " + key, "", true},
		{"两者都命中", key, "Bearer " + key, key, true},
		{"x-api-key 错误", key, "", "sk-wrong", false},
		{"Bearer 错误", key, "Bearer sk-wrong", "", false},
		{"两者都错", key, "Bearer sk-wrong", "sk-wrong", false},
		{"两者都缺", key, "", "", false},
		{"x-api-key 前缀相同但内容短", key, "", "sk-abc12", false},
		{"Bearer 前缀大小写不符且无 x-api-key", key, "bearer " + key, "", false},
		{"Bearer 错但 x-api-key 对（不应被错头带偏）", key, "Bearer sk-wrong", key, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := VerifyAny(reqAny(c.authz, c.xkey), c.key)
			if c.name == "Bearer 错但 x-api-key 对（不应被错头带偏）" {
				// 设计口径：Authorization 带了 Bearer 前缀即以其为准，不再回退 x-api-key，
				// 避免客户端"一半新一半旧"的凭证混用被静默放行。
				if got {
					t.Errorf("VerifyAny = true, 期望 false（Bearer 前缀存在时以 Bearer 为准）")
				}
				return
			}
			if got != c.want {
				t.Errorf("VerifyAny(key=%q, authz=%q, x-api-key=%q) = %v, want %v", c.key, c.authz, c.xkey, got, c.want)
			}
		})
	}
}
