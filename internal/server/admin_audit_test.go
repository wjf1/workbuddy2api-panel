package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 管理操作审计（admin_audit.go）的行为钉子。
//
// 审计的价值在于「事后能追责」，所以三件事必须对：
//  1. 记全（动作 / 目标 / 状态码 / 来源 / key 指纹 / reason）；
//  2. 不泄漏（绝不落 key 原文）；
//  3. 不添乱（写失败不让业务请求失败；未启用时零开销零行为变更）。

// TestAuditRecordsEntry 正常路径：一次写操作落一行，字段齐全。
func TestAuditRecordsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	al, err := NewAuditLog(path, "secret-key")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Audit: al, Tasks: &fakeTaskRunner{}})

	wrapped := h.audit("task.run", auditPathValue("name"), h.adminTaskRun)
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
	req.SetPathValue("name", "checkin")
	req.RemoteAddr = "192.0.2.7:1234"
	rec := httptest.NewRecorder()
	wrapped(rec, req)

	line := readLastLine(t, path)
	var e auditEntry
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("审计行不是合法 JSON: %v (line=%s)", err, line)
	}
	if e.Action != "task.run" {
		t.Errorf("action = %q want task.run", e.Action)
	}
	if e.Target != "checkin" {
		t.Errorf("target = %q want checkin", e.Target)
	}
	if e.Status != http.StatusAccepted {
		t.Errorf("status = %d want 202", e.Status)
	}
	if e.Remote != "192.0.2.7:1234" {
		t.Errorf("remote = %q", e.Remote)
	}
	if e.TS == "" {
		t.Error("ts 不应为空")
	}
}

// TestAuditNeverStoresKeyPlaintext 审计绝不落 key 原文（只落指纹）。
//
// 这是硬要求：审计文件常被收集/备份，落明文 key 等于把凭据泄漏到日志里。
func TestAuditNeverStoresKeyPlaintext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	const secret = "super-secret-api-key-12345"
	al, err := NewAuditLog(path, secret)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Audit: al, Tasks: &fakeTaskRunner{}})
	wrapped := h.audit("task.run", auditPathValue("name"), h.adminTaskRun)
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
	req.SetPathValue("name", "checkin")
	wrapped(httptest.NewRecorder(), req)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("审计文件出现了 api_key 明文——严重泄漏")
	}
	// 指纹应为 8 位 hex
	var e auditEntry
	if err := json.Unmarshal([]byte(readLastLine(t, path)), &e); err != nil {
		t.Fatal(err)
	}
	if len(e.Key) != 8 {
		t.Fatalf("key 指纹应为 8 位 hex, got %q", e.Key)
	}
}

// TestAuditDisabledIsPassthrough Audit 为 nil 时完全旁路（零行为变更）。
func TestAuditDisabledIsPassthrough(t *testing.T) {
	h := NewHandler(Config{}) // Audit nil
	// 构造一个只回 418 的 handler，审计包装后状态码必须原样透出
	called := false
	inner := func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	}
	wrapped := h.audit("x", auditConstant("y"), inner)
	rec := httptest.NewRecorder()
	wrapped(rec, httptest.NewRequest(http.MethodPost, "/x", nil))

	if !called {
		t.Fatal("应直接调用下游")
	}
	if rec.Code != http.StatusTeapot {
		t.Fatalf("状态码应原样透出, got %d", rec.Code)
	}
}

// TestAuditWriteFailureDoesNotFailRequest 审计写失败不影响业务请求。
//
// 路径指向一个不存在的目录深处 + 只读场景难以稳定构造，这里用「打开后删掉
// 目录权限」的等价做法：直接把 path 指到一个目录上（OpenFile 必失败）。
func TestAuditWriteFailureDoesNotFailRequest(t *testing.T) {
	dir := t.TempDir()
	// NewAuditLog 会成功（路径可创建），之后把它换成目录让写入失败
	path := filepath.Join(dir, "audit.jsonl")
	al, err := NewAuditLog(path, "k")
	if err != nil {
		t.Fatal(err)
	}
	// 删文件、在同一路径建目录 → 后续 OpenFile 追加写必失败
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(Config{Audit: al, Tasks: &fakeTaskRunner{}})
	wrapped := h.audit("task.run", auditPathValue("name"), h.adminTaskRun)
	req := httptest.NewRequest(http.MethodPost, "/admin/tasks/checkin/run", nil)
	req.SetPathValue("name", "checkin")
	rec := httptest.NewRecorder()
	wrapped(rec, req)

	// 关键：业务照常成功（审计是旁路）
	if rec.Code != http.StatusAccepted {
		t.Fatalf("审计写失败不应影响业务状态码, got %d", rec.Code)
	}
}

// TestAuditReasonCaptured 请求体里的 reason 被记入审计（运维备注），
// 且 body 被正确恢复给下游（否则下游 handler 读不到 body）。
func TestAuditReasonCaptured(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	al, err := NewAuditLog(path, "k")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{Audit: al})

	var seenBody string
	inner := func(w http.ResponseWriter, r *http.Request) {
		b, _ := readAllString(r)
		seenBody = b
		w.WriteHeader(http.StatusOK)
	}
	wrapped := h.audit("config.save", auditConstant("config"), inner)
	// reason 字段 + 一段其它内容：下游必须能看到**完整** body
	body := `{"reason":"临时补跑","other":"must-survive"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/config", strings.NewReader(body))
	wrapped(httptest.NewRecorder(), req)

	if seenBody != body {
		t.Fatalf("下游应看到完整原 body（peek 后必须恢复）\n got: %q\nwant: %q", seenBody, body)
	}
	var e auditEntry
	if err := json.Unmarshal([]byte(readLastLine(t, path)), &e); err != nil {
		t.Fatal(err)
	}
	if e.Reason != "临时补跑" {
		t.Fatalf("reason 未记录, got %q", e.Reason)
	}
}

// TestNewAuditLogEmptyPathRejected 空路径直接报错（调用方据此跳过启用）。
func TestNewAuditLogEmptyPathRejected(t *testing.T) {
	if _, err := NewAuditLog("", "k"); err == nil {
		t.Fatal("空路径应报错")
	}
	if _, err := NewAuditLog("   ", "k"); err == nil {
		t.Fatal("空白路径应报错")
	}
}

// readLastLine 读审计文件最后一行非空内容。
func readLastLine(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读审计文件失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	return lines[len(lines)-1]
}

// readAllString 读尽 body（测试辅助）。
func readAllString(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", nil
	}
	var sb strings.Builder
	buf := make([]byte, 512)
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return sb.String(), nil
}

// 避免 time import 未使用（审计行用 RFC3339，本文件仅断言非空）。
var _ = time.RFC3339
