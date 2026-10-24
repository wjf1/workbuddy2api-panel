package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestDefaultSnapshotGolden 默认值对账快照（审查发现的漂移防线）：Default() 经
// normalize 后的完整配置与 testdata/default_snapshot.json 逐字节比对。
//
// 任何字段的默认值变化、新增字段（零值或设默认）都会让本测试红——更新流程：
//  1. 确认默认值变更是有意的；
//  2. **对照检查 normalize() 里的同字段兜底是否同步**（这是本测试的核心目的：
//     Default 写了、normalize 漏改 = 分支行为漂移，面板保存一次就触发）；
//  3. GOGEN_GOLDEN=1 go test ./cmd/server/ -run TestDefaultSnapshotGolden 重新生成。
func TestDefaultSnapshotGolden(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GOGEN_GOLDEN") != "" {
		if err := os.WriteFile("testdata/default_snapshot.json", append(raw, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	golden, err := os.ReadFile("testdata/default_snapshot.json")
	if err != nil {
		t.Fatalf("golden 缺失（GOGEN_GOLDEN=1 go test 生成）: %v", err)
	}
	if string(golden) == string(raw)+"\n" {
		return
	}
	// 逐行 diff 定位变化字段（比整体不匹配可操作）。
	g, n := strings.Split(strings.TrimRight(string(golden), "\n"), "\n"), strings.Split(string(raw), "\n")
	for i := 0; i < len(g) || i < len(n); i++ {
		var a, b string
		if i < len(g) {
			a = g[i]
		}
		if i < len(n) {
			b = n[i]
		}
		if a != b {
			t.Errorf("第 %d 行默认值变化:\n  golden: %s\n  当前:   %s\n（若有意变更：同步检查 normalize() 同字段兜底后，GOGEN_GOLDEN=1 重新生成）", i+1, a, b)
		}
	}
}
