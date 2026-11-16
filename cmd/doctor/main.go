// doctor.go — WorkBuddy2API Panel 自检命令（doctor）。
//
// 用法:
//
//	go run ./cmd/doctor                     # 默认读 ./config.json
//	go run ./cmd/doctor -config /path/config.json -json
//
// 输出：默认「人类可读摘要 + 末行单行 JSON」；-json 只输出 JSON（脚本用）。
// 退出码：0 = 全绿；1 = 存在 error 级问题；2 = 配置加载失败（连诊断都没法做）。
//
// 设计取舍：本命令**只读**——不刷 token、不改状态、不触发签到，可以对生产部署
// 直接跑。唯一的网络动作是可选的轻量连通性探测（/healthz 等价上游请求），
// 加 -offline 可完全跳过。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/cst"
	"github.com/linguo2625469/workbuddy2api-panel/internal/safefile"
)

const appVersion = "1.22.0-panel"

// 退出码。
const (
	exitOK      = 0
	exitProblem = 1
	exitFatal   = 2
)

// checkLevel 检查结果等级。
type checkLevel string

const (
	levelOK   checkLevel = "ok"
	levelWarn checkLevel = "warning"
	levelErr  checkLevel = "error"
)

// check 单条检查结果。
type check struct {
	Name   string     `json:"name"`
	Level  checkLevel `json:"level"`
	Detail string     `json:"detail"`
}

// report doctor 的完整报告。
type report struct {
	Service string  `json:"service"`
	Version string  `json:"version"`
	TS      int64   `json:"ts"`
	OK      bool    `json:"ok"`
	Checks  []check `json:"checks"`
	// Summary 按等级计数，便于脚本快速判定。
	Summary map[string]int `json:"summary"`
}

// doctorConfig doctor 关心的配置子集。
//
// 为什么不复用 cmd/server.Config：那是 main 包不可导入。这里只解析自检需要的键，
// 其余键忽略——doctor 的职责是「接线是否正确」，不是完整复现服务端加载语义。
type doctorConfig struct {
	Listen    string `json:"listen"`
	APIKey    string `json:"api_key"`
	AuthDir   string `json:"auth_dir"`
	StateFile string `json:"state_file"`
	Schedule  struct {
		CheckinHours    []int `json:"checkin_hours"`
		TravelHours     []int `json:"travel_hours"`
		ActivityHours   []int `json:"activity_hours"`
		KeepaliveHours  []int `json:"keepalive_hours"`
		BlackcatHours   []int `json:"blackcat_hours"`
		GrowthHours     []int `json:"growth_hours"`
		CheckinEnabled  bool  `json:"checkin_enabled"`
		TravelEnabled   bool  `json:"travel_enabled"`
		ActivityEnabled bool  `json:"activity_enabled"`
		KeepaliveEnable bool  `json:"keepalive_enabled"`
		BlackcatEnabled bool  `json:"blackcat_enabled"`
		GrowthEnabled   bool  `json:"growth_enabled"`
	} `json:"schedule"`
}

func main() {
	cfgPath := flag.String("config", "./config.json", "config.json 路径")
	jsonOnly := flag.Bool("json", false, "只输出 JSON（脚本用）")
	offline := flag.Bool("offline", false, "跳过网络探测（纯离线自检）")
	flag.Parse()

	rep := run(*cfgPath, *offline)

	if !*jsonOnly {
		printHuman(os.Stdout, rep)
	}
	out, err := json.Marshal(rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "doctor: 报告序列化失败: %v\n", err)
		os.Exit(exitFatal)
	}
	fmt.Println(string(out))

	switch {
	case !rep.OK:
		os.Exit(exitProblem)
	default:
		os.Exit(exitOK)
	}
}

// run 执行全部检查并汇总。
func run(cfgPath string, offline bool) report {
	rep := report{
		Service: "workbuddy2api-doctor",
		Version: appVersion,
		TS:      time.Now().Unix(),
		Summary: map[string]int{"ok": 0, "warning": 0, "error": 0},
	}

	cfg, cerr := loadConfig(cfgPath)
	if cerr != nil {
		rep.Checks = append(rep.Checks, check{
			Name: "config", Level: levelErr,
			Detail: fmt.Sprintf("配置加载失败（%s）: %v", cfgPath, cerr),
		})
		rep.Summary["error"]++
		rep.OK = false
		return rep
	}
	rep.Checks = append(rep.Checks, check{
		Name: "config", Level: levelOK,
		Detail: fmt.Sprintf("已加载 %s", cfgPath),
	})

	rep.Checks = append(rep.Checks, checkBinding(cfg))
	rep.Checks = append(rep.Checks, checkAuthDir(cfg)...)
	rep.Checks = append(rep.Checks, checkStateAndStamps(cfg)...)
	rep.Checks = append(rep.Checks, checkSchedule(cfg))

	if !offline {
		rep.Checks = append(rep.Checks, checkUpstream(cfg))
	}

	for _, c := range rep.Checks {
		rep.Summary[string(c.Level)]++
	}
	rep.OK = rep.Summary["error"] == 0
	return rep
}

// loadConfig 读配置；缺失时返回错误（doctor 不自动生成配置，避免误改现场）。
func loadConfig(path string) (*doctorConfig, error) {
	raw, err := safefile.Read(path, nil)
	if err != nil {
		return nil, err
	}
	var c doctorConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	if c.Listen == "" {
		c.Listen = ":7863"
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.StateFile == "" {
		c.StateFile = "./data/state.json"
	}
	return &c, nil
}

// checkBinding 监听地址与鉴权的组合安全性判定（口径对齐 cmd/server/insecure_write_guard.go）。
func checkBinding(cfg *doctorConfig) check {
	host, _, ok := splitHostPort(cfg.Listen)
	nonLoopback := !ok || (host != "" && host != "127.0.0.1" && host != "localhost" && host != "::1")
	switch {
	case nonLoopback && cfg.APIKey == "":
		return check{
			Name: "binding", Level: levelErr,
			Detail: fmt.Sprintf("listen=%q 非回环且 api_key 为空：写方法会被安全闸拒绝（405），请配置 api_key 或改监听 127.0.0.1",
				cfg.Listen),
		}
	case nonLoopback:
		return check{
			Name: "binding", Level: levelWarn,
			Detail: fmt.Sprintf("listen=%q 对外监听：确认已置于 HTTPS 反向代理与访问限流之后", cfg.Listen),
		}
	default:
		return check{Name: "binding", Level: levelOK, Detail: fmt.Sprintf("listen=%q 仅回环", cfg.Listen)}
	}
}

// splitHostPort 宽松拆分 host:port（"::1" 这类不带端口的形态按 host 整体处理）。
func splitHostPort(addr string) (host, port string, ok bool) {
	i := strings.LastIndex(addr, ":")
	if i <= 0 {
		return addr, "", false
	}
	return addr[:i], addr[i+1:], true
}

// checkAuthDir 账号池：可读性、数量、token 有效期分布。
func checkAuthDir(cfg *doctorConfig) []check {
	entries, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		return []check{{Name: "accounts", Level: levelErr,
			Detail: fmt.Sprintf("auth_dir=%q 读取失败: %v", cfg.AuthDir, err)}}
	}
	if len(entries) == 0 {
		return []check{{Name: "accounts", Level: levelWarn,
			Detail: fmt.Sprintf("auth_dir=%q 内没有任何账号（面板「添加账号」完成 OAuth 登录）", cfg.AuthDir)}}
	}

	now := time.Now().Unix()
	var expired, soonExpiring int
	for _, a := range entries {
		switch {
		case a.ExpiresAt <= 0:
			expired++ // 无 expiry 视同已过期（NeedsRefresh 同口径）
		case a.ExpiresAt <= now:
			expired++
		case a.ExpiresAt-now < int64(24*time.Hour/time.Second):
			soonExpiring++
		}
	}

	out := []check{{Name: "accounts", Level: levelOK,
		Detail: fmt.Sprintf("%s 下 %d 个账号文件", cfg.AuthDir, len(entries))}}
	switch {
	case expired > 0:
		out = append(out, check{Name: "token_expiry", Level: levelErr,
			Detail: fmt.Sprintf("%d 个账号的 access token 已过期（保活任务会尝试刷新；若长期不恢复需在面板一键重登）", expired)})
	case soonExpiring > 0:
		out = append(out, check{Name: "token_expiry", Level: levelWarn,
			Detail: fmt.Sprintf("%d 个账号的 token 将在 24 小时内过期（属正常轮换窗口，确认保活排程已启用即可）", soonExpiring)})
	default:
		out = append(out, check{Name: "token_expiry", Level: levelOK, Detail: "全部 token 均在 24 小时有效期之外"})
	}
	return out
}

// checkStateAndStamps 状态文件与补跑标记的可解析性 + 数据目录可写性。
func checkStateAndStamps(cfg *doctorConfig) []check {
	var out []check
	dir := filepath.Dir(cfg.StateFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			out = append(out, check{Name: "data_dir", Level: levelErr,
				Detail: fmt.Sprintf("数据目录 %q 不可创建/写入: %v", dir, err)})
		} else if !dirWritable(dir) {
			out = append(out, check{Name: "data_dir", Level: levelErr,
				Detail: fmt.Sprintf("数据目录 %q 不可写（持久化会失败）", dir)})
		} else {
			out = append(out, check{Name: "data_dir", Level: levelOK, Detail: fmt.Sprintf("数据目录 %q 可写", dir)})
		}
	}

	// state.json：不存在属正常（首次启动会建），存在但不可解析才是问题。
	//
	// 这里不用 safefile.Read：它把「内容坏」也报成 error（并尝试回退 .bak），
	// 而 doctor 恰恰要区分「文件还没建」与「文件坏了」——两者处置完全不同。
	if raw, err := os.ReadFile(cfg.StateFile); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			out = append(out, check{Name: "state_file", Level: levelOK,
				Detail: fmt.Sprintf("%q 尚不存在（首次启动会创建）", cfg.StateFile)})
		} else {
			out = append(out, check{Name: "state_file", Level: levelErr,
				Detail: fmt.Sprintf("%q 读取失败: %v", cfg.StateFile, err)})
		}
	} else {
		var probe any
		if json.Unmarshal(raw, &probe) != nil {
			out = append(out, check{Name: "state_file", Level: levelErr,
				Detail: fmt.Sprintf("%q 无法解析为 JSON（可能写了一半或损坏）", cfg.StateFile)})
		} else {
			out = append(out, check{Name: "state_file", Level: levelOK,
				Detail: fmt.Sprintf("%q 可解析（%d 字节）", cfg.StateFile, len(raw))})
		}
	}

	// runstamps.json：补跑标记，缺失属正常（下次启动会补跑一轮）。
	stampPath := filepath.Join(dir, "runstamps.json")
	if dir == "" {
		stampPath = "runstamps.json"
	}
	if raw, err := os.ReadFile(stampPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			out = append(out, check{Name: "runstamps", Level: levelOK,
				Detail: fmt.Sprintf("%q 尚不存在（下次启动会做一轮启动补跑）", stampPath)})
		} else {
			out = append(out, check{Name: "runstamps", Level: levelWarn,
				Detail: fmt.Sprintf("%q 读取失败: %v", stampPath, err)})
		}
	} else {
		st := map[string]string{}
		if json.Unmarshal(raw, &st) != nil {
			out = append(out, check{Name: "runstamps", Level: levelWarn,
				Detail: fmt.Sprintf("%q 无法解析：补跑会重新跑一轮（不影响正确性）", stampPath)})
		} else {
			today := cst.Day(time.Now())
			var done []string
			for k, v := range st {
				if v == today {
					done = append(done, k)
				}
			}
			sort.Strings(done)
			out = append(out, check{Name: "runstamps", Level: levelOK,
				Detail: fmt.Sprintf("今日（%s）已补跑：%s", today, joinOrNone(done))})
		}
	}
	return out
}

// checkSchedule 六族排程的启用状态与下一次触发时刻。
func checkSchedule(cfg *doctorConfig) check {
	now := time.Now()
	type fam struct {
		name    string
		enabled bool
		hours   []int
	}
	fams := []fam{
		{"checkin", cfg.Schedule.CheckinEnabled, cfg.Schedule.CheckinHours},
		{"travel", cfg.Schedule.TravelEnabled, cfg.Schedule.TravelHours},
		{"activity", cfg.Schedule.ActivityEnabled, cfg.Schedule.ActivityHours},
		{"keepalive", cfg.Schedule.KeepaliveEnable, cfg.Schedule.KeepaliveHours},
		{"blackcat", cfg.Schedule.BlackcatEnabled, cfg.Schedule.BlackcatHours},
		{"growth", cfg.Schedule.GrowthEnabled, cfg.Schedule.GrowthHours},
	}
	var parts []string
	for _, f := range fams {
		if !f.enabled {
			parts = append(parts, f.name+"=off")
			continue
		}
		next := nextFireLocal(now, f.hours)
		if next.IsZero() {
			parts = append(parts, f.name+"=on(未配时点)")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=on@%s", f.name, next.In(cst.Loc).Format("01-02 15:04")))
	}
	return check{Name: "schedule", Level: levelOK, Detail: strings.Join(parts, " · ")}
}

// nextFireLocal 与 scheduler.nextFire 同语义（doctor 不导入内部调度器，只做展示）。
func nextFireLocal(now time.Time, hours []int) time.Time {
	var earliest time.Time
	local := now.In(cst.Loc)
	for _, h := range hours {
		t := time.Date(local.Year(), local.Month(), local.Day(), h, 0, 0, 0, cst.Loc)
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// dirWritable 试探目录可写性（建临时文件后立即删除）。
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".doctor-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "（无）"
	}
	return strings.Join(items, ", ")
}

// printHuman 人类可读摘要。
func printHuman(w *os.File, rep report) {
	fmt.Fprintf(w, "WorkBuddy2API Panel doctor v%s\n", rep.Version)
	fmt.Fprintf(w, "时间 %s\n\n", time.Unix(rep.TS, 0).In(cst.Loc).Format("2006-01-02 15:04:05"))
	for _, c := range rep.Checks {
		mark := map[checkLevel]string{levelOK: "✓", levelWarn: "!", levelErr: "✗"}[c.Level]
		fmt.Fprintf(w, " %s %-14s %s\n", mark, c.Name, c.Detail)
	}
	fmt.Fprintf(w, "\n结果：ok=%d warning=%d error=%d\n",
		rep.Summary["ok"], rep.Summary["warning"], rep.Summary["error"])
	if !rep.OK {
		fmt.Fprintln(w, "存在 error 级问题（退出码 1）")
	}
	fmt.Fprintln(w, "---")
}
