package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// exportToBytes 把池内全部账号导出为 cockpit 兼容 JSON（走真实 exportCockpit handler）。
func exportToBytes(t *testing.T, p *Panel) []byte {
	t.Helper()
	w := callAPI(t, p, "GET", "/panel/api/export/cockpit", "test-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("export status = %d", w.Code)
	}
	return w.Body.Bytes()
}

// importJSON 把导出 JSON 回灌另一个面板（走真实 importCockpit handler）。
func importJSON(t *testing.T, dst *Panel, data []byte) {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("--BOUNDARY\r\nContent-Disposition: form-data; name=\"file\"; filename=\"accounts.json\"\r\nContent-Type: application/json\r\n\r\n")
	buf.Write(data)
	buf.WriteString("\r\n--BOUNDARY--\r\n")
	r := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	r.Header.Set("Authorization", "Bearer test-key")
	r.Header.Set("Content-Type", "multipart/form-data; boundary=BOUNDARY")
	w := httptest.NewRecorder()
	dst.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("import status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Imported == 0 || resp.Skipped != 0 || len(resp.Errors) > 0 {
		t.Fatalf("import resp = %s", w.Body.String())
	}
}

// readAuthJSON 读回 auth 落盘文件并 Parse。
func readAuthJSON(t *testing.T, dir, uid string) *auth.Auth {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-"+uid+".json"))
	if err != nil {
		t.Fatalf("read auth %s: %v", uid, err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("parse auth %s: %v", uid, err)
	}
	return a
}

// 导出 → 导入往返无损：token/昵称/过期（秒↔毫秒）/domain/tags/realm/device_token。
// 覆盖 1.14.1 修复的三个缺口：device_token 丢失、显式 realm 被搬家、（异常）过期兜底。
func TestExportImportRoundTrip(t *testing.T) {
	src := newTrashPanel(t)
	dst := newTrashPanel(t)

	expires := time.Now().Add(72 * time.Hour).Unix()
	// a: global 完整（domain 指向 workbuddy.ai + tags + device_token）
	a := &auth.Auth{UID: "rt-a", AccessToken: "at-a", RefreshToken: "rt-a",
		ExpiresAt: expires, Domain: "www.workbuddy.ai", Nickname: "往返甲",
		DeviceToken: "dt-xyz", FilePath: filepath.Join(src.cfg.AuthDir, "workbuddy-rt-a.json")}
	a.SetTags([]string{"主力", "海外"})
	if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	src.cfg.Pool.Add(a)
	// c: 显式 realm=global 但 domain 为空（显式与推断不一致）
	c := &auth.Auth{UID: "rt-c", AccessToken: "at-c", RefreshToken: "rt-c",
		ExpiresAt: expires, Nickname: "显式global",
		FilePath: filepath.Join(src.cfg.AuthDir, "workbuddy-rt-c.json")}
	if _, err := auth.BackfillRealmFor(c, "global"); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	src.cfg.Pool.Add(c)

	data := exportToBytes(t, src)
	importJSON(t, dst, data)

	// —— a：全字段无损 ——
	got := readAuthJSON(t, dst.cfg.AuthDir, "rt-a")
	if got.AccessTokenValue() != "at-a" || got.RefreshTokenValue() != "rt-a" {
		t.Fatal("token lost in round trip")
	}
	if got.Nickname != "往返甲" || got.DomainValue() != "www.workbuddy.ai" {
		t.Fatalf("nickname/domain mismatch: %q %q", got.Nickname, got.DomainValue())
	}
	if got.ExpiresAt != expires {
		t.Fatalf("expires_at = %d, want %d（秒↔毫秒往返应无损）", got.ExpiresAt, expires)
	}
	tags := got.TagsValue()
	if len(tags) != 2 || tags[0] != "主力" || tags[1] != "海外" {
		t.Fatalf("tags = %v", tags)
	}
	if got.Realm() != "global" {
		t.Fatalf("realm = %s, want global", got.Realm())
	}
	if got.DeviceTokenValue() != "dt-xyz" {
		t.Fatalf("device_token lost: %q", got.DeviceTokenValue())
	}
	// —— c：显式 realm 不被 domain 推断搬家 ——
	gotC := readAuthJSON(t, dst.cfg.AuthDir, "rt-c")
	if gotC.Realm() != "global" {
		t.Fatalf("explicit realm got downgraded to %s, want global", gotC.Realm())
	}
}

// 无显式 realm 的旧格式文件（手工/cockpit-tools 导出，无 realm/device_token 键）：
// 导入回落 domain 推断，既有行为不变。
func TestImportLegacyFormatRealmFallback(t *testing.T) {
	dst := newTrashPanel(t)
	legacy := `[{"id":"workbuddy_old1","uid":"old1","access_token":"at","refresh_token":"rt","expires_at":1893456000000,"domain":"www.workbuddy.ai"}]`
	importJSON(t, dst, []byte(legacy))
	got := readAuthJSON(t, dst.cfg.AuthDir, "old1")
	if got.Realm() != "global" {
		t.Fatalf("legacy domain inference = %s, want global", got.Realm())
	}
	if got.DeviceTokenValue() != "" {
		t.Fatal("legacy import should not invent device_token")
	}
	// CN：domain 空 → cn。
	legacyCN := `[{"id":"workbuddy_old2","uid":"old2","access_token":"at","refresh_token":"rt","expires_at":1893456000000}]`
	importJSON(t, dst, []byte(legacyCN))
	if got := readAuthJSON(t, dst.cfg.AuthDir, "old2"); got.Realm() != "cn" {
		t.Fatalf("legacy empty domain inference = %s, want cn", got.Realm())
	}
}

// 导入零过期时间戳（expires_at 缺失/≤0）回落 +365d 兜底（既有语义，测试锁定）。
func TestImportZeroExpiresFallback(t *testing.T) {
	dst := newTrashPanel(t)
	legacy := `[{"id":"workbuddy_noexp","uid":"noexp","access_token":"at","refresh_token":"rt"}]`
	importJSON(t, dst, []byte(legacy))
	got := readAuthJSON(t, dst.cfg.AuthDir, "noexp")
	if got.ExpiresAt <= time.Now().Unix() {
		t.Fatalf("zero expires should fall back to +365d, got %d", got.ExpiresAt)
	}
}
