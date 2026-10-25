package server

import (
	"net/http"
	"testing"
	"time"
)

// TestRequestEventCarriesUpstreamRequestID 台账那一行必须同时带上本地 rid 与上游
// requestId：用户报障时给的是上游 ID，运维手里的是 rid，缺一个就对不上号。
// 这条断言钉住 chatStat → reqlog.Event 的字段搬运，防止以后加完字段却忘了赋值。
func TestRequestEventCarriesUpstreamRequestID(t *testing.T) {
	tr := &requestTrace{
		id:    "rid-local-1",
		start: time.Now(),
		stat:  &chatStat{upstreamRequestID: "up-remote-9"},
	}
	e := tr.event(http.StatusTooManyRequests)

	if e.RequestID != "rid-local-1" {
		t.Fatalf("本地 rid 丢失：%q", e.RequestID)
	}
	if e.UpstreamRequestID != "up-remote-9" {
		t.Fatalf("上游 requestId 未写进台账：%q", e.UpstreamRequestID)
	}
	if e.Status != http.StatusTooManyRequests {
		t.Fatalf("状态码丢失：%d", e.Status)
	}
}

// TestRequestEventOmitsUpstreamRequestIDWhenAbsent 上游没给时保持空串（omitempty
// 才能生效），不要填 "-" 之类的占位——那会让"上游没给"和"上游给了个破值"混为一谈。
func TestRequestEventOmitsUpstreamRequestIDWhenAbsent(t *testing.T) {
	tr := &requestTrace{id: "rid-2", start: time.Now(), stat: &chatStat{}}
	if got := tr.event(http.StatusOK).UpstreamRequestID; got != "" {
		t.Fatalf("上游未提供时应为空串，got %q", got)
	}
}
