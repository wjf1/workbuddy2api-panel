package panel

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cockpitExportAccount 对齐 cockpit tools 的 WorkBuddy 账号序列化字段
// （jlcodes99/cockpit-tools `crates/cockpit-core/src/models/workbuddy.rs`
// 的 WorkbuddyAccount，serde snake_case 口径）：导出数组既能经 cockpit 的
// WorkBuddy「JSON 导入」回灌其账号库，也能再导入本面板（POST import/cockpit）。
// 本面板没有的运维字段（email / checkin_streak / dosage_notify_code 等）一律
// 省略——cockpit 侧均为 Option、缺失即 None，不影响其导入。
type cockpitExportAccount struct {
	// ID cockpit 侧账号标识。cockpit 按 uid（缺失时 email）重建 id 并去重，
	// 此处给 workbuddy_<uid> 仅作占位，保证其 WorkbuddyAccount.id 必填字段成立。
	ID string `json:"id"`
	// UID 上游账号唯一标识；cockpit 的 identity 去重键，必出。
	UID string `json:"uid,omitempty"`
	// Nickname 昵称；空则 cockpit 落 None、本面板导入时回落 email。
	Nickname string `json:"nickname,omitempty"`
	// AccessToken 必出：cockpit 与本面板的导入均以此判数据有效。
	AccessToken string `json:"access_token"`
	// RefreshToken 必出：无刷新凭证的账号导入后无法续期。
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	// ExpiresAt 毫秒时间戳（cockpit 口径；本面板落盘为 Unix 秒，导出时 ×1000）。
	// 无效（≤0）省略，两侧导入各有兜底。
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Domain    string `json:"domain,omitempty"`
}

// exportCockpit 把账号池导出为 cockpit tools 兼容 JSON（账号数组）。
//
//	GET /panel/api/export/cockpit?uids=uid1,uid2
//	uids 缺省导出全部；uids 全部未命中时返回空数组（不静默回落全量，
//	避免「想导单个却拿到整池」的误操作）。与 import/cockpit 互为逆操作。
//
// 凭据仅出现在响应体；日志只记条数，不落 token。响应带 Content-Disposition
// 附件头，浏览器下载与脚本 curl 均可直接存盘。
func (p *Panel) exportCockpit(w http.ResponseWriter, r *http.Request) {
	wanted := map[string]struct{}{}
	if q := strings.TrimSpace(r.URL.Query().Get("uids")); q != "" {
		for _, s := range strings.Split(q, ",") {
			if s = strings.TrimSpace(s); s != "" {
				wanted[s] = struct{}{}
			}
		}
	}

	statuses := p.cfg.Pool.List()
	accounts := make([]cockpitExportAccount, 0, len(statuses))
	for _, st := range statuses {
		if len(wanted) > 0 {
			if _, ok := wanted[st.UID]; !ok {
				continue
			}
		}
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		accounts = append(accounts, cockpitAccountFromAuth(a))
	}

	log.Printf("panel: cockpit export count=%d", len(accounts))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="workbuddy_accounts_%s.json"`, time.Now().Format("20060102_150405")))
	writeJSON(w, http.StatusOK, accounts)
}

// cockpitAccountFromAuth 把池内凭据转成 cockpit 导出格式。
// UID/Nickname/ExpiresAt 属于刷新线程可能写回的字段集合，经公开锁成组读取；
// token/domain 走自带锁的访问器。均不嵌套加锁，无死锁面。
func cockpitAccountFromAuth(a *auth.Auth) cockpitExportAccount {
	a.Lock()
	uid, nickname, expires := a.UID, a.Nickname, a.ExpiresAt
	a.Unlock()

	acc := cockpitExportAccount{
		ID:           "workbuddy_" + uid,
		UID:          uid,
		Nickname:     nickname,
		AccessToken:  a.AccessTokenValue(),
		RefreshToken: a.RefreshTokenValue(),
		TokenType:    "Bearer",
		Domain:       a.DomainValue(),
	}
	if expires > 0 {
		acc.ExpiresAt = expires * 1000
	}
	return acc
}
