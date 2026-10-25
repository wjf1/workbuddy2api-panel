// Package authlease auths 目录租约锁（v1.16.0 M4）：防「同一个 auths 目录被两个
// 执行体同时挂载」（同机双进程、目录复制/网络盘后原目录仍被另一台机使用）。
//
// 防线边界（README/HANDOFF 同步说明）：
//   - 防得了：同目录双实例（租约文件互斥 + 心跳新鲜度判定）；
//   - 防不了：凭证副本分处两个独立目录（A/B 机各自 auths 的导出导入副本）——
//     那由 M6 rotaudit 的外部轮换检测抓。两道防线互补。
//
// 设计取舍：
//   - 同机租约（hostname 相同）只 WARN 不阻断：保护计划任务异常重启路径
//     （进程崩溃残留租约 + 立即重启不应被自己挡住）；同机双进程由 M6 兜底。
//   - 异机新鲜租约：strict_lease=true（默认）时 ERROR + 拒绝启动；false 降级 WARN
//     放行（逃生门）。
//   - 心跳 30s、新鲜阈值 90s：容忍两端时钟 ±90s 偏差，宁可放过不可误杀。
//   - 租约是尽力而为的防线：写失败仅 WARN，不因它杀服务。
//
// 租约文件 auths/.lease.json 不会被 auth.LoadDir 当账号加载（glob 是 workbuddy*.json）。
package authlease

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
)

// 心跳与新鲜度参数。
const (
	heartbeatEvery = 30 * time.Second
	freshThreshold = 90 * time.Second
	leaseFileName  = ".lease.json"
)

// leaseFile 租约文件内容。
type leaseFile struct {
	Hostname    string `json:"hostname"`
	PID         int    `json:"pid"`
	StartedAt   string `json:"started_at"`
	HeartbeatAt string `json:"heartbeat_at"`
}

// Lease 已获取的租约。Release 释放（停心跳 + 删文件）；进程退出不 Release 也安全
// （心跳停止后租约在 90s 内变 stale，下个实例 WARN 后接管）。
type Lease struct {
	dir  string
	stop chan struct{}
	once sync.Once
}

// hostnameF 仅供测试注入；nil 用 os.Hostname。
var hostnameF = func() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// Acquire 获取租约。dir 为 auths 目录（不存在时创建）。返回的 Lease 非 nil 时
// 心跳已启动；调用方 defer lease.Release()。
// 冲突处理见包注释；strict=true 时异机新鲜租约返回 error（调用方拒绝启动）。
func Acquire(dir string, strict bool) (*Lease, error) {
	if dir == "" {
		return nil, nil // 未配置 auths 目录 = 禁用租约（零回归）
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("authlease: mkdir %s: %w", dir, err)
	}
	fp := filepath.Join(dir, leaseFileName)
	host := hostnameF()
	now := time.Now()

	if data, err := os.ReadFile(fp); err == nil && len(data) > 0 {
		var lf leaseFile
		if json.Unmarshal(data, &lf) == nil && lf.Hostname != "" {
			hb, parseErr := time.Parse(time.RFC3339, lf.HeartbeatAt)
			age := freshThreshold + time.Minute // 解析失败按 stale 处理
			if parseErr == nil {
				age = now.Sub(hb)
			}
			switch {
			case age > freshThreshold:
				logx.Infof("[authlease] 旧租约已过期（%.0f 秒无心跳，宿主 %s pid %d，疑似崩溃残留），接管",
					age.Seconds(), lf.Hostname, lf.PID)
			case lf.Hostname == host:
				logx.Warnf("[authlease] 检测到同机租约（pid %d，心跳 %.0f 秒前）——可能是双进程或残留；已接管。同机多进程由 RT 轮换审计兜底观察",
					lf.PID, age.Seconds())
			default:
				msg := fmt.Sprintf("authlease: 检测到异机新鲜租约：宿主 %s pid %d 心跳 %.0f 秒前 —— 另一实例正在使用同一 auths 目录（RT 双花会连坐吊销整条令牌链）",
					lf.Hostname, lf.PID, age.Seconds())
				if strict {
					return nil, fmt.Errorf("%s；已拒绝启动（auths.strict_lease=true，改 false 可放行）", msg)
				}
				logx.Warnf("[authlease] %s；strict_lease=false 已放行", msg)
			}
		}
	}

	lf := leaseFile{
		Hostname:    host,
		PID:         os.Getpid(),
		StartedAt:   now.Format(time.RFC3339),
		HeartbeatAt: now.Format(time.RFC3339),
	}
	if err := writeLease(fp, lf); err != nil {
		logx.Warnf("[authlease] 写租约失败（防线降级为尽力而为）: %v", err)
	}

	l := &Lease{dir: dir, stop: make(chan struct{})}
	go l.heartbeat(lf)
	return l, nil
}

// writeLease 原子语义不必须（崩溃留下半截文件会被 json.Unmarshal 拒绝后按无租约处理），
// 直接写即可；失败返回错误由调用方降级。
func writeLease(fp string, lf leaseFile) error {
	data, err := json.Marshal(lf)
	if err != nil {
		return err
	}
	return os.WriteFile(fp, data, 0o644)
}

// heartbeat 心跳循环：每 30s 重写 heartbeat_at。Release / 进程退出即停。
func (l *Lease) heartbeat(base leaseFile) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			return
		case now := <-t.C:
			base.HeartbeatAt = now.Format(time.RFC3339)
			if err := writeLease(filepath.Join(l.dir, leaseFileName), base); err != nil {
				logx.Warnf("[authlease] 心跳写失败: %v", err)
			}
		}
	}
}

// Release 停心跳并删除租约文件（幂等）。
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		close(l.stop)
		if err := os.Remove(filepath.Join(l.dir, leaseFileName)); err != nil && !os.IsNotExist(err) {
			logx.Warnf("[authlease] 删除租约: %v", err)
		}
	})
}
