package upstream

import (
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// w3cTraceparentRe 规范形态：00-<32 hex trace-id>-<16 hex span-id>-<2 hex flags>。
// 依据：W3C Trace Context；官方客户端两版实测恒为此形态。
var w3cTraceparentRe = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

// TestChatHeadersTraceparentAlways32Hex 钉住 traceparent 的 trace-id 恒为 32 hex。
//
// 背景：X-B3-TraceId 允许 16 或 32 位，而 traceparent 只允许 32 位。入站客户端
// 若传 16 位 X-Trace-ID（Zipkin 式的 64 位 trace id），旧实现会把 16 位原样拼进
// traceparent，产出官方客户端绝不会发送的非法头——这正是「非官方客户端」的指纹。
func TestChatHeadersTraceparentAlways32Hex(t *testing.T) {
	cases := []struct {
		name       string
		traceID    string
		wantPrefix string // 期望 trace-id 段前缀（空 = 不校验前缀）
	}{
		{name: "32位入站原样保留", traceID: strings.Repeat("ab", 16), wantPrefix: ""},
		{name: "16位入站左补零", traceID: "0123456789abcdef", wantPrefix: "0000000000000000"},
		{name: "空值现造", traceID: "", wantPrefix: ""},
		{name: "非法值回落", traceID: "not-a-hex-trace", wantPrefix: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Client{}
			a := &auth.Auth{AccessToken: "at", UID: "u1"}
			req := httptest.NewRequest("POST", "https://chat.example/v1/chat", nil)
			c.ChatHeaders(req, a, "", ChatMeta{TraceID: tc.traceID})

			got := req.Header.Get("traceparent")
			if !w3cTraceparentRe.MatchString(got) {
				t.Fatalf("traceparent 形态非法（trace-id 必须 32 hex）: %q", got)
			}
			if tc.wantPrefix != "" {
				parts := strings.Split(got, "-")
				if !strings.HasPrefix(parts[1], tc.wantPrefix) {
					t.Errorf("trace-id 应以 %s 开头（左补零扩展），got=%q", tc.wantPrefix, parts[1])
				}
				if !strings.HasSuffix(parts[1], tc.traceID) {
					t.Errorf("补零后应保留原 16 位 trace（链路关系不变），got=%q want suffix=%q", parts[1], tc.traceID)
				}
			}
			// B3 允许 16 位，16 位入站不应被改写。
			if tc.name == "16位入站左补零" {
				if b3 := req.Header.Get("X-B3-TraceId"); b3 != tc.traceID {
					t.Errorf("X-B3-TraceId 应保留入站 16 位原值，got=%q", b3)
				}
			}
		})
	}
}

// TestChatHeadersTraceparentRejectsAllZero 全零 trace-id 在 W3C 里非法，必须回落。
func TestChatHeadersTraceparentRejectsAllZero(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := httptest.NewRequest("POST", "https://chat.example/v1/chat", nil)
	c.ChatHeaders(req, a, "", ChatMeta{TraceID: strings.Repeat("0", 32)})

	got := req.Header.Get("traceparent")
	if !w3cTraceparentRe.MatchString(got) {
		t.Fatalf("traceparent 形态非法: %q", got)
	}
	if strings.Split(got, "-")[1] == strings.Repeat("0", 32) {
		t.Fatalf("全零 trace-id 不应出现在 traceparent 里: %q", got)
	}
}
