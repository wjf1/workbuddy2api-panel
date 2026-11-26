package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
)

// newGroupTestHandler 带主 key "master" 与两个分组的 handler。
// groupA 绑定模型 cn:gpt-a + RPM=60；groupB 无模型限制。
func newGroupTestHandler() *Handler {
	return &Handler{cfg: Config{
		Live: livecfg.New(livecfg.Snapshot{
			APIKey: "master",
			KeyGroups: livecfg.NormalizeKeyGroups([]livecfg.KeyGroup{
				{Name: "team-a", Key: "key-a", Models: []string{"gpt-a"}, RPM: 60},
				{Name: "team-b", Key: "key-b"},
			}),
		}),
	}}
}

func authedReq(path, key string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+key)
	r.RemoteAddr = "10.0.0.1:5000"
	return r
}

// TestWithAuthKeyGroups 鉴权：主 key 直通无组、分组 key 命中注入 ctx、
// 错误 key 401。
func TestWithAuthKeyGroups(t *testing.T) {
	h := newGroupTestHandler()
	var gotGroup *livecfg.KeyGroup
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotGroup = keyGroupFrom(r)
		w.WriteHeader(http.StatusOK)
	})
	wrapped := h.withAuth(next.ServeHTTP)

	t.Run("主 key 直通且组为 nil", func(t *testing.T) {
		gotGroup = nil
		rec := httptest.NewRecorder()
		wrapped(rec, authedReq("/v1/chat/completions", "master"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d", rec.Code)
		}
		if gotGroup != nil {
			t.Fatalf("master key should not bind group, got %v", gotGroup)
		}
	})
	t.Run("分组 key 命中注入", func(t *testing.T) {
		gotGroup = nil
		rec := httptest.NewRecorder()
		wrapped(rec, authedReq("/v1/chat/completions", "key-a"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if gotGroup == nil || gotGroup.Name != "team-a" {
			t.Fatalf("group not injected: %+v", gotGroup)
		}
	})
	t.Run("错误 key 401", func(t *testing.T) {
		rec := httptest.NewRecorder()
		wrapped(rec, authedReq("/v1/chat/completions", "wrong"))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d want 401", rec.Code)
		}
	})
	t.Run("x-api-key 形态同样命中分组", func(t *testing.T) {
		gotGroup = nil
		r := authedReq("/v1/messages", "")
		r.Header.Del("Authorization")
		r.Header.Set("x-api-key", "key-b")
		rec := httptest.NewRecorder()
		wrapped(rec, r)
		if rec.Code != http.StatusOK || gotGroup == nil || gotGroup.Name != "team-b" {
			t.Fatalf("status=%d group=%+v", rec.Code, gotGroup)
		}
	})
}

// TestKeyGroupModelNarrowing 组内模型收窄：绑定白名单的组名单外模型被拒、
// 名单内放行；未绑定组不受影响。
func TestKeyGroupModelNarrowing(t *testing.T) {
	h := newGroupTestHandler()
	ga := keyGroupForTest(t, h, "team-a")
	gb := keyGroupForTest(t, h, "team-b")

	if h.hiddenModelForGroup(ga, "cn", "gpt-a") {
		t.Fatal("组内白名单模型应可见")
	}
	if !h.hiddenModelForGroup(ga, "cn", "gpt-other") {
		t.Fatal("组白名单外模型应被收窄拒绝")
	}
	if h.hiddenModelForGroup(gb, "cn", "gpt-other") {
		t.Fatal("无模型限制的组不应收窄")
	}
	if h.hiddenModelForGroup(nil, "cn", "gpt-other") {
		t.Fatal("主 key（nil 组）不应收窄")
	}
}

func keyGroupForTest(t *testing.T, h *Handler, name string) *livecfg.KeyGroup {
	t.Helper()
	for _, g := range h.loadLive().KeyGroups {
		if g.Name == name {
			gg := g
			return &gg
		}
	}
	t.Fatalf("group %s not found", name)
	return nil
}

// TestKeyGroupRPM 组级限流：RPM=1 的组第二请求立即 429；另一组不受影响。
func TestKeyGroupRPM(t *testing.T) {
	h := &Handler{cfg: Config{
		Live: livecfg.New(livecfg.Snapshot{
			APIKey: "master",
			KeyGroups: livecfg.NormalizeKeyGroups([]livecfg.KeyGroup{
				{Name: "tiny", Key: "key-tiny", RPM: 1},
				{Name: "big", Key: "key-big", RPM: 0},
			}),
		}),
	}}
	next := h.withAuth(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	rec1 := httptest.NewRecorder()
	next(rec1, authedReq("/v1/chat/completions", "key-tiny"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status=%d", rec1.Code)
	}
	rec2 := httptest.NewRecorder()
	next(rec2, authedReq("/v1/chat/completions", "key-tiny"))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status=%d want 429, body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "tiny") {
		t.Fatalf("429 body should name the group: %s", rec2.Body.String())
	}
	// 无 RPM 限制的组不受 tiny 组限流影响。
	rec3 := httptest.NewRecorder()
	next(rec3, authedReq("/v1/chat/completions", "key-big"))
	if rec3.Code != http.StatusOK {
		t.Fatalf("big group status=%d want 200", rec3.Code)
	}
}

// TestNormalizeKeyGroups 归一化：空 key 组丢弃、组名缺省补齐、全空 → nil。
func TestNormalizeKeyGroups(t *testing.T) {
	gs := livecfg.NormalizeKeyGroups([]livecfg.KeyGroup{
		{Name: "", Key: "k1"},
		{Name: "x", Key: ""}, // 空 key 丢弃
		{Name: "y", Key: "k2"},
	})
	if len(gs) != 2 {
		t.Fatalf("len=%d want 2", len(gs))
	}
	if gs[0].Name == "" {
		t.Fatal("组名缺省应补齐")
	}
	if gs[1].Name != "y" {
		t.Fatalf("name=%s want y", gs[1].Name)
	}
	if livecfg.NormalizeKeyGroups([]livecfg.KeyGroup{{Key: ""}}) != nil {
		t.Fatal("全空应归一化为 nil（单密钥模式）")
	}
}
