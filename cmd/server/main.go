// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/alert"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/authlease"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/rotaudit"
	"github.com/linguo2625469/workbuddy2api-panel/internal/safefile"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
	"github.com/linguo2625469/workbuddy2api-panel/internal/websearch"
)

// appVersion 网关版本（fork 版：面板 + 任务体系），透出到 /panel/api/overview。
const appVersion = "1.18.0-panel"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	// M4 租约锁：同一 auths 目录不允许两个执行体同时挂（异机新鲜租约 + strict
	// 拒绝启动）。Acquire 在 LoadDir 前调用；Release 随进程退出删租约文件。
	lease, err := authlease.Acquire(cfg.AuthDir, cfg.Auths.StrictLease)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer lease.Release()

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	// 每日限额护栏（v1.17.0 ⑤）：积分上限换算 milli（内部 ×1000 防精度损失）。
	p.SetDailyLimits(cfg.Pool.DailyTokenLimit, cfg.Pool.DailyCreditLimit*1000, cfg.Pool.ModelDailyTokenLimit)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏）；
			// 裸名走 cn（现状零回归）。闭包内部 resolveModel 剥前缀，再按 realm 过滤。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })

	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.DumpDir = cfg.Upstream.DumpDir
	up.DumpRequestMax = cfg.Upstream.DumpMaxBytes
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled:     !cfg.Schedule.GrowthEnabled,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     cfg.Logging.RequestClientInfo,
		HiddenModels:         livecfg.NormalizeHiddenModels(cfg.Models.Hidden),
		ModelAliases:         livecfg.NormalizeModelAliases(cfg.Models.Aliases),
		ModelFallback:        cfg.Pool.ModelFallback,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// 请求指标始终启用；JSONL 归档只写脱敏元数据，写盘失败不影响聊天请求。
	requestLog := reqlog.New(reqlog.Config{
		Dir:           stateSibling(cfg.StateFile, "request-logs"),
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
	})
	defer requestLog.Close()
	rs := requestLog.Snapshot().Archive
	if rs.Enabled {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	} else {
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档已关闭")
	}

	// M2 批量判死告警：pool 禁用 hook → alert（横幅 + webhook）。webhook_url 空 =
	// 仅横幅不外发；配置页 notify 段热改（SetConfig），无需重启。
	alertMgr := alert.New(alert.Config{
		WebhookURL:        cfg.Notify.WebhookURL,
		MassKillAccounts:  cfg.Notify.MassKillAccounts,
		MassKillWindowMin: cfg.Notify.MassKillWindowMin,
		CooldownMin:       cfg.Notify.CooldownMin,
	}, "", appVersion)
	p.SetOnDisabled(alertMgr.NoteDisabled)
	log.Printf("[alert] 批量判死告警已启用：阈值 %d 号 / %d 分钟窗口%s",
		cfg.Notify.MassKillAccounts, cfg.Notify.MassKillWindowMin,
		map[bool]string{true: "，webhook 已配置", false: "（仅面板横幅，未配 webhook）"}[cfg.Notify.WebhookURL != ""])

	// M6 RT 轮换审计：指纹 JSONL（保留 7 天）+ 外部轮换检测（进程外执行体消费 RT
	// → ERROR + webhook）。外部轮换命中即告警（不凑阈值——1 个也意味着凭证双活）。
	rotRec := rotaudit.New(stateSibling(cfg.StateFile, "refresh-audit"), "", func(uid, old8, cur8, source string) {
		alertMgr.NoteExternalRotation(uid, source)
	})
	up.SetRotationObserver(rotRec.Observe)
	seed := map[string]string{}
	for _, a := range auths {
		seed[a.UID] = a.RefreshTokenValue()
	}
	rotRec.Seed(seed)

	// M5/M7/M8：抖动窗口、智能保活、死号探活（全部热可改）。
	sch.SetJitterMinutes(cfg.Schedule.JitterMinutes)
	sch.SetKeepaliveSmart(cfg.Schedule.KeepaliveMode == "smart", cfg.Schedule.SmartKeepaliveDays)
	sch.SetProbeDeadHours(cfg.Schedule.ProbeDeadHours)
	sch.SetRecoveredHook(alertMgr.NoteRecovered)
	log.Printf("[scheduler] 抖动 %d 分钟窗口；保活模式 %s（阈值 %d 天）；死号探活 %v",
		cfg.Schedule.JitterMinutes, cfg.Schedule.KeepaliveMode, cfg.Schedule.SmartKeepaliveDays,
		map[bool]string{true: "已启用", false: "关闭"}[len(cfg.Schedule.ProbeDeadHours) > 0])
	log.Printf("[rotaudit] RT 轮换审计已启用：%s（外部轮换检测：本进程之外的任何 RT 消费都会告警）",
		stateSibling(cfg.StateFile, "refresh-audit"))

	// pn 先声明再赋值：SaveConfig 闭包需引用 pn 本身（配置页热改备份/回收站设置），
	// 闭包执行时 pn 已完成装配（首次保存必然发生在面板可用之后）。
	var pn *panel.Panel
	pn = panel.New(panel.Config{
		Pool:          p,
		Alert:         alertMgr,
		OnLoginRotate: rotRec.Reset,
		Usage:         rec,
		RequestLog:    requestLog,
		Upstream:      up,
		Scheduler:     sch,
		AuthDir:       cfg.AuthDir,
		APIKey:        cfg.APIKey,
		RedisMode:     redisMode,
		StickyCount:   sessCount,
		Version:       appVersion,
		Live:          live,
		// 回收站与备份目录：均落 state 文件同目录（data/，Docker 已挂载的持久化卷），
		// 容器销毁不丢已删除凭证 / 备份。
		TrashDir:           stateSibling(cfg.StateFile, "trash"),
		TrashRetentionDays: cfg.Trash.RetentionDays,
		BackupDir:          stateSibling(cfg.StateFile, "backups"),
		StateFile:          cfg.StateFile,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile:  stateSibling(cfg.StateFile, "output_probes.json"),
		ConfigPath: *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch, pn, alertMgr)
		},
	})
	// 备份设置注入 + 每日定时备份与回收站过期清理循环（随 ctx 取消退出）。
	pn.SetBackupSettings(cfg.Backup.Enabled, cfg.Backup.Hour, cfg.Backup.Keep)
	log.Printf("定时备份已启用：每日 %d 点 → %s（保留 %d 份）", cfg.Backup.Hour, stateSibling(cfg.StateFile, "backups"), cfg.Backup.Keep)
	// 成长任务队列每日自动执行（与「执行全部待办」同管线）：Sequential 族零点解锁后
	// 无需手动扫描；hook 返回即启动（异步执行），已在跑时内部跳过。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))
	server.SetChatLogOutput(io.MultiWriter(os.Stdout, pn.Logs()))

	// 服务端 web_search 检索后端（/v1/messages 的 web_search 工具，见
	// internal/websearch）：enabled=false 时不构造，handler 侧把该服务端
	// 工具剥离、模型照常作答。
	var searcher websearch.Searcher
	if cfg.Search.Enabled {
		searcher = websearch.NewSo(time.Duration(cfg.Search.TimeoutSeconds)*time.Second, "")
		log.Printf("服务端 web_search 已启用（360 搜索抓取；单次 %d 条 / %ds 超时）",
			cfg.Search.MaxResults, cfg.Search.TimeoutSeconds)
	}

	h := server.NewHandler(server.Config{
		GetKeepaliveStats: func() any { return sch.KeepaliveStats() },
		Pool:              p,
		Upstream:          up,
		APIKey:            cfg.APIKey,
		Session:           sessRouter,
		StickyCount:       sessCount,
		RedisMode:         redisMode,
		SoftCooldown:      cfg.SoftRateDur,
		Panel:             pn,
		Live:              live,
		Usage:             rec,
		RequestLog:        requestLog,
		PromptMode:        cfg.Prompt.Mode,
		PromptText:        cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global: 模型名。
		GlobalEnabled: cfg.Global.Enabled,
		// 模型黑名单原始条目；运行期以 livecfg 快照为准（热生效），此处供
		// Live 为 nil 的裸用/测试路径。
		ModelsHidden: cfg.Models.Hidden,
		// 模型映射原始条目；同上，运行期以 livecfg 快照为准。
		ModelsAliases: cfg.Models.Aliases,
		// 服务端 web_search（/v1/messages）：检索后端 + 条数/超时。
		Search:           searcher,
		SearchMaxResults: cfg.Search.MaxResults,
		SearchTimeout:    time.Duration(cfg.Search.TimeoutSeconds) * time.Second,
	})
	// 上游排队门控快照注入面板：排队（6020/6021）是上游服务端队列水位、与账号无关，
	// 面板横幅据此区分它与账号限流（handler 已装配完，此处才拿得到 h）。
	pn.SetUpstreamQueue(h.UpstreamQueueStatus)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	// dump 保留清理（7 天窗口，启动即清 + 每日一次）：dump 内容含完整提示词与
	// 响应，无限期留存既是磁盘增长点也是敏感数据留存风险。
	up.StartDumpCleaner(ctx)
	defer stop()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)
	// 回收站过期清理（启动即清一次 + 每小时巡检）与每日定时备份循环。
	go pn.StartTrashLoop(ctx)
	go pn.StartBackupLoop(ctx)

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 异步执行：不阻塞监听启动；失败仅记日志（下一轮懒触发或本轮重试仍可补上）。
	go warmModelRates(ctx, up, p)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除）。缺省 300s（issue #100：旧固定
		// 60s 会掐掉大上下文/文件块经反代链的慢速上传，客户端收到
		// 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("workbuddy2api listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// writeConfigInPlace 原地重写配置文件（O_TRUNC + Write + fsync + Close）。
//
// 只用于原子替换不可用的两种 Docker 部署形态：目录不可写（fs.ErrPermission，
// 建不出临时文件）与单文件 bind mount（rename 返回 EBUSY）。代价是丢掉了原子性
// 与 .bak：写一半失败时配置文件会处于半截状态，故只在确实无法原子替换时才走这里。
func writeConfigInPlace(path string, out []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(out)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetCreditFloor
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//   - trash.retention_days / backup.* → panel.SetTrashRetention/SetBackupSettings
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler, pn *panel.Panel, alertMgr *alert.Manager) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)
	// 旧配置解析（供重启字段 diff：只列「本次真正变化」的装配期字段，此前恒返回
	// 全清单——用户改一个热生效项也被告知十几项要重启，提示被稀释成噪声）。
	oldCfg, _ := ParseConfig(oldRaw)

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	// safefile：tmp fsync 后才 rename，并把上一份有效配置留成 config.json.bak
	// —— 掉电把 config.json 留成半截时，下次启动能捡回上一份。
	if err := safefile.WriteKeep(path, out, 0o600); err != nil {
		switch {
		case errors.Is(err, fs.ErrPermission):
			// 配置所在目录不可写：Docker 里 /app 属主是镜像内的 app(10001)，而 compose 让
			// 容器以 PUID(如 1000) 运行 —— 挂载进去的 config.json 可写、目录不行，于是原子
			// 替换连临时文件都建不出来（issue #134：write config: open /app/config.json.tmp:
			// permission denied）。文件本身可写就退回原地重写，不再要求目录可写。
			if inPlaceErr := writeConfigInPlace(path, out); inPlaceErr != nil {
				return nil, fmt.Errorf("write config (目录不可写，原地重写也失败): %w", inPlaceErr)
			}
		case errors.Is(err, syscall.EBUSY):
			// A single-file Docker bind mount cannot be renamed over its mount
			// target (Linux returns EBUSY / "device or resource busy"). Keep the
			// atomic path for regular files, but update the mounted file in place
			// for this specific deployment shape.
			if inPlaceErr := writeConfigInPlace(path, out); inPlaceErr != nil {
				return nil, fmt.Errorf("replace config (bind mount fallback): %w", inPlaceErr)
			}
		default:
			return nil, fmt.Errorf("write config: %w", err)
		}
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     newCfg.Logging.RequestClientInfo,
		HiddenModels:         livecfg.NormalizeHiddenModels(newCfg.Models.Hidden),
		ModelAliases:         livecfg.NormalizeModelAliases(newCfg.Models.Aliases),
		ModelFallback:        newCfg.Pool.ModelFallback,
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)
	// 回收站保留天数与备份设置热生效（panel 侧锁字段，下一次巡检/触发即用新值）。
	if pn != nil {
		pn.SetTrashRetention(newCfg.Trash.RetentionDays)
		pn.SetBackupSettings(newCfg.Backup.Enabled, newCfg.Backup.Hour, newCfg.Backup.Keep)
	}

	// M2 告警配置热生效（阈值/窗口/冷却/webhook 全部无需重启）。
	alertMgr.SetConfig(alert.Config{
		WebhookURL:        newCfg.Notify.WebhookURL,
		MassKillAccounts:  newCfg.Notify.MassKillAccounts,
		MassKillWindowMin: newCfg.Notify.MassKillWindowMin,
		CooldownMin:       newCfg.Notify.CooldownMin,
	})
	// 每日限额护栏热生效。
	p.SetDailyLimits(newCfg.Pool.DailyTokenLimit, newCfg.Pool.DailyCreditLimit*1000, newCfg.Pool.ModelDailyTokenLimit)
	// M5/M7/M8 配置热生效。
	sch.SetJitterMinutes(newCfg.Schedule.JitterMinutes)
	sch.SetKeepaliveSmart(newCfg.Schedule.KeepaliveMode == "smart", newCfg.Schedule.SmartKeepaliveDays)
	sch.SetProbeDeadHours(newCfg.Schedule.ProbeDeadHours)

	return restartRequiredDiff(oldCfg, newCfg), nil
}

// restartRequiredDiff 逐字段比较新旧配置，只列出**真正变化**的装配期字段（此前
// restartRequiredFields 恒返回全清单）。字段清单同旧函数 + 补漏：prompt.mode/file、
// upstream.client_version/cli_version/client_name/device_token*、upstream.dump_dir
// （装配期捕获且无热应用路径的字段）。
func restartRequiredDiff(oldC, newC *Config) []string {
	var out []string
	add := func(changed bool, name string) {
		if changed {
			out = append(out, name)
		}
	}
	add(oldC.Listen != newC.Listen, "listen")
	add(oldC.AuthDir != newC.AuthDir, "auth_dir")
	add(oldC.StateFile != newC.StateFile, "state_file")
	add(oldC.Upstream.TimeoutSeconds != newC.Upstream.TimeoutSeconds, "upstream.timeout_seconds")
	add(oldC.Upstream.HeaderTimeoutSeconds != newC.Upstream.HeaderTimeoutSeconds, "upstream.header_timeout_seconds")
	add(oldC.Upstream.IdleTimeoutSeconds != newC.Upstream.IdleTimeoutSeconds, "upstream.idle_timeout_seconds")
	add(oldC.Upstream.UserAgent != newC.Upstream.UserAgent, "upstream.user_agent")
	add(oldC.Upstream.ClientVersion != newC.Upstream.ClientVersion, "upstream.client_version")
	add(oldC.Upstream.CliVersion != newC.Upstream.CliVersion, "upstream.cli_version")
	add(oldC.Upstream.ClientName != newC.Upstream.ClientName, "upstream.client_name")
	add(oldC.Upstream.DeviceToken != newC.Upstream.DeviceToken, "upstream.device_token")
	add(oldC.Upstream.DeviceTokenFile != newC.Upstream.DeviceTokenFile, "upstream.device_token_file")
	add(oldC.Upstream.DumpDir != newC.Upstream.DumpDir, "upstream.dump_dir")
	add(oldC.Upstream.DumpMaxBytes != newC.Upstream.DumpMaxBytes, "upstream.dump_max_bytes")
	add(oldC.Upstash.URL != newC.Upstash.URL || oldC.Upstash.Token != newC.Upstash.Token, "upstash")
	add(oldC.SessionTTL != newC.SessionTTL, "session_sticky.ttl")
	add(oldC.SessionGCInterval != newC.SessionGCInterval, "session_sticky.gc_interval")
	add(oldC.Logging.RequestArchiveEnabled != newC.Logging.RequestArchiveEnabled, "logging.request_archive_enabled")
	add(oldC.Logging.RequestRetentionDays != newC.Logging.RequestRetentionDays, "logging.request_retention_days")
	add(oldC.Logging.RequestArchiveMaxMB != newC.Logging.RequestArchiveMaxMB, "logging.request_archive_max_mb")
	add(oldC.ServerReadTimeoutDur != newC.ServerReadTimeoutDur, "server.read_timeout")
	add(oldC.Auths.StrictLease != newC.Auths.StrictLease, "auths.strict_lease")
	add(oldC.Prompt.Mode != newC.Prompt.Mode, "prompt.mode")
	add(oldC.Prompt.File != newC.Prompt.File, "prompt.file")
	return out
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。
func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
