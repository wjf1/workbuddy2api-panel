package panel

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
)

// TestLogxOutputReachesRingWithCorrectChannel 钉住 main 的装配契约：
//
//	cmd/server/main.go: log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
//
// 装配好之后，internal/logx 的输出必须 ①被 ring 收下 ②行首时间戳被剥离
// ③按任务前缀正确分频道（**带着 WARN: / ERR: 前缀也仍要落 task**）。
//
// 为什么值得一条测试：分频道、剥时间戳、级别前缀是三处独立逻辑，任何一处
// 单独改对了、合起来却错，面板的症状都是「日志视图少了行」——
// 不报错、不崩，只是运维找不到东西，回归时极难发现。
func TestLogxOutputReachesRingWithCorrectChannel(t *testing.T) {
	oldOut, oldFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	})

	ring := NewRing(16)
	log.SetOutput(io.MultiWriter(io.Discard, ring)) // 与 main 同构，只是把 stderr 换成 Discard
	log.SetFlags(log.LstdFlags)

	logx.Warnf("checkin %s: 签到失败: 429", "5c162cc9")
	logx.Infof("scheduler: 余额后台刷新每 10m0s")
	logx.Errorf("[pool] state.json 落盘失败: disk full")

	es := ring.Snapshot()
	if len(es) != 3 {
		t.Fatalf("ring 条目数=%d，期望 3（logx 输出没进环？）", len(es))
	}

	want := []struct {
		ch   string
		text string
	}{
		{ChTask, "WARN: checkin 5c162cc9: 签到失败: 429"}, // 带级别前缀仍要落 task
		{ChSys, "scheduler: 余额后台刷新每 10m0s"},           // Info 不加前缀
		{ChSys, "ERR: [pool] state.json 落盘失败: disk full"},
	}
	for i, w := range want {
		if es[i].Ch != w.ch {
			t.Errorf("[%d] channel=%q，期望 %q（文本 %q）", i, es[i].Ch, w.ch, es[i].Text)
		}
		if es[i].Text != w.text {
			t.Errorf("[%d] text=%q，期望 %q", i, es[i].Text, w.text)
		}
		if strings.Contains(es[i].Text, "level=") || strings.Contains(es[i].Text, "time=") {
			t.Errorf("[%d] 出现了 slog 的键值形状，面板按前缀解析会失效: %q", i, es[i].Text)
		}
	}
}
