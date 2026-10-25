// refresh.go 凭据刷新：RT 换发（单飞去重 + 60s 结果复用 + 轮换审计），
// 锁外两段式网络 I/O 与 server 侧过期前主动续期。
package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// refreshIOTimeout 刷新端点网络 I/O 上限（两段式锁外执行，防上游 hang 长占锁）。
const refreshIOTimeout = 30 * time.Second

// refreshTokenExpiresInMax refresh 响应 expiresIn 的量级上限（10 年，纯防御值：
// 实测 R-D 响应恒 5184000=60d）。超限视为上游脏数据，不写 ExpiresAt（保留旧值），
// 防止 NeedsRefresh 永假导致 token 永不刷新反而真过期失效。
const refreshTokenExpiresInMax = 10 * 365 * 24 * time.Hour

// refreshGrace 刷新成功后结果的复用窗口：拿着旧 refresh token 的后来者
// （keepalive 与请求路径、持有旧快照的调用方）在这段时间内直接复用结果，
// 不再向上游花第二次 RT。与 magpie 的 WorkBuddy 认证插件口径一致（60s）。
var refreshGrace = 60 * time.Second

// refreshSweepAt renewed 表超过该条数时清理过期项（防止长期运行累积）。
const refreshSweepAt = 256

// refreshResult 一次刷新的结果（成功或失败），供在途等待者/宽限缓存复用。
type refreshResult struct {
	accessToken  string
	refreshToken string
	domain       string
	expiresAt    int64
	err          error
	at           time.Time
}

// refreshCall 一次在途刷新：done 关闭后 res 可读。
type refreshCall struct {
	done chan struct{}
	res  *refreshResult
}

// refreshKey 按账号 + 当前 refresh token 标识一次刷新。RT 进 key 是为了让
// 持有旧快照的调用方与已换新的调用方区分开；UID 为空时回落 FilePath/指针
// （生产账号两者都有值，测试可用 UID 固定 key）。
func refreshKey(a *auth.Auth, rt string) string {
	id := a.UID
	if id == "" {
		id = a.FilePath
	}
	if id == "" {
		id = fmt.Sprintf("%p", a)
	}
	return id + "\n" + rt
}

// RotationObserver RT 轮换观察者（v1.16.0 M6 审计）：每次 leader 刷新完成
// （成功或失败）回调一次；等待者 / 60s 宽限复用不回调（同一次 RT 消费只记一条）。
// oldRT8/newRT8 是 refresh token 的 sha256 前 8 位指纹（不落明文）；newRT8 仅成功
// 且上游返回新 RT 时非空；err 非 nil 表示本次刷新失败（RT 未被消费成功）。
type RotationObserver func(uid, oldRT8, newRT8, source string, err error)

// SetRotationObserver 注入轮换观察者（main 装配 internal/rotaudit；nil = 不观察）。
func (c *Client) SetRotationObserver(fn RotationObserver) {
	c.refreshMu.Lock()
	c.rotationObs = fn
	c.refreshMu.Unlock()
}

// rtFingerprint RT 指纹：sha256 前 8 位 hex。审计日志只落指纹不落明文。
func rtFingerprint(rt string) string {
	sum := sha256.Sum256([]byte(rt))
	return hex.EncodeToString(sum[:4])
}

// RefreshToken 强制刷新（keepalive 用）：不检查是否临近过期，但同一个
// refresh token 的在途刷新/60s 结果会被复用，避免与请求路径并发时双花 RT。
// source 标记调用来源（keepalive/chat/balance/signin/probe），进轮换审计日志。
func (c *Client) RefreshToken(a *auth.Auth, source string) error {
	return c.refresh(a, 0, true, source)
}

// RefreshTokenIfNeeded 按需刷新（请求路径 / signin 用）：先快速判断是否临近
// 过期；进入刷新事务后再双检一次，等锁期间别人已刷好就直接复用。
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, within time.Duration, source string) error {
	if !a.NeedsRefresh(within) {
		return nil
	}
	return c.refresh(a, within, false, source)
}

// refresh 刷新事务编排：
//  1. 取 RT/AT 快照（a.mu 内，短）；
//  2. 按 RT 查 60s 宽限结果 → 命中直接合并；
//  3. 同 RT 已有在途刷新 → 等它，不重复发请求；
//  4. 否则作为 leader 锁外发 HTTP，完成后把结果交给等待者并缓存 60s；
//  5. 写回前双检 token 是否已被别人换新；失败时同样先判断，避免把
//     「并发落败」误判成 12153 session dead。
//
// 锁序：refreshMu → a.mu；网络 I/O 不持任何锁。
func (c *Client) refresh(a *auth.Auth, within time.Duration, force bool, source string) error {
	// 第 1 段（锁内）：读快照。
	a.Lock()
	rtSnapshot := a.RefreshToken
	atBefore := a.AccessToken
	a.Unlock()
	if strings.TrimSpace(rtSnapshot) == "" {
		return fmt.Errorf("no refreshToken")
	}
	// 快照后二次检查：读快照/排队的瞬间别人可能已经刷好（NeedsRefresh 自己加锁）。
	if !force && !a.NeedsRefresh(within) {
		return nil
	}

	key := refreshKey(a, rtSnapshot)

	c.refreshMu.Lock()
	if c.refreshing == nil {
		c.refreshing = make(map[string]*refreshCall)
	}
	if c.renewed == nil {
		c.renewed = make(map[string]*refreshResult)
	}
	if !force {
		// 60s 宽限只服务于「按需刷新」：请求路径/旧快照在并发窗口内复用结果。
		// 强制刷新（keepalive）必须真正问一次上游——它同时承担 12153 连续失败
		// 计数；若强制调用也吃宽限，上游不换 RT 时会把后续真失败掩盖成成功。
		if r, ok := c.renewed[key]; ok && time.Since(r.at) < refreshGrace {
			c.refreshMu.Unlock()
			log.Printf("refresh %s: reuse result within %s", logfmt.Label(a.UID, a.Nickname), refreshGrace)
			return c.applyRefresh(a, r, atBefore, rtSnapshot)
		}
	}
	if call, ok := c.refreshing[key]; ok {
		c.refreshMu.Unlock()
		<-call.done
		return c.applyRefresh(a, call.res, atBefore, rtSnapshot)
	}
	call := &refreshCall{done: make(chan struct{})}
	c.refreshing[key] = call
	c.refreshMu.Unlock()

	// finish 只执行一次：正常路径与 panic 清理共用，避免 done 被重复关闭。
	var once sync.Once
	finish := func(res *refreshResult) {
		once.Do(func() {
			c.refreshMu.Lock()
			call.res = res
			delete(c.refreshing, key)
			if res != nil && res.err == nil {
				c.renewed[key] = res
				c.sweepRenewedLocked()
			}
			c.refreshMu.Unlock()
			close(call.done)
		})
	}
	defer func() {
		if r := recover(); r != nil {
			finish(&refreshResult{err: fmt.Errorf("refresh panic: %v", r), at: time.Now()})
			panic(r) // 释放等待者后继续上抛，不吞掉 leader 的 panic
		}
	}()

	res := c.doRefresh(a, rtSnapshot)
	finish(res)
	err := c.applyRefresh(a, res, atBefore, rtSnapshot)
	// M6 审计：只有 leader 记录（等待者/宽限复用共享同一次 RT 消费）。
	if c.rotationObs != nil {
		switch {
		case res == nil:
			c.rotationObs(a.UID, rtFingerprint(rtSnapshot), "", source, fmt.Errorf("refresh: no result"))
		case res.err != nil:
			c.rotationObs(a.UID, rtFingerprint(rtSnapshot), "", source, res.err)
		default:
			c.rotationObs(a.UID, rtFingerprint(rtSnapshot), rtFingerprint(res.refreshToken), source, nil)
		}
	}
	return err
}

// doRefresh 只做网络刷新，不写回。调用方已保证同一 RT 只有它一个 leader。
func (c *Client) doRefresh(a *auth.Auth, rtSnapshot string) *refreshResult {
	endpoint := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return &refreshResult{err: err, at: time.Now()}
	}
	// RefreshHeaders 读取 a 的字段（domain/uid 等）注入请求头——需在锁内取快照值，
	// 用一个显式逐字段拷贝的临时 auth 构造头（不拷贝 sync.Mutex，避免 vet copies-lock）。
	a.Lock()
	hdrSnapshot := auth.Auth{
		AccessToken:  a.AccessToken,
		RefreshToken: rtSnapshot,
		ExpiresAt:    a.ExpiresAt,
		Domain:       a.Domain,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
		Nickname:     a.Nickname,
		DeviceToken:  a.DeviceToken,
	}
	a.Unlock()
	c.RefreshHeaders(req, &hdrSnapshot)

	// 网络 I/O（锁外，30s 上限）。
	data, err := c.doJSON(req)
	if err != nil {
		return &refreshResult{err: err, at: time.Now()}
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return &refreshResult{
			err: fmt.Errorf("refresh_failed: no accessToken in response — re-login required"),
			at:  time.Now(),
		}
	}
	res := &refreshResult{
		accessToken:  tok.AccessToken,
		refreshToken: tok.RefreshToken,
		domain:       tok.Domain,
		at:           time.Now(),
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	// 实测 R-D 响应恒带 expiresIn=5184000（60d）——缺省分支仅为防御，保留旧值
	// 避免过期判定漂移。同理，超过 10 年的 expiresIn 按脏值处理保留旧值：
	// 实测 JWT exp-iat 与 expiresIn 严格自洽（R-F），超量级值只会是上游脏数据，
	// 照写会把 ExpiresAt 推到荒谬未来 → NeedsRefresh 永假 → token 永不刷新
	// 反而真过期失效。
	if tok.ExpiresIn > 0 && time.Duration(tok.ExpiresIn)*time.Second < refreshTokenExpiresInMax {
		res.expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return res
}

// applyRefresh 把刷新结果合并回 a。写回前重新校验快照（AND 语义，与旧实现一致）：
// 锁外期间另一刷新已完成 → 两 token 必同时变化，本次结果不必再写；失败时同样
// 先看 token 是否已变——变了说明别人刷成功，本次失败只是并发落败，不该禁用账号。
func (c *Client) applyRefresh(a *auth.Auth, r *refreshResult, atBefore, rtSnapshot string) error {
	if r == nil {
		return fmt.Errorf("refresh: no result")
	}
	if r.err != nil {
		a.Lock()
		changed := a.AccessToken != atBefore && a.RefreshToken != rtSnapshot
		a.Unlock()
		if changed {
			return nil
		}
		return r.err
	}
	a.Lock()
	defer a.Unlock()
	// 写回守卫是 AND 语义：锁外期间另一刷新已完成 → 两 token 必同时变化（实测 R-D：
	// refresh 响应 accessToken/refreshToken 总是一起 rotate，写回也同时写两个），AND
	// 即「并发刷新已完成」判据；AND 与 OR 在真实形态下等价。唯 OR 会额外放弃的
	// 「只有单 token 变化」（如手工只改 auth 文件一个字段）不构成放弃条件——本次
	// 结果覆盖手工编辑。
	if a.AccessToken != atBefore && a.RefreshToken != rtSnapshot {
		// 锁外期间另一 goroutine 已完成刷新：新 token 已生效，本次结果不必再写
		// （实测 R-E：服务端无 rotation 撤销，并发双刷新拿到的两个新 token 都有效，
		// 后写覆盖先写二者等价可用；提前返回避免无意义覆盖与 ExpiresAt 抖动）。
		return nil
	}
	a.AccessToken = r.accessToken
	if r.refreshToken != "" {
		a.RefreshToken = r.refreshToken
	}
	if r.domain != "" {
		a.Domain = r.domain
	}
	if r.expiresAt > 0 {
		a.ExpiresAt = r.expiresAt
	}
	return nil
}

// sweepRenewedLocked 清理超过 2×grace 的旧结果；调用方必须持有 refreshMu。
func (c *Client) sweepRenewedLocked() {
	if len(c.renewed) <= refreshSweepAt {
		return
	}
	cutoff := time.Now().Add(-2 * refreshGrace)
	for k, r := range c.renewed {
		if r.at.Before(cutoff) {
			delete(c.renewed, k)
		}
	}
}
