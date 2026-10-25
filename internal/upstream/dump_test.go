package upstream

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// DumpDir 为空时零改动：同一个 reader 原样返回，不建目录不写文件。
func TestDumpUpstreamDisabled(t *testing.T) {
	c := New()
	rc := io.NopCloser(strings.NewReader("data: x\n\n"))
	if got := c.dumpUpstream(rc, []byte(`{}`), nil); got != io.ReadCloser(rc) {
		t.Fatalf("DumpDir 为空时应原样返回 reader")
	}
}

// dumpFiles 返回目录下的 dump 文件列表（测试断言用）。
func dumpFiles(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	return files
}

// 打开后一次请求一个自包含文件：出站请求体 + 上游原始字节，且字节不被改写。
func TestDumpUpstreamWritesRaw(t *testing.T) {
	dir := t.TempDir()
	c := New()
	c.DumpDir = dir
	raw := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}\n\ndata: [DONE]\n\n"
	a := &auth.Auth{UID: "0011223344556677", Domain: "www.codebuddy.cn"}

	rc := c.dumpUpstream(io.NopCloser(strings.NewReader(raw)), []byte(`{"model":"m"}`), a)
	got, err := io.ReadAll(rc)
	if err != nil || string(got) != raw {
		t.Fatalf("透传字节被改写: %q err=%v", got, err)
	}
	rc.Close()

	files := dumpFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("期望 1 个 dump 文件，得到 %d", len(files))
	}
	b, _ := os.ReadFile(files[0])
	s := string(b)
	if !strings.Contains(s, `{"model":"m"}`) || !strings.HasSuffix(s, raw) {
		t.Fatalf("dump 文件应含请求体与原始响应，实际 %q", s)
	}
	if strings.Contains(filepath.Base(files[0]), "0011223344556677") {
		t.Fatalf("文件名不应含完整 UID: %s", files[0])
	}
}

// DumpRequestMax 覆盖默认 256 KB 请求体上限：片段按配置截断，响应段不受影响。
func TestDumpUpstreamRequestMaxOverride(t *testing.T) {
	dir := t.TempDir()
	c := New()
	c.DumpDir = dir
	c.DumpRequestMax = 16
	raw := "data: [DONE]\n\n"

	req := `{"model":"m","tools":[{"type":"function","function":{"name":"read_file"}}]}`
	rc := c.dumpUpstream(io.NopCloser(strings.NewReader(raw)), []byte(req), nil)
	io.Copy(io.Discard, rc)
	rc.Close()

	files := dumpFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("期望 1 个 dump 文件，得到 %d", len(files))
	}
	b, _ := os.ReadFile(files[0])
	s := string(b)
	if !strings.Contains(s, req[:16]) || strings.Contains(s, `"read_file"`) {
		t.Fatalf("请求段应截断到 16 字节，实际 %q", s)
	}
	if !strings.HasSuffix(s, raw) {
		t.Fatalf("响应段不应受请求上限影响，实际 %q", s)
	}
}

// TestDumpUpstreamRequestMaxCases 钉住「请求段上限」的边界语义（表驱动）：
// 0 = 默认 256 KB，负数同 0（都是「未配置」）；超长时恰好切在上限处。
func TestDumpUpstreamRequestMaxCases(t *testing.T) {
	cases := []struct {
		name     string
		maxBytes int
		bodyLen  int
		wantLen  int
	}{
		{name: "0回退默认上限", maxBytes: 0, bodyLen: dumpRequestMax + 100, wantLen: dumpRequestMax},
		{name: "负值回退默认上限", maxBytes: -1, bodyLen: dumpRequestMax + 100, wantLen: dumpRequestMax},
		{name: "自定义上限生效", maxBytes: 1024, bodyLen: 4096, wantLen: 1024},
		{name: "短请求体不补齐", maxBytes: 4096, bodyLen: 10, wantLen: 10},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c := New()
			c.DumpDir = dir
			c.DumpRequestMax = tc.maxBytes

			req := strings.Repeat("a", tc.bodyLen)
			rc := c.dumpUpstream(io.NopCloser(strings.NewReader("data: [DONE]\n\n")), []byte(req), nil)
			io.Copy(io.Discard, rc)
			rc.Close()

			b, err := os.ReadFile(dumpFiles(t, dir)[0])
			if err != nil {
				t.Fatalf("读取 dump 文件: %v", err)
			}
			s := string(b)
			const headMark, tailMark = "=== request ===\n", "\n=== response ===\n"
			i := strings.Index(s, headMark)
			j := strings.Index(s, tailMark)
			if i < 0 || j < 0 || j < i {
				t.Fatalf("dump 文件缺少分段标记: %q", s)
			}
			if got := j - (i + len(headMark)); got != tc.wantLen {
				t.Errorf("请求段长度 %d，期望 %d", got, tc.wantLen)
			}
			if !strings.HasSuffix(s, "data: [DONE]\n\n") {
				t.Errorf("响应段应原样保留，实际 %q", s)
			}
		})
	}
}

// TestDumpReaderQuotaStopsWritingNotStream 钉住「超限只停写、流照常推进」：
// 配额耗尽后文件不再增长，但 ReadAll 拿到的字节与底流完全一致。
// 直接构造 dumpReader 并给一个小额度，避免往磁盘写真的 8 MB；dumpLimit 的
// 取值另由 TestDumpLimitIs8MiB 钉住。
func TestDumpReaderQuotaStopsWritingNotStream(t *testing.T) {
	const quota = 8
	raw := strings.Repeat("0123456789", 10) // 100 字节，远超 quota

	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "quota.log"))
	if err != nil {
		t.Fatalf("建文件: %v", err)
	}
	d := &dumpReader{rc: io.NopCloser(strings.NewReader(raw)), f: f, left: quota}

	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("透传读取失败: %v", err)
	}
	if string(got) != raw {
		t.Fatalf("配额耗尽后流被截断: 拿到 %d 字节，期望 %d", len(got), len(raw))
	}
	if d.left > 0 {
		t.Errorf("配额应已耗尽，剩余 %d", d.left)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close 应返回底流结果: %v", err)
	}
	if err := d.Close(); err != nil { // 重复 Close 不应因文件已关而报错
		t.Errorf("重复 Close 应幂等: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "quota.log"))
	if err != nil {
		t.Fatalf("读回文件: %v", err)
	}
	if len(b) != quota || string(b) != raw[:quota] {
		t.Fatalf("文件应恰好写入前 %d 字节，实际 %d 字节: %q", quota, len(b), b)
	}
}

// TestDumpLimitIs8MiB 钉住单文件封顶常量：响应段预算的语义基线，改值需同步
// 提交信息与排障手册。
func TestDumpLimitIs8MiB(t *testing.T) {
	if dumpLimit != 8<<20 {
		t.Errorf("dumpLimit 应为 8 MiB，实际 %d", dumpLimit)
	}
	if dumpRequestMax != 256<<10 {
		t.Errorf("dumpRequestMax 默认应为 256 KiB，实际 %d", dumpRequestMax)
	}
}

// TestDumpUpstreamOpenFailureFallsBack 目录不可创建时只记 WARN：reader 原样返回，
// 请求照常可读——诊断开关不能变成故障点。
func TestDumpUpstreamOpenFailureFallsBack(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("准备占位文件: %v", err)
	}

	c := New()
	c.DumpDir = filepath.Join(blocker, "sub") // 父路径是普通文件 → MkdirAll 必失败
	raw := "data: [DONE]\n\n"
	rc := io.NopCloser(strings.NewReader(raw))

	got := c.dumpUpstream(rc, []byte(`{}`), nil)
	if got != io.ReadCloser(rc) {
		t.Fatalf("目录创建失败时应原样返回 reader")
	}
	b, err := io.ReadAll(got)
	if err != nil || string(b) != raw {
		t.Fatalf("回落后应能正常读到底流: %q err=%v", b, err)
	}
}

// ---------------------------------------------------------------------------
// 保留期清理：dump 里含完整提示词与上游响应，无限期留存是隐私与磁盘双重风险
// ---------------------------------------------------------------------------

// dumpFileName 造一个符合 dumpName 形状的文件名（15 字符时间戳前缀 + 标签）。
func dumpFileName(t time.Time) string {
	return t.Format("20060102-150405.000") + "-www.codebuddy.cn-00112233.log"
}

// TestCleanDumpDirRemovesExpiredOnly 只清理「带时间戳前缀且已过保留期」的文件：
// 窗口内的新文件、用户手放的对照样本（无时间戳前缀）都必须留着。
func TestCleanDumpDirRemovesExpiredOnly(t *testing.T) {
	dir := t.TempDir()
	expired := dumpFileName(time.Now().AddDate(0, 0, -(dumpRetentionDays + 1)))
	fresh := dumpFileName(time.Now())
	samples := []string{"notes.txt", "x.log", "对照样本-2026.log"} // 无 15 字符时间戳前缀
	for _, name := range append([]string{expired, fresh}, samples...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("准备 %s: %v", name, err)
		}
	}

	c := New()
	c.DumpDir = dir
	c.CleanDumpDir()

	if _, err := os.Stat(filepath.Join(dir, expired)); !os.IsNotExist(err) {
		t.Errorf("超过 %d 天的 dump 应被删除: %s", dumpRetentionDays, expired)
	}
	if _, err := os.Stat(filepath.Join(dir, fresh)); err != nil {
		t.Errorf("窗口内的 dump 不应被删除: %v", err)
	}
	for _, name := range samples {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("无时间戳前缀的文件不应被删除（可能是用户手放的样本）: %s", name)
		}
	}
}

// TestCleanDumpDirNoDir DumpDir 未配置 / 目录不存在时静默返回，不 panic、不建目录。
func TestCleanDumpDirNoDir(t *testing.T) {
	c := New()
	c.CleanDumpDir() // DumpDir 为空

	missing := filepath.Join(t.TempDir(), "nope")
	c.DumpDir = missing
	c.CleanDumpDir()
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("清理不应顺手建目录: %v", err)
	}
}

// TestStartDumpCleanerCleansOnStartAndStops 启动即清一次（同步完成），
// ctx 取消后后台协程退出。
func TestStartDumpCleanerCleansOnStartAndStops(t *testing.T) {
	dir := t.TempDir()
	expired := dumpFileName(time.Now().AddDate(0, 0, -(dumpRetentionDays + 1)))
	if err := os.WriteFile(filepath.Join(dir, expired), []byte("x"), 0o600); err != nil {
		t.Fatalf("准备过期文件: %v", err)
	}

	c := New()
	c.DumpDir = dir
	ctx, cancel := context.WithCancel(context.Background())
	c.StartDumpCleaner(ctx)
	// 启动清理是同步的（在 go func 之前调用），这里无需等待。
	if _, err := os.Stat(filepath.Join(dir, expired)); !os.IsNotExist(err) {
		t.Errorf("启动时应立即清理过期 dump")
	}
	cancel() // 后台每日协程应随之退出（无 ctx 泄漏）
}

// TestStartDumpCleanerDisabled 未配置 DumpDir 时既不清理也不起协程。
func TestStartDumpCleanerDisabled(t *testing.T) {
	c := New()
	c.StartDumpCleaner(context.Background()) // 不应 panic
}
