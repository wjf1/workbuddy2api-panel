package authlease

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func withHost(t *testing.T, host string) {
	t.Helper()
	old := hostnameF
	hostnameF = func() string { return host }
	t.Cleanup(func() { hostnameF = old })
}

func readLease(t *testing.T, dir string) leaseFile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, leaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	var lf leaseFile
	if err := json.Unmarshal(data, &lf); err != nil {
		t.Fatal(err)
	}
	return lf
}

// TestAcquireFreshNoConflict 空目录获取成功，租约文件存在且心跳归本机。
func TestAcquireFreshNoConflict(t *testing.T) {
	withHost(t, "host-a")
	dir := t.TempDir()
	l, err := Acquire(dir, true)
	if err != nil || l == nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	lf := readLease(t, dir)
	if lf.Hostname != "host-a" || lf.PID != os.Getpid() {
		t.Fatalf("租约内容不符: %+v", lf)
	}
}

// TestAcquireStaleOverwrite 心跳过期（>90s）= 崩溃残留，直接接管不阻断。
func TestAcquireStaleOverwrite(t *testing.T) {
	withHost(t, "host-b")
	dir := t.TempDir()
	stale := leaseFile{
		Hostname:    "host-x",
		PID:         123,
		StartedAt:   time.Now().Add(-time.Hour).Format(time.RFC3339),
		HeartbeatAt: time.Now().Add(-5 * time.Minute).Format(time.RFC3339),
	}
	data, _ := json.Marshal(stale)
	os.WriteFile(filepath.Join(dir, leaseFileName), data, 0o644)

	l, err := Acquire(dir, true)
	if err != nil {
		t.Fatalf("stale 租约应被接管: %v", err)
	}
	defer l.Release()
	if got := readLease(t, dir); got.Hostname != "host-b" {
		t.Fatalf("接管后宿主=%s want host-b", got.Hostname)
	}
}

// TestAcquireSameHostWarnsNotBlocks 同机租约只接管不拒绝（保护计划任务重启路径）。
func TestAcquireSameHostWarnsNotBlocks(t *testing.T) {
	withHost(t, "host-a")
	dir := t.TempDir()
	same := leaseFile{Hostname: "host-a", PID: 999, HeartbeatAt: time.Now().Format(time.RFC3339)}
	data, _ := json.Marshal(same)
	os.WriteFile(filepath.Join(dir, leaseFileName), data, 0o644)

	l, err := Acquire(dir, true)
	if err != nil {
		t.Fatalf("同机租约不应拒绝: %v", err)
	}
	defer l.Release()
}

// TestAcquireForeignFresh 异机新鲜租约：strict 拒绝，非 strict 放行。
func TestAcquireForeignFresh(t *testing.T) {
	withHost(t, "host-a")
	dir := t.TempDir()
	foreign := leaseFile{Hostname: "host-zz", PID: 42, HeartbeatAt: time.Now().Format(time.RFC3339)}
	data, _ := json.Marshal(foreign)
	os.WriteFile(filepath.Join(dir, leaseFileName), data, 0o644)

	if l, err := Acquire(dir, true); err == nil || l != nil {
		t.Fatal("strict=true 异机新鲜租约应拒绝启动")
	}
	l, err := Acquire(dir, false)
	if err != nil || l == nil {
		t.Fatalf("strict=false 应放行: %v", err)
	}
	l.Release()
}

// TestReleaseRemovesFile Release 删除租约文件；幂等。
func TestReleaseRemovesFile(t *testing.T) {
	withHost(t, "host-a")
	dir := t.TempDir()
	l, _ := Acquire(dir, true)
	if _, err := os.Stat(filepath.Join(dir, leaseFileName)); err != nil {
		t.Fatalf("获取后租约应存在: %v", err)
	}
	l.Release()
	if _, err := os.Stat(filepath.Join(dir, leaseFileName)); !os.IsNotExist(err) {
		t.Fatal("Release 后租约应删除")
	}
	l.Release() // 幂等
}

// TestAcquireEmptyDirDisabled dir 为空 = 禁用租约（零回归）。
func TestAcquireEmptyDirDisabled(t *testing.T) {
	l, err := Acquire("", true)
	if err != nil || l != nil {
		t.Fatalf("空 dir 应返回 nil lease: l=%v err=%v", l, err)
	}
}
