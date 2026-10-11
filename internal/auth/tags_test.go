package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SetTags 归一化：trim、去空、去重、限量（16 个）、超长截断（32 rune）。
func TestNormalizeTags(t *testing.T) {
	got := NormalizeTags([]string{"  主力 ", "", "签到号", "主力", "  ", "试用"})
	want := []string{"主力", "签到号", "试用"}
	if len(got) != len(want) {
		t.Fatalf("NormalizeTags = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("NormalizeTags[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := NormalizeTags(nil); got != nil {
		t.Fatalf("NormalizeTags(nil) = %v, want nil", got)
	}
	// 超限：第 17 个被丢弃。
	many := make([]string, 20)
	for i := range many {
		many[i] = "tag" + string(rune('a'+i))
	}
	if got := NormalizeTags(many); len(got) != maxTags {
		t.Fatalf("NormalizeTags len = %d, want %d", len(got), maxTags)
	}
	// 单标签超长截断到 32 rune。
	long := make([]rune, 40)
	for i := range long {
		long[i] = '标'
	}
	got2 := NormalizeTags([]string{string(long)})
	if len(got2) != 1 || len([]rune(got2[0])) != maxTagRunes {
		t.Fatalf("long tag len = %d, want %d", len([]rune(got2[0])), maxTagRunes)
	}
}

// SaveAtomic → Parse 往返：tags 落盘 account.tags，重读无损；空 tags 不写键。
func TestTagsSaveParseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{
		UID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: 1893456000, FilePath: fp,
	}
	a.SetTags([]string{"主力", " 签到号 ", "主力"})
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	// 落盘形状：嵌套形 account.tags，重复值已归一化掉。
	raw, _ := os.ReadFile(fp)
	var probe struct {
		Account struct {
			Tags []string `json:"tags"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(probe.Account.Tags) != 2 || probe.Account.Tags[0] != "主力" || probe.Account.Tags[1] != "签到号" {
		t.Fatalf("account.tags = %v, want [主力 签到号]", probe.Account.Tags)
	}
	// 重读往返。
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := b.TagsValue()
	if len(got) != 2 || got[0] != "主力" || got[1] != "签到号" {
		t.Fatalf("round trip tags = %v", got)
	}
}

// 旧文件（无 tags 键）零回归：Parse 得空标签，再次 SaveAtomic 不引入 tags 键。
func TestTagsAbsentBackwardCompat(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u2.json")
	legacy := map[string]any{
		"auth":    map[string]any{"accessToken": "at", "refreshToken": "rt", "expiresAt": 1893456000},
		"account": map[string]any{"uid": "u2", "nickname": "n2"},
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := a.TagsValue(); len(got) != 0 {
		t.Fatalf("legacy tags = %v, want empty", got)
	}
	a.FilePath = fp
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic: %v", err)
	}
	raw2, _ := os.ReadFile(fp)
	if strings.Contains(string(raw2), "\"tags\"") {
		t.Fatalf("empty tags 不应写键，文件含 tags: %s", raw2)
	}
}

// 扁平形顶层 tags 也能解析（手写/旧工具兼容口径）。
func TestTagsFlatFormParse(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1893456000,"uid":"u3","nickname":"n3","tags":["试用号"]}`)
	a, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := a.TagsValue()
	if len(got) != 1 || got[0] != "试用号" {
		t.Fatalf("flat tags = %v, want [试用号]", got)
	}
}

// SetTags 变更检测：相同内容返回 false（不触发无谓落盘）。
func TestSetTagsChangeDetection(t *testing.T) {
	a := &Auth{UID: "u4"}
	if a.SetTags([]string{"a", "b"}) != true {
		t.Fatal("first SetTags should report changed")
	}
	if a.SetTags([]string{" a ", "b"}) != false {
		t.Fatal("normalized-equal SetTags should report unchanged")
	}
	if a.SetTags([]string{"a", "b", "c"}) != true {
		t.Fatal("appending tag should report changed")
	}
	if a.SetTags(nil) != true {
		t.Fatal("clearing tags should report changed")
	}
	if a.SetTags(nil) != false {
		t.Fatal("clearing empty tags should report unchanged")
	}
}
