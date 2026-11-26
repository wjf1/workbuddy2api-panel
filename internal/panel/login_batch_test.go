package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// newBatchTestPanel 批量纳管测试用的最小 Panel：临时 AuthDir、空 Pool、
// Upstream 走「签到/余额全成功」的假 Transport（finalizeLogin 的顺带调用不阻塞）。
func newBatchTestPanel(t *testing.T) *Panel {
	t.Helper()
	dir := t.TempDir()
	c := &upstream.Client{
		ChatBaseCN:    "https://chat.fake.example",
		BillingBaseCN: "https://billing.fake.example",
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":0,"msg":"","data":{"remain":100,"total":1000}}`)),
			}, nil
		})},
	}
	return New(Config{
		Version: "test", APIKey: "k",
		AuthDir:  dir,
		Pool:     pool.New(""),
		Upstream: c,
	})
}

// mockOAuthServer 模拟设备授权上游三端点：state 发号、token 按 state 回包、
// account 回 uid/nickname。tokenDone 控制 state 是否「已完成扫码」。
type mockOAuthServer struct {
	t *testing.T
	// issued 已发出的 state 计数（batch start 的发起数断言用）。
	issued atomic.Int64
	// doneStates 已完成授权的 state 集（token 端点对它们回 accessToken）。
	doneStates map[string]bool
	srv        *httptest.Server
}

func newMockOAuthServer(t *testing.T, doneStates map[string]bool) *mockOAuthServer {
	m := &mockOAuthServer{t: t, doneStates: doneStates}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
			n := m.issued.Add(1)
			st := fmt.Sprintf("state-%d", n)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "",
				"data": map[string]any{"state": st, "authUrl": "https://example.com/auth?state=" + st},
			})
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token"):
			st := r.URL.Query().Get("state")
			if !m.doneStates[st] {
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 1001, "msg": "login ing"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "",
				"data": map[string]any{
					"accessToken": "at-" + st, "refreshToken": "rt-" + st,
					"expiresIn": 3600, "domain": "example.com",
				},
			})
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/login/account"):
			st := r.URL.Query().Get("state")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "",
				"data": map[string]any{"uid": "uid-" + st, "nickname": "nick-" + st},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// withLoginUpstream 把 OAuth 上游基址替换为 mock，测试结束还原。
func withLoginUpstream(t *testing.T, url string) {
	t.Helper()
	old := loginBaseURL
	loginBaseURL = func(string) string { return url }
	t.Cleanup(func() { loginBaseURL = old })
}

// TestLoginBatchStart 批量发起：count 个会话挂同一 batch，状态写入 logins。
func TestLoginBatchStart(t *testing.T) {
	mock := newMockOAuthServer(t, nil)
	withLoginUpstream(t, mock.srv.URL)

	p := newBatchTestPanel(t)
	body := strings.NewReader(`{"realm":"cn","count":3}`)
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login/batch/start", body)
	rec := httptest.NewRecorder()
	p.loginBatchStart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Batch    string `json:"batch"`
		Sessions []struct {
			State string `json:"state"`
			URL   string `json:"url"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Sessions) != 3 {
		t.Fatalf("sessions=%d want 3", len(resp.Sessions))
	}
	p.loginMu.Lock()
	n := 0
	for _, s := range resp.Sessions {
		if sess, ok := p.logins[s.State]; ok && sess.batch == resp.Batch {
			n++
		}
	}
	p.loginMu.Unlock()
	if n != 3 {
		t.Fatalf("logins 中 batch=%s 的会话数=%d want 3", resp.Batch, n)
	}
}

// TestLoginBatchStartCountLimit count 超上限直接 400。
func TestLoginBatchStartCountLimit(t *testing.T) {
	p := newBatchTestPanel(t)
	body := strings.NewReader(fmt.Sprintf(`{"count":%d}`, loginBatchMax+1))
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login/batch/start", body)
	rec := httptest.NewRecorder()
	p.loginBatchStart(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
}

// TestLoginBatchPoll 聚合轮询：完成态会话经 finalizeLogin 落盘并从 logins 移除，
// 未完成态保持 pending。全部完成后批次响应 items 为空。
func TestLoginBatchPoll(t *testing.T) {
	mock := newMockOAuthServer(t, map[string]bool{"state-2": true}) // 只有第 2 个完成
	withLoginUpstream(t, mock.srv.URL)

	p := newBatchTestPanel(t)
	// 直接种两个会话进 logins（绕过 start，专注 poll 聚合语义）。
	p.logins["state-1"] = loginSession{created: time.Now(), realm: "cn", batch: "b1"}
	p.logins["state-2"] = loginSession{created: time.Now(), realm: "cn", batch: "b1"}

	req := httptest.NewRequest(http.MethodGet, "/panel/api/login/batch/poll?batch=b1", nil)
	rec := httptest.NewRecorder()
	p.loginBatchPoll(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []struct {
			State  string `json:"state"`
			Status string `json:"status"`
			UID    string `json:"uid"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("items=%d want 2: %s", len(resp.Items), rec.Body.String())
	}
	byState := map[string]string{}
	for _, it := range resp.Items {
		byState[it.State] = it.Status
	}
	if byState["state-1"] != "pending" {
		t.Fatalf("state-1 status=%s want pending", byState["state-1"])
	}
	if byState["state-2"] != "done" {
		t.Fatalf("state-2 status=%s want done", byState["state-2"])
	}
	// 完成的会话已从 logins 移除（finalizeLogin 回收），未完成保留。
	p.loginMu.Lock()
	_, has1 := p.logins["state-1"]
	_, has2 := p.logins["state-2"]
	p.loginMu.Unlock()
	if !has1 {
		t.Fatal("pending 会话 state-1 不应被回收")
	}
	if has2 {
		t.Fatal("done 会话 state-2 应被 finalizeLogin 回收")
	}
	// 落盘文件存在（finalizeLogin 的 SaveAtomic 生效）。
	if _, err := os.Stat(filepath.Join(p.cfg.AuthDir, "workbuddy-uid-state-2.json")); err != nil {
		t.Fatalf("落盘凭证缺失: %v", err)
	}
}
