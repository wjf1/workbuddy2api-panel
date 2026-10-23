// Package rotaudit RT 轮换审计与外部轮换检测（v1.16.0 M6）。
//
// 背景：2026-10-23 实案 6 个账号 RT 链被上游批量吊销（12153 invalid_grant），
// 高度怀疑是同一批凭证的**外部执行体双花**（另一台机器的部署副本 / 独立
// signin.sh 进程）触发上游 rotation/replay 检测连坐吊销。v1.15.7 的进程内
// 单飞防住了同进程并发，但防不了进程外——本包把「谁在消费 RT」变成可见事实：
//
//   - 审计日志：每次 leader 刷新（成功或失败）追加一条 JSONL（按天切文件，
//     保留 7 天），记录 RT 新旧指纹（sha256 前 8 位，不落明文）、来源路径、
//     host/pid。排查双花时按 uid 聚合即可还原消费时序。
//   - 外部轮换检测：启动时用池内当前 RT 播种基准（Seed）；此后任何一次刷新
//     的旧 RT 指纹与基准不符 = 本进程之外还有执行体在消费该账号的 RT
//     （重登走 Reset 重置基准，不误报）。命中即 ERROR 日志 + 回调 main 注入的
//     onExternal（→ alert.Manager 外发 webhook + 横幅计数）。
//
// 审计写盘失败只 WARN，绝不阻断刷新主流程。
package rotaudit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 保留窗口与硬上限：审计文件按天切，保留 7 天（与 reqlog 惯例一致）。
const retentionDays = 7

// Rec 审计记录（JSONL 每行一条）。
type Rec struct {
	Ts      string `json:"ts"`                  // RFC3339
	UID     string `json:"uid"`                 // 账号 uid（脱敏场景由调用方决定，本包不处理）
	OldRT8  string `json:"old_rt8"`             // 旧 RT 指纹（sha256 前 8 hex）
	NewRT8  string `json:"new_rt8,omitempty"`   // 新 RT 指纹（失败时为空）
	Source  string `json:"source"`              // keepalive / chat / balance / signin / probe
	Host    string `json:"host"`                // 自证来源（排查多机部署）
	PID     int    `json:"pid"`                 // 自证进程
	Err     string `json:"err,omitempty"`       // 刷新失败原因（成功为空）
	ErrKind string `json:"err_kind,omitempty"`  // "session_dead"（12153/裸 401）等归类，非死错误为空
}

// Recorder 审计器。并发安全。
type Recorder struct {
	mu     sync.Mutex
	dir    string // 审计目录（data/refresh-audit）
	cur    map[string]string // uid → 基准 RT 指纹
	host   string
	onExternal func(uid, old8, cur8, source string) // 外部轮换回调（main 注入；nil = 只记日志）
	// nowF 仅供测试注入时钟；nil 用 time.Now。
	nowF func() time.Time
}

// New 构造审计器并清理过期文件。dir 为审计输出目录（建议 data/refresh-audit）。
func New(dir, host string, onExternal func(uid, old8, cur8, source string)) *Recorder {
	if host == "" {
		if h, err := os.Hostname(); err == nil {
			host = h
		}
	}
	r := &Recorder{
		dir:        dir,
		cur:        map[string]string{},
		host:       host,
		onExternal: onExternal,
		nowF:       time.Now,
	}
	r.cleanup()
	return r
}

// Seed 播种基准：池内每个账号的当前 RT（明文入参，本包内立即指纹化，不落盘不保留明文）。
// main 在 pool 加载完成后、任何刷新发生前调用一次。
func (r *Recorder) Seed(entries map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for uid, rt := range entries {
		if rt != "" {
			r.cur[uid] = fingerprint(rt)
		}
	}
	log.Printf("[rotaudit] 基准播种完成：%d 个账号", len(r.cur))
}

// Reset 重置单账号基准（面板重登成功后调用——新凭证的第一次刷新不构成外部轮换）。
func (r *Recorder) Reset(uid, rt string) {
	if uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rt == "" {
		delete(r.cur, uid)
		return
	}
	r.cur[uid] = fingerprint(rt)
}

// Observe 轮换观察入口（upstream.Client.SetRotationObserver 的回调实现）。
// err != nil 时 newRT8 应为空（RT 未被成功换新）。
func (r *Recorder) Observe(uid, oldRT8, newRT8, source string, err error) {
	rec := Rec{
		Ts:     r.now().Format(time.RFC3339),
		UID:    uid,
		OldRT8: oldRT8,
		NewRT8: newRT8,
		Source: source,
		Host:   r.host,
		PID:    os.Getpid(),
	}
	if err != nil {
		rec.Err = err.Error()
		rec.ErrKind = classifyErr(err)
	}
	r.write(rec)

	// 外部轮换检测：基准存在且本次旧指纹 ≠ 基准 → 本进程之外有执行体消费过该 RT。
	if uid == "" || oldRT8 == "" {
		return
	}
	r.mu.Lock()
	cur, ok := r.cur[uid]
	external := ok && cur != oldRT8
	if newRT8 != "" {
		r.cur[uid] = newRT8
	}
	onExt, oldBase := r.onExternal, cur
	r.mu.Unlock()
	if external {
		log.Printf("[rotaudit] ERROR: 检测到外部 RT 轮换 uid=%s 本进程基准=%s 实际旧指纹=%s 来源=%s —— 有另一执行体（其他机器副本 / signin.sh 等进程）在共享该账号凭证，正是 RT 链连坐吊销的成因",
			uid, oldBase, oldRT8, source)
		if onExt != nil {
			onExt(uid, oldRT8, oldBase, source)
		}
	}
}

// write 追加审计记录（按天切文件）。失败仅 WARN。
func (r *Recorder) write(rec Rec) {
	if r.dir == "" {
		return
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		log.Printf("[rotaudit] WARN: mkdir %s: %v", r.dir, err)
		return
	}
	fp := filepath.Join(r.dir, "refresh-audit-"+r.now().Format("20060102")+".jsonl")
	line, err := json.Marshal(rec)
	if err != nil {
		log.Printf("[rotaudit] WARN: marshal: %v", err)
		return
	}
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		log.Printf("[rotaudit] WARN: open %s: %v", fp, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		log.Printf("[rotaudit] WARN: write: %v", err)
	}
}

// cleanup 清理超过保留期的审计文件（按文件名日期后缀判定，解析失败跳过）。
func (r *Recorder) cleanup() {
	if r.dir == "" {
		return
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return // 目录不存在 = 首次运行，无需清理
	}
	cutoff := r.now().AddDate(0, 0, -retentionDays)
	cut := cutoff.Format("20060102")
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "refresh-audit-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, "refresh-audit-"), ".jsonl")
		if len(day) != 8 || day >= cut {
			continue
		}
		if err := os.Remove(filepath.Join(r.dir, name)); err != nil {
			log.Printf("[rotaudit] WARN: 清理过期审计 %s: %v", name, err)
		} else {
			log.Printf("[rotaudit] 已清理过期审计文件 %s", name)
		}
	}
}

// classifyErr 错误归类：12153 / 裸 401 → "session_dead"（连坐吊销的直接证据形态），
// 其余为空（网络抖动等常规失败）。文案本地化兜底与 upstream.Classify 同思路。
func classifyErr(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "session_dead"), strings.Contains(msg, "session dead"),
		strings.Contains(msg, "12153"), strings.Contains(msg, "401"):
		return "session_dead"
	}
	return ""
}

// SeedSnapshot 供 main 播种：从 uid → RT 明文的映射构建（Pool 侧组装）。
// 与 Seed 等价；独立命名仅为调用点可读性。
func Fingerprint(rt string) string { return fingerprint(rt) }

// fingerprint sha256 前 8 位 hex。
func fingerprint(rt string) string {
	if rt == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(rt))
	return hex.EncodeToString(sum[:4])
}

func (r *Recorder) now() time.Time {
	if r.nowF != nil {
		return r.nowF()
	}
	return time.Now()
}

// SortedUIDs 返回基准表内 uid 的排序副本（测试断言用）。
func (r *Recorder) SortedUIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.cur))
	for uid := range r.cur {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}
