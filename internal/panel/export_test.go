package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// newExportPanel 构造带账号池与鉴权的测试面板；调用方按需向池内 Add 账号。
func newExportPanel(t *testing.T) *Panel {
	t.Helper()
	return New(Config{
		Version: "test",
		APIKey:  "test-key",
		Pool:    pool.New(t.TempDir() + "/pool_state.json"),
	})
}

func addExportAccount(p *Panel, uid, at, rt, domain string, expiresSec int64) {
	p.cfg.Pool.Add(&auth.Auth{
		UID:          uid,
		AccessToken:  at,
		RefreshToken: rt,
		ExpiresAt:    expiresSec,
		Domain:       domain,
	})
}

// 导出接口必须经 withAuth：未带 key / 错 key 一律 401，凭据绝不裸奔。
func TestExportCockpitRequiresAuth(t *testing.T) {
	p := newExportPanel(t)
	cases := []struct {
		name   string
		key    string
		wantOK bool
	}{
		{"no key", "", false},
		{"wrong key", "Bearer nope", false},
		{"right key", "Bearer test-key", true},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/panel/api/export/cockpit", nil)
		if c.key != "" {
			req.Header.Set("Authorization", c.key)
		}
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if c.wantOK && rec.Code != http.StatusOK {
			t.Errorf("%s: want 200, got %d", c.name, rec.Code)
		}
		if !c.wantOK && rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: want 401, got %d", c.name, rec.Code)
		}
	}
}

// 空池导出必须是合法 JSON 空数组（而不是 null / 对象），下游按数组解析才不会炸。
func TestExportCockpitEmptyPool(t *testing.T) {
	p := newExportPanel(t)
	req := httptest.NewRequest("GET", "/panel/api/export/cockpit", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var out []cockpitExportAccount
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body not a JSON array: %v; body=%s", err, rec.Body.String())
	}
	if len(out) != 0 {
		t.Fatalf("want empty array, got %d entries", len(out))
	}
	if cd := rec.Header().Get("Content-Disposition"); !bytes.Contains([]byte(cd), []byte("attachment")) {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
}

// 字段与格式契约：秒→毫秒、id 前缀、token_type、数组结构（与 cockpit
// WorkbuddyAccount 的 serde snake_case 对齐）。
func TestExportCockpitPayload(t *testing.T) {
	p := newExportPanel(t)
	addExportAccount(p, "uid-a", "at-a", "rt-a", "workbuddy.ai", 1700000000)
	addExportAccount(p, "uid-b", "at-b", "rt-b", "workbuddy.qq.com", 0) // 无效过期时间 → 省略

	req := httptest.NewRequest("GET", "/panel/api/export/cockpit", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}

	var raw []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(raw) != 2 {
		t.Fatalf("want 2 accounts, got %d", len(raw))
	}
	byUID := map[string]map[string]any{}
	for _, m := range raw {
		byUID[m["uid"].(string)] = m
	}
	a := byUID["uid-a"]
	if a == nil {
		t.Fatal("uid-a missing")
	}
	if a["id"] != "workbuddy_uid-a" {
		t.Errorf("id = %v, want workbuddy_uid-a", a["id"])
	}
	if a["access_token"] != "at-a" || a["refresh_token"] != "rt-a" {
		t.Errorf("tokens mismatch: %v", a)
	}
	if a["token_type"] != "Bearer" {
		t.Errorf("token_type = %v, want Bearer", a["token_type"])
	}
	if ms, ok := a["expires_at"].(float64); !ok || int64(ms) != 1700000000*1000 {
		t.Errorf("expires_at = %v, want %d (sec→ms)", a["expires_at"], int64(1700000000)*1000)
	}
	if b := byUID["uid-b"]; b != nil {
		if _, ok := b["expires_at"]; ok {
			t.Errorf("uid-b expires_at should be omitted for zero, got %v", b["expires_at"])
		}
	}
}

// uids 过滤：命中子集；全部未命中 → 空数组（不回落全量）。
func TestExportCockpitUIDsFilter(t *testing.T) {
	p := newExportPanel(t)
	addExportAccount(p, "uid-a", "at-a", "rt-a", "", 1700000000)
	addExportAccount(p, "uid-b", "at-b", "rt-b", "", 1700000000)

	get := func(url string) []cockpitExportAccount {
		t.Helper()
		req := httptest.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: want 200, got %d", url, rec.Code)
		}
		var out []cockpitExportAccount
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("GET %s: invalid json: %v", url, err)
		}
		return out
	}

	if got := get("/panel/api/export/cockpit?uids=uid-b"); len(got) != 1 || got[0].UID != "uid-b" {
		t.Errorf("uids=uid-b: want only uid-b, got %+v", got)
	}
	if got := get("/panel/api/export/cockpit?uids=uid-a%2Cuid-b"); len(got) != 2 {
		t.Errorf("uids=a,b: want 2, got %d", len(got))
	}
	if got := get("/panel/api/export/cockpit?uids=nope"); len(got) != 0 {
		t.Errorf("uids=nope: want empty, got %+v", got)
	}
	if got := get("/panel/api/export/cockpit"); len(got) != 2 {
		t.Errorf("no uids: want all 2, got %d", len(got))
	}
}

// 互导回环：导出体必须能被本面板的 cockpit 导入结构（[]cockpitAccount）完整
// 解析且必填字段齐全——这正是「先导出、再导回 / 喂给 cockpit tools」的路径。
func TestExportCockpitRoundTripToImportStruct(t *testing.T) {
	p := newExportPanel(t)
	addExportAccount(p, "uid-a", "at-a", "rt-a", "workbuddy.ai", time.Now().Add(24*time.Hour).Unix())

	req := httptest.NewRequest("GET", "/panel/api/export/cockpit", nil)
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	var accounts []cockpitAccount
	if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil {
		t.Fatalf("export body not parseable as []cockpitAccount: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("want 1 account, got %d", len(accounts))
	}
	acc := accounts[0]
	if acc.UID == "" || acc.AccessToken == "" || acc.RefreshToken == "" {
		t.Errorf("required fields (uid/access_token/refresh_token) must survive round-trip: %+v", acc)
	}
	if acc.ExpiresAt <= 0 {
		t.Errorf("expires_at (ms) must be positive after round-trip: %d", acc.ExpiresAt)
	}
}
