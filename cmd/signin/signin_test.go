package main

import (
	"testing"
	"unicode/utf8"
)

// TestIsAlready 已签判定：只有这几种上游回执算幂等成功，其余一律进 fail 列。
func TestIsAlready(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"已签到，明天再来", true},
		{"already checked in", true},
		{"ALREADY", true},
		{"code=400 参数错误", true},
		{"code=400 今日已签到", true},
		{"http 500 internal error", false},
		{"context deadline exceeded", false},
		{"refresh: session dead", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.msg, func(t *testing.T) {
			if got := isAlready(c.msg); got != c.want {
				t.Fatalf("isAlready(%q) = %v，want %v", c.msg, got, c.want)
			}
		})
	}
}

// TestShort 详情列单行化：换行会把表格行拆断，过长会挤爆终端宽度。
func TestShort(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "短串原样", in: "ok", want: "ok"},
		{name: "换行替换为空格", in: "line1\nline2", want: "line1 line2"},
		{name: "多换行全部替换", in: "a\nb\nc", want: "a b c"},
		{name: "恰好 60 字节不截断", in: repeat("x", 60), want: repeat("x", 60)},
		{name: "超过 60 字节截断", in: repeat("x", 61), want: repeat("x", 60)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := short(c.in); got != c.want {
				t.Fatalf("short(%q) = %q，want %q", c.in, got, c.want)
			}
		})
	}
}

// TestTruncASCII 表格列宽截断（ASCII 部分；中文昵称见 TestTruncKeepsValidUTF8）。
func TestTruncASCII(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{in: "abcdef", n: 3, want: "abc"},
		{in: "abc", n: 5, want: "abc"},
		{in: "", n: 5, want: ""},
		{in: "0123456789", n: 10, want: "0123456789"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			if got := trunc(c.in, c.n); got != c.want {
				t.Fatalf("trunc(%q,%d) = %q，want %q", c.in, c.n, got, c.want)
			}
		})
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// TestTruncKeepsValidUTF8 中文昵称被截断后必须仍是合法 UTF-8。
//
// 回归场景：s[:n] 按字节硬切，"张三李四" 取 11 字节会剩下半个「四」，
// 打印出非法字节序列（终端显示 �）。
func TestTruncKeepsValidUTF8(t *testing.T) {
	const nick = "张三李四" // 12 字节，每字 3 字节
	if got := trunc(nick, 11); got != "张三李" {
		t.Fatalf("trunc(%q,11) = %q，want %q（应回退到最近的字符边界）", nick, got, "张三李")
	}
	// 边界比首个字符还小：宁可输出空，也不输出半个字符。
	if got := trunc("张", 1); got != "" {
		t.Fatalf("trunc(%q,1) = %q，want 空串", "张", got)
	}
	// 扫一遍所有切点，任何位置都不许出现非法 UTF-8。
	for n := 0; n <= len(nick)+2; n++ {
		out := trunc(nick, n)
		if !utf8.ValidString(out) {
			t.Fatalf("trunc(%q,%d) = %q，含非法 UTF-8", nick, n, out)
		}
		if len(out) > n {
			t.Fatalf("trunc(%q,%d) 长度 %d 超过上限", nick, n, len(out))
		}
	}
}
