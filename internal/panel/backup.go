package panel

// 定时备份：每日整点把 auths/*.json + state.json 打包为 zip 落 BackupDir，
// 保留最近 keep 份轮转；配置页可手动触发 / 列表 / 下载。
//
// 设计约束：
//   - 备份范围刻意不含 config.json（内有 api_key 明文）：凭据泄露面最小化，
//     配置恢复走版本管理/手工（README 说明）；
//   - 打包前 Pool.Flush()：把内存态 state 强制落盘，避免备份到几分钟前的旧状态
//     （flusher 默认 5s 周期，通常已落，Flush 只是兜底）；
//   - 触发为独立轻量循环（10 分钟粒度检查「整点命中且当日未跑」），不并入
//     scheduler 的整点任务体系——备份与签到类任务无资源竞争，独立设置独立
//     热改（SetBackupSettings），重启后 lastRunDay 重置最多导致当天多跑一次
//     （文件名带时间戳，幂等无害）；
//   - BackupDir 必须落在持久化卷内（main 按 state_file 同目录推导 data/backups）。

import (
	"archive/zip"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// backupSettings 备份设置（main 装配 + 配置页热改）。
type backupSettings struct {
	mu      sync.Mutex
	enabled bool
	hour    int  // 每日触发整点（本地时区，0-23）
	keep    int  // 保留份数（<=0 = 不轮转）
	lastDay string // 最近一次自动触发的本地日期 "20060102"（手动触发不记录）
}

// SetBackupSettings 热更新备份设置（saveConfig 调用）。
func (p *Panel) SetBackupSettings(enabled bool, hour, keep int) {
	p.backupMu.Lock()
	defer p.backupMu.Unlock()
	p.backup.enabled = enabled
	if hour >= 0 && hour <= 23 {
		p.backup.hour = hour
	}
	p.backup.keep = keep
}

// backupSnapshot 读当前设置快照。
func (p *Panel) backupSnapshot() (enabled bool, hour, keep int) {
	p.backupMu.Lock()
	defer p.backupMu.Unlock()
	return p.backup.enabled, p.backup.hour, p.backup.keep
}

// StartBackupLoop 每日定时备份循环：10 分钟粒度唤醒，命中配置整点且当日未跑
// 才执行。ctx 取消即退出（不阻塞停机）。
func (p *Panel) StartBackupLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			enabled, hour, _ := p.backupSnapshot()
			if !enabled || now.Hour() != hour {
				continue
			}
			today := now.Format("20060102")
			p.backupMu.Lock()
			ran := p.backup.lastDay == today
			if !ran {
				p.backup.lastDay = today
			}
			p.backupMu.Unlock()
			if ran {
				continue
			}
			if name, err := p.RunBackupNow(); err != nil {
				log.Printf("panel: 定时备份失败: %v（明日 %d 点重试）", err, hour)
			} else {
				log.Printf("panel: 定时备份完成: %s", name)
			}
		}
	}
}

// RunBackupNow 执行一次备份，返回 zip 文件名。
func (p *Panel) RunBackupNow() (string, error) {
	if p.cfg.BackupDir == "" {
		return "", fmt.Errorf("backup dir not configured")
	}
	if err := os.MkdirAll(p.cfg.BackupDir, 0o700); err != nil {
		return "", fmt.Errorf("create backup dir: %w", err)
	}
	// state.json 取最新内存态（无 state_file 配置的裸测试跳过）。
	p.cfg.Pool.Flush()

	// 命名 = 当前时刻（毫秒精度），冲突（同毫秒连点 / CI 高速循环）时外推 +i 毫秒
	// 重试：文件名恒唯一，且**字典序 = 生成序**——同精度文件系统（NTFS 100ns）
	// 下 mtime 可能相等，此时名字序兜底依然正确（列表轮转均依赖该不变量）。
	dst := ""
	name := ""
	for i := 0; ; i++ {
		ts := time.Now().Add(time.Duration(i) * time.Millisecond).Format("20060102-150405.000")
		cand := filepath.Join(p.cfg.BackupDir, fmt.Sprintf("wb2api-backup-%s.zip", ts))
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			name = filepath.Base(cand)
			dst = cand
			break
		}
	}
	if err := writeBackupZip(dst, p.cfg.AuthDir, p.cfg.StateFile); err != nil {
		return "", err
	}
	p.rotateBackups()
	return name, nil
}

// writeBackupZip 打包 auth 目录全部凭证 + state 文件。条目内文件权限 0600，
// 与源文件口径一致（zip 无加密，备份目录本身依赖文件系统权限保护）。
func writeBackupZip(dst, authDir, stateFile string) error {
	files, err := filepath.Glob(filepath.Join(authDir, "workbuddy*.json"))
	if err != nil {
		return fmt.Errorf("glob auths: %w", err)
	}
	if stateFile != "" {
		if _, err := os.Stat(stateFile); err == nil {
			files = append(files, stateFile)
		}
	}
	if len(files) == 0 {
		return fmt.Errorf("nothing to backup (auths 为空且 state 不存在)")
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create zip: %w", err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			zw.Close()
			_ = os.Remove(dst)
			return fmt.Errorf("read %s: %w", f, err)
		}
		// state.json 与 auth 文件可能同名冲突（手工把 state 放 auths 的怪配置）：
		// state 用 data/ 前缀隔离。
		name := filepath.Base(f)
		if stateFile != "" && f == stateFile {
			name = "data/" + name
		}
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()}
		hdr.SetMode(0o600)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			zw.Close()
			_ = os.Remove(dst)
			return fmt.Errorf("zip entry %s: %w", name, err)
		}
		if _, err := w.Write(raw); err != nil {
			zw.Close()
			_ = os.Remove(dst)
			return fmt.Errorf("zip write %s: %w", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close zip: %w", err)
	}
	return nil
}

// rotateBackups 按文件名排序（时间戳命名天然有序）保留最近 keep 份。
func (p *Panel) rotateBackups() {
	_, _, keep := p.backupSnapshot()
	if keep <= 0 {
		return
	}
	entries, err := os.ReadDir(p.cfg.BackupDir)
	if err != nil {
		return
	}
	var zips []string
	for _, e := range entries {
		if !e.IsDir() && validBackupName(e.Name()) {
			zips = append(zips, e.Name())
		}
	}
	if len(zips) <= keep {
		return
	}
	sort.Strings(zips) // 升序；最旧在前
	for _, name := range zips[:len(zips)-keep] {
		if err := os.Remove(filepath.Join(p.cfg.BackupDir, name)); err == nil {
			log.Printf("panel: backup rotate 删除旧备份 %s（保留 %d 份）", name, keep)
		}
	}
}

// validBackupName 校验备份文件名（防路径穿越；仅放行本服务产出的命名）。
func validBackupName(name string) bool {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return false
	}
	return strings.HasPrefix(name, "wb2api-backup-") && strings.HasSuffix(name, ".zip")
}

// backupRun 手动触发备份（同步执行：账号文件个位数、zip 毫秒级，同步返回更确定）。
//
//	POST /panel/api/backup/run
func (p *Panel) backupRun(w http.ResponseWriter, r *http.Request) {
	if p.cfg.BackupDir == "" {
		writeErr(w, http.StatusNotImplemented, "backup not configured")
		return
	}
	name, err := p.RunBackupNow()
	if err != nil {
		log.Printf("panel: 手动备份失败: %v", err)
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: 手动备份完成: %s", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

// backupList 备份文件列表（新→旧）。
//
//	GET /panel/api/backup/list
func (p *Panel) backupList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.BackupDir == "" {
		writeErr(w, http.StatusNotImplemented, "backup not configured")
		return
	}
	entries, err := os.ReadDir(p.cfg.BackupDir)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": []any{}})
			return
		}
		writeErr(w, http.StatusInternalServerError, "read backup dir: "+err.Error())
		return
	}
	type backupItem struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		MTime int64 `json:"mtime"`
	}
	var items []backupItem
	var times []time.Time
	for _, e := range entries {
		if e.IsDir() || !validBackupName(e.Name()) {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, backupItem{Name: e.Name(), Size: st.Size(), MTime: st.ModTime().Unix()})
		times = append(times, st.ModTime())
	}
	// 新→旧按完整 mtime 排序（秒级 Unix 截断分不出同秒文件；同毫秒冲突后缀的
	// 文件名序也 ≠ 时间序）。times 与 items 同序。
	idx := make([]int, len(items))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		i, j := idx[a], idx[b]
		if !times[i].Equal(times[j]) {
			return times[i].After(times[j])
		}
		return items[i].Name > items[j].Name
	})
	sorted := make([]backupItem, len(items))
	for a, i := range idx {
		sorted[a] = items[i]
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": sorted})
}

// backupDownload 下载备份 zip（Content-Disposition 附件，便于把备份拿出容器）。
//
//	GET /panel/api/backup/{name}/download
func (p *Panel) backupDownload(w http.ResponseWriter, r *http.Request) {
	if p.cfg.BackupDir == "" {
		writeErr(w, http.StatusNotImplemented, "backup not configured")
		return
	}
	name := r.PathValue("name")
	if !validBackupName(name) {
		writeErr(w, http.StatusBadRequest, "invalid backup name")
		return
	}
	fp := filepath.Join(p.cfg.BackupDir, name)
	f, err := os.Open(fp)
	if err != nil {
		writeErr(w, http.StatusNotFound, "backup not found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "stat: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, st.ModTime(), f)
}
