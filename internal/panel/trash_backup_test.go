package panel

import (
	"archive/zip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// openTestZip 打开并校验 zip 可读。
func openTestZip(t *testing.T, path string) *zip.Reader {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	return zr
}

// newTrashPanel 构造带回收站/备份目录与真实 auth 落盘的测试面板。
func newTrashPanel(t *testing.T) *Panel {
	t.Helper()
	root := t.TempDir()
	stateFp := filepath.Join(root, "data", "state.json")
	p := New(Config{
		Version:            "test",
		APIKey:             "test-key",
		Pool:               pool.New(stateFp),
		AuthDir:            filepath.Join(root, "auths"),
		TrashDir:           filepath.Join(root, "data", "trash"),
		TrashRetentionDays: 30,
		BackupDir:          filepath.Join(root, "data", "backups"),
		StateFile:          stateFp,
	})
	if err := os.MkdirAll(p.cfg.AuthDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// addDiskAccount 落一个真 auth 文件并加入池（回收站移动的是磁盘文件）。
func addDiskAccount(t *testing.T, p *Panel, uid string) *auth.Auth {
	t.Helper()
	a := &auth.Auth{
		UID: uid, AccessToken: "at-" + uid, RefreshToken: "rt-" + uid,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Nickname:  "昵称-" + uid,
		FilePath:  filepath.Join(p.cfg.AuthDir, "workbuddy-"+uid+".json"),
	}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	p.cfg.Pool.Add(a)
	return a
}

// callAPI 经 Panel.ServeHTTP 走完整 mux+鉴权链（PathValue 由 mux 填充）。
func callAPI(t *testing.T, p *Panel, method, path, key string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("")
	} else {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	return w
}

// 移除账号 → 凭证文件进入回收站（软删除），账号出池。
func TestRemoveAccountSoftDeletesToTrash(t *testing.T) {
	p := newTrashPanel(t)
	a := addDiskAccount(t, p, "u1")

	w := callAPI(t, p, "POST", "/panel/api/accounts/u1/remove", "test-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("remove status = %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Trashed string `json:"trashed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.OK || resp.Trashed == "" {
		t.Fatalf("remove resp = %s", w.Body.String())
	}
	// auth 文件已离开 auths/。
	if _, err := os.Stat(a.FilePath); !os.IsNotExist(err) {
		t.Fatalf("auth file still present after remove: %v", err)
	}
	// 出池。
	if p.cfg.Pool.AuthByUID("u1") != nil {
		t.Fatal("account still in pool after remove")
	}
	// 回收站可见（含元数据）。
	items, err := p.trashItems()
	if err != nil || len(items) != 1 {
		t.Fatalf("trashItems = %v, %v", items, err)
	}
	if items[0].UID != "u1" || items[0].Nickname != "昵称-u1" || items[0].DeletedAt == 0 || items[0].PurgeAt == 0 {
		t.Fatalf("trash item metadata incomplete: %+v", items[0])
	}
}

// 回收站列表接口需鉴权：未带 key 401（凭据元数据不裸奔）。
func TestTrashRequiresAuth(t *testing.T) {
	p := newTrashPanel(t)
	if w := callAPI(t, p, "GET", "/panel/api/trash", "", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("trash no-key status = %d", w.Code)
	}
	if w := callAPI(t, p, "POST", "/panel/api/trash/workbuddy-u1.json/purge", "wrong", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("trash bad-key status = %d", w.Code)
	}
}

// 恢复：移回 auths/ 规范名并热加载进池。
func TestTrashRestore(t *testing.T) {
	p := newTrashPanel(t)
	a := addDiskAccount(t, p, "u2")
	w := callAPI(t, p, "POST", "/panel/api/accounts/u2/remove", "test-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("remove status = %d", w.Code)
	}
	w = callAPI(t, p, "POST", "/panel/api/trash/workbuddy-u2.json/restore", "test-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("restore status = %d body=%s", w.Code, w.Body.String())
	}
	// 回到 auths/ 且重新入池。
	if _, err := os.Stat(filepath.Join(p.cfg.AuthDir, "workbuddy-u2.json")); err != nil {
		t.Fatalf("restored file missing: %v", err)
	}
	if p.cfg.Pool.AuthByUID("u2") == nil {
		t.Fatal("restored account not in pool")
	}
	items, _ := p.trashItems()
	if len(items) != 0 {
		t.Fatalf("trash not empty after restore: %+v", items)
	}
	_ = a
}

// 同 uid 删除→重登→再删除：回收站同名冲突时追加时间戳，两个都在。
func TestTrashNameConflict(t *testing.T) {
	p := newTrashPanel(t)
	addDiskAccount(t, p, "u3")
	callAPI(t, p, "POST", "/panel/api/accounts/u3/remove", "test-key", "")
	addDiskAccount(t, p, "u3")
	callAPI(t, p, "POST", "/panel/api/accounts/u3/remove", "test-key", "")
	items, err := p.trashItems()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("trash items = %d, want 2（冲突应共存）", len(items))
	}
}

// 彻底删除 / 清空 / 非法名拒绝（路径穿越防护）。
func TestTrashPurgeAndNameValidation(t *testing.T) {
	p := newTrashPanel(t)
	addDiskAccount(t, p, "u4")
	callAPI(t, p, "POST", "/panel/api/accounts/u4/remove", "test-key", "")

	// 路径穿越 / 杂项名一律 400。
	for _, bad := range []string{"..%2F..%2Fsecret.json", "other.json", "workbuddy-u4.txt"} {
		if w := callAPI(t, p, "POST", "/panel/api/trash/"+bad+"/purge", "test-key", ""); w.Code != http.StatusBadRequest {
			t.Fatalf("purge %q status = %d, want 400", bad, w.Code)
		}
	}
	if w := callAPI(t, p, "POST", "/panel/api/trash/workbuddy-u4.json/purge", "test-key", ""); w.Code != http.StatusOK {
		t.Fatalf("purge status = %d", w.Code)
	}
	if items, _ := p.trashItems(); len(items) != 0 {
		t.Fatalf("trash not empty after purge")
	}
	// 清空：混合杂项文件只清 workbuddy-*.json。
	addDiskAccount(t, p, "u5")
	callAPI(t, p, "POST", "/panel/api/accounts/u5/remove", "test-key", "")
	if err := os.WriteFile(filepath.Join(p.cfg.TrashDir, "notes.txt"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if w := callAPI(t, p, "POST", "/panel/api/trash/purge_all", "test-key", ""); w.Code != http.StatusOK {
		t.Fatalf("purge_all status = %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(p.cfg.TrashDir, "notes.txt")); err != nil {
		t.Fatal("purge_all should not touch non-json files")
	}
}

// 过期清理：mtime 超过保留期的文件被删，新鲜文件保留。
func TestTrashSweepExpired(t *testing.T) {
	p := newTrashPanel(t)
	p.SetTrashRetention(30)
	addDiskAccount(t, p, "u6")
	callAPI(t, p, "POST", "/panel/api/accounts/u6/remove", "test-key", "")
	items, _ := p.trashItems()
	if len(items) != 1 {
		t.Fatal("setup failed: trash should have 1 item")
	}
	// 把 mtime 拨回 31 天前 → 过期。
	old := time.Now().Add(-31 * 24 * time.Hour)
	fp := filepath.Join(p.cfg.TrashDir, items[0].Name)
	if err := os.Chtimes(fp, old, old); err != nil {
		t.Fatal(err)
	}
	p.sweepTrash()
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatal("expired trash file should be swept")
	}
	// 永久保留（0）时不清理。
	addDiskAccount(t, p, "u7")
	callAPI(t, p, "POST", "/panel/api/accounts/u7/remove", "test-key", "")
	items, _ = p.trashItems()
	if err := os.Chtimes(filepath.Join(p.cfg.TrashDir, items[0].Name), old, old); err != nil {
		t.Fatal(err)
	}
	p.SetTrashRetention(0)
	p.sweepTrash()
	if items2, _ := p.trashItems(); len(items2) != 1 {
		t.Fatal("retention=0 (永久保留) should not sweep")
	}
}

// 备份：生成 zip（含 auths + state）、轮转 keep 份、文件名校验、列表。
func TestBackupRunRotateAndList(t *testing.T) {
	p := newTrashPanel(t)
	p.SetBackupSettings(true, 3, 2)
	addDiskAccount(t, p, "u8")
	// Add 路径（upsertLocked）不标记 dirty——pool 既有设计：首次状态变更才落盘。
	// 触发一次 credits 写入，让 Flush 有东西可落（全新部署备份只含 auth 文件是合法形态）。
	p.cfg.Pool.SetCreditsDetailed("u8", 100, 200, 0, time.Time{}, 0)

	n1, err := p.RunBackupNow()
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if !validBackupName(n1) {
		t.Fatalf("backup name %q invalid", n1)
	}
	// zip 内容：auth 文件 + data/state.json。
	zr := openTestZip(t, filepath.Join(p.cfg.BackupDir, n1))
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["workbuddy-u8.json"] || !names["data/state.json"] {
		t.Fatalf("zip entries = %v", names)
	}
	// keep=2 轮转：跑 3 次只留 2 份。
	if _, err := p.RunBackupNow(); err != nil {
		t.Fatal(err)
	}
	n3, err := p.RunBackupNow()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(p.cfg.BackupDir)
	if len(entries) != 2 {
		t.Fatalf("backups = %d, want 2（keep 轮转）", len(entries))
	}
	// 列表接口（新→旧，最新的是 n3）。
	w := callAPI(t, p, "GET", "/panel/api/backup/list", "test-key", "")
	var listResp struct {
		Items []struct{ Name string } `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil || len(listResp.Items) != 2 || listResp.Items[0].Name != n3 {
		t.Fatalf("backup list = %s", w.Body.String())
	}
	// 下载接口 + 穿越名拒绝：mux 层先做 URL 规范化（.. → 307 重定向到 404），
	// 即便绕到 handler 也有 validBackupName 兜底 400——两种防线都算拒绝。
	if w := callAPI(t, p, "GET", "/panel/api/backup/"+n3+"/download", "test-key", ""); w.Code != http.StatusOK {
		t.Fatalf("download status = %d", w.Code)
	}
	if w := callAPI(t, p, "GET", "/panel/api/backup/../state.json/download", "test-key", ""); w.Code == http.StatusOK {
		t.Fatalf("download traversal must not return file content, got 200")
	}
}

// overview 现算：credit_floor 之下的账号进 low_credit；标签透出 tags。
func TestOverviewLowCreditAndTags(t *testing.T) {
	p := newTrashPanel(t)
	a := addDiskAccount(t, p, "u9")
	a.SetTags([]string{"主力"})
	p.cfg.Pool.SetCreditFloor(100)
	// u9 credits=50/total=200（低于 floor）；u10 从未查询（0/0，不参与预警）。
	p.cfg.Pool.SetCreditsDetailed("u9", 50, 200, 0, time.Time{}, 0)
	addDiskAccount(t, p, "u10")

	w := callAPI(t, p, "GET", "/panel/api/overview", "test-key", "")
	if w.Code != http.StatusOK {
		t.Fatalf("overview status = %d", w.Code)
	}
	var d struct {
		CreditFloor int64             `json:"credit_floor"`
		LowCredit   []map[string]any  `json:"low_credit"`
		Tags        map[string][]string `json:"tags"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("overview json: %v", err)
	}
	if d.CreditFloor != 100 {
		t.Fatalf("credit_floor = %d", d.CreditFloor)
	}
	if len(d.LowCredit) != 1 || d.LowCredit[0]["uid"] != "u9" {
		t.Fatalf("low_credit = %v, want only u9", d.LowCredit)
	}
	if len(d.Tags["u9"]) != 1 || d.Tags["u9"][0] != "主力" {
		t.Fatalf("tags = %v", d.Tags)
	}
	if _, ok := d.Tags["u10"]; ok {
		t.Fatalf("u10 has no tags, should be absent")
	}
}

// 标签 API：设置 / 落盘 / overview 透出联动。
func TestAccountTagsAPI(t *testing.T) {
	p := newTrashPanel(t)
	addDiskAccount(t, p, "u11")

	w := callAPI(t, p, "POST", "/panel/api/accounts/u11/tags", "test-key", `{"tags":["签到号"," 试用 "],"x":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("tags status = %d body=%s", w.Code, w.Body.String())
	}
	// 落盘验证：重读文件。
	a := p.cfg.Pool.AuthByUID("u11")
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	re, err := auth.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := re.TagsValue()
	if len(got) != 2 || got[0] != "签到号" || got[1] != "试用" {
		t.Fatalf("persisted tags = %v", got)
	}
	// 清空标签。
	if w := callAPI(t, p, "POST", "/panel/api/accounts/u11/tags", "test-key", `{"tags":[]}`); w.Code != http.StatusOK {
		t.Fatalf("clear tags status = %d", w.Code)
	}
	if re2 := p.cfg.Pool.AuthByUID("u11"); len(re2.TagsValue()) != 0 {
		t.Fatalf("tags not cleared: %v", re2.TagsValue())
	}
	// 不存在的账号 404。
	if w := callAPI(t, p, "POST", "/panel/api/accounts/nope/tags", "test-key", `{"tags":[]}`); w.Code != http.StatusNotFound {
		t.Fatalf("missing account status = %d", w.Code)
	}
}

// 未配置 TrashDir/BackupDir 的面板（旧装配/裸测试）：remove 回退硬删、接口 501。
func TestLegacyBehaviorWithoutTrashAndBackup(t *testing.T) {
	p := New(Config{
		Version: "test", APIKey: "test-key",
		Pool:    pool.New(filepath.Join(t.TempDir(), "state.json")),
		AuthDir: t.TempDir(),
	})
	a := addDiskAccount(t, p, "u12")
	if w := callAPI(t, p, "POST", "/panel/api/accounts/u12/remove", "test-key", ""); w.Code != http.StatusOK {
		t.Fatalf("remove status = %d", w.Code)
	}
	if _, err := os.Stat(a.FilePath); !os.IsNotExist(err) {
		t.Fatal("legacy remove should hard-delete auth file")
	}
	for _, c := range []struct{ m, p string }{
		{"GET", "/panel/api/trash"},
		{"POST", "/panel/api/backup/run"},
		{"GET", "/panel/api/backup/list"},
	} {
		if w := callAPI(t, p, c.m, c.p, "test-key", ""); w.Code != http.StatusNotImplemented {
			t.Fatalf("%s %s status = %d, want 501", c.m, c.p, w.Code)
		}
	}
}
