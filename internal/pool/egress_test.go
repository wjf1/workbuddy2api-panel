package pool

import (
	"testing"
	"time"
)

// 出口级瞬时失败计数（egress.go）的行为钉子。
//
// 核心语义：传输层失败（DNS/拨号/TLS）不是账号的问题，是共享出口的问题。
// 记账对象错了，一次链路抖动会顺着轮转把整池号逐个拖进连败降权
// （hub issue #271 实测：4 小时 196 次 URLError 降权 13 个号，链路只断几秒）。

func TestEgressStreakAccumulatesAndResets(t *testing.T) {
	p := New("")
	now := time.Now()

	// 连续记 3 次：streak 1→2→3
	for i := 1; i <= 3; i++ {
		if got := p.NoteEgressFailure("direct:cn", now); got != i {
			t.Fatalf("第 %d 次应返回 %d, got %d", i, i, got)
		}
	}
	if got := p.EgressStreak("direct:cn", now); got != 3 {
		t.Fatalf("EgressStreak 应为 3, got %d", got)
	}

	// 超过窗口（60s）后再记：视为新一轮，streak 重置为 1 而不是 4
	if got := p.NoteEgressFailure("direct:cn", now.Add(egressWindow+time.Second)); got != 1 {
		t.Fatalf("窗口外应重置为 1, got %d", got)
	}
}

// TestEgressStreakExpiredReadsZero 窗口外查询报 0（不残留旧阶梯）。
func TestEgressStreakExpiredReadsZero(t *testing.T) {
	p := New("")
	now := time.Now()
	p.NoteEgressFailure("direct:cn", now)
	p.NoteEgressFailure("direct:cn", now)

	if got := p.EgressStreak("direct:cn", now.Add(egressWindow+time.Second)); got != 0 {
		t.Fatalf("窗口外查询应为 0, got %d", got)
	}
}

// TestEgressKeysIndependent 两域独立：cn 的抖动不压 global 的恢复。
//
// 共用一条阶梯会让一个域的抖动吃掉另一个域刚恢复的窗口（两域是不同上游域名
// 与网络环境）。
func TestEgressKeysIndependent(t *testing.T) {
	p := New("")
	now := time.Now()
	p.NoteEgressFailure("direct:cn", now)
	p.NoteEgressFailure("direct:cn", now)

	if got := p.EgressStreak("direct:global", now); got != 0 {
		t.Fatalf("global 未记过失败应为 0, got %d", got)
	}
	if got := p.NoteEgressFailure("direct:global", now); got != 1 {
		t.Fatalf("global 首次应为 1, got %d", got)
	}
}

// TestEgressEmptyKeyIgnored 空 key 不写表：调用方拿不到出口身份时不能悄悄记进
// 共享桶——那等于把「张冠李戴」换个地方重演。
func TestEgressEmptyKeyIgnored(t *testing.T) {
	p := New("")
	now := time.Now()
	if got := p.NoteEgressFailure("", now); got != 0 {
		t.Fatalf("空 key 应返回 0, got %d", got)
	}
	if got := p.EgressStreak("", now); got != 0 {
		t.Fatalf("空 key 查询应返回 0, got %d", got)
	}
}

// TestEgressKeyCap 表有硬上限：键数失控时清空重来（防御，正常到不了）。
func TestEgressKeyCap(t *testing.T) {
	p := New("")
	now := time.Now()
	// 写满 maxEgressKeys + 5 个不同键
	for i := 0; i < maxEgressKeys+5; i++ {
		p.NoteEgressFailure(string(rune('a'+i%26))+"-"+string(rune('a'+i/26)), now)
	}
	// 表不应无限增长（超限后清空只保留当前键）
	p.mu.Lock()
	n := len(p.egress)
	p.mu.Unlock()
	if n > maxEgressKeys {
		t.Fatalf("计数表超过硬上限: %d > %d", n, maxEgressKeys)
	}
}
