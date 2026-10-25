package panel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
)

// cockpitAccount 映射 cockpit tools 导出格式的单个账号。
type cockpitAccount struct {
	ID           string `json:"id"`
	Email        string `json:"email"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	// Realm 显式账号域（export.go 同口径；空 = 无显式标识，按 domain 回落推断）。
	// DeviceToken 设备风控 Token（导出往返保留，缺省空 = 不注入该头）。
	Realm       string `json:"realm"`
	DeviceToken string `json:"device_token"`
	// Tags 标签（cockpit WorkbuddyAccount.tags 口径），导入后落盘 account.tags。
	Tags           []string `json:"tags"`
	DosageNotify   string   `json:"dosage_notify_code"`
	PaymentType    string   `json:"payment_type"`
	Status         string   `json:"status"`
	UsageUpdatedAt int64    `json:"usage_updated_at"`
	LastCheckin    int64    `json:"last_checkin_time"`
	CheckinStreak  int      `json:"checkin_streak"`
	CreatedAt      int64    `json:"created_at"`
	LastUsed       int64    `json:"last_used"`
}

// importCockpit 接收账号 JSON 文件（多文件 / zip 包）批量导入账号到池中。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json|zip>（字段可重复，多文件一次请求）
//
// 兼容负载形状（1.14.2 起逐文件/逐元素嗅探）：
//   - cockpit 扁平数组 [{"uid":...,"access_token":...}]（毫秒，既有口径）
//   - 扁平单对象；嵌套形 {"account":{...},"auth":{...}}（落盘/插件输出，秒）单对象或数组
//   - 以上形状的混合数组；zip 包自动解包内部 *.json（跳过目录 / __MACOSX / ._ 前缀 / 非 json）
//
// 错误语义：整个请求只有一个负载单元时硬错误直接 400（兼容既有单文件行为）；
// 多负载（zip / 多文件）时单个负载失败只记 errors 不阻断其余。
// 返回 {ok, total, imported, skipped, errors}。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	fhs := r.MultipartForm.File["file"]
	if len(fhs) == 0 {
		writeErr(w, http.StatusBadRequest, "missing file field")
		return
	}

	// 展开阶段：每个上传文件 → 若干负载单元（zip 解包；读取/结构失败成为 Err 单元）。
	var units []payloadFile
	for _, fh := range fhs {
		f, err := fh.Open()
		if err != nil {
			units = append(units, payloadFile{Name: fh.Filename, Err: err})
			continue
		}
		raw, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			units = append(units, payloadFile{Name: fh.Filename, Err: err})
			continue
		}
		fs, err := expandPayloadFile(fh.Filename, raw)
		if err != nil {
			units = append(units, payloadFile{Name: fh.Filename, Err: err})
			continue
		}
		units = append(units, fs...)
	}

	// 解析阶段：单单元请求保留"硬错误直接 400"的既有语义（非 zip 单文件、仅含一个
	// 条目的 zip）；多单元时单个负载失败降级为 errors 条目，不阻断其余。
	single := len(units) == 1
	var errs []string
	var pending []cockpitAccount
	skippedElems := 0
	for _, u := range units {
		if u.Err != nil {
			if single {
				writeErr(w, http.StatusBadRequest, u.Name+": "+u.Err.Error())
				return
			}
			errs = append(errs, u.Name+": "+u.Err.Error())
			continue
		}
		accts, elemErrs, err := parseImportPayload(u.Data)
		if err != nil {
			if single {
				writeErr(w, http.StatusBadRequest, u.Name+": "+err.Error())
				return
			}
			errs = append(errs, u.Name+": "+err.Error())
			continue
		}
		pending = append(pending, accts...)
		for _, e := range elemErrs {
			errs = append(errs, u.Name+": "+e)
		}
		skippedElems += len(elemErrs)
	}
	if len(pending) == 0 && skippedElems == 0 && len(errs) == 0 {
		writeErr(w, http.StatusBadRequest, "empty accounts array")
		return
	}

	var total, imported, skipped int

	for _, acc := range pending {
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
					logx.Warnf("panel: import global 注册激活 uid=%s: %v", uid, err)
				} else if activated {
					logx.Infof("panel: import global 注册激活 uid=%s 完成", uid)
				}
				if claimed, err := p.cfg.Upstream.ClaimTrial(a); err != nil {
					logx.Warnf("panel: import global trial uid=%s: %v", uid, err)
				} else if claimed {
					logx.Infof("panel: import global trial uid=%s 已领", uid)
				}
			} else {
				if err := p.cfg.Upstream.DailyCheckin(a); err != nil {
					logx.Warnf("panel: import checkin uid=%s: %v", uid, err)
				}
			}
			if rm, tt, err := p.cfg.Upstream.UserResource(a); err == nil {
				p.cfg.Pool.ReenableIfCredits(uid, rm, tt)
			}
		}

		imported++
	}

	total = len(pending) + skippedElems
	skipped += skippedElems
	logx.Infof("panel: cockpit import finished total=%d imported=%d skipped=%d", total, imported, skipped)
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

// maxImportEntry zip 内单个 *.json 条目读取上限（防 zip bomb；整包总量由
// ParseMultipartForm 的 32MB 上限兜底）。
const maxImportEntry = 4 << 20

// payloadFile 一个待解析的 JSON 负载单元（来源名 + 字节）。
// Err 非 nil：该单元在展开/读取阶段失败，调用方记错跳过。
type payloadFile struct {
	Name string
	Data []byte
	Err  error
}

// expandPayloadFile 展开上传的单个文件为若干 JSON 负载单元：zip 包（PK magic 探测）
// 解包内部 *.json（跳过目录条目、__MACOSX/ 与 ._ 前缀、非 .json 文件），其余原样返回
// 单单元。zip 结构无效或包内无 json → 整体错误。
func expandPayloadFile(name string, raw []byte) ([]payloadFile, error) {
	if !looksLikeZip(raw) {
		return []payloadFile{{Name: name, Data: raw}}, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("invalid zip: %w", err)
	}
	var out []payloadFile
	for _, f := range zr.File {
		base := path.Base(f.Name)
		if f.FileInfo().IsDir() ||
			strings.HasPrefix(f.Name, "__MACOSX/") ||
			strings.HasPrefix(base, "._") ||
			!strings.HasSuffix(strings.ToLower(f.Name), ".json") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			out = append(out, payloadFile{Name: f.Name, Err: err})
			continue
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxImportEntry))
		rc.Close()
		if err != nil {
			out = append(out, payloadFile{Name: f.Name, Err: err})
			continue
		}
		out = append(out, payloadFile{Name: f.Name, Data: data})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("zip contains no .json files")
	}
	return out, nil
}

// looksLikeZip 判断内容是否为 zip（本地文件头 / 空档记录两种 magic）。
func looksLikeZip(raw []byte) bool {
	return bytes.HasPrefix(raw, []byte("PK\x03\x04")) || bytes.HasPrefix(raw, []byte("PK\x05\x06"))
}

// parseImportPayload 嗅探导入负载的形状并归一化为 cockpit 导入口径。
//
// 兼容形状（1.14.2 起）：
//   - cockpit 扁平数组 [{"uid":...,"access_token":...}]（毫秒）
//   - 扁平单对象 / 嵌套形 {"account":{...},"auth":{...}}（落盘/插件输出，秒）单对象
//   - 嵌套形与 cockpit 扁平条目的混合数组
//
// 语义：整个负载不可用（语法坏 / 空 / 顶层无法识别）→ err（单单元语境 400）；
// 数组内单个元素不可用 → elemErrs（调用方记错并计 skipped），其余元素照常导入。
func parseImportPayload(raw []byte) (accts []cockpitAccount, elemErrs []string, err error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil, fmt.Errorf("empty payload")
	}
	switch trimmed[0] {
	case '{':
		acc, err := classifyAccountEntry(trimmed)
		if err != nil {
			return nil, nil, err
		}
		return []cockpitAccount{acc}, nil, nil
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(trimmed, &elems); err != nil {
			return nil, nil, fmt.Errorf("invalid json: %w", err)
		}
		for i, elem := range elems {
			acc, err := classifyAccountEntry(elem)
			if err != nil {
				elemErrs = append(elemErrs, fmt.Sprintf("entry %d: %v", i, err))
				continue
			}
			accts = append(accts, acc)
		}
		return accts, elemErrs, nil
	default:
		return nil, nil, fmt.Errorf("unrecognized format: expected JSON object or array")
	}
}

// classifyAccountEntry 识别单个账号条目的形状：嵌套形 / 扁平 camelCase 形走
// auth.Parse（落盘口径，秒），cockpit 扁平 snake_case 形直接反序列化（毫秒）。
func classifyAccountEntry(raw json.RawMessage) (cockpitAccount, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return cockpitAccount{}, fmt.Errorf("not a JSON object")
	}
	_, hasNested := probe["auth"]
	_, hasCamel := probe["accessToken"]
	if hasNested || hasCamel {
		a, err := auth.Parse(raw)
		if err != nil {
			return cockpitAccount{}, err
		}
		return nestedToCockpit(a), nil
	}
	_, hasUID := probe["uid"]
	_, hasSnake := probe["access_token"]
	if !hasUID && !hasSnake {
		return cockpitAccount{}, fmt.Errorf("unrecognized format: no account fields")
	}
	var acc cockpitAccount
	if err := json.Unmarshal(raw, &acc); err != nil {
		return cockpitAccount{}, err
	}
	return acc, nil
}

// nestedToCockpit 把 auth.Parse 出的嵌套/扁平 camelCase 账号映射为 cockpit 导入口径。
// 落盘 expiresAt 为秒，×1000 对齐 cockpit 毫秒（≥1e12 视为已是毫秒不换算）；
// realm 取 RealmStored 显式持久值（空则由导入主循环按 domain 回落推断）。
func nestedToCockpit(a *auth.Auth) cockpitAccount {
	expires := a.ExpiresAt
	if expires > 0 && expires < 1e12 {
		expires *= 1000
	}
	return cockpitAccount{
		ID:           "workbuddy_" + a.UID,
		UID:          a.UID,
		Nickname:     a.Nickname,
		AccessToken:  a.AccessTokenValue(),
		RefreshToken: a.RefreshTokenValue(),
		Domain:       a.DomainValue(),
		Realm:        a.RealmStored(),
		ExpiresAt:    expires,
		Tags:         a.TagsValue(),
		DeviceToken:  a.DeviceTokenValue(),
	}
}
