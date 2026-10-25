// ratelimit.go 入站请求限流（令牌桶，按客户端主机维度）。
//
// 定位：`server.max_body_mb` 管"单个请求多大"，本文件管"单个客户端多快"——两者
// 配套才构成完整的入站防线。默认关闭（rps<=0 时 NewRateLimiter 返回 nil，ServeHTTP
// 里那条分支根本不会进），所以对本就正常的部署零行为变更。
//
// 为什么用 RemoteAddr 而不是 X-Forwarded-For：
//
//	XFF / X-Real-IP 由客户端任意构造。若按它限流，攻击者每个请求换一个伪造 IP 就能
//	拿到无限个独立桶，限流形同虚设。RemoteAddr 是 TCP 对端地址，客户端改不了。
//	代价：部署在反向代理后面时，所有请求的对端都是代理地址，桶退化为"全局 RPS 上限"。
//	这是**安全方向的退化**——限不住单机，但一定能兜住总量，而"跑飞的脚本打满上游"
//	恰恰是这里要防的主要事故。
//
// 为什么按 IP 而不是按 api_key：
//
//	本网关只有一个 api_key，按 key 限流等价于全局限流，起不到"隔离某台机器"的作用。
//	真要按 key 限，得先有"多 key + 多租户"，那是另一个量级的改动。
package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// idleBucketTTL 空闲桶回收阈值：超过这段时间没来请求的客户端直接丢掉桶，
	// 避免 map 随扫描流量无限增长。到期后新请求会重新拿到满桶（burst），
	// 对正常客户端无感。
	idleBucketTTL = 10 * time.Minute
	// sweepInterval 触发回收扫描的最小间隔，避免每个请求都遍历 map。
	sweepInterval = time.Minute
	// maxBuckets 硬上限：一旦并发客户端数异常膨胀（如被大量源 IP 冲击），
	// 宁可整体退化为"全局限流"也不让内存无界增长。
	maxBuckets = 20000
)

// RateLimiter 令牌桶限流器。零值不可用，用 NewRateLimiter 构造。
type RateLimiter struct {
	rps   float64 // 每秒补充的令牌数
	burst float64 // 桶容量（允许的瞬时突发）
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*tokenBucket
	lastSweep time.Time
}

type tokenBucket struct {
	tokens float64   // 当前可用令牌（可为小数，按时间连续补充）
	last   time.Time // 上次补充时刻
	seen   time.Time // 最近一次访问（回收依据）
}

// NewRateLimiter 构造限流器。rps<=0 返回 nil，表示不启用（调用方据此跳过检查）。
// burst<=0 时取 ceil(rps)（至少 1），即"允许一秒的量作为突发"。
func NewRateLimiter(rps float64, burst int) *RateLimiter {
	if rps <= 0 {
		return nil
	}
	b := float64(burst)
	if b <= 0 {
		b = float64(int(rps))
		if float64(b) < rps {
			b++ // ceil
		}
		if b < 1 {
			b = 1
		}
	}
	return &RateLimiter{
		rps:       rps,
		burst:     b,
		now:       time.Now,
		buckets:   make(map[string]*tokenBucket),
		lastSweep: time.Now(),
	}
}

// Allow 判断该客户端本次请求是否放行。不放行时返回建议的重试等待时长
// （用于 Retry-After），恒 > 0。
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	if l == nil { // 允许在调用点直接 l.Allow，不必先判空
		return true, 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) >= sweepInterval {
		l.sweepLocked(now)
	}

	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) >= maxBuckets {
			// 硬上限兜底：先扫一遍空闲桶；仍满则退化为共享桶，保证进程不被内存拖死。
			l.sweepLocked(now)
			if len(l.buckets) >= maxBuckets {
				key = ""
				b = l.buckets[key]
			}
		}
		if b == nil {
			b = &tokenBucket{tokens: l.burst, last: now}
			l.buckets[key] = b
		}
	}

	// 按经过时间连续补充，封顶 burst。
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens += elapsed * l.rps
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	b.seen = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	need := (1 - b.tokens) / l.rps
	d := time.Duration(need * float64(time.Second))
	if d < time.Millisecond {
		d = time.Millisecond
	}
	return false, d
}

// sweepLocked 回收空闲桶（调用方必须持 l.mu）。
func (l *RateLimiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.seen) > idleBucketTTL {
			delete(l.buckets, k)
		}
	}
	l.lastSweep = now
}

// rateLimitedPath 参与限流的路径。刻意不含 /status 与 /healthz：
// 前者是监控面板轮询、后者是负载均衡探活，被限流会造成"看着像挂了"的误判。
func rateLimitedPath(path string) bool {
	return path == "/v1/chat/completions" ||
		path == "/v1/models" ||
		strings.HasPrefix(path, "/v1/messages")
}

// rateKey 限流维度：TCP 对端主机（不含端口）。取不到时归入共享桶，
// 宁可让这些请求共享额度，也不放过限流。
func rateKey(r *http.Request) string {
	if r == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr // 形如 "192.0.2.1"（无端口）时原样使用
}
