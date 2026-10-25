package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// readRevision 读本地 state.json 里落盘的 revision。
func readRevision(t *testing.T, fp string) int64 {
	t.Helper()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("解析 state.json: %v", err)
	}
	return sf.Revision
}

// TestRevisionIncrementsOnFlush 每次落盘 revision +1 并写进文件。
// 这是"不靠时钟排序"的基础：序号必须严格单调。
func TestRevisionIncrementsOnFlush(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	defer p.Close()

	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0) // 置 dirty：Add 本身不标记变更，Flush 会短路不写盘
	p.Flush()
	if got := readRevision(t, fp); got != 1 {
		t.Fatalf("首次落盘 revision 应为 1，got %d", got)
	}

	p.SetCredits("u1", 100, 0) // 置 dirty
	p.Flush()
	if got := readRevision(t, fp); got != 2 {
		t.Fatalf("二次落盘 revision 应为 2，got %d", got)
	}

	// 无变更时 Flush 不写盘（persistState 的 dirty 短路），revision 不该空转。
	p.Flush()
	if got := readRevision(t, fp); got != 2 {
		t.Fatalf("无变更时不应推进 revision，got %d", got)
	}
}

// TestRevisionCarriedIntoSnapshot Redis 快照必须与本地文件带同一个 revision——
// 否则两侧排序会各说各话。
func TestRevisionCarriedIntoSnapshot(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	defer p.Close()
	p.SetStore(&memStore{})

	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42, 0) // 置 dirty
	p.Flush()
	fileRev := readRevision(t, fp)
	if fileRev == 0 {
		t.Fatal("文件里应有 revision")
	}

	ms := p.store.(*memStore)
	ms.mu.Lock()
	raw := append([]byte(nil), ms.saved...)
	ms.mu.Unlock()
	var snap struct {
		Revision int64     `json:"revision"`
		SavedAt  time.Time `json:"saved_at"`
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("快照解析失败: %v", err)
	}
	if snap.Revision != fileRev {
		t.Fatalf("快照 revision(%d) 应与文件(%d) 一致", snap.Revision, fileRev)
	}
	if snap.SavedAt.IsZero() {
		t.Fatal("快照仍应带 saved_at")
	}
}

// TestRevisionResumesFromLocalFile 重启后从磁盘续上序号，不能从 0 重来
// ——否则恢复后的第一次落盘会"倒退"，日志里看起来像状态被回滚。
func TestRevisionResumesFromLocalFile(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":1}},"revision":7}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	defer p.Close()
	if p.revision != 7 {
		t.Fatalf("应从文件续上 revision=7，got %d", p.revision)
	}

	p.SetCredits("u1", 50, 0)
	p.Flush()
	if got := readRevision(t, fp); got != 8 {
		t.Fatalf("续写后应为 8，got %d", got)
	}
}

// TestRevisionLegacyFileStartsAtZero 旧版 state.json 没有 revision 字段 → 0，
// 首次落盘从 1 开始。行为与历史完全一致，不会因为加了字段就改变恢复判定。
func TestRevisionLegacyFileStartsAtZero(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	defer p.Close()
	if p.revision != 0 {
		t.Fatalf("旧文件应解析为 revision=0，got %d", p.revision)
	}
	p.SetCredits("u1", 60, 0)
	p.Flush()
	if got := readRevision(t, fp); got != 1 {
		t.Fatalf("首次落盘应为 1，got %d", got)
	}
}

// TestAdoptSnapshotKeepsRevision 从 Redis 快照恢复后序号要接上，不能归零。
func TestAdoptSnapshotKeepsRevision(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 本地文件存在但不带 revision（0），快照带 revision=9 → 采用快照并接上序号。
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	snapRaw, err := json.Marshal(snapshot{
		stateFile: stateFile{
			Accounts: map[string]stateAccount{"u1": {Credits: 2}},
			Revision: 9,
		},
		SavedAt: time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadData: snapRaw, loadOK: true}

	p := New(fp)
	defer p.Close()
	p.SetStore(ms)
	p.RestoreFromSnapshot()

	if p.revision != 9 {
		t.Fatalf("采纳快照后 revision 应接上 9，got %d", p.revision)
	}
	p.Flush()
	if got := readRevision(t, fp); got != 10 {
		t.Fatalf("恢复后首次落盘应为 10，got %d", got)
	}
}
