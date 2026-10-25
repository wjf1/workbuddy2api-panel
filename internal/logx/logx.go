// Package logx 分级日志门面，**输出形状与历史 log.Printf 完全一致**。
//
// 为什么不是直接用 slog 的 TextHandler：面板的日志环（internal/panel/ring.go）按行首
// 前缀分频道、并用 tsPrefixRe 剥掉 "2026/10/25 23:00:13 " 这段 log 包时间戳；裸 slog
// 会输出 time=... level=... msg=...，时间戳认不出、task 频道全塌成 sys，面板日志视图
// 会静默丢掉筛选能力——这正是迁移最容易被忽略的回归点。
//
// 本包的 Handler 因此只做三件事：
//  1. 时间戳仍由 log 包默认 flags 打（ring 的剥离逻辑一行不用改）
//  2. 级别沿用仓库既有前缀约定（见 internal/logfmt 的说明）：Warn → "WARN: "、
//     Error → "ERR: "、Info/Debug 不加前缀（正常流转保持简洁）
//  3. 结构化属性按 " k=v" 追加在行尾；值含空格/等号/引号时用 Go 字符串字面量引起来，
//     避免 "msg=两段 值" 这种把属性劈成两半的输出
//
// 循序渐进的迁移方式：一个包一个包换，每换一个包跑一次面板日志视图回归。用 Infof/
// Warnf/Errorf 是「零文本变化」的机械替换；需要结构化字段时用 Default() 拿 *slog.Logger。
package logx

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"strconv"
	"strings"
)

// Handler 把 slog 记录渲染成 log 包一行的形状，再交给标准 logger 输出。
//
// 输出走 log.Print 而不是自己写 os.Stderr：main 用
// log.SetOutput(io.MultiWriter(os.Stderr, panel.Ring)) 把标准 logger 镜像进面板日志环，
// 只有经它输出的行才会出现在面板里。
type Handler struct {
	level slog.Level
	attrs []slog.Attr
	group string
}

// NewHandler 构造 Handler；低于 level 的记录被丢弃（Enabled 返回 false）。
func NewHandler(level slog.Level) *Handler { return &Handler{level: level} }

// Enabled 实现 slog.Handler。
func (h *Handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

// WithAttrs 实现 slog.Handler：属性对后续所有记录生效（含子 logger）。
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := *h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &next
}

// WithGroup 实现 slog.Handler：组名作为属性键前缀（"grp.key=value"）。
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	if next.group != "" {
		next.group += "."
	}
	next.group += name
	return &next
}

// Handle 实现 slog.Handler：拼一行 "级别前缀 + 消息 + 属性"，交给标准 logger 打时间戳。
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(levelPrefix(r.Level))
	b.WriteString(r.Message)
	for _, a := range h.attrs {
		appendAttr(&b, h.group, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		appendAttr(&b, h.group, a)
		return true
	})
	log.Print(b.String())
	return nil
}

// levelPrefix 返回历史前缀约定对应的级别标记。
func levelPrefix(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERR: "
	case l >= slog.LevelWarn:
		return "WARN: "
	default:
		return ""
	}
}

// appendAttr 追加 " k=v"。空 Attr（零值）跳过；group 非空时作为键前缀。
func appendAttr(b *strings.Builder, group string, a slog.Attr) {
	if a.Equal(slog.Attr{}) {
		return
	}
	key := a.Key
	if group != "" && key != "" {
		key = group + "." + key
	}
	writeAttr(b, key, a.Value.Resolve())
}

// writeAttr 递归渲染单个值：Group 展开成 "k.sub=v"，其余原样输出（必要时加引号）。
func writeAttr(b *strings.Builder, key string, v slog.Value) {
	if v.Kind() == slog.KindGroup {
		for _, sub := range v.Group() {
			k := key
			if sub.Key != "" {
				if k != "" {
					k += "."
				}
				k += sub.Key
			}
			writeAttr(b, k, sub.Value.Resolve())
		}
		return
	}
	s := v.String()
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteByte('=')
	if needsQuote(s) {
		b.WriteString(strconv.Quote(s))
		return
	}
	b.WriteString(s)
}

// needsQuote 判断属性值是否需要引号：空值与含分隔符的值必须引起来，
// 否则 "uid=" 或 "msg=a b" 会让下游按空格切分时错位。
func needsQuote(s string) bool {
	return s == "" || strings.ContainsAny(s, " \t\r\n\"=")
}

// Logger 一个已绑定输出形状的 logger；零值不可用，用 New 构造。
type Logger struct{ l *slog.Logger }

// New 返回 level 为下限的 Logger。
func New(level slog.Level) Logger { return Logger{l: slog.New(NewHandler(level))} }

// Default 返回 Info 级 Logger（最常用：历史 log.Printf 的等价物）。
func Default() Logger { return New(slog.LevelInfo) }

// Slog 返回底层 *slog.Logger（需要结构化字段、With 链式属性时用）。
func (l Logger) Slog() *slog.Logger { return l.l }

// Infof 正常流转。不加级别前缀（与历史 log.Printf 逐字一致）。
func (l Logger) Infof(format string, args ...any) {
	l.l.Info(fmt.Sprintf(format, args...))
}

// Warnf 可疑/降级。行首加 "WARN: "。
func (l Logger) Warnf(format string, args ...any) {
	l.l.Warn(fmt.Sprintf(format, args...))
}

// Errorf 失败。行首加 "ERR: "。
func (l Logger) Errorf(format string, args ...any) {
	l.l.Error(fmt.Sprintf(format, args...))
}

// 包级快捷方式：等价于 Default().*f，供「只改 import 与函数名」的机械迁移。

// Infof 见 Logger.Infof。
func Infof(format string, args ...any) { Default().Infof(format, args...) }

// Warnf 见 Logger.Warnf。
func Warnf(format string, args ...any) { Default().Warnf(format, args...) }

// Errorf 见 Logger.Errorf。
func Errorf(format string, args ...any) { Default().Errorf(format, args...) }
