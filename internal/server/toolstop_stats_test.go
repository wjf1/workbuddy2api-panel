// toolstop_stats_test.go 钉住 issue #44 的观测链路：从 SSE 逐帧抓出
// 「本流是否出现过非空 tool_calls」与「最后一个非空 finish_reason」，再与请求侧
// 的「声明了几个 tools」合成「静默回合」判据。
//
// 判据必须看**数组长度**而不是字段存在：上游每帧都带 "tool_calls":[]，
// 按字段存在判定会把零调用的静默回合误判成「有工具调用」，WARN 永远不触发。
package server

import (
	"strings"
	"testing"
	"time"
)

func TestStatsReaderSeesNoToolCallsOnStop(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s,
		`data: {"choices":[{"delta":{"content":"。","tool_calls":[]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"","tool_calls":[]},"finish_reason":"stop"}]}`,
	)
	if s.SawToolCalls() {
		t.Fatal("空数组不应算作见过 tool_calls")
	}
	if got := s.FinishReason(); got != "stop" {
		t.Fatalf("finish=%q want stop", got)
	}
}

func TestStatsReaderSeesToolCalls(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":""},"finish_reason":"tool_calls"}]}`,
	)
	if !s.SawToolCalls() {
		t.Fatal("非空 tool_calls 应被记录")
	}
	if got := s.FinishReason(); got != "tool_calls" {
		t.Fatalf("finish=%q want tool_calls", got)
	}
}

// 三条判据的组合：只有「声明了 tools + 全流零调用 + finish_reason=stop」同时成立
// 才是静默回合。三个反向用例是这条 WARN 不误报的关键——任一条不成立就不能记，
// 否则正常回合（真的调了工具）会淹没排障现场，而「模型判断不需要工具、直接回答」
// 这类合法回合也会被当成异常噪音。
func TestSilentToolStopJudgement(t *testing.T) {
	const (
		// 上游真实形态：正文帧与收尾帧都带空的 tool_calls 数组。
		stopNoCalls = `data: {"choices":[{"delta":{"content":"答案","tool_calls":[]},"finish_reason":null}]}`
		stopFinal   = `data: {"choices":[{"delta":{"content":"","tool_calls":[]},"finish_reason":"stop"}]}`
		// 真的产出了工具调用。
		callFrame    = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":null}]}`
		toolCallsEnd = `data: {"choices":[{"delta":{"content":""},"finish_reason":"tool_calls"}]}`
		// 流中途断掉：只有正文帧，上游没给过 finish_reason。
		noFinish = `data: {"choices":[{"delta":{"content":"半句","tool_calls":[]},"finish_reason":null}]}`
	)

	cases := []struct {
		name        string
		declared    int // 请求体 tools 数组长度
		lines       []string
		wantSilent  bool
		wantWhyText string
	}{
		{
			name:        "声明tools+零调用+stop: 记",
			declared:    3,
			lines:       []string{stopNoCalls, stopFinal},
			wantSilent:  true,
			wantWhyText: "三条判据同时成立",
		},
		{
			name:        "反向①stop但tool_calls非空: 不记",
			declared:    3,
			lines:       []string{callFrame, stopFinal},
			wantSilent:  false,
			wantWhyText: "已产出调用条目，不是静默回合",
		},
		{
			name:        "反向②有tool_calls但未声明tools: 不记",
			declared:    0,
			lines:       []string{callFrame, toolCallsEnd},
			wantSilent:  false,
			wantWhyText: "请求没声明 tools，零调用不是异常",
		},
		{
			name:        "反向③声明tools+零调用+finish_reason=tool_calls: 不记",
			declared:    2,
			lines:       []string{stopNoCalls, toolCallsEnd},
			wantSilent:  false,
			wantWhyText: "finish_reason 不是 stop",
		},
		{
			name:        "反向④未声明tools+零调用+stop: 不记",
			declared:    0,
			lines:       []string{stopNoCalls, stopFinal},
			wantSilent:  false,
			wantWhyText: "纯问答请求，本来就不该有调用",
		},
		{
			name:        "反向⑤断流无finish_reason: 不记",
			declared:    2,
			lines:       []string{noFinish},
			wantSilent:  false,
			wantWhyText: "空 finish_reason 不构成 stop 收尾",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
			feedStatsLines(s, c.lines...)
			got := isSilentToolStop(c.declared, s.SawToolCalls(), s.FinishReason())
			if got != c.wantSilent {
				t.Fatalf("isSilentToolStop=%v want %v（%s）declared=%d sawToolCalls=%v finish=%q",
					got, c.wantSilent, c.wantWhyText, c.declared, s.SawToolCalls(), s.FinishReason())
			}
		})
	}
}

// 观测不得改变既有统计口径：只多了 tool_calls/finish_reason 两个字段的解析，
// usage 与 error 帧的采集结果必须与改造前一致（同帧同时带 choices 与 usage 时
// 两条链路都要走到——这正是把「Usage==nil 就 return」挪到 choices 循环之后的用意）。
func TestStatsReaderKeepsUsageAndErrorOnChoiceFrames(t *testing.T) {
	s := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s,
		`data: {"choices":[{"delta":{"content":"x","tool_calls":[]},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"credit":0.5}}`,
	)
	if got, ok := s.Tokens(); !ok || got != 2 {
		t.Fatalf("completion_tokens=%d ok=%v want 2/true", got, ok)
	}
	if got, ok := s.Credit(); !ok || got != 0.5 {
		t.Fatalf("credit=%v ok=%v want 0.5/true", got, ok)
	}
	if s.SawErrorFrame() {
		t.Fatal("正常帧不应标 errorFrame")
	}

	// error 帧（无 usage）仍要被识别：改造把 errorFrame 判定挪到 unmarshal 之后，
	// 不能因为「没有 usage」提前 return 而丢掉。
	s2 := newChatStatsReaderSince(strings.NewReader(""), time.Now())
	feedStatsLines(s2, `data: {"error":{"code":6004,"message":"rate limited"}}`)
	if !s2.SawErrorFrame() {
		t.Fatal("error 帧未被识别")
	}
	if _, ok := s2.Tokens(); ok {
		t.Fatal("error 帧不应产生 token 观测")
	}
}
