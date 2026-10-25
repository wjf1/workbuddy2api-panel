package panel

// 回收站（软删除）：面板「移除账号」从硬删改为把 auth 文件移入 TrashDir，
// 可恢复 / 彻底删除；超过保留天数的文件自动清理。
//
// 设计约束：
//   - TrashDir 必须落在持久化卷内（main 按 state_file 同目录推导 data/trash），
//     Docker bind-mount 场景回收站数据才不会随容器销毁；
//   - 删除时刻记在文件 mtime（os.Chtimes），不引入额外元数据文件——trash 目录
//     保持「纯 json 文件」形状，肉眼可见、手工可救；
//   - 跨盘移动（os.Rename 失败）回退 copy+delete，auth 目录与 data 目录挂在不同
//     卷的部署也能工作；
//   - 未配置 TrashDir（空）时 accountRemove 保持既有硬删行为——测试面板与
//     旧装配零回归。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
)

// trashItem 回收站列表条目。
type trashItem struct {
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	DeletedAt int64  `json:"deleted_at"` // Unix 秒（文件 mtime，moveToTrash 时写入）
	PurgeAt   int64  `json:"purge_at"`   // 预计自动清理时刻（Unix 秒）；0 = 永久保留
}

// moveToTrash 把 auth 凭证文件移入回收站，返回回收站内的文件名。
// 冲突（同 uid 删除→重登→再删除）时追加时间戳保持 .json 结尾，
// 文件名仍匹配 LoadDir 的 workbuddy*.json glob 与 validTrashName 校验。
func (p *Panel) moveToTrash(a *auth.Auth) (string, error) {
	src := a.FilePath
	if src == "" {
		return "", fmt.Errorf("account has no file path")
	}
	if err := os.MkdirAll(p.cfg.TrashDir, 0o700); err != nil {
		return "", fmt.Errorf("create trash dir: %w", err)
	}
	base := filepath.Base(src)
	dst := filepath.Join(p.cfg.TrashDir, base)
	if _, err := os.Stat(dst); err == nil {
		dst = filepath.Join(p.cfg.TrashDir,
			strings.TrimSuffix(base, ".json")+"."+time.Now().Format("20060102-150405")+".json")
	}
	if err := moveFile(src, dst); err != nil {
		return "", err
	}
	// mtime = 删除时刻：sweepTrash 的保留期依据 + 列表展示，双用。
	now := time.Now()
	if err := os.Chtimes(dst, now, now); err != nil {
		logx.Infof("panel: trash %s set mtime: %v（不影响功能，仅清理时刻取创建时间）", dst, err)
	}
	return filepath.Base(dst), nil
}

// moveFile 移动文件：rename 优先（同盘零拷贝），跨盘（EXDEV / Windows 不同卷）
// 回退 copy+delete。src 消失即成功，两种路径都保证不留半份。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create dest: %w", err)
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}
	if err = out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("write dest: %w", err)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("remove source (copy 已完成): %w", err)
	}
	return nil
}

// validTrashName 校验回收站文件名：纯文件名（防路径穿越）、workbuddy- 前缀、
// .json 结尾——与 auth.LoadDir 的 glob 口径一致，杂项文件一律拒操作。
func validTrashName(name string) bool {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return false
	}
	return strings.HasPrefix(name, "workbuddy-") && strings.HasSuffix(name, ".json")
}

// trashList 回收站内容列表（按删除时间倒序）。
//
//	GET /panel/api/trash
func (p *Panel) trashList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TrashDir == "" {
		writeErr(w, http.StatusNotImplemented, "trash not configured")
		return
	}
	items, err := p.trashItems()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "items": items})
}

// trashItems 扫描回收站目录，组装列表（解析失败的文件也列出，uid 留空——
// 凭证损坏的文件同样需要用户决定恢复或清除）。
func (p *Panel) trashItems() ([]trashItem, error) {
	entries, err := os.ReadDir(p.cfg.TrashDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []trashItem{}, nil
		}
		return nil, fmt.Errorf("read trash dir: %w", err)
	}
	retention := p.trashRetention()
	out := make([]trashItem, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !validTrashName(e.Name()) {
			continue
		}
		fp := filepath.Join(p.cfg.TrashDir, e.Name())
		item := trashItem{Name: e.Name()}
		if st, err := os.Stat(fp); err == nil {
			item.DeletedAt = st.ModTime().Unix()
			if retention > 0 {
				item.PurgeAt = st.ModTime().Add(retention).Unix()
			}
		}
		if raw, err := os.ReadFile(fp); err == nil {
			if a, err := parseAuthForMeta(raw); err == nil {
				item.UID, item.Nickname = a.UID, a.Nickname
			}
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeletedAt > out[j].DeletedAt })
	return out, nil
}

// parseAuthForMeta 从回收站文件取 uid/nickname（复用 auth 包解析；调用方只取
// 元数据两个字段，凭证不外泄）。
func parseAuthForMeta(raw []byte) (*auth.Auth, error) {
	return auth.Parse(raw)
}

// trashRestore 恢复回收站账号：移回 auth 目录（规范名 workbuddy-<uid>.json）
// 并热加载进池。
//
//	POST /panel/api/trash/{name}/restore
func (p *Panel) trashRestore(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TrashDir == "" {
		writeErr(w, http.StatusNotImplemented, "trash not configured")
		return
	}
	name := r.PathValue("name")
	if !validTrashName(name) {
		writeErr(w, http.StatusBadRequest, "invalid trash name")
		return
	}
	src := filepath.Join(p.cfg.TrashDir, name)
	raw, err := os.ReadFile(src)
	if err != nil {
		writeErr(w, http.StatusNotFound, "trash file not found: "+err.Error())
		return
	}
	a, err := parseAuthForMeta(raw)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "parse trash file: "+err.Error())
		return
	}
	if strings.TrimSpace(a.UID) == "" {
		writeErr(w, http.StatusUnprocessableEntity, "trash file has no uid")
		return
	}
	dst := filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", a.UID))
	if _, err := os.Stat(dst); err == nil {
		writeErr(w, http.StatusConflict,
			"auths/ 已存在同 uid 账号文件（workbuddy-"+a.UID+".json），请先移除现账号再恢复")
		return
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "create auth dir: "+err.Error())
		return
	}
	if err := moveFile(src, dst); err != nil {
		writeErr(w, http.StatusInternalServerError, "restore move: "+err.Error())
		return
	}
	// 重新 Parse 校验 + 补 FilePath，再入池（与 import.go 同路径：Add + Revive）。
	a2, err := parseAuthForMeta(raw)
	if err != nil {
		// 理论不可达（刚 Parse 过）；兜底把文件留回原处描述给用户。
		writeErr(w, http.StatusInternalServerError, "re-parse restored file: "+err.Error())
		return
	}
	a2.FilePath = dst
	p.cfg.Pool.Add(a2)
	p.cfg.Pool.Revive(a2.UID)
	logx.Infof("panel: trash restore uid=%s -> %s", a2.UID, dst)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": a2.UID})
}

// trashPurge 彻底删除单个回收站文件（不可恢复，前端二次确认）。
//
//	POST /panel/api/trash/{name}/purge
func (p *Panel) trashPurge(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TrashDir == "" {
		writeErr(w, http.StatusNotImplemented, "trash not configured")
		return
	}
	name := r.PathValue("name")
	if !validTrashName(name) {
		writeErr(w, http.StatusBadRequest, "invalid trash name")
		return
	}
	if err := os.Remove(filepath.Join(p.cfg.TrashDir, name)); err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "purge: "+err.Error())
		return
	}
	logx.Infof("panel: trash purge %s", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// trashPurgeAll 清空回收站（仅 workbuddy-*.json，杂项文件不动）。
//
//	POST /panel/api/trash/purge_all
func (p *Panel) trashPurgeAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TrashDir == "" {
		writeErr(w, http.StatusNotImplemented, "trash not configured")
		return
	}
	entries, err := os.ReadDir(p.cfg.TrashDir)
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, "read trash dir: "+err.Error())
		return
	}
	removed := 0
	var errs []string
	for _, e := range entries {
		if e.IsDir() || !validTrashName(e.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(p.cfg.TrashDir, e.Name())); err != nil {
			errs = append(errs, e.Name()+": "+err.Error())
			continue
		}
		removed++
	}
	logx.Infof("panel: trash purge_all removed=%d errs=%d", removed, len(errs))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed, "errors": errs})
}

// trashRetention 保留期（config trash.retention_days；<=0 = 永久保留）。
// 经 Panel.trashRetentionDays 锁字段读取（配置页热改生效）。
func (p *Panel) trashRetention() time.Duration {
	p.trashMu.Lock()
	defer p.trashMu.Unlock()
	if p.trashRetentionDays <= 0 {
		return 0
	}
	return time.Duration(p.trashRetentionDays) * 24 * time.Hour
}

// StartTrashLoop 回收站过期清理：启动即清一次，此后每小时巡检。
// 单次 sweep 全程轻量（目录 stat + 按需 unlink），无需并发保护。
func (p *Panel) StartTrashLoop(ctx context.Context) {
	p.sweepTrash()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.sweepTrash()
		}
	}
}

// sweepTrash 按 mtime + retention 清理过期回收站文件。retention<=0（永久保留）时
// 直接返回——目录扫描都省掉。
func (p *Panel) sweepTrash() {
	retention := p.trashRetention()
	if p.cfg.TrashDir == "" || retention <= 0 {
		return
	}
	entries, err := os.ReadDir(p.cfg.TrashDir)
	if err != nil {
		return
	}
	deadline := time.Now().Add(-retention)
	for _, e := range entries {
		if e.IsDir() || !validTrashName(e.Name()) {
			continue
		}
		fp := filepath.Join(p.cfg.TrashDir, e.Name())
		st, err := os.Stat(fp)
		if err != nil || st.ModTime().After(deadline) {
			continue
		}
		if err := os.Remove(fp); err == nil {
			logx.Infof("panel: trash sweep 过期清理 %s（保留 %d 天）", e.Name(), int(retention/(24*time.Hour)))
		}
	}
}
