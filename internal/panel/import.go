package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	UID           string `json:"uid"`
	Nickname      string `json:"nickname"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	TokenType     string `json:"token_type"`
	ExpiresAt     int64  `json:"expires_at"`
	Domain        string `json:"domain"`
	// Realm 显式账号域（export.go 同口径；空 = 无显式标识，按 domain 回落推断）。
	// DeviceToken 设备风控 Token（导出往返保留，缺省空 = 不注入该头）。
	Realm         string   `json:"realm"`
	DeviceToken   string   `json:"device_token"`
	// Tags 标签（cockpit WorkbuddyAccount.tags 口径），导入后落盘 account.tags。
	Tags          []string `json:"tags"`
	DosageNotify  string   `json:"dosage_notify_code"`
	PaymentType   string   `json:"payment_type"`
	Status        string   `json:"status"`
	UsageUpdatedAt int64   `json:"usage_updated_at"`
	LastCheckin   int64    `json:"last_checkin_time"`
	CheckinStreak int      `json:"checkin_streak"`
	CreatedAt     int64    `json:"created_at"`
	LastUsed      int64    `json:"last_used"`
}

// importCockpit 接收 cockpit tools 导出的 JSON 文件，批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	var accounts []cockpitAccount
	if err := json.Unmarshal(raw, &accounts); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(accounts) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var total, imported, skipped int
	var errs []string

	for _, acc := range accounts {
		uid := strings.TrimSpace(acc.UID)
		at := strings.TrimSpace(acc.AccessToken)
		rt := strings.TrimSpace(acc.RefreshToken)
		if uid == "" || at == "" || rt == "" {
			skipped++
			errs = append(errs, fmt.Sprintf("missing required fields (id=%s)", acc.ID))
			continue
		}
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, fmt.Sprintf("invalid uid (id=%s)", acc.ID))
			continue
		}

		// realm：显式标识优先（export.go 同口径导出 RealmStored），空才按 domain
		// 推断——显式与推断不一致的账号（如手工标 global 的 CN 域）往返不"搬家"。
		explicitRealm := strings.TrimSpace(acc.Realm)
		realm := auth.ResolveRealm(explicitRealm, acc.Domain)

		// cockpit tools 的 expires_at 为毫秒时间戳，转为秒。
		expiresAt := acc.ExpiresAt / 1000
		if expiresAt <= 0 {
			expiresAt = time.Now().Add(365 * 24 * time.Hour).Unix()
		}

		nickname := acc.Nickname
		if strings.TrimSpace(nickname) == "" {
			nickname = acc.Email
		}

		a := &auth.Auth{
			AccessToken:  at,
			RefreshToken: rt,
			ExpiresAt:    expiresAt,
			Domain:       acc.Domain,
			UID:          uid,
			Nickname:     nickname,
			DeviceToken:  strings.TrimSpace(acc.DeviceToken),
			FilePath:     filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid)),
		}
		a.SetTags(acc.Tags)

		if explicitRealm != "" {
			if _, err := auth.BackfillRealmFor(a, realm); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			_, _ = a.BackfillRealm()
		}

		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}

		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)

		// 顺带签到/激活（幂等；失败仅记日志，不阻断导入）。Upstream 为 nil 的裸
		// 装配（测试面板）跳过副作用——导入的核心语义是凭据落盘 + 入池。
		if p.cfg.Upstream != nil {
			if realm == "global" {
				if activated, err := p.cfg.Upstream.GlobalCompleteRegistration(a); err != nil {
					log.Printf("panel: import global 注册激活 uid=%s: %v", uid, err)
				} else if activated {
					log.Printf("panel: import global 注册激活 uid=%s 完成", uid)
				}
				if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
					log.Printf("panel: import global trial uid=%s: %v", uid, err)
				} else if claimed {
					log.Printf("panel: import global trial uid=%s 已领", uid)
				}
			} else {
				if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
					log.Printf("panel: import checkin uid=%s: %v", uid, err)
				}
			}
			if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
				p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
			}
		}

		imported++
	}

	total = len(accounts)
	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"total":    total,
		"imported": imported,
		"skipped":  skipped,
		"errors":   errs,
	})
}

// validImportUID 校验导入 uid 是否可用于拼文件名（同 login.go validUID 口径）。
func validImportUID(uid string) bool {
	if uid == "" || len(uid) > 64 {
		return false
	}
	for _, c := range uid {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
