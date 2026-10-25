package main

import "testing"

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
