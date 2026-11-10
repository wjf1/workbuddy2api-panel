// admin_audit.go 管理操作审计：把 /admin 与面板的写操作追加一行 JSONL。
//
// 与 reqlog（请求流水）的分工：
//   - reqlog 记的是**推理请求**（/v1/*）的用量与耗时，用于成本与排障；
//   - 本文件记的是**管理操作**（补跑排程、改配置、增删账号、导出凭据），
//     用于事后追责：「谁在什么时候用哪把 key 干了什么、成没成」。
//
// 设计取舍：
//   - **缺省关闭**：路径为空即不启用，无行为变更；开启只需配一个路径。
//   - **写失败不让请求失败**：审计是旁路，磁盘满/权限错时只记日志，
//     业务操作照常生效（否则日志问题会变成可用性问题）。
//   - **不落 key 原文**：只落 sha256 前 4 字节指纹，能区分不同 key 但无法还原。
//   - 挂在鉴权之内（withAuth 之后），匿名请求不落盘——否则任何人都能刷日志。
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/logx"
)

// AuditLog 管理操作审计日志（追加写 JSONL）。
type AuditLog struct {
	path  string
	keyFP string
	mu    sync.Mutex
}

// NewAuditLog 打开（或创建）审计日志。
//
// 权限 0600：审计行含操作者 IP 与 key 指纹，属内部信息。
func NewAuditLog(path, apiKey string) (*AuditLog, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("audit log path is empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create audit log dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log %s: %w", path, err)
	}
	_ = f.Close()
	return &AuditLog{path: path, keyFP: keyFingerprint(apiKey)}, nil
}

// Path 审计日志路径（空 = 未启用）。
func (a *AuditLog) Path() string {
	if a == nil {
		return ""
	}
	return a.path
}

// keyFingerprint key 指纹（sha256 前 4 字节 hex）。
//
// 只用于区分「不同 key 的不同操作者」，不可逆推原文。
// 空 key（未启用鉴权）记为 "-"。
func keyFingerprint(apiKey string) string {
	if apiKey == "" {
		return "-"
	}
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:4])
}

// auditEntry 一行审计记录。
type auditEntry struct {
	TS     string `json:"ts"`
	Action string `json:"action"`
	Target string `json:"target"`
	Status int    `json:"status"`
	Remote string `json:"remote"`
	Key    string `json:"key"`
	Reason string `json:"reason,omitempty"`
}

// record 追加一行（失败只记日志，不影响调用方）。
func (a *AuditLog) record(e auditEntry) {
	if a == nil {
		return
	}
	e.Key = a.keyFP
	line, err := json.Marshal(e)
	if err != nil {
		logx.Errorf("[audit] marshal failed, entry not written (operation already applied): %v", err)
		return
	}
	line = append(line, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		logx.Errorf("[audit] open %s failed, entry not written (operation already applied): %v", a.path, err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		logx.Errorf("[audit] write %s failed, entry not written (operation already applied): %v", a.path, err)
	}
}

// auditBodyPeek 从请求体里读 reason 的字节上限。
//
// 只为取一个可选的 reason 字段；读多了会把大 body（配置保存、批量导入）
// 整个缓冲进内存。
const auditBodyPeek = 4 << 10

// audit 包装一个写操作 handler，记录操作结果。
//
// 必须挂在 withAuth **之内**（匿名请求不落盘）。
// h.cfg.Audit 为 nil 时原样返回 next（零开销、零行为变更）。
func (h *Handler) audit(action string, target func(*http.Request) string, next http.HandlerFunc) http.HandlerFunc {
	if h.cfg.Audit == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		reason := peekAuditReason(r)
		rec := &auditResponseWriter{ResponseWriter: w}
		next(rec, r)
		h.cfg.Audit.record(auditEntry{
			TS:     time.Now().Format(time.RFC3339),
			Action: action,
			Target: target(r),
			Status: rec.statusCode(),
			Remote: r.RemoteAddr,
			Reason: reason,
		})
	}
}

// auditPathValue target 取路径参数（如 /admin/tasks/{name}/run 的 name）。
func auditPathValue(field string) func(*http.Request) string {
	return func(r *http.Request) string { return r.PathValue(field) }
}

// auditConstant target 取固定串（无路径参数的端点，如配置保存）。
func auditConstant(s string) func(*http.Request) string {
	return func(*http.Request) string { return s }
}

// auditResponseWriter 记录最终状态码。
//
// 只实现 ResponseWriter 的最小集：若下游需要 Flusher/Hijacker（流式），
// 本包装会阻断——所以审计只用于**非流式**的管理端点。
type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *auditResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// peekAuditReason 从请求体里取可选的 "reason" 字段（运维备注）。
//
// 读完必须把 body 恢复给下游 handler，否则下游读到空 body。
func peekAuditReason(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	buf, _ := io.ReadAll(io.LimitReader(r.Body, auditBodyPeek))
	r.Body = auditBody{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
	var body struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(buf, &body) != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

// auditBody 把「已读出的前缀 + 剩余 body」拼回一个 ReadCloser。
type auditBody struct {
	io.Reader
	io.Closer
}
