package server

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 会话失效处置策略的回归钉子：裸 401 连续计数、业务层 12153 单发禁用、
// 以及几个容易在重构中失守的边界（uerr==nil 的 SSE error frame 路径、
// 429+12153 混排、非 401 错误不得计入 session dead）。

const qa2Apisix401 = `<html><head><title>401 Authorization Required</title></head><body><center><h1>401 Authorization Required</h1></center><hr><center>openresty</center></body></html>`
const qa2Envelope12153 = `{"code":12153,"msg":"Offline user session not found"}`

func qa2Pool(uid string) *pool.Pool {
	p := pool.New("")
	p.Add(&auth.Auth{UID: uid, AccessToken: "at", ExpiresAt: 9999999999})
	return p
}

func qa2BareErr() *upstream.Error {
	return &upstream.Error{Kind: upstream.ErrSessionDead, Status: http.StatusUnauthorized, Msg: qa2Apisix401}
}

func qa2CaptureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	old := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(old) })
	return buf
}

// #1 裸 401 连续三次才禁用；计数 1/3 → 2/3 → 禁用；日志两段文案都要出现。
func TestSessionDeadPolicyBare401ThreeStrikes(t *testing.T) {
	logs := qa2CaptureLog(t)
	p := qa2Pool("q1")
	h := NewHandler(Config{Pool: p})

	for i := 1; i <= 2; i++ {
		h.applyErrorPolicy("q1", upstream.ErrSessionDead, qa2Apisix401, "", qa2BareErr())
		st, _ := p.Status("q1")
		if st.Disabled {
			t.Fatalf("第 %d 次就禁用（want 第 3 次）: %+v", i, st)
		}
		if st.SessionDeadFails != i {
			t.Fatalf("第 %d 次后计数=%d want %d", i, st.SessionDeadFails, i)
		}
	}
	h.applyErrorPolicy("q1", upstream.ErrSessionDead, qa2Apisix401, "", qa2BareErr())
	st, _ := p.Status("q1")
	if !st.Disabled {
		t.Fatalf("第 3 次裸 401 未禁用: %+v", st)
	}
	if !strings.Contains(st.DisabledReason, "12153 session dead") {
		t.Fatalf("reason=%q", st.DisabledReason)
	}
	out := logs.String()
	t.Logf("日志:\n%s", strings.TrimSpace(out))
	if !strings.Contains(out, "裸 401（session dead 计数 1/3）") {
		t.Error("缺少 `裸 401（session dead 计数 1/3）`")
	}
	if !strings.Contains(out, "裸 401（session dead 计数 2/3）") {
		t.Error("缺少 `裸 401（session dead 计数 2/3）`")
	}
	if !strings.Contains(out, "连续 3 次裸 401 — 禁用") {
		t.Error("缺少 `连续 3 次裸 401 — 禁用`")
	}
}

// #2 带信封 12153 单发即禁，且不得经过计数；日志不得出现「裸 401」计数行。
func TestSessionDeadPolicyEnvelope12153SingleStrike(t *testing.T) {
	logs := qa2CaptureLog(t)
	p := qa2Pool("q2")
	h := NewHandler(Config{Pool: p})

	h.applyErrorPolicy("q2", upstream.ErrSessionDead, qa2Envelope12153, "",
		&upstream.Error{Kind: upstream.ErrSessionDead, Status: http.StatusUnauthorized, Msg: qa2Envelope12153})

	st, _ := p.Status("q2")
	if !st.Disabled {
		t.Fatalf("带信封 12153 应一次禁用: %+v", st)
	}
	if st.SessionDeadFails != 0 {
		t.Fatalf("12153 不该动连续计数，got %d", st.SessionDeadFails)
	}
	out := logs.String()
	if strings.Contains(out, "裸 401（session dead 计数") || strings.Contains(out, "连续 3 次裸 401") {
		t.Fatalf("12153 路径出现了裸 401 计数日志:\n%s", out)
	}
}

// #3 403 + HTML 仍是 WAF：不进计数、不禁用、进软冷却。
func TestSessionDeadPolicy403HTMLStaysWaf(t *testing.T) {
	p := qa2Pool("q3")
	h := NewHandler(Config{Pool: p})
	body403 := `<html><head><title>403 Forbidden</title></head><body>openresty</body></html>`
	h.applyErrorPolicy("q3", upstream.ErrWafBlock, body403, "",
		&upstream.Error{Kind: upstream.ErrWafBlock, Status: http.StatusForbidden, Msg: body403})

	st, _ := p.Status("q3")
	if st.SessionDeadFails != 0 || st.Disabled {
		t.Fatalf("WAF 403 被计成 session dead: %+v", st)
	}
	if !st.Cooling {
		t.Fatalf("WAF 403 应进软冷却: %+v", st)
	}
	t.Logf("WAF 403 → cooling=%v", st.Cooling)
}

// #4 成功一次必须清零：1 → 0 → 1（不是 2）。这是「网关抖动攒满 3 次误杀」的护栏。
func TestSessionDeadPolicyCounterClearsOnSuccess(t *testing.T) {
	p := qa2Pool("q4")
	h := NewHandler(Config{Pool: p})

	h.applyErrorPolicy("q4", upstream.ErrSessionDead, qa2Apisix401, "", qa2BareErr())
	if st, _ := p.Status("q4"); st.SessionDeadFails != 1 {
		t.Fatalf("首次后计数=%d want 1", st.SessionDeadFails)
	}
	p.NoteSuccess("q4") // chat 成功路径（handler.go:1181/1191/1270）调的就是它
	if st, _ := p.Status("q4"); st.SessionDeadFails != 0 {
		t.Fatalf("成功后计数未清零: %d", st.SessionDeadFails)
	}
	h.applyErrorPolicy("q4", upstream.ErrSessionDead, qa2Apisix401, "", qa2BareErr())
	st, _ := p.Status("q4")
	if st.SessionDeadFails != 1 {
		t.Fatalf("成功后再裸 401 计数=%d want 1（不能累积成 2）", st.SessionDeadFails)
	}
	if st.Disabled {
		t.Fatalf("中间成功过不该禁用: %+v", st)
	}
}

// 反例 A：uerr == nil（SSE error frame 防御路径）必须保持单发 Disable。
// 这条不能降级成计数，否则帧内 12153 这种权威判据会被弱化。
func TestSessionDeadPolicyNilUerrKeepsSingleStrike(t *testing.T) {
	p := qa2Pool("q5")
	h := NewHandler(Config{Pool: p})
	h.applyErrorPolicy("q5", upstream.ErrSessionDead, qa2Apisix401, "", nil)
	if st, _ := p.Status("q5"); !st.Disabled {
		t.Fatalf("uerr=nil 应保持单发 Disable: %+v", st)
	}
}

// 反例 B：429 + 12153 混排（Classify 判 ErrSessionDead）必须是单发 Disable，
// 不能因为 body 无信封就降到 3 次计数——状态码不是 401。
func TestSessionDeadPolicyNon401EnvelopeBodyNotCounted(t *testing.T) {
	p := qa2Pool("q6")
	h := NewHandler(Config{Pool: p})
	h.applyErrorPolicy("q6", upstream.ErrSessionDead, `{"code":12153,"msg":"x"}`, "",
		&upstream.Error{Kind: upstream.ErrSessionDead, Status: http.StatusTooManyRequests, Msg: `{"code":12153,"msg":"x"}`})
	if st, _ := p.Status("q6"); !st.Disabled {
		t.Fatalf("429+12153 应单发禁用: %+v", st)
	}
}

// 反例 C：401 带未知业务码信封 → ErrClient 分支，绝不能碰 session dead 计数。
// （applyErrorPolicy 收到 ErrClient 时走 default，这里只确认不误入计数路径。）
func TestSessionDeadPolicyEnvelopedUnknownCodeNoCount(t *testing.T) {
	p := qa2Pool("q7")
	h := NewHandler(Config{Pool: p})
	body := `{"code":10001,"msg":"x"}`
	h.applyErrorPolicy("q7", upstream.ErrClient, body, "",
		&upstream.Error{Kind: upstream.ErrClient, Status: http.StatusUnauthorized, Msg: body})
	st, _ := p.Status("q7")
	if st.SessionDeadFails != 0 || st.Disabled {
		t.Fatalf("带信封的未知 401 被误罚: %+v", st)
	}
}
