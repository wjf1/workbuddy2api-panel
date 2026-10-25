package upstream

import (
	"errors"
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestParseUpstreamRequestID 三种常见形态都要认；没有就返回空串，绝不编造。
func TestParseUpstreamRequestID(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"顶层 requestId", `{"code":6021,"msg":"queue.full","requestId":"up-1a2b"}`, "up-1a2b"},
		{"顶层 request_id", `{"code":14018,"msg":"no credit","request_id":"up-snake"}`, "up-snake"},
		{"嵌套 error.requestId", `{"error":{"message":"bad","requestId":"up-nested"}}`, "up-nested"},
		{"空白值视为没有", `{"code":1,"requestId":"   "}`, ""},
		{"无该字段", `{"code":1,"msg":"boom"}`, ""},
		{"非 JSON", `boom`, ""},
		{"空串", ``, ""},
	}
	for _, c := range cases {
		if got := ParseUpstreamRequestID(c.body); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestChatStreamErrorCarriesUpstreamRequestID 错误信封必须把上游 requestId 带出来：
// 这是用户报障时唯一能与上游客服对上的标识（成功响应没有该字段）。
func TestChatStreamErrorCarriesUpstreamRequestID(t *testing.T) {
	const body = `{"code":6021,"msg":"queue.full.title","requestId":"up-9f8e7d"}`
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(http.StatusTooManyRequests, body), nil
	})

	a := &auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}
	_, _, _, err := c.ChatStream(a, []byte(`{"model":"m","messages":[]}`), "", ChatMeta{})
	if err == nil {
		t.Fatal("上游 429 应返回错误")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("错误应为 *upstream.Error，got %T", err)
	}
	if ue.RequestID != "up-9f8e7d" {
		t.Fatalf("Error.RequestID 未填充：got %q", ue.RequestID)
	}
}
