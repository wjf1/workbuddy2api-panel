package server

import (
	"net/http"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestHTTPStatusForKind 把「错误类别 → 对外状态码」这张契约表钉死。
//
// 这张表是客户端行为的依据（Claude Code / ZCode 按状态码决定重试、换模型还是放弃），
// 所以它变动必须是**有意识**的：改了实现不改这里，测试就红。
func TestHTTPStatusForKind(t *testing.T) {
	want := map[upstream.ErrKind]int{
		// 请求/模型问题 → 400（重试无用）
		upstream.ErrBadParams:         http.StatusBadRequest,
		upstream.ErrPromptTooLong:     http.StatusBadRequest,
		upstream.ErrImageInvalid:      http.StatusBadRequest,
		upstream.ErrModelParamInvalid: http.StatusBadRequest,
		upstream.ErrContentBlocked:    http.StatusBadRequest,
		upstream.ErrModelBlocked:      http.StatusBadRequest,

		// 限流/排队 → 429（等一会儿有意义）
		upstream.ErrSoftRate:      http.StatusTooManyRequests,
		upstream.ErrUpstreamQueue: http.StatusTooManyRequests,

		// 账号池/上游暂时不可用 → 503（换号或稍后重试有意义）
		upstream.ErrHardCredit:   http.StatusServiceUnavailable,
		upstream.ErrSessionDead:  http.StatusServiceUnavailable,
		upstream.ErrNotFound:     http.StatusServiceUnavailable,
		upstream.ErrServer:       http.StatusServiceUnavailable,
		upstream.ErrAccountFault: http.StatusServiceUnavailable,
		upstream.ErrWafBlock:     http.StatusServiceUnavailable,
		upstream.ErrClient:       http.StatusServiceUnavailable,
		upstream.ErrNone:         http.StatusServiceUnavailable,
	}

	for kind, expect := range want {
		if got := httpStatusForKind(kind); got != expect {
			t.Errorf("%v(%d): got %d, want %d", kind, int(kind), got, expect)
		}
	}

	// 完整性：枚举区间内每个值都必须在上表表态。新增 ErrKind 时这条会失败，
	// 逼着改动者想清楚新类别该回 400 还是 503，而不是默默落到默认分支。
	for k := upstream.ErrNone; int(k) <= int(upstream.ErrClient); k++ {
		if _, ok := want[k]; !ok {
			t.Errorf("ErrKind(%d) 未在映射表中表态：新增类别请同步 errmap.go 与这张表", int(k))
		}
	}
}

// TestHTTPStatusForKindGroups 守住分组语义：只允许出现 400 / 429 / 503 三种码。
// 出现别的码通常意味着有人把某个类别"顺手"改成了 401/500，而客户端对这两者的
// 处理（重新鉴权 / 视为服务崩溃）会让排查方向彻底跑偏。
func TestHTTPStatusForKindGroups(t *testing.T) {
	allowed := map[int]bool{
		http.StatusBadRequest:         true,
		http.StatusTooManyRequests:    true,
		http.StatusServiceUnavailable: true,
	}
	for k := upstream.ErrNone; int(k) <= int(upstream.ErrClient); k++ {
		got := httpStatusForKind(k)
		if !allowed[got] {
			t.Errorf("%v(%d) 映射到 %d，超出契约允许的 400/429/503", k, int(k), got)
		}
	}
}
