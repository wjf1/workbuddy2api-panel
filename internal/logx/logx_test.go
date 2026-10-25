package logx

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

// captureLog 把标准 logger 的输出截到 buffer（并固定 flags/prefix），
// 测试结束后还原——logx 是经 log.Print 输出的，必须从这一层验证形状。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldFlags, oldPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})
	return &buf
}

// tsPrefix 与 internal/panel/ring.go 的 tsPrefixRe 同形：ring 就是靠它剥时间戳的。
var tsPrefix = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `)

// body 去掉行首时间戳与行尾换行，得到「面板里看到的那一行」。
func body(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()
	line := strings.TrimSuffix(buf.String(), "\n")
	if !tsPrefix.MatchString(line) {
		t.Fatalf("行首必须是 log 包默认时间戳（面板 ring 靠它剥离）: %q", line)
	}
	return tsPrefix.ReplaceAllString(line, "")
}

// TestInfofKeepsLegacyShape Infof 必须与历史 log.Printf 逐字一致：
// 时间戳 + 原消息，不加级别前缀、不加结构化标记。
func TestInfofKeepsLegacyShape(t *testing.T) {
	buf := captureLog(t)
	Infof("[usage] 已恢复 %d 个用量桶（%s）", 3, "data/usage.json")

	want := "[usage] 已恢复 3 个用量桶（data/usage.json）"
	if got := body(t, buf); got != want {
		t.Fatalf("Infof 输出形状变了:\n got=%q\nwant=%q", got, want)
	}
}

// TestLevelPrefixConvention Warn/Error 沿用仓库既有前缀约定（WARN: / ERR:），
// 而不是 slog 的 level=WARN 键值对。
func TestLevelPrefixConvention(t *testing.T) {
	cases := []struct{ name, want string }{
		{name: "warn", want: "WARN: [usage] 序列化失败: boom"},
		{name: "error", want: "ERR: [usage] 原子替换失败: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)
			switch tc.name {
			case "warn":
				Warnf("[usage] 序列化失败: %v", "boom")
			case "error":
				Errorf("[usage] 原子替换失败: %v", "boom")
			}
			got := body(t, buf)
			if got != tc.want {
				t.Fatalf("got=%q want=%q", got, tc.want)
			}
			if strings.Contains(got, "level=") {
				t.Fatalf("不应出现 slog 的 level= 键值对（面板按前缀解析）: %q", got)
			}
		})
	}
}

// TestStructuredAttrs 结构化字段追加在行尾（k=v），不影响行首前缀分类。
func TestStructuredAttrs(t *testing.T) {
	buf := captureLog(t)
	Default().Slog().With("uid", "abcdef12").Warn("checkin 失败", "code", 500, "retry", true)

	got := body(t, buf)
	want := "WARN: checkin 失败 uid=abcdef12 code=500 retry=true"
	if got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
	// 面板的 task 频道靠行首前缀识别：级别前缀不能盖掉任务前缀。
	if !strings.HasPrefix(strings.TrimPrefix(got, "WARN: "), "checkin ") {
		t.Fatalf("消息原文必须仍能在级别前缀之后按原样识别: %q", got)
	}
}

// TestAttrQuoting 含空格/等号/空值的属性必须引起来，否则下游按空格切分会错位。
func TestAttrQuoting(t *testing.T) {
	buf := captureLog(t)
	Default().Slog().Info("m", "msg", "a b", "kv", "x=1", "empty", "")

	got := body(t, buf)
	want := `m msg="a b" kv="x=1" empty=""`
	if got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

// TestGroupPrefix WithGroup 的组名作为键前缀。
func TestGroupPrefix(t *testing.T) {
	buf := captureLog(t)
	Default().Slog().WithGroup("pool").Info("m", "uid", "u1")

	if got, want := body(t, buf), "m pool.uid=u1"; got != want {
		t.Fatalf("got=%q want=%q", got, want)
	}
}

// TestLevelFilter 低于下限的记录既不写盘也不输出（Debug 默认关闭）。
func TestLevelFilter(t *testing.T) {
	buf := captureLog(t)
	New(slog.LevelWarn).Infof("不应出现")
	Default().Slog().Debug("也不应出现")

	if s := buf.String(); s != "" {
		t.Fatalf("低于阈值的记录不应输出，实际 %q", s)
	}
}

// TestHandlerEnabled 直接钉住 Enabled 的边界：等于阈值放行，低一档丢弃。
func TestHandlerEnabled(t *testing.T) {
	h := NewHandler(slog.LevelWarn)
	if !h.Enabled(context.Background(), slog.LevelWarn) {
		t.Error("等于阈值应放行")
	}
	if !h.Enabled(context.Background(), slog.LevelError) {
		t.Error("高于阈值应放行")
	}
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("低于阈值应丢弃")
	}
}

// TestWithAttrsAndGroupImmutable WithAttrs/WithGroup 必须返回新 Handler，
// 不能改写原 Handler 的切片（否则子 logger 会污染父 logger）。
func TestWithAttrsAndGroupImmutable(t *testing.T) {
	base := NewHandler(slog.LevelInfo)
	child := base.WithAttrs([]slog.Attr{slog.String("a", "1")})
	child2 := child.WithGroup("g")

	if len(base.attrs) != 0 {
		t.Fatalf("父 Handler 的 attrs 被改写: %+v", base.attrs)
	}
	if base.group != "" {
		t.Fatalf("父 Handler 的 group 被改写: %q", base.group)
	}
	if len(child.(*Handler).attrs) != 1 || child2.(*Handler).group != "g" {
		t.Fatalf("子 Handler 未按预期派生: attrs=%d group=%q",
			len(child.(*Handler).attrs), child2.(*Handler).group)
	}
	// 空参数必须原样返回（slog 会频繁用 With() 无参调用）。
	if base.WithAttrs(nil) != slog.Handler(base) || base.WithGroup("") != slog.Handler(base) {
		t.Error("空 attrs / 空 group 应返回原 Handler")
	}
}
