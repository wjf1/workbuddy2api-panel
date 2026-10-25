package main

import (
	"errors"
	"testing"
	"unicode/utf8"
)

// TestClassifyTrial 结果归一：错误优先于「是否领到」。
//
// 这个函数原注释就写着「供 main 循环与测试直接断言」，但此前没有任何测试。
// 判错的后果实际存在：把失败当 ALREADY 会让 exit 统计失真（fail=0 却有账号没到账）。
func TestClassifyTrial(t *testing.T) {
	cases := []struct {
		name    string
		claimed bool
		err     error
		want    trialStatus
		detail  string
	}{
		{name: "领到", claimed: true, want: trialOK, detail: "trial granted"},
		{name: "幂等已领", claimed: false, want: trialAlready, detail: "already claimed (idempotent)"},
		{name: "有错误即失败", claimed: false, err: errors.New("code=400 已领取过"), want: trialFailed, detail: "code=400 已领取过"},
		{
			// 错误优先：claimed 与 err 同时为真时按失败处理，否则上游返回「成功但带 warn」会被算成 OK。
			name: "错误优先于 claimed", claimed: true, err: errors.New("boom"), want: trialFailed, detail: "boom",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, detail := classifyTrial(c.claimed, c.err)
			if got != c.want {
				t.Fatalf("classifyTrial 状态 = %q，want %q", got, c.want)
			}
			if detail != c.detail {
				t.Fatalf("classifyTrial 详情 = %q，want %q", detail, c.detail)
			}
		})
	}
}

// TestTrialStatusStrings 状态字面量是给人看的报表列，改名等于改输出契约。
func TestTrialStatusStrings(t *testing.T) {
	// 用切片而不是 map：map 的 key 天然去重，重复字面量会被静默吞掉，测不出来。
	cases := []struct {
		st   trialStatus
		want string
	}{
		{trialOK, "OK"},
		{trialAlready, "ALREADY"},
		{trialNotApp, "N/A"},
		{trialFailed, "FAIL"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if string(c.st) != c.want {
			t.Errorf("状态字面量 = %q，want %q", string(c.st), c.want)
		}
		// 四个状态必须互不相同，否则报表里两种结果会并成一列。
		if seen[string(c.st)] {
			t.Fatalf("状态字面量重复：%q", string(c.st))
		}
		seen[string(c.st)] = true
	}
}

// TestTruncKeepsValidUTF8 昵称列截断同理：中文不能切成半个字符。
func TestTruncKeepsValidUTF8(t *testing.T) {
	const nick = "张三李四" // 12 字节
	if got := trunc(nick, 11); got != "张三李" {
		t.Fatalf("trunc(%q,11) = %q，want %q", nick, got, "张三李")
	}
	if got := trunc("abcdef", 3); got != "abc" {
		t.Fatalf("ASCII 行为不变：trunc(%q,3) = %q，want %q", "abcdef", got, "abc")
	}
	for n := 0; n <= len(nick)+2; n++ {
		if out := trunc(nick, n); !utf8.ValidString(out) {
			t.Fatalf("trunc(%q,%d) = %q，含非法 UTF-8", nick, n, out)
		}
	}
}
