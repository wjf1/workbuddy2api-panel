// summary_paths_test.go 钉住 2026-11-23 上游端点迁移后的会员档位摘要路径策略。
//
// 背景：CN 侧 /v2/billing/meter/get-user-resource-summary 已在网关层下线（POST/GET
// 均 404 Route Not Found），无 /v2 的同名路径可达业务层并返回 data.IsPaidUser /
// data.SubscriptionPackageName。两个 realm 因此统一「无 /v2 优先、有 /v2 兜底」，
// 靠 billingMeterJSON 的 404-only fallback 生效；401/403 仍直接返回，不得静默降级
// 成 free（会员档位是面板结论，读失败只能保留旧值）。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const (
	summaryNoV2 = "/billing/meter/get-user-resource-summary"
	summaryV2   = "/v2/billing/meter/get-user-resource-summary"
)

// TestSummaryMeterPathsSameOrderForBothRealms 断言路径序列不再按 realm 分叉：
// 两个 realm 都是「无 /v2 在前、带 /v2 兜底」。
func TestSummaryMeterPathsSameOrderForBothRealms(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("unused")
	})
	c.GlobalEnabled = true
	want := summaryNoV2 + "," + summaryV2
	cases := []struct {
		name string
		a    *auth.Auth
	}{
		{"cn", &auth.Auth{AccessToken: "at"}},
		{"global", &auth.Auth{AccessToken: "at", Domain: "www.workbuddy.ai"}},
	}
	for _, tc := range cases {
		got := strings.Join(c.summaryMeterPaths(tc.a), ",")
		if got != want {
			t.Errorf("%s: paths=%s want=%s", tc.name, got, want)
		}
	}
}

// TestUserResourceSummaryPrefersNoV2Path 断言新路径一次命中：不再先撞 404 再换路，
// 也不会有第二次请求。
func TestUserResourceSummaryPrefersNoV2Path(t *testing.T) {
	var paths []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"IsPaidUser":true,"SubscriptionPackageName":"专业版"}}`), nil
	})
	mt, err := c.UserResourceSummary(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if !mt.Paid || mt.Pkg != "专业版" {
		t.Errorf("tier=%+v want paid=true pkg=专业版", mt)
	}
	if len(paths) != 1 || paths[0] != summaryNoV2 {
		t.Errorf("paths=%v want 仅 [%s]", paths, summaryNoV2)
	}
}

// TestUserResourceSummaryFallsBackToV2On404 钉住 404-only fallback：若上游哪天把
// 路径挪回 /v2，无 /v2 先 404 后仍能取到档位（不因单次迁移再断一次）。
func TestUserResourceSummaryFallsBackToV2On404(t *testing.T) {
	var paths []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case summaryNoV2:
			return jsonResp(404, `{"error_msg":"404 Route Not Found"}`), nil
		case summaryV2:
			return jsonResp(200, `{"code":0,"data":{"IsPaidUser":false,"SubscriptionPackageName":"体验版"}}`), nil
		}
		return nil, errors.New("unexpected path: " + r.URL.Path)
	})
	mt, err := c.UserResourceSummary(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("fallback 应成功，err=%v", err)
	}
	if mt.Paid || mt.Pkg != "体验版" {
		t.Errorf("tier=%+v want paid=false pkg=体验版", mt)
	}
	if len(paths) != 2 || paths[0] != summaryNoV2 || paths[1] != summaryV2 {
		t.Errorf("paths=%v want [%s %s]", paths, summaryNoV2, summaryV2)
	}
}

// TestUserResourceSummaryNoFallbackOnAuthError 断言 403（风控/鉴权）不换路径：
// 换路径只会把同一个鉴权失败再打一遍，且双路径不得掩盖「读不到档位」这一事实。
func TestUserResourceSummaryNoFallbackOnAuthError(t *testing.T) {
	var calls int32
	c := testClient(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return jsonResp(403, `{"code":10085,"msg":"请求不合法，如有疑问请联系客服"}`), nil
	})
	if _, err := c.UserResourceSummary(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("403 必须返回错误（读失败不得写成 free）")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("calls=%d want 1（非 404 不 fallback）", n)
	}
}
