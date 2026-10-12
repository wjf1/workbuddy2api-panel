package panel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// importResp 导入接口响应体（1.14.2 起 total 含无法解析的条目数）。
type importResp struct {
	OK       bool     `json:"ok"`
	Total    int      `json:"total"`
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors"`
}

// importRaw 上传单个任意负载（json/zip 字节）并返回状态码、解析后的响应与原始 body。
func importRaw(t *testing.T, dst *Panel, filename string, data []byte) (int, importResp, []byte) {
	t.Helper()
	var buf bytes.Buffer
	buf.WriteString("--BOUNDARY\r\nContent-Disposition: form-data; name=\"file\"; filename=\"" + filename + "\"\r\nContent-Type: application/octet-stream\r\n\r\n")
	buf.Write(data)
	buf.WriteString("\r\n--BOUNDARY--\r\n")
	r := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	r.Header.Set("Authorization", "Bearer test-key")
	r.Header.Set("Content-Type", "multipart/form-data; boundary=BOUNDARY")
	w := httptest.NewRecorder()
	dst.ServeHTTP(w, r)
	var resp importResp
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("import resp parse: %v body=%s", err, w.Body.String())
		}
	}
	return w.Code, resp, w.Body.Bytes()
}

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

// 嵌套形（落盘/插件输出 {"account":{...},"auth":{...}}，秒）单对象直接导入。
func TestImportNestedSingleFormat(t *testing.T) {
	dst := newTrashPanel(t)
	nested := `{"account":{"enterpriseId":"","nickname":"嵌套甲","uid":"nested-1"},` +
		`"auth":{"accessToken":"at-n1","refreshToken":"rt-n1","expiresAt":1893456000,` +
		`"domain":"www.codebuddy.cn","realm":"cn"}}`
	code, resp, body := importRaw(t, dst, "workbuddy-nested-1.json", []byte(nested))
	if code != http.StatusOK || resp.Imported != 1 || resp.Skipped != 0 || len(resp.Errors) != 0 {
		t.Fatalf("code=%d resp=%+v body=%s", code, resp, body)
	}
	got := readAuthJSON(t, dst.cfg.AuthDir, "nested-1")
	if got.AccessTokenValue() != "at-n1" || got.RefreshTokenValue() != "rt-n1" {
		t.Fatal("token lost on nested import")
	}
	if got.Nickname != "嵌套甲" || got.DomainValue() != "www.codebuddy.cn" {
		t.Fatalf("nickname/domain mismatch: %q %q", got.Nickname, got.DomainValue())
	}
	if got.ExpiresAt != 1893456000 { // 秒→毫秒→秒 往返应无损
		t.Fatalf("expires = %d, want 1893456000", got.ExpiresAt)
	}
	if got.Realm() != "cn" {
		t.Fatalf("realm = %s, want cn", got.Realm())
	}
}

// 嵌套形数组与 cockpit 扁平条目混合：逐元素嗅探，各自走对应口径（秒/毫秒、realm）。
func TestImportNestedArrayMixed(t *testing.T) {
	dst := newTrashPanel(t)
	payload := `[` +
		`{"account":{"nickname":"嵌套乙","uid":"nested-2"},"auth":{"accessToken":"at-n2",` +
		`"refreshToken":"rt-n2","expiresAt":1893456000,"domain":"www.codebuddy.cn","realm":"cn"}},` +
		`{"id":"workbuddy_flat-1","uid":"flat-1","access_token":"at-f1","refresh_token":"rt-f1",` +
		`"expires_at":1893456000000,"domain":"www.workbuddy.ai"}]`
	code, resp, body := importRaw(t, dst, "mixed.json", []byte(payload))
	if code != http.StatusOK || resp.Imported != 2 || resp.Skipped != 0 {
		t.Fatalf("code=%d resp=%+v body=%s", code, resp, body)
	}
	if got := readAuthJSON(t, dst.cfg.AuthDir, "nested-2"); got.ExpiresAt != 1893456000 || got.Realm() != "cn" {
		t.Fatalf("nested entry: expires=%d realm=%s", got.ExpiresAt, got.Realm())
	}
	if got := readAuthJSON(t, dst.cfg.AuthDir, "flat-1"); got.ExpiresAt != 1893456000 || got.Realm() != "global" {
		t.Fatalf("flat entry: expires=%d realm=%s", got.ExpiresAt, got.Realm())
	}
}

// 数组内坏元素（非对象/无账号字段）只计 skipped+errors，其余条目照常导入。
func TestImportArrayBadEntry(t *testing.T) {
	dst := newTrashPanel(t)
	payload := `[` +
		`{"uid":"keep-1","access_token":"at-k1","refresh_token":"rt-k1","expires_at":1893456000000},` +
		`{"foo":1},` +
		`"not-an-object"]`
	code, resp, body := importRaw(t, dst, "entries.json", []byte(payload))
	if code != http.StatusOK || resp.Imported != 1 || resp.Skipped != 2 {
		t.Fatalf("code=%d resp=%+v body=%s", code, resp, body)
	}
	if len(resp.Errors) != 2 {
		t.Fatalf("errors = %v", resp.Errors)
	}
	if _, err := os.Stat(filepath.Join(dst.cfg.AuthDir, "workbuddy-keep-1.json")); err != nil {
		t.Fatalf("good entry not persisted: %v", err)
	}
}

// 扁平 snake_case 单对象（非数组）导入。
func TestImportSingleFlatObject(t *testing.T) {
	dst := newTrashPanel(t)
	payload := `{"id":"workbuddy_single-1","uid":"single-1","access_token":"at-s1",` +
		`"refresh_token":"rt-s1","expires_at":1893456000000,"domain":"www.workbuddy.ai"}`
	code, resp, body := importRaw(t, dst, "one.json", []byte(payload))
	if code != http.StatusOK || resp.Imported != 1 {
		t.Fatalf("code=%d resp=%+v body=%s", code, resp, body)
	}
	if got := readAuthJSON(t, dst.cfg.AuthDir, "single-1"); got.Realm() != "global" {
		t.Fatalf("realm = %s, want global", got.Realm())
	}
}

// zip 整包导入：内部多个 workbuddy-*.json（嵌套形）+ 一个坏 json + 无关文件，
// 好的全部导入，坏的只进 errors 不阻断，无关文件静默跳过。
func TestImportZipPayload(t *testing.T) {
	dst := newTrashPanel(t)
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	write := func(name, content string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	write("workbuddy-zip-1.json", `{"account":{"nickname":"包甲","uid":"zip-1"},`+
		`"auth":{"accessToken":"at-z1","refreshToken":"rt-z1","expiresAt":1893456000,`+
		`"domain":"www.codebuddy.cn","realm":"cn"}}`)
	write("workbuddy-zip-2.json", `{"account":{"nickname":"包乙","uid":"zip-2"},`+
		`"auth":{"accessToken":"at-z2","refreshToken":"rt-z2","expiresAt":1893456000,`+
		`"domain":"www.codebuddy.cn","realm":"cn"}}`)
	write("broken.json", `{"foo": 1}`)
	write("__MACOSX/._junk.json", "junk")
	write("readme.txt", "not json")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	code, resp, body := importRaw(t, dst, "accounts.zip", zbuf.Bytes())
	if code != http.StatusOK {
		t.Fatalf("zip import code = %d body=%s", code, body)
	}
	if resp.Imported != 2 || resp.Total != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	if len(resp.Errors) != 1 || !strings.Contains(resp.Errors[0], "broken.json") {
		t.Fatalf("errors = %v", resp.Errors)
	}
	for _, uid := range []string{"zip-1", "zip-2"} {
		if _, err := os.Stat(filepath.Join(dst.cfg.AuthDir, "workbuddy-"+uid+".json")); err != nil {
			t.Fatalf("uid %s not persisted: %v", uid, err)
		}
	}
}

// 无法识别的形状（对象但无任何账号字段）：单文件请求 400 且报错明确。
func TestImportUnrecognizedFormat(t *testing.T) {
	dst := newTrashPanel(t)
	code, _, body := importRaw(t, dst, "weird.json", []byte(`{"foo":1}`))
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 body=%s", code, body)
	}
	if !strings.Contains(string(body), "unrecognized") {
		t.Fatalf("body should explain unrecognized format: %s", body)
	}
}
