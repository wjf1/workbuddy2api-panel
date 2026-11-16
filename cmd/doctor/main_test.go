package main

// doctor 自检的行为钉子：三个场景（全绿 / 有 error / 配置损坏）的等级判定与退出码。
//
// 全部走 -offline 语义（run(..., true)），不触网——CI 与离线环境必须能跑。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/safefile"
)

// writeConfig 造一份最小 config.json。
func writeConfig(t *testing.T, dir string, extra map[string]any) string {
	t.Helper()
	cfg := map[string]any{
		"listen":     "127.0.0.1:7863",
		"api_key":    "k",
		"auth_dir":   filepath.Join(dir, "auths"),
		"state_file": filepath.Join(dir, "data", "state.json"),
		"schedule": map[string]any{
			"checkin_enabled": true, "checkin_hours": []int{9, 21},
			"travel_enabled": true, "travel_hours": []int{9, 21},
			"activity_enabled": true, "activity_hours": []int{10},
			"keepalive_enabled": true, "keepalive_hours": []int{22},
			"blackcat_enabled": true, "blackcat_hours": []int{23},
			"growth_enabled": true, "growth_hours": []int{1},
		},
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, _ := json.Marshal(cfg)
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeAccount 造一个账号文件（扁平形，auth.Parse 支持）。
func writeAccount(t *testing.T, dir, uid string, expiresAt int64) {
	t.Helper()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"uid": uid, "nickname": uid,
		"accessToken": "at-x", "refreshToken": "rt-x",
		"expiresAt": expiresAt, "domain": "codebuddy.cn",
	})
	if err := os.WriteFile(filepath.Join(authDir, "workbuddy-"+uid+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDoctorCleanRunIsOK 配置正常 + 账号有效 → 无 error。
func TestDoctorCleanRunIsOK(t *testing.T) {
	dir := t.TempDir()
	writeAccount(t, dir, "u1", time.Now().Add(7*24*time.Hour).Unix())
	cfg := writeConfig(t, dir, nil)

	rep := run(cfg, true)
	if !rep.OK {
		t.Fatalf("应无 error，实际 checks=%+v", rep.Checks)
	}
	if rep.Summary["error"] != 0 {
		t.Errorf("error 计数应为 0，got %d", rep.Summary["error"])
	}
	// 关键检查项必须都在（缺项意味着接线漏了）。
	names := map[string]bool{}
	for _, c := range rep.Checks {
		names[c.Name] = true
	}
	for _, want := range []string{"config", "binding", "accounts", "token_expiry", "data_dir", "state_file", "runstamps", "schedule"} {
		if !names[want] {
			t.Errorf("缺少检查项 %q", want)
		}
	}
}

// TestDoctorExpiredTokenIsError 过期 token → error + OK=false（退出码 1）。
func TestDoctorExpiredTokenIsError(t *testing.T) {
	dir := t.TempDir()
	writeAccount(t, dir, "u1", time.Now().Add(-time.Hour).Unix())
	cfg := writeConfig(t, dir, nil)

	rep := run(cfg, true)
	if rep.OK {
		t.Fatal("过期 token 应判为 error")
	}
	var found bool
	for _, c := range rep.Checks {
		if c.Name == "token_expiry" && c.Level == levelErr {
			found = true
		}
	}
	if !found {
		t.Fatalf("token_expiry 应为 error 级，实际 checks=%+v", rep.Checks)
	}
}

// TestDoctorCorruptConfigIsFatal 配置无法解析 → 单个 error 项 + OK=false。
func TestDoctorCorruptConfigIsFatal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := run(p, true)
	if rep.OK {
		t.Fatal("配置损坏应判为 error")
	}
	if len(rep.Checks) != 1 || rep.Checks[0].Name != "config" {
		t.Fatalf("配置损坏时应只有一条 config error，实际 %+v", rep.Checks)
	}
}

// TestDoctorMissingConfigIsFatal 配置文件不存在 → error（不是 panic）。
func TestDoctorMissingConfigIsFatal(t *testing.T) {
	rep := run(filepath.Join(t.TempDir(), "nope.json"), true)
	if rep.OK || len(rep.Checks) == 0 || rep.Checks[0].Level != levelErr {
		t.Fatalf("缺失配置应为 error，实际 %+v", rep.Checks)
	}
}

// TestDoctorNonLoopbackWithoutKeyIsError 非回环 + 空 key → error（写保护会挡 405）。
func TestDoctorNonLoopbackWithoutKeyIsError(t *testing.T) {
	dir := t.TempDir()
	writeAccount(t, dir, "u1", time.Now().Add(7*24*time.Hour).Unix())
	cfg := writeConfig(t, dir, map[string]any{"listen": "0.0.0.0:7863", "api_key": ""})

	rep := run(cfg, true)
	var got *check
	for i := range rep.Checks {
		if rep.Checks[i].Name == "binding" {
			got = &rep.Checks[i]
		}
	}
	if got == nil || got.Level != levelErr {
		t.Fatalf("binding 应为 error，实际 %+v", rep.Checks)
	}
}

// TestDoctorCorruptRunstampsIsWarning 补跑标记损坏 → warning（不影响正确性）。
func TestDoctorCorruptRunstampsIsWarning(t *testing.T) {
	dir := t.TempDir()
	writeAccount(t, dir, "u1", time.Now().Add(7*24*time.Hour).Unix())
	cfg := writeConfig(t, dir, nil)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := safefile.Write(filepath.Join(dir, "data", "runstamps.json"), []byte("{bad"), 0o600); err != nil {
		t.Fatal(err)
	}

	rep := run(cfg, true)
	var got *check
	for i := range rep.Checks {
		if rep.Checks[i].Name == "runstamps" {
			got = &rep.Checks[i]
		}
	}
	if got == nil || got.Level != levelWarn {
		t.Fatalf("runstamps 损坏应为 warning，实际 %+v", rep.Checks)
	}
	if !rep.OK {
		t.Error("warning 不应让整体判为失败")
	}
}

// TestDoctorScheduleSummary 排程摘要包含六族且禁用态可读。
func TestDoctorScheduleSummary(t *testing.T) {
	dir := t.TempDir()
	writeAccount(t, dir, "u1", time.Now().Add(7*24*time.Hour).Unix())
	cfg := writeConfig(t, dir, map[string]any{
		"schedule": map[string]any{
			"checkin_enabled": true, "checkin_hours": []int{9},
			"travel_enabled": false, "activity_enabled": false,
			"keepalive_enabled": false, "blackcat_enabled": false, "growth_enabled": false,
		},
	})
	rep := run(cfg, true)
	var detail string
	for _, c := range rep.Checks {
		if c.Name == "schedule" {
			detail = c.Detail
		}
	}
	for _, want := range []string{"checkin=on", "travel=off", "growth=off"} {
		if !contains(detail, want) {
			t.Errorf("排程摘要缺 %q，实际 %q", want, detail)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
