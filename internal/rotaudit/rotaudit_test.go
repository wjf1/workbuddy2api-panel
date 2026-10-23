package rotaudit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTest(t *testing.T) (*Recorder, string) {
	t.Helper()
	dir := t.TempDir()
	r := New(filepath.Join(dir, "audit"), "host-test", nil)
	r.nowF = func() time.Time { return time.Date(2026, 10, 24, 12, 0, 0, 0, time.Local) }
	return r, dir
}

// TestSeedThenNormalRotation 播种后正常轮换（old == 基准）不误报外部轮换。
func TestSeedThenNormalRotation(t *testing.T) {
	r, dir := newTest(t)
	r.Seed(map[string]string{"u1": "rt-original"})
	r.Observe("u1", Fingerprint("rt-original"), Fingerprint("rt-new"), "keepalive", nil)
	if got := r.SortedUIDs(); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("基准=%v want [u1]", got)
	}
	// 基准已更新为新 RT 指纹：再以新 RT 刷新同样不误报（通过不 panic/无回调验证，
	// onExternal 为 nil 时只落日志）。
	r.Observe("u1", Fingerprint("rt-new"), Fingerprint("rt-3"), "chat", nil)
	if _, err := os.Stat(filepath.Join(dir, "audit", "refresh-audit-20261024.jsonl")); err != nil {
		t.Fatalf("审计文件未落盘: %v", err)
	}
}

// TestExternalRotationDetected 旧指纹 ≠ 基准 → 外部轮换（回调 + 基准更新）。
func TestExternalRotationDetected(t *testing.T) {
	dir := t.TempDir()
	var fired []string
	r := New(filepath.Join(dir, "audit"), "host-test", func(uid, old8, cur8, source string) {
		fired = append(fired, uid+"|"+old8+"|"+cur8+"|"+source)
	})
	r.nowF = func() time.Time { return time.Now() }
	r.Seed(map[string]string{"u1": "rt-A"})
	// 外部执行体已把 RT 换成 rt-X（本进程不知道），本进程拿旧基准外的指纹来刷。
	r.Observe("u1", Fingerprint("rt-B"), Fingerprint("rt-C"), "keepalive", nil)
	if len(fired) != 1 {
		t.Fatalf("外部轮换应触发回调, fired=%v", fired)
	}
	want := "u1|" + Fingerprint("rt-B") + "|" + Fingerprint("rt-A") + "|keepalive"
	if fired[0] != want {
		t.Fatalf("回调参数=%s want %s", fired[0], want)
	}
	// 基准更新为本次新指纹，重复同源刷新不再触发。
	r.Observe("u1", Fingerprint("rt-C"), Fingerprint("rt-D"), "keepalive", nil)
	if len(fired) != 1 {
		t.Fatalf("基准更新后不应再触发, fired=%v", fired)
	}
}

// TestResetSkipsExternal 重登后 Reset 重置基准：新 RT 首刷不构成外部轮换。
func TestResetSkipsExternal(t *testing.T) {
	dir := t.TempDir()
	var fired []string
	r := New(filepath.Join(dir, "audit"), "host-test", func(uid, old8, cur8, source string) { fired = append(fired, uid) })
	r.nowF = func() time.Time { return time.Now() }
	r.Seed(map[string]string{"u1": "rt-dead"})
	r.Reset("u1", "rt-fresh") // 重登：新凭证链
	r.Observe("u1", Fingerprint("rt-fresh"), Fingerprint("rt-next"), "keepalive", nil)
	if len(fired) != 0 {
		t.Fatalf("Reset 后首刷不应触发外部轮换: %v", fired)
	}
}

// TestAuditRecordShape 审计 JSONL 只落指纹不落明文 RT，字段完整。
func TestAuditRecordShape(t *testing.T) {
	r, dir := newTest(t)
	secret := "rt-super-secret-value"
	r.Seed(map[string]string{"u1": secret})
	r.Observe("u1", Fingerprint(secret), Fingerprint("rt-new"), "chat", errors.New("upstream session_dead (http 401): boom"))
	data, err := os.ReadFile(filepath.Join(dir, "audit", "refresh-audit-20261024.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("应 1 条记录, got %d", len(lines))
	}
	if strings.Contains(string(data), secret) {
		t.Fatal("审计日志不得包含 RT 明文")
	}
	var rec Rec
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.UID != "u1" || rec.Source != "chat" || rec.Host != "host-test" || rec.PID != os.Getpid() {
		t.Fatalf("字段不符: %+v", rec)
	}
	if rec.ErrKind != "session_dead" {
		t.Fatalf("err_kind=%q want session_dead", rec.ErrKind)
	}
	if rec.OldRT8 != Fingerprint(secret) || rec.NewRT8 != Fingerprint("rt-new") {
		t.Fatalf("指纹不符: old=%s new=%s", rec.OldRT8, rec.NewRT8)
	}
}

// TestCleanupRemovesExpired 清理 7 天前的审计文件。
func TestCleanupRemovesExpired(t *testing.T) {
	dir := t.TempDir()
	auditDir := filepath.Join(dir, "audit")
	if err := os.MkdirAll(auditDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(auditDir, "refresh-audit-20260101.jsonl")
	fresh := filepath.Join(auditDir, "refresh-audit-20990101.jsonl")
	os.WriteFile(old, []byte("{}\n"), 0o644)
	os.WriteFile(fresh, []byte("{}\n"), 0o644)

	r := New(auditDir, "h", nil)
	r.nowF = func() time.Time { return time.Date(2026, 10, 24, 0, 0, 0, 0, time.Local) }
	r.cleanup()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("7 天前的审计文件应被清理")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("新审计文件不应被清理")
	}
}

// TestFingerprintStability 指纹稳定且非空；空 RT 指纹为空。
func TestFingerprintStability(t *testing.T) {
	a := Fingerprint("abc")
	if a == "" || len(a) != 8 {
		t.Fatalf("指纹长度=%d want 8", len(a))
	}
	if a != Fingerprint("abc") {
		t.Fatal("指纹应稳定")
	}
	if Fingerprint("") != "" {
		t.Fatal("空 RT 指纹应为空")
	}
	if a == Fingerprint("abcd") {
		t.Fatal("不同 RT 指纹应不同")
	}
}
