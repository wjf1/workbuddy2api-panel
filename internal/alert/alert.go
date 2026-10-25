// Package alert 批量判死横幅与 webhook 告警管理器（v1.16.0 M2）。
//
// 事件源：pool 的禁用 hook（Pool.SetOnDisabled → Manager.NoteDisabled，单一收口）。
// 职责分两层：
//   - 面板横幅（overview.mass_kill）：**无冷却**——Payload() 实时计算窗口内事件，
//     事件老化后横幅自然消失；2026-10-23 实案（6 号 11:41 集体判死、13:40 才被发现）
//     的直接对策，横幅在下一轮 5s 轮询即出现。
//   - webhook 外发：受 cooldown 约束（默认 30m），可对接飞书自建应用 / 企业微信 /
//     Bark 等通用 JSON webhook。失败静默重试 1 次，仍失败仅 ERROR 日志——外发是
//     尽力而为的旁路，绝不影响横幅与禁用主流程。
//
// 只统计会话失效类禁用（pool.IsSessionDeadReason）：人工禁用、上游封号（banned）
// 不构成「批量吊销」信号——前者是运维动作，后者自有 reason 文案且重登无用。
package alert

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// 默认参数（config 缺省 / normalize 兜底口径一致）。
const (
	DefaultMassKillAccounts = 3
	DefaultMassKillWindow   = 10 * time.Minute
	DefaultCooldown         = 30 * time.Minute
)

// Config 告警配置（config.json 的 notify 段映射，main 解析后注入；热可改）。
// 时间用分钟整数承载（与 config JSON 对齐），Manager 内部归一为 Duration。
type Config struct {
	WebhookURL        string // 空 = 仅面板横幅，不外发
	MassKillAccounts  int    // 窗口内判死 distinct uid 阈值；<=0 用默认 3
	MassKillWindowMin int    // 判死窗口分钟数；<=0 用默认 10
	CooldownMin       int    // webhook 外发冷却分钟数；<=0 用默认 30
}

// evt 一次会话失效类禁用事件。
type evt struct {
	UID      string
	Nickname string
	Reason   string
	At       time.Time
}

// MassKill overview.mass_kill 载荷：窗口内会话失效类禁用汇总（无事件时整体为 null）。
type MassKill struct {
	Count         int       `json:"count"`               // distinct uid 数
	WindowMinutes int       `json:"window_minutes"`      // 当前窗口（分钟）
	UIDs          []string  `json:"uids"`                // 判死账号 uid（首次出现序）
	Nicknames     []string  `json:"nicknames,omitempty"` // 与 UIDs 一一对应
	FirstAt       time.Time `json:"first_at"`            // 窗口内最早事件
	LastAt        time.Time `json:"last_at"`             // 窗口内最晚事件
}

// Manager 批量判死告警器。并发安全；零值不可用，经 NewManager 构造。
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	events   []evt     // 保留窗口内（按最大窗口裁剪）的会话失效事件
	lastSent time.Time // 最近一次外发尝试（成功失败都算——失败也节流，防重试风暴）
	// ext 外部 RT 轮换事件（M6 rotaudit 检测 → NoteExternalRotation）：uid → 最近时间。
	// 与 mass_kill 不同：**任意数量即告警**（哪怕 1 个也意味着凭证双活，无需凑阈值）。
	ext         map[string]time.Time
	lastExtSent time.Time // 外部轮换 webhook 的独立冷却键
	host        string    // webhook 载荷自证来源（os.Hostname）
	version     string    // 面板版本（同载荷）
	hc          *http.Client
}

// New 构造告警器。host/version 为 webhook 载荷的自证字段（缺省自动取 hostname）。
func New(cfg Config, host, version string) *Manager {
	if host == "" {
		if h, err := os.Hostname(); err == nil {
			host = h
		}
	}
	m := &Manager{
		ext:     map[string]time.Time{},
		host:    host,
		version: version,
		// webhook 外发是旁路：5s 超时 + 不跟随代理环境变量由调用方 env 决定，
		// 超时足够短，禁用路径的 goroutine 不会积压。
		hc: &http.Client{Timeout: 5 * time.Second},
	}
	m.SetConfig(cfg)
	return m
}

// SetConfig 热更新配置（面板配置页 saveConfig → main 闭包调用）。并发安全。
func (m *Manager) SetConfig(c Config) {
	if c.MassKillAccounts <= 0 {
		c.MassKillAccounts = DefaultMassKillAccounts
	}
	if c.MassKillWindowMin <= 0 {
		c.MassKillWindowMin = int(DefaultMassKillWindow.Minutes())
	}
	if c.CooldownMin <= 0 {
		c.CooldownMin = int(DefaultCooldown.Minutes())
	}
	m.mu.Lock()
	m.cfg = c
	m.mu.Unlock()
}

// NoteDisabled 禁用 hook 入口（pool.SetOnDisabled 注入；在 pool 锁外的独立
// goroutine 中调用）。只记会话失效类禁用；同 uid 短窗口内重复禁用各自成事件，
// 但计数按 distinct uid。
func (m *Manager) NoteDisabled(uid, nickname, reason string) {
	if !pool.IsSessionDeadReason(reason) {
		return
	}
	m.mu.Lock()
	cfg := m.cfg
	m.events = append(m.events, evt{UID: uid, Nickname: nickname, Reason: reason, At: time.Now()})
	m.trimLocked(cfg)
	m.mu.Unlock()

	m.maybeNotify()
}

// trimLocked 裁剪窗口外旧事件：按最大窗口（1 小时，覆盖一切合理配置）保留，
// 使窗口调小后老事件仍可能落在「历史窗口」外而不误计。调用方必须持有 m.mu。
func (m *Manager) trimLocked(cfg Config) {
	cutoff := time.Now().Add(-time.Duration(cfg.MassKillWindowMin) * time.Minute)
	// 保险：窗口配置被调得很大时也不无限增长——硬上限 1 小时。
	if hard := time.Now().Add(-time.Hour); cutoff.Before(hard) {
		cutoff = hard
	}
	keep := m.events[:0]
	for _, e := range m.events {
		if e.At.After(cutoff) {
			keep = append(keep, e)
		}
	}
	m.events = keep
}

// snapshot 返回窗口内事件拷贝（无事件时 nil）。
func (m *Manager) snapshot() ([]evt, Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trimLocked(m.cfg)
	if len(m.events) == 0 {
		return nil, m.cfg
	}
	out := make([]evt, len(m.events))
	copy(out, m.events)
	return out, m.cfg
}

// Payload overview.mass_kill 载荷：窗口内 distinct uid 数达标即返回汇总（**无冷却**，
// 事件老化后自然消失）；未达标返回 nil（前端横幅隐藏）。并发安全。
func (m *Manager) Payload() any {
	evts, cfg := m.snapshot()
	if len(evts) == 0 {
		return nil
	}
	seen := map[string]int{}
	var uids, nicks []string
	first, last := evts[0].At, evts[0].At
	for _, e := range evts {
		if i, ok := seen[e.UID]; ok {
			nicks[i] = e.Nickname // 同号重复禁用：昵称取最新
			continue
		}
		seen[e.UID] = len(uids)
		uids = append(uids, e.UID)
		nicks = append(nicks, e.Nickname)
		if e.At.Before(first) {
			first = e.At
		}
		if e.At.After(last) {
			last = e.At
		}
	}
	if len(uids) < cfg.MassKillAccounts {
		return nil
	}
	return &MassKill{
		Count:         len(uids),
		WindowMinutes: cfg.MassKillWindowMin,
		UIDs:          uids,
		Nicknames:     nicks,
		FirstAt:       first,
		LastAt:        last,
	}
}

// ExternalRotations 外部 RT 轮换横幅载荷（M6）：窗口内被检测到「进程外执行体
// 消费 RT」的账号汇总。无冷却约束——横幅只要事件未老化就显示。
type ExternalRotations struct {
	Count  int       `json:"count"`
	UIDs   []string  `json:"uids"`
	LastAt time.Time `json:"last_at"`
}

// NoteExternalRotation 记录一次外部 RT 轮换（M6 rotaudit 检测回调）：
// 计入横幅（任意数量即显示）+ 立即评估 webhook 外发（独立冷却键）。
func (m *Manager) NoteExternalRotation(uid, source string) {
	m.mu.Lock()
	if m.ext == nil {
		m.ext = map[string]time.Time{}
	}
	m.ext[uid] = time.Now()
	m.trimExtLocked()
	cfg := m.cfg
	url := cfg.WebhookURL
	cool := time.Duration(cfg.CooldownMin) * time.Minute
	shouldSend := url != "" && time.Since(m.lastExtSent) >= cool
	if shouldSend {
		m.lastExtSent = time.Now()
	}
	lastAt := m.ext[uid]
	m.mu.Unlock()

	if shouldSend {
		m.sendWebhook2("external_token_rotation", url, map[string]any{
			"event":   "external_token_rotation",
			"host":    m.host,
			"version": m.version,
			"accounts": []map[string]string{
				{"uid": uid, "source": source, "at": lastAt.Format(time.RFC3339)},
			},
		})
	}
}

// trimExtLocked 裁剪 1 小时前的外部轮换事件（硬上限，同 mass_kill 口径）。
// 调用方必须持有 m.mu。
func (m *Manager) trimExtLocked() {
	cutoff := time.Now().Add(-time.Hour)
	for uid, at := range m.ext {
		if at.Before(cutoff) {
			delete(m.ext, uid)
		}
	}
}

// ExternalPayload overview.external_rotations 载荷：无事件 nil；有事件（任意数量）
// 即返回汇总——1 个外部轮换也意味着凭证双活，不设阈值。
func (m *Manager) ExternalPayload() any {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trimExtLocked()
	if len(m.ext) == 0 {
		return nil
	}
	out := &ExternalRotations{Count: len(m.ext)}
	uids := make([]string, 0, len(m.ext))
	var last time.Time
	for uid, at := range m.ext {
		uids = append(uids, uid)
		if at.After(last) {
			last = at
		}
	}
	sort.Strings(uids)
	out.UIDs = uids
	out.LastAt = last
	return out
}

// NoteRecovered 死号复活事件（M8 探活命中）：频率天然低（每号每日至多一轮），
// 直接外发不走冷却。
func (m *Manager) NoteRecovered(uid, nickname string) {
	m.sendWebhook2("dead_recovered", m.cfgSnapshotURL(), map[string]any{
		"event":    "dead_recovered",
		"host":     m.host,
		"version":  m.version,
		"accounts": []map[string]string{{"uid": uid, "nickname": nickname, "at": time.Now().Format(time.RFC3339)}},
	})
}

// cfgSnapshotURL 快照读取 webhook URL。
func (m *Manager) cfgSnapshotURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg.WebhookURL
}

// maybeNotify 窗口内 distinct uid 达阈值且距上次外发 ≥ cooldown → 外发 webhook。
// 外发在调用方 goroutine 同步执行（pool hook 已是独立 goroutine，5s 超时兜底）。
func (m *Manager) maybeNotify() {
	evts, cfg := m.snapshot()
	if len(evts) == 0 || cfg.WebhookURL == "" {
		return
	}
	seen := map[string]bool{}
	for _, e := range evts {
		seen[e.UID] = true
	}
	if len(seen) < cfg.MassKillAccounts {
		return
	}
	m.mu.Lock()
	if time.Since(m.lastSent) < time.Duration(cfg.CooldownMin)*time.Minute {
		m.mu.Unlock()
		return
	}
	m.lastSent = time.Now() // 成功失败都推进：失败也节流，防每个新事件都触发重试
	url := cfg.WebhookURL
	m.mu.Unlock()

	m.sendWebhook(url, evts, len(seen), cfg.MassKillWindowMin)
}

// webhookPayload 通用 JSON webhook 载荷（飞书/企业微信/Bark 等按需二次加工）。
type webhookPayload struct {
	Event         string `json:"event"`
	Host          string `json:"host"`
	Version       string `json:"version"`
	Count         int    `json:"count"`
	WindowMinutes int    `json:"window_minutes"`
	Accounts      []struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname"`
		Reason   string `json:"reason"`
		At       string `json:"at"`
	} `json:"accounts"`
}

func (m *Manager) sendWebhook(url string, evts []evt, count, windowMin int) {
	pl := webhookPayload{
		Event:         "mass_session_dead",
		Host:          m.host,
		Version:       m.version,
		Count:         count,
		WindowMinutes: windowMin,
	}
	for _, e := range evts {
		pl.Accounts = append(pl.Accounts, struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
			Reason   string `json:"reason"`
			At       string `json:"at"`
		}{e.UID, e.Nickname, e.Reason, e.At.Format(time.RFC3339)})
	}
	body, err := json.Marshal(pl)
	if err != nil {
		log.Printf("[alert] webhook marshal: %v", err)
		return
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ { // 失败静默重试 1 次
		resp, err := m.hc.Post(url, "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Printf("[alert] webhook 已外发：mass_session_dead ×%d → %s", count, url)
				return
			}
			lastErr = &statusError{Code: resp.StatusCode}
		} else {
			lastErr = err
		}
	}
	log.Printf("[alert] ERROR: webhook 外发失败（已重试 1 次，%dm 内不再重试）: %v", int(DefaultCooldown.Minutes()), lastErr)
}

// sendWebhook2 通用载荷外发（外部轮换等非常规事件；与 sendWebhook 共用重试与日志口径）。
func (m *Manager) sendWebhook2(event, url string, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[alert] webhook marshal(%s): %v", event, err)
		return
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := m.hc.Post(url, "application/json", bytes.NewReader(body))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				log.Printf("[alert] webhook 已外发：%s → %s", event, url)
				return
			}
			lastErr = &statusError{Code: resp.StatusCode}
		} else {
			lastErr = err
		}
	}
	log.Printf("[alert] ERROR: webhook 外发失败（%s，已重试 1 次，%dm 内不再重试）: %v",
		event, int(DefaultCooldown.Minutes()), lastErr)
}

type statusError struct{ Code int }

func (e *statusError) Error() string {
	return http.StatusText(e.Code)
}
