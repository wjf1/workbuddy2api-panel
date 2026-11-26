package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// TestParseAllowedIPs 解析条目：CIDR / 精确 IP / 空条目跳过 / 非法条目报错。
func TestParseAllowedIPs(t *testing.T) {
	t.Run("cidr 与精确 IP", func(t *testing.T) {
		nets, err := ParseAllowedIPs([]string{"10.0.0.0/8", " 1.2.3.4 ", "", "2001:db8::1"})
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if len(nets) != 3 {
			t.Fatalf("len(nets)=%d want 3", len(nets))
		}
	})
	t.Run("非法 CIDR 报错", func(t *testing.T) {
		if _, err := ParseAllowedIPs([]string{"10.0.0.0/33"}); err == nil {
			t.Fatal("want error for invalid CIDR")
		}
	})
	t.Run("非法 IP 报错", func(t *testing.T) {
		if _, err := ParseAllowedIPs([]string{"not-an-ip"}); err == nil {
			t.Fatal("want error for invalid IP")
		}
	})
	t.Run("空列表 nil", func(t *testing.T) {
		nets, err := ParseAllowedIPs(nil)
		if err != nil || nets != nil {
			t.Fatalf("nil entries: nets=%v err=%v", nets, err)
		}
	})
}

// TestAllowIPGate 白名单闸门行为：未配置恒放行、回环始终放行、healthz/status 豁免、
// 命中放行、未命中 403。
func TestAllowIPGate(t *testing.T) {
	nets, err := ParseAllowedIPs([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		remoteAddr string
		path       string
		wantStatus int
	}{
		{name: "命中白名单放行", remoteAddr: "10.1.2.3:5000", path: "/v1/chat/completions", wantStatus: http.StatusOK},
		{name: "未命中 403", remoteAddr: "8.8.8.8:5000", path: "/v1/chat/completions", wantStatus: http.StatusForbidden},
		{name: "回环始终放行", remoteAddr: "127.0.0.1:5000", path: "/v1/chat/completions", wantStatus: http.StatusOK},
		{name: "IPv6 回环放行", remoteAddr: "[::1]:5000", path: "/v1/chat/completions", wantStatus: http.StatusOK},
		{name: "healthz 豁免", remoteAddr: "8.8.8.8:5000", path: "/healthz", wantStatus: http.StatusOK},
		{name: "status 豁免", remoteAddr: "8.8.8.8:5000", path: "/status", wantStatus: http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			h.cfg.AllowedNets = nets
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.RemoteAddr = tc.remoteAddr
			rec := httptest.NewRecorder()
			got := h.allowIP(rec, req)
			if tc.wantStatus == http.StatusOK {
				if !got {
					t.Fatalf("allowIP=false want true; body=%s", rec.Body.String())
				}
				return
			}
			if got {
				t.Fatal("allowIP=true want false")
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status=%d want %d", rec.Code, tc.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), "ip_not_allowed") {
				t.Fatalf("body missing ip_not_allowed: %s", rec.Body.String())
			}
		})
	}

	t.Run("未配置恒放行", func(t *testing.T) {
		h := &Handler{}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.RemoteAddr = "8.8.8.8:5000"
		if !h.allowIP(httptest.NewRecorder(), req) {
			t.Fatal("nil AllowedNets should allow all")
		}
	})
}

// TestHiddenModelWhitelist 白名单模式：非空集合仅名单内可用，Hidden 再叠加。
func TestHiddenModelWhitelist(t *testing.T) {
	// 未启用白名单：Hidden 黑名单语义不变。
	h := &Handler{cfg: Config{
		Live: livecfg.New(livecfg.Snapshot{
			HiddenModels: map[string]struct{}{"cn:gpt-x": {}},
		}),
	}}
	if h.hiddenModel("cn", "gpt-y") {
		t.Fatal("whitelist disabled: gpt-y should be visible")
	}
	if !h.hiddenModel("cn", "gpt-x") {
		t.Fatal("hidden should still apply")
	}

	// 启用白名单：仅名单内可见，Hidden 叠加。
	h2 := &Handler{cfg: Config{
		Live: livecfg.New(livecfg.Snapshot{
			ModelWhitelist: map[string]struct{}{"cn:gpt-y": {}, "global:gpt-z": {}},
			HiddenModels:   map[string]struct{}{"cn:gpt-x": {}},
		}),
	}}
	if h2.hiddenModel("cn", "gpt-y") {
		t.Fatal("whitelisted model should be visible")
	}
	if !h2.hiddenModel("cn", "gpt-x") {
		t.Fatal("whitelisted-out model should be hidden")
	}
	if !h2.hiddenModel("global", "gpt-y") {
		t.Fatal("cn-only whitelist entry should not cover global")
	}
	if h2.hiddenModel("global", "gpt-z") {
		t.Fatal("global whitelist entry should be visible in global")
	}
}
