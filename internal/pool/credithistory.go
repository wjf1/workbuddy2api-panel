// 余额趋势历史（misakano PR #49 同方向的轻量自实现，v1.17.0 ③）：
// 每次 SetCreditsDetailed（余额刷新，默认 5 分钟）按账号采样一个点，**按小时桶
// 去重**（同桶覆盖）——余额曲线的观测粒度是小时级，分钟级采样只浪费存储。
// 存储：state.json 同目录 credits-history.json（观测数据独立文件，不拖累状态
// 恢复关键路径；损坏静默从空开始），保留 30 天，落盘节流 5 分钟（写盘失败仅
// WARN，趋势缺失不影响任何路由决策）。
package pool

import (
	"encoding/json"
	"log"
	"path/filepath"
	"sort"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/safefile"
)

// creditHistoryDays 趋势保留窗口。
const creditHistoryDays = 30

// creditHistorySaveInterval 落盘节流。
const creditHistorySaveInterval = 5 * time.Minute

// creditPoint 一个小时桶的余额采样。
type creditPoint struct {
	Unix    int64 `json:"t"` // 桶起点（Unix 秒）
	Credits int64 `json:"c"`
}

// creditHistoryFile 落盘结构：版本化，方便未来改形态。
type creditHistoryFile struct {
	Version  int                     `json:"version"`
	Accounts map[string][]creditPoint `json:"accounts"`
}

// recordCreditsPointLocked 采样（SetCreditsDetailed 持锁内调用）：小时桶 upsert
// + 30 天裁剪 + 置脏。**锁内不做任何 IO**——safefile 写盘可能被实时扫描拖慢，
// 持 p.mu 写盘会与 flusher（saveLocked 同样持锁写盘）互相放大等待（实测死锁形态：
// 主 goroutine 持 p.mu 等 credits-history 文件锁、flusher 持文件锁等 p.mu）。
// 落盘由 flushCreditsHistory 在锁外执行（flusher 周期 + Close 兜底）。
func (p *Pool) recordCreditsPointLocked(uid string, credits int64, now time.Time) {
	if uid == "" {
		return
	}
	if p.creditHist == nil {
		p.creditHist = map[string][]creditPoint{}
	}
	bucket := now.Truncate(time.Hour).Unix()
	pts := p.creditHist[uid]
	if n := len(pts); n > 0 && pts[n-1].Unix == bucket {
		pts[n-1].Credits = credits // 同桶覆盖（该小时最新值）
	} else {
		pts = append(pts, creditPoint{Unix: bucket, Credits: credits})
	}
	// 裁剪窗口外旧点。
	cutoff := now.AddDate(0, 0, -creditHistoryDays).Truncate(time.Hour).Unix()
	i := 0
	for i < len(pts) && pts[i].Unix < cutoff {
		i++
	}
	if i > 0 {
		pts = pts[i:]
	}
	p.creditHist[uid] = pts
	p.creditHistDirty.Store(true)
}

// flushCreditsHistory 节流落盘（锁外 IO）：锁内拷贝快照 → 锁外 safefile.Write。
// force=true 跳过节流（Close 兜底，消「节流窗口内退出丢尾」）。写失败 WARN，
// 观测数据丢了不心疼（下个窗口重写）。
func (p *Pool) flushCreditsHistory(force bool) {
	if p.creditHistPath == "" {
		return
	}
	if !force {
		if !p.creditHistDirty.Load() {
			return
		}
		p.mu.Lock()
		if time.Since(p.creditHistSaved) < creditHistorySaveInterval {
			p.mu.Unlock()
			return
		}
		p.creditHistSaved = time.Now()
		snap := p.creditHistSnapshotLocked()
		p.mu.Unlock()
		p.writeCreditsHistory(snap)
		return
	}
	p.mu.Lock()
	p.creditHistSaved = time.Now()
	snap := p.creditHistSnapshotLocked()
	p.mu.Unlock()
	p.writeCreditsHistory(snap)
}

// creditHistSnapshotLocked 拷贝快照（调用方必须持有 p.mu）。
func (p *Pool) creditHistSnapshotLocked() map[string][]creditPoint {
	out := make(map[string][]creditPoint, len(p.creditHist))
	for uid, pts := range p.creditHist {
		out[uid] = append([]creditPoint(nil), pts...)
	}
	return out
}

// writeCreditsHistory 锁外写盘。
func (p *Pool) writeCreditsHistory(accounts map[string][]creditPoint) {
	raw, err := json.Marshal(creditHistoryFile{Version: 1, Accounts: accounts})
	if err == nil {
		err = safefile.Write(p.creditHistPath, raw, 0o644)
	}
	if err != nil {
		log.Printf("[credits-history] WARN: 落盘失败: %v", err)
		return
	}
	p.creditHistDirty.Store(false)
}

// loadCreditsHistory 启动恢复（损坏静默从空开始——观测数据不阻断启动）。
func (p *Pool) loadCreditsHistory() {
	if p.creditHistPath == "" {
		return
	}
	raw, err := safefile.Read(p.creditHistPath, nil)
	if err != nil {
		return // 文件不存在 = 首次运行
	}
	var f creditHistoryFile
	if json.Unmarshal(raw, &f) != nil || f.Version != 1 {
		log.Printf("[credits-history] WARN: %s 损坏，趋势从空开始重建", p.creditHistPath)
		return
	}
	p.creditHist = f.Accounts
}

// CreditTrendPoint 趋势曲线点。
type CreditTrendPoint struct {
	T       int64 `json:"t"` // 桶起点 Unix 秒
	Credits int64 `json:"c"`
}

// CreditTrendSeries 单账号趋势。
type CreditTrendSeries struct {
	UID      string            `json:"uid"`
	Nickname string            `json:"nickname,omitempty"`
	Points   []CreditTrendPoint `json:"points"`
}

// CreditTrend 面板载荷：total = 全池总和曲线（桶并集，缺失账号不计入该桶）；
// series = 每账号明细（前端可选渲染）。
type CreditTrend struct {
	Days   int                `json:"days"`
	Total  []CreditTrendPoint `json:"total"`
	Series []CreditTrendSeries `json:"series"`
}

// CreditsTrend 聚合趋势载荷（panel /panel/api/credits_trend 用）。
func (p *Pool) CreditsTrend() CreditTrend {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := CreditTrend{Days: creditHistoryDays}

	// 桶并集（升序）。
	buckets := map[int64]bool{}
	for _, pts := range p.creditHist {
		for _, pt := range pts {
			buckets[pt.Unix] = true
		}
	}
	var sorted []int64
	for b := range buckets {
		sorted = append(sorted, b)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// 全池总和：每桶累加有值的账号（缺失账号不计入——不是 0）。
	totalByBucket := map[int64]int64{}
	for _, pts := range p.creditHist {
		for _, pt := range pts {
			totalByBucket[pt.Unix] += pt.Credits
		}
	}
	for _, b := range sorted {
		out.Total = append(out.Total, CreditTrendPoint{T: b, Credits: totalByBucket[b]})
	}

	uids := make([]string, 0, len(p.creditHist))
	for uid := range p.creditHist {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		pts := p.creditHist[uid]
		if len(pts) == 0 {
			continue
		}
		ser := CreditTrendSeries{UID: uid}
		if e, ok := p.byUID[uid]; ok {
			ser.Nickname = e.a.Nickname
		}
		for _, pt := range pts {
			ser.Points = append(ser.Points, CreditTrendPoint{T: pt.Unix, Credits: pt.Credits})
		}
		out.Series = append(out.Series, ser)
	}
	return out
}

// creditHistPathFor 由 state 文件路径派生历史文件路径（同目录）。
func creditHistPathFor(stateFp string) string {
	if stateFp == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFp), "credits-history.json")
}
