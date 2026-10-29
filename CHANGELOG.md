# 更新日志（CHANGELOG）

本仓库从 `1.13.0-panel` 起维护本变更日志；更早版本的历史请查阅 Git tag（`v1.9.2` 及以前）与提交记录。

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循语义化版本（`<主>.<次>.<修订>-panel` 后缀表示 fork 面板增强分支）。

## [1.19.1-panel] - 2026-10-28

本版为**请求改写补丁**：修复 MCP 类客户端（ZCode 等）的工具定义触发上游 11133 的问题。

### 修复

- **tools schema 的 `$ref`/`$defs` 触发上游 11133 `model_param_invalid`**。
  背景：MCP 类客户端为压缩体积，会把重复的 schema 片段抽到 `$defs` 里、用
  `{"$ref":"#/$defs/X"}` 引用。WorkBuddy 上游对 `tools[].function.parameters` 做
  **扁平化校验、不解析 `$ref`**，命中即整体拒收 400 code=11133。

  实测对照（2026-10-14）：**保留 `$ref` 返 400、内联展开后 200，且与具体模型无关**
  （cn 域多个模型一致复现），属上游校验器能力边界而非模型差异。schema 级拒绝换账号无用，
  只能在发送前修。

  修法：新增 `derefToolSchemas`，在 `normalizeToolPatterns` **之前**把
  `#/$defs/X` 与 `#/definitions/X` 两类本地引用内联展开，并删除定义块。

### 刻意收窄的语义边界（都有测试钉住）

| 边界 | 理由 |
|---|---|
| 只解 `#/$defs/` 与 `#/definitions/` 两种**本地**引用 | 远程 `$ref`（`https://…`）需要网络，网关不能为一次改写去拉第三方资源 |
| 循环引用（A→B→A）**保留 `$ref` 原样**，只展开一层 | 内联会无限展开；宁可让上游照旧报 11133，也不能把请求体撑爆 |
| `maxDerefDepth = 16` 硬上限 | 仅靠引用链去重挡不住跨链指数展开——首版实现实测可把 8KB 请求体撑到 GB 级并 **stack overflow**（进程级崩溃）。16 层远超真实最深的 4 层 MCP schema |
| 只动 `tools` 子树 | `messages` 正文里出现的 `$ref` 字面量不碰 |
| 无 `$defs`/`$ref` 时零改动 | 出站报文逐字节不变 |
| 同一个定义被多处引用时深拷贝 | 共享指针会让后续 `delete $defs` 互相污染 |

### 顺序约束

`derefToolSchemas` 必须在 `normalizeToolPatterns` **之前**跑：`$defs` 里的片段同样可能
带 `\_` 非标转义（11129 的成因），反过来的话从 `$defs` 内联进来的 pattern 会漏掉归一化。

### 测试

新增 `TestDerefToolSchemas`（9 个子用例）：顶层 `$ref` 内联且 `$defs` 删除、嵌套属性
`$ref`、`definitions` 旧写法、循环引用保留 `$ref` 且不冗余嵌套、远程 `$ref` 不解析、
定义缺失保留原样、同一定义多处引用互不影响、裸 `parameters` 形态、无 `tools` 不 panic、
`messages` 正文 `$ref` 字面量不碰。

**红-绿反证**：去掉"已展开键不再遍历"的跳过逻辑后，循环引用用例立即失败（已恢复）。
`go test ./...` 全绿、`go vet` 无输出、`gofmt -l` 空。

### 部署

本版**未**部署到生产（A/B 两机）。升级流程与回滚见 HANDOFF §5。

## [1.19.0-panel] - 2026-10-26

本版为**加固、可观测与测试基线版**：在 `1.18.3` 之后累计的 59 个提交，涵盖一处进程级 P0 修复、三个 opt-in 的新运维开关、日志全面分级结构化，以及把测试从「能跑」推到「能反证」。全部新开关默认关闭或零行为变更，升级不改变现有部署的运行语义。

### 修复

- **P0：SSE 心跳与错误路径并发写 header map 导致进程 fatal**。`anthropicStreamWriter` 被当作 `ResponseWriter` 下传给 `chatCompletions`，其 `Header()` 直通底层 map 且不持锁；心跳在锁内改头 → 两个写者 → `fatal error: concurrent map writes`。这是**进程级 abort，不可 recover**，一旦触发整条网关退出。修法：`ensureSSEHeaders` 在 `headerWritten` 后直接返回、删除 `startWith` 的锁外调用、心跳改抓局部 stop channel。回归测试 `TestAnthropicHeartbeat` 可反证（回退修复立即失败）。
- **测试顺序依赖**：`dynamicModelsCache` 为包级全局，跨用例污染曾被误判成「偶发 flaky」。补 `resetDynamicModelsCache(t)` 并加 `-shuffle` 复跑。
- **面板频道分类**：任务失败行带 `WARN: `/`ERR: ` 前缀，会掉进 sys 频道而非任务频道。分类前先剥前缀。
- **`truncateStr` / `trunc` 切中文产生非法 UTF-8**：按字节硬切会把末尾字符切成半个（面板显示 `�`）。改为回退到最近的 rune 起点。
- **`autoActionIndex` 与 `autoActionFor` 口径不一致**：前者未 `TrimSpace`，带空白的 task_code 在一侧命中、另一侧当未知，导致排序打乱依赖链。
- **`stopHeartbeat` 只关 channel 不等协程退出**：`select` 在 ticker 与 stop 同时就绪时随机选分支，返回后心跳 goroutine 可能仍写一帧——`finish` 之后的调用方会与它并发访问同一个 writer。现加 `hbDone` 信号并等待退出。该缺陷由 CI 的 `-race` 作业在 `TestAnthropicHeartbeatNeverTouchesHeadersAfterFirstFrame` 上以 DATA RACE 形式暴露（读 `ResponseRecorder.Body` 时写者未停）。
- **gofmt 版本间行为不一致导致 CI 红**：`payload_test.go` 有一行使 `bs := string(byte(92))` 与超长字符串字面量同行，gofmt 1.22 会把行尾注释对齐到该行的字符列末，生成上千空格；gofmt 1.27 反而不认这种对齐，本地检查通过而 CI 失败。现把注释独立成行，消除对齐锚点。

### 新增

- **入站限流 `server.rate_limit_rps` / `rate_limit_burst`（默认 0 = 关闭）**：令牌桶按 **TCP 对端主机**划分，超限返回 429 + `Retry-After`（Anthropic 面为 `rate_limit_error`）。豁免 `/status` 与 `/healthz`——监控探活被限流会被误判成服务挂了。**刻意不用 `X-Forwarded-For`**：该头客户端可任意伪造，按它限流等于每请求换一个假 IP 拿无限额度。
- **请求体上限 `server.max_body_mb`（默认 0 = 不限制）**：超限请求在读 body 前被拒（413 / `request_too_large`），不消耗选号与上游配额。
- **诊断监听 `server.debug_addr`（默认空 = 关闭）**：pprof + 低基数 `/metrics`，**独立端口**而非挂主 mux。pprof 无鉴权且 `profile` 能导出 heap（含请求体与账号 token），主端口通常绑 `0.0.0.0`；回归测试 `TestMainMuxHasNoDebugEndpoints` 钉住这一点。
- **出站元数据剥除 `features.strip_tool_metadata`（默认 false）**：剥除 tool-call 块里的 `metaData`（traceId / traceparent / baggage 等约 20 个纯观测字段），实测 39 个 tool-call 约省 55–70KB。只删 `tool.metaData` / `result.metaData`，其余字段不动；关闭时出站报文逐字节不变。
- **上游 requestId 与本地 rid 同列落台账**：新增 `Error.RequestID`、`ParseUpstreamRequestID`（识别三种形态）与 `reqlog.Event.upstream_request_id`，排障时本地上游两侧 ID 不再脱节。
- **状态快照单调 `revision`**：`RestoreFromSnapshot` 改以 `revision` 为主判据（两侧可比且不等取大者 / 相等取本地 / 一侧为 0 回落 mtime）。解决「三源（内存 / state.json / 对账快照）择新」在多实例下靠 mtime 判定的不可靠问题，旧文件行为不变。
- **`internal/logx` 分级日志门面**：保形 slog Handler——时间戳仍由 log 包 flags 打、级别写成 `WARN: `/`ERR: ` 前缀（**不写 `level=`**）、结构化属性以 ` k=v` 追加行尾。全仓 255 处 `log.Printf` 迁完，分级实测 **Infof 129 / Warnf 105 / Errorf 21**。**为什么不能直接换 `slog.TextHandler`**：它输出 `time=… level=… msg=…`，而面板 `tsPrefixRe` 只认 log 包默认前缀、任务频道靠行首匹配——直接换会让日志视图静默丢时间戳与任务筛选（不报错、只是难用）。
- **面板字号收敛为 6 档语义令牌**（`--fs-xs/sm/base/md/lg/num`），账号表表头吸附（`--topbar-h`）。
- **弹窗打开时给主容器加 `inert`**：读屏不再能浏览遮罩背后的内容。
- **CI 增加 `-race` 作业**（pool/server/scheduler 三包，首轮 `continue-on-error` 观察态）。

### 变更

- **上游错误类别 → HTTP 状态码收敛为单一映射表**（`errmap.go`）：此前散在 `handler_chat.go` 内 6 处，改一处必漏五处。附「每个 ErrKind 都必须表态」的完整性测试。
- **`client.go`（2580 行）按职责拆 6 文件**、**`handler.go`（1962 行）按端点族拆 5 文件**：均为纯搬运，用「符号集合 + 行多重集」双机械校验（154 / 47 个声明零缺失、行丢失 0）。
- **全仓 gofmt + `.gitattributes` 固定 LF + CI 加 gofmt 门禁**：此前本地 gofmt 会因 CRLF 误报 100+ 文件，本地与 CI 判定不一致。

### 测试

- `go test ./...` 26 包全绿，**约 10 秒**；`internal/panel` 覆盖率 **33.0% → 49.4%**。
- 14 处测试不再打真实网络（改毒桩 + `calls == 0` 这类更强断言）；`TestMain` 补 `rotateBackoffBase = 0`，server 包 11.3s → 0.7s；scheduler 单条 12.01s → 0.02s（`SetBillingRetryDelayForTest` 注入点）。
- 任务中心测试从零推到覆盖：读取/回读路径 19 子用例、四个动作 13 子用例、本轮再补 20 个 Test / 约 60 子用例（mp 闭环、专家链、`runAutoAll`、两个 handler）。**每处关键断言都做过反证**（回退修复确认用例变红）。
- 前端纯函数层抽为 `internal/panel/logic.js`（14 个函数），`logic_test.js` 60 条零依赖断言 + 3 处反证；`TestLogicJSNeedsNoDOM` 拦住往纯函数层塞 DOM 调用。

### 验证

- `go build ./... && go vet ./... && gofmt -l ./cmd ./internal && go test ./... -count=1` 全部通过。
- 面板无障碍：对比度达 WCAG AA、57 处 `<th scope>`、弹窗 ARIA + 焦点管理、浅色 FOUC 消除。

## [1.18.3-panel] - 2026-10-24

本版为**账号池排版清理**：集中处理账号池页肉眼可见的对齐与间距问题（用户截图反馈批）。

### 修复

- **账号池头部工具栏高矮不齐**：搜索框与「全部状态 / 全部标签」下拉此前是全尺寸控件（约 35px），右侧「全部签到 / 旅行巡检 / 活跃上报 / 全部保活」等 `.xs` 按钮是紧凑规格（约 27px），同一栏两种高度。现统一紧凑规格——新增 `input.xs`（与 `select.xs` / `button.xs` 同款），账号池两个筛选下拉改挂 `xs` 类；`select.xs` 上下内边距各补 1px（select 内容盒由原生选项行高决定，同字号下比 button/input 矮 2px），三种控件实测严格同高（27.7px，偏差 0）。
- **工具栏间距混乱**：搜索框/下拉的 inline `margin-right:6px` 叠加 flex `gap:12px` 造成 12/18px 混用；空的 `#accNote` 还占一个 gap 位，「全部标签」与「全部签到」之间凭空多出一截。现移除 inline margin 统一走 gap（实测相邻控件间距全部 12px），并加 `#accNote:empty { display:none }`。
- **窄屏头部工具栏被裁切（功能性缺陷）**：860px 断点只对表格列做了降级，账号池卡片头部工具栏没有换行能力，窄屏下搜索/筛选/批量按钮整排被 `.box` 的 `overflow:hidden` 裁掉——实测 820px 视口溢出 319px，「全部保活」「探测死号」不可点。现给 `#view-accounts .box > header` 加 `flex-wrap`（与用量/日志页头部同做法），复测溢出 0、全部按钮可见可点。
- **回收站弹窗 footer 按钮一高一矮**：「清空回收站」是 `xs`、「关闭」是默认尺寸，同排不齐；「关闭」统一为 `xs`。
- **操作列竖向错位（用户复验反馈）**：行内按钮此前右对齐（`justify-content: flex-end`），而行与行的按钮集合不同（普通行多「暂停选号」「禁用」、待重盛行「重登 + 解冻」、封号行仅「解冻」），右对齐让「签到」的起始 x 逐行漂移最多约 80px，竖看整列错位。现改为**左对齐**：签到 / 余额 / 任务 / 标签 / 变体按钮（重登·解冻·恢复选号·暂停选号）各占稳定竖列（实测左缘逐行偏差 0）；「移除」用 `margin-left: auto` 单独钉在本列最右（右缘逐行偏差 0，破坏性操作位置固定且与其余按钮自然分隔）。
- **余额趋势 footer 数字对齐**：底部「区间 X ~ Y 分 · N 个小时点」统计行补 `font-variant-numeric: tabular-nums`。

### 验证

- `go test ./... -count=1` 全绿（含 internal/panel 的 id 唯一性 / JS 语法 / 无内联脚本 / 安全测试）。
- UI 可编程 DOM 断言（chrome-devtools，隔离实例 + mock 数据）：宽屏 1920px 下工具栏 7 控件高度偏差 0、垂直中线偏差 0、相邻间距全 12px、单行排布；操作列五类行（可用 / 待重登 / 封号 / 已暂停 / 冷却中）签到·余额·任务·标签·变体槽左缘逐行偏差 0、移除右缘逐行偏差 0、全部单行；趋势 footer 左右贴边距 17px、中段居中；820px 窄屏头部溢出 0；回收站/积分构成页 `xs` 控件与按钮同高；明暗两主题均核对。

## [1.18.2-panel] - 2026-10-24

本版为**面板体验小件与默认值防线**：审查留档项的按需收尾批（评估结论：其余留档项在当前规模下收益不抵回归风险，明确搁置）。

### 新增

- **Esc 关闭弹窗**：全部 8 个 veil 弹窗支持 Esc 关闭——有清理逻辑的（添加账号/重登/任务）注册专属 close 函数（定时器/轮询随关随清），其余默认收起；`keyVeil` 带 `data-lock`（鉴权锁，密钥未填不能误关）。注册表置顶声明（中段注册调用先执行，const 声明在后会 TDZ——面板冒烟测试当场抓到，已修正）。
- **默认值对账快照测试**（`TestDefaultSnapshotGolden`）：`Default()` 经 normalize 的完整配置与 `testdata/default_snapshot.json` 逐行比对，任何默认值变化/新增字段都会红并指出具体行——更新时强制对照检查 normalize() 同字段兜底（Default 写了、normalize 漏改 = 面板保存一次就触发分支漂移，这正是审查发现的漂移窗口）。已做变异验证（改 jitter 默认值 → 测试红并精确定位）。
- **窄屏列优先级降级**（≤860px）：账号表隐藏 成功失败/在途/用量 列、请求记录表隐藏 IP/UA/Token/积分/请求ID 列，保留核心列；替代横向滚动。作用域限定 `#accTable`/`.acc.req`（不误伤模型/积分页表格）。

### 变更

- **`--ink-3` 对比度调整**：深色 `#6b7488 → #85909f`、浅色 `#8b93a5 → #6f7889`——该色大量用于 11.5-12.5px 小字，原对比度 3.8:1 / 3.1:1 低于 WCAG AA（4.5:1），各调一档。

### 验证

- `go test ./... -count=1` 全绿；UI 真实键盘走查（chrome-devtools）：Esc 关闭回收站/添加账号弹窗（含清理路径）、keyVeil 抗 Esc、800px 窄屏 5/6/7 列隐藏且账号/状态列保留、新对比度色值实测生效。

### 搁置决策（留档）

审查清单其余项经评估明确搁置：body 解析收敛 + handler 拆分（按需：等延迟体感或下次动 handler）、每帧双份解析（无体感）、usage 时区（Docker 化才需要）、client.go 文件拆分（纯组织）、autotask 表驱动（下次新增任务时顺手）、前端架构收敛（风险>收益）、RWMutex 改造（不成比例）。

## [1.18.1-panel] - 2026-10-24

本版为**安全加固与可观测**：全面代码审查第二批落地——安全三件、可观测三组、噪声治理三处、性能尾巴与前端体验批。

### 安全

- **`/status` 未配 key 脱敏**：`api_key` 为空（未启用鉴权）时不再透出 accounts 全量列表（uid/昵称/模型成本台账此前对任何能连到端口的人可见）；计数汇总保留（负载均衡/探活够用）。已配 key 的部署不变。
- **dump 目录 7 天保留清理**：dump 文件含完整用户提示词与上游响应，无限期留存既是磁盘增长点也是敏感数据留存风险。按文件名时间戳前缀清理（启动即清 + 每日一次），无时间戳前缀的文件不动。
- **入站透传头白名单**：`X-Conversation-Request-ID` / `X-Trace-ID` 此前任意串原样出站——既是非官方指纹，异常值（超长/控制字符）还可能让每次出站变 transport error 触发无谓轮转罚号。现过 `sanitizePassthroughHeader`（8-128 可打印 ASCII），不合法丢弃走网关生成路径。

### 新增（可观测，/status 新字段）

- **`guards`**：积分保底与每日限额护栏在选号主路径的拦截命中计数（此前完全静默——触发多少次面板与 /status 均不可见，审查发现的盲区）。
- **`persist_health`**：state.json 落盘健康（最近成功 Unix 秒 + 最近错误串）；持续落盘失败此前只活在节流日志里，面板无感。
- **`keepalive`**：smart 开关 / 阈值天数 / 退避表规模（`GetKeepaliveStats` 闭包注入，server 与 scheduler 保持解耦）。

### 修复（运维体验）

- **`fallback_earliest_expiry` WARN 节流 5 分钟一条**：池降级期（全池冷却+持续流量）每条 chat 请求一行的刷屏形态；状态本身经计数与 /status 可见。
- **probe 死号逐号「仍失效」行删除**：收尾已有「本小时探测 N 个死号」汇总行，长期全失效时逐号行是固定噪声。
- **`checkinAll` 日志文案更正**：旅行早已剥离独立排程，不再「含猫猫旅行」。
- **`restartRequiredFields` 恒返回全清单改为 `restartRequiredDiff` 逐字段比较**：只列真正变化的装配期字段（此前改一个热生效项也被告知十几项要重启，提示被稀释成噪声）；顺带补漏 `prompt.mode/file`、`upstream.client_version/cli_version/client_name/device_token*/dump_dir`。

### 性能

- **reqlog read 按文件日期倒序早停**：面板轮询此前每次全量解析所有归档文件（上限 100MB/7 天）后才截断 limit；现凑满 limit 且剩余文件日期均早于已见最早事件时停止。调试中发现归档文件名日期是 `2006-01-02` 连字符格式——首版按 `YYYYMMDD` 解析失败返回零值，零值 `Before` 恒真导致误早停丢数据（既有测试抓住），解析失败现按「不多停」兜底双保险。
- **reqlog stats 30s 缓存**：磁盘统计（Files/Bytes）此前每次 5s 轮询都 ReadDir+逐文件 Info；实时字段（dropped/lastError）不缓存。
- usage `SnapshotWindow` 锁内窗口过滤**评估后放弃**：桶键是复合 scope（`h:`/`d:` 前缀 + 小时/天双粒度），简单字典序过滤会错——留档待专项设计。

### 前端

- **轮询离线指示**：后端不可达时此前界面仍显示上次的「服务正常」（轮询静默吞错）；连续 2 次失败后 navPulse 转红并显示「连接断开（上次成功 N 秒前）」，恢复自动回归。
- **批量按钮防双击**：五个批量任务按钮接入 `busyBtn` 包装（点击禁用 + 进行中文案），此前双击即重复 POST。
- **弹窗自动关闭定时器句柄管理**：添加账号 / 重登的 1600ms 自动关闭此前不保存句柄——期间手动关闭再重开会杯旧定时器误关新弹窗。
- **localStorage 安全包装**（`lsGet`/`lsSet`）：隐私模式/禁用存储的浏览器此前顶层裸访问抛 SecurityError 中断整份脚本（面板整体白屏）。
- **toast 容器 `role="status" aria-live="polite"`**：屏幕阅读器可感知成功/失败反馈。

### 验证

- `go test ./... -count=1` 全绿（25 包，含 /status 脱敏语义新用例）；`node --check` 与前端断言通过。

## [1.18.0-panel] - 2026-10-24

本版为**正确性与热路径加固**：全面代码审查（后端核心链路 / 支撑模块 / 前端单文件三路并行）后的第一批落地——P0 全部正确性问题 + P1 局部性能三项。

### 修复（正确性）

- **SSE 写出竞态补齐**（最严重，1.16.1 引入）：注释行透传与 `[DONE]` 收尾此前未走 `wmu` 互斥，与心跳 goroutine 并发写同一 ResponseWriter 是数据竞争；Anthropic 路径下 w 是 `anthropicStreamWriter`，并发 `Write` 同时 append 共享 buf 直接踩内存。
- **`data:` 无空格形态**：SSE 规范允许 `data:` 后不带空格——此前该形态落入注释分支原样透传（绕过白名单、不计帧数），随后被误判空流补写伪造 error 帧（正文+假错误并发给客户端）。
- **双层心跳抑制**：`upstream.heartbeatSuppressor` 约定接口——anthropicStreamWriter 实现后 StreamHint 层不再起心跳 goroutine（其 `: ping` 写给转换 writer 会被丢弃，纯白跑且徒增并发面）。
- **`chatPaths` 空集兜底 panic → 防御性 error**：生产请求路径不得断连。
- **web_search 多轮观测累加**：trace.stat 此前被每轮覆盖，归档只反映最后一轮（前几轮 token/credit/attempts 丢失）。
- **state 落盘改锁外 IO**：saveLocked（持锁 Marshal+fsync+rename）拆为 persistState——磁盘被实时扫描拖慢时不再阻塞选号与状态查询（credithistory 死锁同款残留的最后清理）；写盘失败 dirty 复位自动重试。
- **账号删除清理趋势历史**：Remove/SyncToDir 顺带清 creditHist——30 天裁剪只发生在该号下一次采样，已删号永不采样、点位无限滞留。
- **余额轮询齐射**：RunBalanceRefreshNow 加 semaphore=4 并发上限 + 号间 200-600ms 随机延迟（M5 抖动消了任务时点齐射，漏了轮询内齐射）。
- **探活可取消 + 异步**：不可取消的 time.Sleep 同步阻塞余额刷新循环与保活退避重试（死号多时一探几分钟）。
- **抖动键加任务种类**：默认配置签到与旅行同为 [9,21] 点，共用 uid|day 键时同号两任务同秒双请求——「每号双请求齐射」特征未消净。
- **备份时点 CST**：UTC 容器上 backup.hour=3 此前按宿主本地时判，实际 CST 11 点才跑。
- **前端 3 处回落值补 esc**（日志频道/队列状态/fmtClock 失败原样返回）+ 两处授权页 `open()` 加 `'noopener'`（window.open 不享受隐式 noopener，授权页可拿 window.opener）。

### 性能

- overview 轮询消第二次全池 List 深拷贝（tagsByUID 复用已取快照）。
- reqlog.Record 头插改尾插（O(1) 摊销，此前每请求搬移 ~100 条×~200B）；读侧反转保持 JSON 契约。
- Classify 四组 marker 循环改 init 预计算小写表（错误风暴下每路径重算 ToLower 的纯浪费消除）。

### 审查留档（后续迭代）

- **v1.18.x 候选**（见 HANDOFF §4）：body 每请求 5+ 次全量 JSON 解析的收敛（与 chatCompletions 拆分一起做）、出站每轮转 3 个 parse+marshal 周期、每帧双份 JSON 解析；usage 时区统一 CST（涉存量桶键迁移，单独评估）；/status 在 api_key 为空时的账号拓扑脱敏；dump 目录保留策略；护栏触达率/保活退避水位/抖动分布的可观测透出。
- **P2 重构清单**：chatCompletions 868 行拆五块（六个 fail-fast 4xx 分支是同一模板 5 份复制）、client.go 2553 行按域拆五文件、autotask 双口径三处散落收敛、config Default/normalize 双写漂移、前端 30 个全局变量与双授权流重复。

### 验证

- `go test ./... -count=1` 全绿（25 包）；`go build` / `go vet` 干净；`node --check` 与前端断言通过。

## [1.17.1-panel] - 2026-10-24

本版为**出厂默认配置对齐**：把 A 机生产验证过的运维值固化为 `Default()` 出厂默认与 `config.example.json` 参考配置（「默认是否最优」评估的落地）。

### 变更（⚠ 默认值变更，升级即生效于「未显式配置」的部署）

- **`schedule.keepalive_mode` 出厂默认 `daily` → `smart`**：RT 恒 60 天滚动，每日全量强刷 ≈97% 冗余且构成齐射特征（10-23 批量吊销的可疑形态）。未显式配置的部署升级后自动进入 smart（只刷 14 天内临期号）；显式 `"daily"` 可回退。normalize 的非法值回落同步对齐出厂默认。
- **`schedule.balance_refresh_minutes` 出厂默认 `5` → `10`**：小时级趋势粒度下 5 分钟过度采样，上游可见调用频率减半。未显式配置的部署升级后生效。
- **`pool.credit_floor` 出厂默认 `0` → `150`**：每号永远留够免费模型额度，防收费模型打穿后 402 硬冷却连免费都不可用。**未显式配置的部署升级后保底从关闭变为开启**——不想要请显式 `"credit_floor": 0`。已显式配置的部署（如 A 机 150）无变化。

### 显式说明

- `prompt.mode` 出厂默认本就是 `passthrough`（对齐上游），与生产一致，不变。
- 其余全部配置项（任务时点 / 抖动 / 冷却熔断参数 / sticky / backup / trash / notify / strict_lease）与生产生效值一致，未动。
- **A 机生产无需升级**（三项已显式配置为相同值，1.17.1 无其他行为变化）。

### 验证

- `go test ./... -count=1` 全绿（25 包）；`TestCreditFloorDefault` / `TestBalanceRefreshDefaults` 随默认值更新（含显式回退用例：5 分钟 / 0 保底仍合法）。
- `config.example.json` 三值对齐。

## [1.17.0-panel] - 2026-10-24

本版为**路由与护栏能力版**（对标 `ardeyouxipianyi/workbuddy2api-hub` 与同源上游 `misakano7545/workbuddy2api-panel` 的 P1 清单，落地四项；一项依赖上游接口实测顺延）。

### 新增

- **模型降级路由 `pool.model_fallback`**（misakano PR #52 同语义）：请求模型在 6004 模型级限流时改用配置的等价模型继续服务。两个触发点——①上游 6004 响应后：冷却按**原模型**记账，**同账号**换等价模型重试（降级是换模型不是换号）；②选不到号兜底：全池该模型都在冷却时先降级再报 503（否则最需要的场景走不到降级）。`curModel`（实际用途）与 `bareModel`（冷却记账）分离——误记等价模型会把降级目标也冷却掉；`fallbackTried` 防 A→B→A 循环；降级不写持久状态，冷却到期自动回原模型。热生效。测试 4 例（同账号重试+冷却归属 / 兜底 / 防循环 / 未配置零回归）。
- **每日限额护栏三件套**（hub 同语义精简版）：`pool.daily_token_limit`（单号当日 token 总量达限不接单）/ `pool.daily_credit_limit`（当日积分消耗达限只接免费模型）/ `pool.model_daily_token_limit`（单模型当日 token 达限只拦该模型）。计数按 CST 日键随请求记账（失败尝试不计），**跨日自动清零解封**（无需定时任务）；随 state.json 持久化（重启不重置）；选号三路径（普通轮换 / 粘性 / 全冷却兜底）统一生效，免费/付费判定与「积分保底」同口径（实测台账优先、目录倍率兜底、未知放行）。热生效。测试 3 例。
- **余额趋势视图**（misakano PR #49 同方向轻量自实现）：`SetCreditsDetailed` 按小时桶采样（同桶覆盖、30 天窗口），独立文件 `data/credits-history.json`（safefile 原子写、不进 state.json 关键路径）；面板账号池新增「余额趋势」box——全池积分总和 SVG 曲线（区间/点数/起止标注、悬停看每小时值），前端 5 分钟缓存。**锁纪律**：采样只改内存 + 原子脏标记，落盘由 flusher 周期（5 分钟节流）与 Close 兜底在**锁外**执行——实测持锁写盘与 flusher 的 saveLocked 互撞放大等待（死锁形态），教训已注释。测试 2 例。
- **会员档位显示**（hub #621 同语义）：新增 `upstream.UserResourceSummary`（get-user-resource-summary 双路径，解析宽松——`IsPaidUser`/`SubscriptionPackageName` 任意嵌套层级大小写不敏感）；余额刷新循环内顺带拉取（**CST 一天一次**节流，避免高频轮询成为上游可观测特征），失败保留旧值不写结论；面板账号行昵称旁胶囊（paid = 套餐名、free = Free、未读不显示）。测试 1 例。

### 顺延（v1.17.1 待办）

- **模型目录动态同步上游 /v3/config**：依赖对上游接口响应形态的实测验证（宣告字段与风控指纹关联，不可盲并），单独迭代。

### 验证

- **单测**：`go test ./... -count=1` 全绿（25 包），新增 10 测试例（fallback 4 / 护栏 3 / 趋势 2 / 档位 1）。
- **构建**：`go build ./...` / `go vet ./...` 干净；`node --check` 与前端断言通过。
- 配置增量：`pool.model_fallback` / `pool.daily_token_limit` / `pool.daily_credit_limit` / `pool.model_daily_token_limit`（均热生效，`config.example.json` 同步）。

## [1.16.1-panel] - 2026-10-24

本版为**流式稳定性加固**：对标 `ardeyouxipianyi/workbuddy2api-hub`（v1.7.33）与同源上游 `misakano7545/workbuddy2api-panel` 后的精选迭代，只取四项小成本高收益修复；两项核对确认我方已有防护（记录结论防未来回归），一项核对发现 hub 修的能力我方早已具备。

### 新增

- **SSE 空闲心跳（OpenAI 路径）**：长思考模型（hy3 / deepseek-v4-pro 等）推理 30 秒+无输出时，客户端或中间代理会因读超时断连（表现为「对话中途断开」）。`upstream.StreamHint` 起旁路 goroutine，距上一次写出超过 15 秒即写一条 SSE 注释帧 `: ping\n\n`——SSE 规范保留语法，EventSource / OpenAI SDK 的解析器一律忽略，仅保活链路。写侧经 mutex 与主循环互斥（注释帧原子完整不撕裂）；帧密集流跳过 tick（`lastWriteAt` 判定，不给活跃流加噪音）；客户端已断连时写失败静默（主循环下一次写正常走断连处置）。
- **SSE 空闲心跳（Anthropic 路径）**：`/v1/messages` 流式经 `anthropicStreamWriter` 翻译，StreamHint 的注释帧会被其 `handleFrame` 丢弃（不带 `data: ` 前缀，到不了客户端）——故心跳必须由本层发：首帧写出后每 15 秒发一条 Anthropic 官方 `ping` 事件（SDK 规范忽略），`event()` 经 mutex 与心跳互斥，`finish()` 停止。zCode / Claude Code 的长推理会话不再中途掉线。

### 修复

- **流式 chunk id 整流统一**（hub #607 口径）：issue #35 已兜「缺 id 补哨兵」，但**带不同 id 的帧保留原样**——上游确有流内 id 漂移的实测形态，客户端按 chunk id 管理流状态时 id 变化会让界面卡在「生成中」。改为首帧捕获 id 后全帧强制覆写统一（OpenAI 流式规范：一条响应一个 id）；error 帧走 writeRaw 原样透传不受影响。
- **签到幂等误判回归钉子**（hub #596 教训）：hub 实测上游 `200 + code 10001「请求处理中，请勿重复操作」`曾被判成签到成功。核对结论：我方 `IsAlreadyCheckin` 的 markers 只有「已签到 / already」两类真实幂等文案，**无此 bug**——补 6 断言回归测试钉住（失败回执 / 限流文案 / 幂等正例 / 网络层错误四类边界），防未来 markers 漂移。

### 对标核对结论（不开发，记录防重复调研）

- **模型冷却窗口跨重启恢复**（hub #638）：我方 `modelCooldowns` 自 1.15.6 起已持久化进 `state.json`，恢复时惰性过滤过期条目 + safefile 原子写 + `.bak` 回退 + `pool_test`「survive reload」覆盖——hub 修的正是我方已有能力，零缺口。

### 验证

- **单测**：`go test ./... -count=1` 全绿（25 包）。新增 `internal/upstream/sse_heartbeat_test`（id 整流 / 慢流心跳 / 活跃流无心跳 3 例）、`internal/server/anthropic_ping_test`（ping 事件与 finish 停止 / 高频事件与心跳并发不撕裂 2 例）、`checkin_retry_test` 追加回执判定 1 例。
- 发布物与 v1.16.0 同流程：版本常量 `1.16.1-panel`、tag `v1.16.1`、CI 五平台产物。

## [1.16.0-panel] - 2026-10-24

本版为**会话失效恢复与防复发**：针对 1.15.7 实案（2026-10-23 六账号 RT 链被上游批量吊销、只能人工重登、死号混在普通禁用里不可辨识、无任何告警）的系统性治理，按「可见 → 恢复 → 防复发」三层布防（设计文档 `docs/DESIGN-v1.16.0.md`）。

**触发背景**：2026-10-24 会话复查——生产 `state.json` 中 7 个禁用号（6 个 `12153 session dead` + 1 个 `11140 banned`）在面板「积分到期提醒」上持续显示「查询失败：upstream session_dead (http 401)」，用户每天点「刷签」保活却无效。根因复核：①「签到」按钮只领积分不刷 token；②禁用号被所有保号任务跳过；③RT 链已被上游吊销（refresh 端点回 `code=12153 invalid_grant`），任何刷新/保活/重导 JSON 都救不回来；④批量吊销的最可疑诱因是「整点齐射刷新 + 同批凭证外部执行体双花」。

### 新增

- **死号分组与一键重登（M1）**：`pool.Status` 新增 `dead_kind` 派生字段（`session_dead` / `banned`，分类收口 `deadKindOf`，前端不猜 reason 字符串）；导出 `pool.IsSessionDeadReason` / `IsBannedReason` 供告警与探活复用。账号池新增状态筛选（待重登/封号 vs 可用正常）；死号行显示醒目「待重登」（session_dead）/「上游封号」（banned）标签，session_dead 行内挂「重登」按钮 → `#reloginVeil` 弹窗**复用现有设备授权流**（loginPoll 成功后 SaveAtomic 覆盖同 uid 凭证 + `Pool.Add` upsert + `Pool.Revive` 全清复活，后端零新增端点）；处理「登成另一个账号」边界提示与 15 分钟授权会话过期重试。
- **批量判死实时告警（M2，`internal/alert`）**：`pool.SetOnDisabled` 禁用 hook（`disableLocked` 末尾锁外 goroutine 触发，全部禁用路径单一收口）→ `alert.Manager`：窗口内（默认 10 分钟）distinct uid 判死数达阈值（默认 3）即 ① 面板 `#killBox` 横幅（`overview.mass_kill`，**无冷却**——事件老化自然消失）② webhook 外发（`event=mass_session_dead`，默认冷却 30 分钟，失败重试 1 次静默，不影响横幅）。只统计会话失效类禁用（banned / 人工禁用不计入）。配置 `notify` 段（webhook_url / mass_kill_accounts / mass_kill_window_minutes / cooldown_minutes）热生效。
- **亚健康可视化（M3）**：`session_dead_fails` API 此前已透出但前端未渲染——账号池状态列补 `session n/3` 橙色角标（连续判死进度，满 3 自动禁用；保活/请求成功即清零），赶在满阈前人工干预。
- **RT 轮换审计与外部双花检测（M6，`internal/rotaudit`）**：`upstream.Client` 的 `RefreshToken`/`RefreshTokenIfNeeded` 加 `source` 参数（keepalive/chat/balance/signin 五调用点），新增 `RotationObserver`——**仅 leader 记录**（等待者/60s 宽限复用不重复），RT 以 sha256 前 8 位指纹落 JSONL（按天切、保留 7 天、host/pid 自证，不落明文）。启动以池内当前 RT 播种基准；此后任何一次刷新的旧指纹 ≠ 基准 = **本进程之外有执行体在消费该账号的 RT**（另一台机器的部署副本 / 独立 signin.sh 进程）——ERROR 日志 + webhook（`event=external_token_rotation`，独立冷却）+ 横幅「凭证双活」行（`overview.external_rotations`，**任意数量即显示**，不凑阈值）。重登成功经 `OnLoginRotate` 回调重置基准，不误报。
- **auths 目录租约锁（M4，`internal/authlease`）**：`auths/.lease.json` 租约（30s 心跳 / 90s 新鲜阈值），防「同一 auths 目录被两个执行体同时挂载」。异机新鲜租约 + `auths.strict_lease=true`（默认）→ ERROR 拒绝启动；false 降级 WARN 放行（逃生门）。同机租约（崩溃残留/双进程）只 WARN 接管——保护计划任务重启路径，同机双进程由 M6 轮换审计兜底。文件名不匹配 `auth.LoadDir` 的 `workbuddy*.json` glob，零回归。
- **死号探活（M8）**：`schedule.probe_dead_hours`（默认空 = 关闭）定时 + `POST /panel/api/probe_dead_all` 手动触发，对 session_dead 类禁用号 refresh 探测（**banned 不探**；号间随机 5–11s）：成功即 `ReviveDisabled` + 落盘 + WARN + webhook（`event=dead_recovered`）——捕捉上游误吊销解除的恢复窗口；失败静默不计数。默认关闭（探死请求本身是上游可观测特征），手动按钮仅在存在死号时显示。

### 变更

- **保号任务时点抖动（M5）**：定时五类任务（签到/旅行/活跃/保活/夜猫子）每号按 `fnv32(uid|CST日期)` 分散到 `[0, jitter_minutes)`（默认 20 分钟，0 = 关闭回到齐射）执行，消除「17 号 22:00:00 同一秒齐射 refresh」的批量风控特征；偏移同号同日稳定、跨号分散、跨日轮换；等待可被 ctx 取消（优雅停机）；**面板手动按钮不受影响**（运维语义是"现在就做"）。签到/保活/旅行/夜猫子的单号逻辑抽为 `checkinOne`/`keepaliveOne`/`blackcatOne`（旅行沿用既有 `travelOne`），手动与定时两路共用，无逻辑复制。
- **智能保活（M7）**：`schedule.keepalive_mode=smart`（默认 `daily` 兼容）时定时保活只刷 `ExpiresAt` 距今 < `smart_keepalive_days`（默认 14 天）的账号——RT 实测恒 60 天滚动（`expiresIn=5184000`），每日全量强刷 ≈97% 是冗余上游调用且构成齐射特征；smart 下 17 号日均刷新量降至 ~0.4 次且各号天然错峰。**手动「全部保活」始终全量**。刷新失败（非 session dead）进退避 30m→2h→6h→24h 封顶，由余额刷新循环驱动重试（修「一轮失败要等 24h」断档）；12153 判死不进退避（死号重试无意义，走连续计数）。
- **配置项**：`schedule` 段新增 `jitter_minutes` / `keepalive_mode` / `smart_keepalive_days` / `probe_dead_hours`；`notify` 段与 `auths.strict_lease` 见上；`config.example.json` 同步；全部热生效（`auths.strict_lease` 除外，进重启清单）。

### 验证

- **单测**：`go test ./... -count=1` 全绿（25 包）。新增 `internal/pool/deadkind_test`（分类契约/状态流转/判定函数 3 例）、`internal/alert/alert_test`（reason 过滤/distinct uid 达标/窗口老化/webhook 载荷与冷却/失败静默/硬上限裁剪 6 例）、`internal/rotaudit/rotaudit_test`（播种后正常轮换/外部检测/Reset 免误报/审计形状（不含明文 RT）/清理过期/指纹稳定性 6 例）、`internal/authlease/authlease_test`（新鲜获取/stale 接管/同机不阻断/异机 strict 拒绝与放行/Release/空目录 6 例）、`internal/scheduler/jitter_test`（偏移稳定性/范围/跨日轮换/17 号分散度/退避序列/退避驱动/smart 过滤/探活复活/探活失败静默 9 例）；既有 refresh 测试随签名适配。
- **沙箱端到端**（`:7871`，生产 17 账号副本、调度全关）：`overview` 透出 `version=1.16.0-panel`、`dead_kind`（6×session_dead + 1×banned）、`mass_kill=null`、`external_rotations=null`；启动日志确认租约获取（`.lease.json` 落盘）、审计播种 17 号、告警配置；`POST /probe_dead_all` 全链路（死号 refresh 回 12153 静默处理）；`GET /panel/api/config` 新字段回显正确。
- **UI 真实点击走查**（chrome-devtools 真实输入事件）：死号「待重登/上游封号」标签渲染；状态筛选 dead=7 行 / ok=10 行；「重登」→ `#reloginVeil` 打开（目标昵称/realm 预选）→「获取授权链接」→ 真实上游授权 URL（`copilot.tencent.com/login?platform=CLI&state=…`）+ 复制/打开按钮；「探测死号」toast「死号探活已开始」；配置页新表单回填（jitter=20 / mode=daily / days=14 / probe 空）；`TestIndexHTMLUniqueIDs` / CSP / `node --check` 断言通过。
- **发布前**：`go build ./...` / `go vet ./...` 干净；版本常量 `1.16.0-panel`。

## [1.15.7-panel] - 2026-10-23

本版为**会话失效治理**：从同源上游 [misakano7545/workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel) 逐补丁移植三项（网关 401 分类 / 余额 401 求证 / RT 单飞），另针对 chat 路径补一处本分支加固（裸 401 不再单发致命）。

**触发背景**（2026-10-23 本机实案）：账号池 6 个账号在 11:41 一次批量刷新后集体 401，面板显示 `upstream client (http 401)` 却是 `ErrClient`（只换号不罚），死号从 11:41 一直挂到 13:40 仍在池内参与调度；查 refresh 端点全部回 `code=12153 invalid_grant: Session doesn't have required client`，确认会话已在上游作废、只能重登。

### 新增

- **网关 401 归类为会话失效**（移植上游）：APISIX/openresty 的 401 HTML 页（`<title>401 Authorization Required</title>`，无 `{"code":…}`/`{"msg":…}` 业务信封）此前落 `ErrClient`——语义是「只换号不罚」，于是死号永远留在池里，每次请求都白打一轮上游、日志刷一串 401。新增 `upstream.IsGatewayUnauthorized(status, body)`，在 `Classify` 的 WAF 403 之后、图片/参数判定之前判为 `ErrSessionDead`。业务层 401（带信封，如 `code=12153`）分类路径不变。
- **余额探测 401 的登录态求证**（移植上游 `internal/scheduler/balance_401.go`）：余额刷新是池内唯一的分钟级健康观测量，此前错误分支只 `log.Printf` + `return`，不碰池状态——上游批量注销会话后面板仍显示「可用」，要等每日 22:00 保活连败满阈（≈3 天）才转 disabled。新增 `noteBalanceAuthFailure`：401 后用 refresh 做权威求证，区分两种成因——refresh **成功**即「降级 token」（billing 恒 401 但 chat 仍可用），换新 token + `ClearSessionDead`，**不误杀**；refresh 回 12153 才计入 `sessionDeadFails`，满 3 次禁用。判定同时看 `Kind == ErrSessionDead` 与 `strings.Contains(msg, "12153")`（上游 12153 文案本地化后可能不含 marker，会落成 ErrClient）。30 分钟退避**只记成功**，避免降级 token 每轮探测都轮换 RT（一天 288 次）。
- **`pool.Status` 透出 `session_dead_fails`**：此前只落 `state.json` 不透出，运维在 `/panel/api/overview` 看不到「离禁用还差几次」的进度。

### 修复

- **并发刷新同一 RT 导致整条 RT 链被上游吊销**（移植上游 PR #7，merge `28497e8`）：同一账号的 refresh token 被两个 goroutine 同时消耗时，第二次会撞上游 rotation/replay 检测（12153 `refresh token is invalid`），把整条 RT 链吊销、账号只能重新 OAuth。旧实现是两段式（锁内读快照 → 锁外 HTTP → 锁内写回），写回守卫只能防重复写，**挡不住第二个刷新请求已经发出**。新增按 `UID + "\n" + RT` 的单飞事务：同 RT 并发共享一个 leader、只打一次上游；按需刷新（`RefreshTokenIfNeeded`）成功后 60s 内复用结果；**keepalive 的强制刷新只共享在途结果、不吃 60s 宽限**（否则上游不换 RT 时会把后续真失败掩盖成成功，破坏 12153 连续计数）；并发落败方双检 token 已换新则返回 nil，不误判 `ErrSessionDead`；leader panic 经 `sync.Once` 释放等待者。
- **落盘重试**：新增 `auth.SaveAtomicRetry`（0 / 100ms / 300ms），chat / keepalive / signin / 余额求证四路统一改走重试。上游已轮换 RT 后落盘抖动一次就会丢新 token，账号下次仍需重登——与 RT 单飞防的是同一类事故（余额取证路径刻意偏离上游 `SaveAtomic`）。
- **chat 路径裸 401 单发永久禁用**（本分支加固，刻意偏离上游一处）：移植上述 401 分类后，一条 chat 请求撞上一次裸 401 即 `Pool.Disable`，而同一事实在余额链路要 3 次 + refresh 求证才判死，口径冲突。若 APISIX 侧短时故障或认证规则误配、对整批 token 统一回裸 401，网关轮转几个号就能在几分钟内清空整池（修复前该形态是 `ErrClient`，只换号不罚，所以这是本补丁新引入的杀伤面）。改为：裸 401 走 `NoteSessionDead` 连续计数，满 `sessionDeadThreshold`（3）次才禁用；**带业务信封的 12153 与 refresh 端点回 12153 仍单发禁用**（权威判据不打折）；`NoteSuccess` 会清计数，网关抖动攒不满 3 次。日志打 `裸 401（session dead 计数 n/3）` 与 `连续 N 次裸 401 — 禁用`，与余额链路口径一致。

### 语义修正

- **`cmd/signin` 的刷新窗口**：`NeedsRefresh(2*3600)` 传的是 7200 **纳秒**（≈ 每次必刷），改为 `2*time.Hour`。属与函数语义对齐的修正，会实质降低 signin 的 RT 轮换频率，**不是等价移植**，单独列出。

### 验证

- **单测**：`go test ./... -count=1` → 21 个包全绿，0 FAIL。新增 `internal/upstream/refresh_test.go`（8 例：单飞 / 强制与按需共享在途 / 宽限复用与过期 / 强制绕宽限 / 落败不误判 / 新鲜跳过 / panic 释放）、`client_test.go` 5 例（401 裸 HTML、空体、纯文本、401+12153、401+未知信封）、`internal/scheduler/balance_401_test.go` 5 例（含本地化 12153 文案）、`internal/server/handler_test.go` 3 例（裸 401 三次才禁 / 信封 12153 一次禁 + 负向日志断言 / 成功清零）。
- **独立 QA 回归（两轮，含真实上游沙箱）**：17 账号副本、`include_disabled_in_tasks=true`、其余任务全关，7871 端口。6 个已失效账号三轮走完 `1/3 → 2/3 → 禁用`，日志从 `upstream client (http 401)` 变为 `upstream session_dead (http 401)`，全量 stdout 过滤旧字符串 **0 命中**；落盘 `disabled=true, reason="12153 session dead"`。健康账号 `6cf0d820` 三轮前后各查一次余额均 **200**、credits 正常、`session_dead_fails` 保持 0，其余 9 个健康号同样未被波及。
- **边界反例**：403+HTML 仍判 `ErrWafBlock` 不进计数；`{"code":10001}` 走 `ErrClient` 不判死；429+12153 混排仍单发禁用；`uerr == nil`（SSE error frame 路径）仍单发禁用；401 后 refresh 回非 12153 错误不计罚；无 RT 账号不 refresh 不计罚。
- **未覆盖**：`-race`（本机无 gcc，并发正确性靠单测 ×10 + 锁序审查 `refreshMu → a.mu` 无反向）；真实「降级 token」账号（池内无样本，用假上游构造）；多进程共享 `auths` 目录（单实例部署不涉及，单飞与退避均为进程内状态）。

## [1.15.6-panel] - 2026-10-22

本版为**上游同步版**：从同源上游 [misakano7545/workbuddy2api-panel](https://github.com/misakano7545/workbuddy2api-panel) 选择性移植 4 项 P0 修复（不合并分支，逐补丁移植，未引入上游的 `/v1/responses`、`/v1/search`、API Key、预算双闸等大功能）。

### 新增

- **`14003` 模型繁忙改用独立短窗口**（移植上游 `f728859`）：上游模型提供方繁忙返回 `14003` 时，此前落到 `status==429` 兜底走 `ErrSoftRate` 的**账号级**阶梯冷却（600s 起、封顶 2h），一个模型的繁忙会连坐整个账号、健康号被无谓停用。本版新增 `pool.CooldownModelBusy`（窗口 `10s` 起、封顶 `1m`，只写 `modelCooldowns[model]`、不碰账号级 `until`；窗口内重复命中不延长、已有更晚冷却不缩短）与 `upstream.IsModelBusy`（`code=14003`，容忍 JSON 空白与字符串形态）。`handler` 侧仅当上游**未给出**恢复时刻（无重置文案 / 无 `Retry-After`）时才走短窗口，给出明确时刻则维持既有口径；`model` 为空时退回账号级路径（避免写 `modelCooldowns[""]` 等于不罚）。**`6004` 的判定与冷却参数逐字节未动**（`IsModelRateLimit` 仍只认 6004）。
- **CST 固定时区**（移植上游 `025cc10`）：新增 `internal/cst`（`Loc`/`Now`/`Day`，固定 UTC+8），收敛此前散落的内联 `FixedZone`，并把**上游自然日语义**的位置统一到 CST：签到日键、次日 04:00 重置、调度 `nextFire` 的 `hours` 解释、连登补签的「昨天」、夜间窗口与促销时段。此前这些点取宿主本地时区，进程一旦跑在非 UTC+8 主机上，日界会整体漂移 8 小时且无任何报错。**`internal/usage`（用时分桶）与 `internal/reqlog`（日志切天）保留宿主本地时区**——那是给操作者看的统计口径，不随本改动变化。

### 修复

- **状态文件写入非原子，掉电会留下全 0/半截文件并冲掉整个账号池**（移植上游 `4356a34`）：新增 `internal/safefile`（`tmp → fsync → rename`；`WriteKeep` 把上一份有效内容保留为 `.bak`，坏文件另存 `.bad-<时间>` 而不污染 `.bak`；`Read` 正文损坏时回退 `.bak` 并打 WARN，两边都坏才报错，文件不存在不复活 `.bak`）。接入 `pool/persist.go`（`state.json`）、`auth/auth.go`（`SaveAtomic`/`LoadDir`，账号文件损坏从「静默跳过」改为 **ERROR 点名**）、`cmd/server/config.go`、`cmd/server/main.go`、`internal/usage`、`upstream/model_catalog.go`。**落盘路径与 JSON 结构一字未改**，只换写法；`auth.LoadDir` 的 glob 仍是 `workbuddy*.json`，`.bak` / `.bad-*` 不会被当成账号加载。
- **出站 `traceparent` 在入站 trace 只有 16 位 hex 时非法**（本版实测发现，上游同源缺陷）：`X-B3-TraceId` 允许 16 或 32 位，而 W3C `traceparent` 的 trace-id **只允许 32 位**。客户端传 16 位 `X-Trace-ID`（Zipkin 式 64 位 trace id）时，旧实现把 16 位原样拼进 `traceparent`，产出官方客户端绝不会发送的非法头——这是比"头族不全"更明显的非官方指纹。修复：16 位入站值**左补零扩展**到 128 位（OpenTelemetry 对 64 位 trace id 的等价做法，链路关系不变），全零（W3C 明令非法）回落恒 32 hex 的 `messageID`；`X-B3-TraceId` 仍保留入站 16 位原值。
- **出站头族与标识对齐官方两版真机**（移植上游 `8f7366f` `d030e0f` `5605781` `edd0dba`）：`headers.go` 补齐 `b3` 单头、`traceparent`、`X-B3-ParentSpanId`、`x-stainless-*` 九项、`x-agent-type`/`x-agent-intent`、`acp-connection-id`（同 `(账号, 会话)` 稳定派生）；`X-Trace-ID` 不再回落 `conversationRequestId`、`SpanId` 独立随机；`X-Product` 恒 `SaaS`（去掉重复的 `X-Product=<name>`）。`cache_key` 从 `wb2a-<uid8>-<hex>`(48) 改为 **UUID v4 形态**(36)，键里不再含 uid 明文与项目缩写（`purpose==""` 时与旧 key 逐字节一致，缓存槽零漂移）。`desktop.go` 的专家对话不再手搓头，统一走 `ChatHeaders`。上报体里的 `wb2api-mp-*` / `wb2api-<ms>` / `wb2api-adopt-*` / `wb2api-night-*` 等自证前缀一律换成 `session.NewConversationID()`/`NewMessageID()`。`conv_fields.go` 给**当前轮最后一条 user 消息**补 `conversationRequestId`/`startsNewUserRequest`/`agent:"cli"`（历史轮次不补、客户端自带值优先、坏 body 原样返回）。`internal/session/ids.go` 的 `NewTraceID`/`NewSpanID`/`NewConversationID` 对齐官方形态。
  - 未采用：上游同批把默认版本抬到 CN 5.8.3 / global 5.8.4 + CLI 2.161.0。版本段是运维可配项（`upstream.client_version` / `cli_version`），需要时在 config 里覆盖，不改代码默认值。
  - 未改动：本地 `sanitize.go` 与 `X-IDE-*` 头族原样保留；`X-Auth-Method`/`X-Userinfo`/`X-Id-Source` 本地本就零注入（上游对应补丁无净改动）。

### 验证

- **单测**：`internal/upstream/headers_test.go` 新增 `TestChatHeadersTraceparentAlways32Hex`（4 子例：32 位原样 / 16 位左补零且保留原值 / 空值现造 / 非法值回落）与 `TestChatHeadersTraceparentRejectsAllZero`；`cache_key_test.go` 断言升级为 UUID v4 形态 + 不含 `wb2a`/`wb2api`/`workbuddy2api` 与 uid 明文；`pool` 新增 14003 短窗口阶梯/不缩短更晚冷却、`safefile` 撕裂文件回退、`cst` 的 UTC 主机回归。全量 `go test ./... -count=1` **18 个包全绿**（基线 16 包 + 新增 `cst`/`safefile` 2 个）。
- **独立 QA 用例**（临时文件，验证后删除）：16 条对抗性用例覆盖「config 缺失仍返回 `fs.ErrNotExist` 让 main 走自动生成」「`.bak`/`.bad-*`/`.tmp` 不被当账号加载」「`TZ=UTC` 下 `cst.Now()` 仍 +8」「连续 3 次 14003 不越过 1m 且账号 `cooling=false`」「`6004` 语义未变」「撕裂文件回退 `.bak`」——其中 **1 条抓到真实缺陷**（即上文 `traceparent`），其余 15 条通过。
- **发布前验证**：本版在沙箱 `:7871`（17 个账号副本、全部调度关闭）对**真实上游**做了端到端验证（healthz 17/17、`/v1/models` 56 个模型、非流式 / 流式 / `/v1/messages` / 工具调用均 200），并于 2026-10-22 15:11 完成生产切换（`1.15.4-panel` → `1.15.6-panel`，停机 5–8 秒）后由真实客户端流量验证；`go build ./...` / `go vet ./...` 干净。

## [1.15.5-panel] - 2026-10-15

### 修复

- **上游免费档排队（`6020 queue.waiting` / `6021 queue.full`）被误判为账号级限流，106 秒内把全池 16/17 个账号烧进冷却（本机实案）**：上游服务端队列积压时返回 `HTTP 429` + 业务码 `6020`（`queue.waiting.title`，带 `queue_position` / `queue_size` / `retry_after`）或 `6021`（`queue.full.title`），响应还带 `client_user_type:"free"`。这是**上游服务端队列水位**，与「哪个账号」「哪个模型」都无关（实测同一时刻换 `cn:kimi-k3-1` 与 `cn:deepseek-v4.1-flash` 回的都是排队码）。`Classify` 此前没有该码，落到 `status==429` 兜底 → 被当成「这个账号被限流了」逐号软冷却。实案时间线：`11:18:46 → 11:20:32` 仅 6 条客户端请求，但每条 `attempts=3`（绕转换号）共 18 次上游调用，**16 个不同账号各吃一次 429、全进 600s/1200s/2400s 阶梯冷却**，面板一片「429 rate limit」，而真凶是上游队列积压。
  - **修复**：新增错误分类 `ErrUpstreamQueue`（`upstream_queue`），判定 `IsUpstreamQueue`（非 2xx + 业务码 6020/6021，或 `queue.waiting.title` / `queue.full.title` 文案兜底；码位容忍 JSON 空白与字符串形态）。`applyErrorPolicy` 对该类**零动作**——不冷却、不熔断、不 `NoteError`、不喂连败（把队列积压记成账号限流正是全池被烧穿的根因）。
  - **新增进程级排队门控 `queueGate`（`internal/server/queuegate.go`）**：与 WAF IP 级门控（`wafip.go`）同哲学但**不靠多号取证**——排队码单条响应就已自证是队列水位，故首次命中即激活 fail-fast：命中处（`chatCompletions` 轮转循环内）立即终止轮转，且门控激活期内后续请求**不发上游请求**直接回 429（队列堵着时发出请求只是把一个请求放大 `MaxRotate` 倍再喂给已过载的上游、并把健康号白白推进冷却）。门控时长采信上游 `retry_after`（缺失回落 `estimated_wait`，再回落 60s），钳到 `[15s, 2m]`；**任一请求成功即清除**（上游队列疏通后不被网关自己挡住）。进程内状态、重启清零。
  - **末端错误映射独立化**：排队回 `429` + 独立 `code=upstream_queue_busy`（此前会与账号限流共用 `rate_limit_exceeded`），`message` 透传上游排队原文（含 `queue_position` / `retry_after`），`gateway_hint` 明说「not an account problem; switching accounts or models will not help」——避免用户/客户端把队列积压读成账号问题而做无谓的换号/换模型。
  - **面板与 `/status` 可辨**：`/panel/api/overview` 与 `/status` 新增 `upstream_queue` 字段（激活时给 `until` / `code` / `position` / `size` / `retry_after`，未激活为 `null`），面板账号池上方新增「上游队列忙」横幅，明确写出「与账号无关、换号换模型都不绕过、已暂停轮转、无需手动解冻」——把队列积压与账号限流在界面上彻底分开（2026-10-15 的认知根源就是界面上只显示「429 rate limit」）。

### 验证

- **单测**：`internal/upstream/client_test.go` 的 `TestClassify` 新增 9 条排队断言（6021 / 6020 本机实测原文、码位字符串与空白形态、嵌套 `error.data`、文案兜底、`200+排队码` 不误判、`6004` 不被劫持）；新增 `TestParseQueueInfo`（6020 完整 data / 6021 无 data / 数字字符串 / 非排队与非 JSON）与 `TestIsUpstreamQueue`（10 例，含 `200` 与普通 `429` 反例）。`internal/server/handler_test.go` 新增端到端用例 `TestChatUpstreamQueueFailsFastWithoutPenalty`（三账号池断言**只打一次上游**、回 `429 upstream_queue_busy`、透传上游排队原文、**0 处冷却/禁用/熔断/errTotal**）、`TestChatUpstreamQueueGateBlocksRotationWhileActive`（第二次请求**零上游调用**且响应带 `queue_position`）、`TestChatUpstreamQueueGateClearsOnSuccess`、`TestQueueGateActivationAndDuration`（5 子例：时长采信与上下限钳制、`clear` 归零）。
  - **负向验证**：临时移除 `Classify` 的排队分支 → `TestClassify` 报 `=soft_rate want upstream_queue`（复现全池误冷却的原始分类）；临时移除轮转处的 `noteQueue` → handler 用例复现**请求放大**（`calls=map[at-a1:1 at-a2:1 at-a3:1] want exactly 1`）与门控未激活，证明测试能抓到回归。两处均已恢复。
- **真实二进制端到端**（沙箱 `:7873`，global 账号指向本地假上游）：假上游回 `429 {"code":6021,"msg":"queue.full.title"}` → 网关回 **`429 upstream_queue_busy`**，`message` 原样带 `6021/queue.full.title/requestId`，**上游只被调用 1 次**；门控激活后第二条请求**零上游调用**直接回 429 且 `message` 带 `queue_position=5399`；`/status` 与 `/panel/api/overview` 均透出 `upstream_queue`，账号 `cooling=false`（零惩罚）；切假上游为成功模式、门控过期后请求回 **200** 且 `upstream_queue` 自动归 `null`。
- `go build ./...`（Go 1.27.0）、`go vet ./internal/upstream/ ./internal/server/ ./internal/panel/ ./cmd/server/` 干净，`go test ./... -count=1` **18 包全绿**。

## [1.15.4-panel] - 2026-10-14

### 修复

- **上游 `11133 model_param_invalid` 被误判为「只换号不罚」的未知 4xx，导致请求长时间无响应（本机 ZCode + `cn:kimi-k3-1` 实案）**：上游模型提供方拒绝请求参数时返回 `HTTP 400` + 业务码 `11133`（`Invalid request parameters` / `extError.code=400001`，义为「参数不满足该模型要求」，如模型不支持某能力）。`Classify` 此前**没有 11133 分支**，它落进通用 4xx 兜底的 `ErrClient`——按设计 `ErrClient` 只换号不罚、但**继续轮转**并喂连败计数。于是同一个 body 被网关连续换 3 个账号重试，三号全撞同样的 11133（`payload.go` 早已记录「schema 级拒绝换账号无用」），轮转烧满后末端落到「其余保持 503」；客户端（ZCode）判定 503 可重试，按指数退避最多重试 11 次（单次退避涨到约 28s），表现为**会话长时间无响应、长时间转圈**。
  - **修复**：新增错误分类 `ErrModelParamInvalid`（`model_param_invalid`），与既有的 `ErrBadParams`（11101）/ `ErrImageInvalid`（11135）/ `ErrPromptTooLong`（11115）同属**请求级错误**——`chatCompletions` 命中即**立即透传上游原文回 400、终止轮转**（不换号、不罚号、不喂熔断/连败），客户端得到确定的 400 而非可重试的 503，不再陷入长退避重试。
  - 分类判定：`HTTP 400` + `isModelParamInvalid(body)`（复用 hint 层既有形态判定：`code 11133` / `extError.code=model_param_invalid` / 精确文案），并绑定 400 状态码收紧（`500` 等非请求级状态码不误归）。
  - `applyErrorPolicy` 增补 `ErrModelParamInvalid` 分支（零动作，同 `ErrPromptTooLong`/`ErrImageInvalid` 待遇，仅为文档完备）；`/v1/messages`（Anthropic 路径）复用同一 `chatCompletions` 管线，同步受益。

### 验证

- **单测**：`TestClassify` 新增 11133 断言——`{"code": 11133, ...model does not support image}`、真实错误体（`extError.code=400001`）、`{"code": 11133, "msg":"x"}` 均归 `ErrModelParamInvalid`；`{"code":11133}` + `500` 状态码归 `ErrServer`（防过宽）。`internal/server/handler_test.go` 新增端到端用例 `TestChatModelParamInvalidFailsFastWithoutPenalty`：两账号池断言**只打一次上游**（`bad` 1 次、`good` 0 次，**零轮转**）、直接回 400、账号完好（无冷却/禁用/熔断/errTotal）。
  - **负向验证**：临时把 11133 改回 `ErrClient`，两条用例如期失败——`Classify` 断言报 `=client want model_param_invalid`，handler 用例复现原始故障形态 `code=503 body={"error":{"code":"no_healthy_account",...}}`，证明测试能抓到回归。
- **真实二进制端到端**（本机沙箱 `:7869` + 指向本地假上游的 `global` 账号，全部调度与备份禁用）：上游返回 11133 → 网关回 **HTTP 400 `model_param_invalid`**（`/v1/chat/completions` 与 `/v1/messages` 两条路径均验证），请求日志显示**仅一次上游调用**（零轮转）。
- **多分类不串味验证**（同一沙箱，按模型分派不同上游错误）：`11133→model_param_invalid/400`、`11101→bad_params/400`、`11135→image_invalid/400`、`11102→model_unavailable/400`、内容拦截`→content_blocked/400`、限流`→rate_limit_exceeded/429`、`5xx`/通用 4xx 保持原语义（单账号池无号可轮时仍 503），各分类互不影响。
- `go build ./...`（Go 1.27.0）、`go vet ./internal/upstream/ ./internal/server/` 干净，`go test ./... -count=1` 18 包全绿。

## [1.15.3-panel] - 2026-10-14

### 修复

- **用量时序图的悬停提示看不出含义（面板答疑发现）**：提示文案沿用了 OpenAI API 术语与原始 ISO 串，中文界面里靠猜——原文案 `2026-10-14T00  11.00M prompt / 22.9k completion / 47 次`，既没说清「`T00` 是哪一段」（是 00:00 这一小时还是当天？），也没给出用户最直观看到的柱高（总量），还得自己做加法。改为三段式中文明示：**时间段 · 输入 + 输出 = 合计 · N 次**，例如 `10-14 00:00–01:00 · 输入 11.00M + 输出 22.9k = 合计 11.02M · 47 次`。时间段按桶粒度展开（小时桶 `MM-DD HH:00–HH+1:00`，日桶 `MM-DD 全天`；23 点收尾写 `24:00`，避免回绕成次日 00:00 产生歧义）。
- **柱子悬停时提示偶发不弹出**：网格线 / 均值线 / 基线 / 轴刻度与标注文字都是几何叠加在柱子上的 SVG 元素，默认 `pointer-events: auto` 会截断柱子的命中测试——跨天日期分隔线恰好压在柱子中心时，悬停落在线上、柱子的 `<title>` 不弹（柱子越窄越明显）。修复：`.uschart-body line, .uschart-body text { pointer-events: none; }`，让装饰元素不参与命中测试。
- **术语统一**：用量区的用户可见标签从 API 术语改为中文——图表图例 `prompt / completion` → `输入 / 输出`，用量明细表头 `Prompt / Completion` → `输入 / 输出`，指标卡 `prompt / completion` → `输入 token / 输出 token`，占比条 tooltip 同步。JSON 字段名（`prompt_tokens` 等）保持不变。

### 验证

- **单测**：`TestAppJSUsageChartBarClass` 的 tooltip 断言从「含原始 ISO 串」升级为「**逐点精确比对完整文案**」，并新增 `noLegacyTerms`（tooltip 内不得再出现 `prompt` / `completion`）；新增 `TestIndexHTMLChartPointerEvents` 断言装饰元素的 `pointer-events: none` 与 `rect.usbar:hover` 仍在。两条断言均做**负向验证**——把术语改回 `prompt` 后前者失败（`noLegacyTerms:false`、`titlesPerPoint:false`），符合预期。
- **真实浏览器走查**（本地沙箱 `:7867`，载入生产用量数据副本）：真实鼠标点击导航进入「用量」→ 24 根柱逐根 `elementFromPoint` + 祖先链归属断言**全部命中自身 `<g>`（0 处错配）**；修复前被跨天分隔线截断的那根柱子现已正常。截图那条对应的桶读出 `10-14 00:00–01:00 · 输入 11.00M + 输出 22.9k = 合计 11.02M · 47 次`，与截图数字一致。
- `go build ./...`、`go vet ./internal/panel/` 干净，`go test ./... -count=1` 全绿。

## [1.15.2-panel] - 2026-10-13

### 修复

- **Anthropic 兼容层拒绝「对话中途 system」，ZCode 接入直接 400**（本机接入实测发现）：客户端会把中途注入的系统提醒作为 `{"role":"system"}` 放进 `messages` 数组（Anthropic Messages API 的 mid-conversation system 形态，ZCode、Claude Code 均会发），而适配层的 role 白名单只放行 `user` / `assistant`，命中即回 `400 messages[n]: unsupported role "system"`——在 ZCode 侧表现为「Provider rejected the model request / invalid_request status=400」。修复：`convertAnthropicMessage` 增加 `system` 分支，透传为 OpenAI `role:"system"` 消息并保持其在消息流中的既有位置（既不合并、也不重排）；`content` 为字符串或文本块数组都归一为纯文本，空 content 直接丢弃（不向上游转发空 system 消息）。
- 白名单外的 role（`developer` 等）仍照旧拒绝，保持「畸形请求不静默透传」的既有约束。

### 验证

- **单测**：新增 4 条——中途 system（字符串形态 / 文本块数组形态）透传且位置不变、空 content 的中途 system 被丢弃、白名单外 role 仍报错；`go test ./internal/server/ -run Anthropic -v` 共 20 条 Anthropic 用例全过。
- **端到端复现与上游语义核对**（本机 `:7863` 生产实例）：修复前 `/v1/messages` 带中途 system 返回 `400 unsupported role "system"`；上游侧经 `/v1/chat/completions` 实测接受 messages 中段的 system（单条中途 system、以及「顶层 system + 中途 system + user」组合）均 `200`，故透传方案与上游语义一致（此前 `internal/upstream/payload.go` 注释中「上游对多 system 的行为尚未实测」的空白本次补齐）。
- `go build ./...` 干净，`go test ./... -count=1` 16 包全绿。

## [1.15.1-panel] - 2026-10-13

### 修复

- **用量时序图的悬停提示绑错元素（面板实测发现）**：`<title>` 被写成 `<svg>` 的**直接子元素**，而 SVG 里 `<title>` 只描述它的**父元素**——浏览器于是把它当成整张图的说明、且只取第一个，导致**不论悬停哪根柱子，提示都显示第一个时间桶的数字**（实测：在 14:00 的 23.97M 峰值柱上悬停，显示的是 09:00 的 4.62M / 44.6k / 65 次）。修复：每个点的「prompt 柱 + completion 柱 + `<title>`」用 `<g>` 包住，提示回到各自的柱子。
- 顺带把该行为固化成断言：`internal/panel/frontend_test.go` 的图表用例新增三项——`<svg>` 的直接子元素里不得出现 `<title>`（修复前为 2）、每个数据点一个 `<g>`、每个 `<g>` 恰好一个 `<title>` 且对应各自的时间桶。

### 验证

- 新增断言在修复前会失败（`<svg>` 直接子 `<title>` 计数 2 / 归属错位），修复后通过；`go build ./...`、`go vet ./internal/panel/` 干净，`go test ./... -count=1` 16 包全绿。
- **真实浏览器走查**（本地面板 + 把生产用量数据复制进沙箱以复现原图）：先逐柱用 `elementFromPoint` + 祖先链断言归属（`<svg>` 直接子 `<title>` = 0，5 个点各自命中自己的提示）；再用**真实鼠标移动** + `mouseover` 监听复核三根柱子——09:00 → `4.62M prompt / 44.6k completion / 65 次`、14:00 → `23.97M / 313.7k / 149`、15:00 → `24.82M / 249.7k / 193`，各自命中互不串台。
- 说明：原生 `<title>` 提示由浏览器 UI 层绘制、**不出现在页面截图里**，因此验收以「真实鼠标事件实际命中的归属」为准（截图仅作图表渲染留档）。

## [1.15.0-panel] - 2026-10-13

### 新增

- **Anthropic Messages API 兼容层 `POST /v1/messages`（含 `POST /v1/messages/count_tokens`）**：Anthropic 协议客户端（Claude Code、ZCode 等）可直接把 base URL 指向本网关。实现策略是**适配而非另起一套**：Anthropic 请求 → 内部 OpenAI chat body → 复用既有 `chatCompletions` 管线（挑号 / 轮转 / 超时与 WAF 处置 / 记账）→ 响应翻译回 Anthropic 形状（流式用「转换型 ResponseWriter」把 OpenAI SSE 逐帧改写为 `message_start` / `content_block_start` / `content_block_delta` / `content_block_stop` / `message_delta` / `message_stop` 事件序列；非流式聚合为 message 对象）。错误统一为 Anthropic 错误体（`{"type":"error","error":{"type":…,"message":…}}`，状态码映射 `authentication_error` / `invalid_request_error` / `rate_limit_error` 等）。请求侧支持 `system`（字符串或文本块数组）、文本 / 图片 / `tool_use` / `tool_result` 块、`tool_choice`、`stop_sequences`；非 `web_search` 的其它服务端工具（`web_fetch_*` / `code_execution_*`）出站前静默剥离，模型照常作答（能力降级而非报错）。
- **服务端 `web_search` 工具由网关自执行**：上游供应商不实现任何服务端工具，故客户端声明 `web_search_*` 时由网关注入函数工具 → 模型决定查询 → 网关执行检索（`internal/websearch`：so.com 结果页抓取与解析，`data-mdurl` 取真实 URL、`res-title` / `res-desc` / `data-result-summary` 取标题与摘要、`page_age` 规范化）→ 以 `server_tool_use` / `web_search_tool_result` 块回吐客户端，同时把结果以 `role:"tool"` 回灌模型继续作答。一轮内支持多轮检索（`max_uses` 预算，缺省 8、钳制 1..8）；预算耗尽自动**撤掉搜索工具 + 追加显式限额提示**；正文完全没引用来源时自动补 `Sources:` 链接列表（客户端 WebSearch 工具从正文 Markdown 链接提取来源）；检索失败以 `web_search_tool_result_error` 回执、不阻断整轮作答；模型单轮同时调用客户端工具时，检索照常执行但立即终局，把客户端工具以 `tool_use` 交回（网关内部搜索循环与客户端工具循环不混跑）。
- **双凭证鉴权**：`internal/httpauth.VerifyAny`——Anthropic 端点同时接受 `x-api-key: <api_key>` 与 `Authorization: Bearer <api_key>`（任一命中即通过，仍走 SHA-256 摘要 + 常量时间比较）。
- **伪工具调用文本清洗（`internal/server/dsml.go`）**：模型对「想调用但不可用的工具」会把调用语法当**正文**吐出，实测两种形态——全角竖线 DSML 标记（开块 / 参数 / 闭块，`calls` / `invoke` / `parameter` 标签）与 `<tool_call>{…}</tool_call>` 伪 XML 调用（同一提示词约 1/3 次数出现）；两者在**流式与非流式路径一致清洗**（流式为增量过滤：跨帧 holdback、字节级锚点、64KB 兜底），`<tool_call>` 形态只在块内是「工具调用形状的 JSON」时丢弃——正文里对语法的引用、普通 JSON 原样保留，防误伤。

### 修复

- **`StripDSMLText` 过度截断**：完整块剥离后，残留的闭合标签会再次触发「截到文末」规则，把块之后的正常正文整段吞掉（单测复现：`正文开头` + 块 + `正文结尾` → 只剩 `正文开头`）。未闭合尾规则改为只认**起始**（非闭合）标记。
- **流式过滤器漏判闭合形态的标记起点**：只认「尖括号紧跟双竖线」，闭合标签的斜杠挡在前面，导致残留的闭合标签被当正文放行——端到端实测该缺陷会把一个闭合 `calls` 标签（17 字符）作为整段正文下发给客户端。锚点改为「尖括号 + 可选空白 + 可选斜杠 + 空白 + 双竖线」。
- **逐字节 / 逐片喂入时的 holdback 缺口**：半截多字节竖线、半个锚点会被当正文放行（且 `strings.ToLower` 对半个多字节字符的替换会让长度与索引错位），改为字节级锚点 + 只折叠 ASCII 的 `asciiLower`，补齐前缀扣留判定。
- **`<tool_call>` 形态清洗漏写标签之前的前缀**：删除伪调用块时把块之前的正常正文一并丢掉（新增单测锁定）。
- **挂起态收尾判定**：流在 `<tool_call>` 未闭合时结束，改为按**载荷**（开标签之后）而非整段缓冲判断是否伪调用；非伪调用载荷会重新扫描残余锚点后原样放行。

### 变更

- `config.json` 新增 `search` 段：`enabled`（缺省 `true`）、`max_results`（缺省 5，钳制 1..10）、`timeout_seconds`（缺省 10，钳制 1..60），越界静默钳制；`config.example.json` 同步补充。
- 面板「账号池」批量按钮（全部签到 / 旅行巡检 / 活跃上报 / 全部保活）补充悬浮说明（`title`）：把各按钮实际触发的动作、与每日定时任务的对应关系写在按钮上，降低「点了会发生什么」的猜测成本。
- 版本号 `1.14.2-panel` → `1.15.0-panel`（`cmd/server/main.go` 的 `appVersion`）。

### 验证

- **单元测试**：`internal/server/anthropic_test.go` 21 用例（请求映射 / 图片与未知块 / 服务端工具剥离 / 非流式与流式翻译 / 工具调用事件 / 鉴权 / 参数校验 / `count_tokens` / 检索编排的多轮、预算耗尽与后端失败 / 错误体形状 / DSML 两种形态的整段与流式清洗 / 真实帧切分 / 截断兜底）；`internal/httpauth` 新增 `VerifyAny` 11 子用例；`internal/websearch` 5 用例（richsum、多空格开标签、畸形条目、去重截断、空查询与拦截页）。`go build ./...`、`go vet`（server / httpauth / websearch / cmd/server）干净，`go test ./... -count=1` **16 包全绿**。
- **端到端**（独立沙箱实例 `:7866`，隔离 `auths/`，全调度与备份禁用，真实上游账号 + 真实 so.com 检索）：① 非流式多轮检索 3 次——`richsum` 解析出 title / url / page_age、`encrypted_content` 已填、`usage.server_tool_use.web_search_requests=3`、正文 1819 字含来源链接；② 流式事件序列完整（`message_start` → `message_stop`、5 个内容块）且正文 1700 字含来源；③ `max_uses=1` 预算耗尽——实际检索 1 次、无 DSML 泄漏、模型基于已有结果作答；④ 客户端自定义工具回传——`stop_reason=tool_use` 且 `tool_use` 块为客户端工具；⑤ 鉴权失败 401 / 缺参 400 均为 Anthropic 错误体，`count_tokens` 200；⑥ 伪调用泄漏路径 6 次采样（「模型想搜但未声明工具」场景）：**零残留**，其中 5 次伪调用被整段清除（正文为空，属预期：该轮模型只吐了伪调用）、1 次正文 1106 字完整未被误伤。

## [1.14.2-panel] - 2026-10-12

### 新增

- **导入格式兼容升级：嵌套形账号文件 / 扁平单对象 / zip 整包 / 多文件**。`POST /panel/api/import/cockpit` 从「仅 cockpit 扁平数组」升级为逐文件、逐元素的形状嗅探（复用 `auth.Parse` 的 probe 双形态解析模式），直接吃下四类既有产物，前端「导入 JSON」不再对合法账号文件报错：
  - **嵌套形**（`{"account":{...},"auth":{...}}`，本面板 `auths/` 落盘 / 插件输出口径，秒级 `expiresAt`）单对象或数组——此前单对象直接 400（`cannot unmarshal object into []panel.cockpitAccount`）、嵌套数组全员 skipped（`missing required fields`），是导入受阻的主要来源；
  - **扁平单对象**与**嵌套形 + cockpit 扁平条目的混合数组**（逐元素嗅探，`realm` 显式优先、`expiresAt` 秒↔毫秒自动对齐）；
  - **zip 整包上传**：自动解包内部 `*.json`（跳过目录条目 / `__MACOSX` / `._` 前缀 / 非 json 文件；条目 4MB 读取上限防 zip bomb），包内单个坏文件只记 `errors` 不阻断其余；
  - **多文件 multipart**（前端 input 加 `multiple`，一次可选多个 json / zip）。
- 前端：「导入 JSON」页签提示文案与 `accept` 更新（`.json,.zip` + `multiple`）；导入结果区 `#importErr` 展示 `errors` 前 3 条明细（此前仅写入 console）。

### 变更

- 导入错误语义分层：整个请求只有一个负载单元时硬错误直接 400（保留既有单文件行为与文案）；多负载（zip / 多文件）时单个负载失败降级为 `errors` 条目。`total` 口径 = 所有负载单元中提取到的账号条目数（含无法解析条目）。
- 无法识别的形状报错更明确（`unrecognized format: no account fields`，此前笼统 `invalid json`），并带来源文件名前缀（如 `weird.json: unrecognized format: ...`）。
- cockpit 扁平数组口径与导出口径零改动（往返契约不动，既有测试全绿为回归门禁）。

### 验证

- 新增 `roundtrip_test.go` 六用例：嵌套单对象、嵌套数组混合 cockpit 条目、数组坏元素跳过、扁平单对象、zip 整包（2 好 + 1 坏 + 无关文件）、无法识别形状 400；既有三用例（导出导入往返 / 旧格式回落 / 零过期兜底）回归全绿。`go vet` 干净，`go test ./... -count=1` 全绿（15 包）。
- 真实浏览器走查（临时实例，隔离端口 `:7864` 与独立 auths 目录）：添加账号 → 导入页签 → 上传 zip（3 个嵌套形账号 + 1 坏 json + 1 无关文件）→ 「导入完成：成功 3 个」+ 坏文件明细红字（断言祖先链含 `#addTabImport`）→ 账号列表渲染 3 个新账号；非法 JSON 单文件 → 「导入失败：weird.json: unrecognized format」（清晰指向来源与原因）；账号落盘嵌套形无损（token / realm / 秒级时间戳原值）。
- 走查环境说明：本次走查在本机 ZCode 内置浏览器（IAB）执行，该后端定位器/坐标点击在本页面持续超时（宿主工具限制），点击动作经页面内真实 handler 触发完成；其余链路（change 事件 → fetch 上传 → 后端解析 → DOM 渲染）均为真实执行，断言按规范验证祖先链。

## [1.14.1-panel] - 2026-10-11

### 修复

- **导出 → 导入往返不完整（真实服务端到端实测发现）**：用边界账号（带 device_token / 显式 realm 与 domain 推断不一致 / 零过期时间戳）实测「面板导出 → 面板导入」全字段对比，发现三处缺口并修复两处：
  - **device_token 丢失**：导出/导入格式均无 `device_token` 键，带设备风控 Token（X-Device-Token 头来源）的账号往返后该头静默消失。修复：`cockpitExportAccount` / `cockpitAccount` 补 `device_token`（omitempty / 空不落盘），导入写回 auth 文件。新增 `auth.DeviceTokenValue()` 带锁访问器（出站取值点 `upstream/client.go` 既有直读保持不变——该字段落盘后静态）。
  - **显式 realm 被搬家**：导出格式无 `realm` 键，导入按 domain 推断——显式 `realm=global` 但 domain 为空/CN 域的账号（手工标注场景）往返后变成 cn，路由被切到错误域。修复：导出补 `realm`（`RealmStored()`，omitempty）；导入**显式标识优先**（`ResolveRealm(acc.Realm, acc.Domain)`），无显式标识时回落 domain 推断（cockpit-tools 导出等旧格式行为不变，测试锁定）。
  - **零过期时间戳回落 +365d**（未改，既有兜底设计）：`expires_at=0` 的账号导出时 omitempty 省略、导入回落 +365 天——正常账号均有过期时间（keepalive 持续续期），影响面为零，测试锁定该语义防漂移。
  - 导入的上游副作用（注册激活 / trial / 签到 / 余额刷新）增加 `Upstream == nil` 守卫：裸装配（测试面板）导入不再 panic，导入核心语义（凭据落盘 + 入池）不依赖上游。
- 兼容性：导出 JSON 新增 `realm` / `device_token` 键对 cockpit-tools 无影响（serde 默认忽略未知字段，已核对其 `WorkbuddyAccount` 无 `deny_unknown_fields`）；旧格式（无这两个键）导入行为不变。

### 验证

- 新增 `roundtrip_test.go`：导出 → 导入全字段往返（token/昵称/过期秒↔毫秒/domain/tags/realm/device_token）、旧格式回落推断、零过期兜底三个用例。
- 真实服务端到端复测（修复版）：4 个边界账号导出 → 导入全字段对比 **全部完美往返**（含 device_token 保留、显式 realm 保留）。
- `go vet` 干净；`go test ./... -count=1` 全绿（15 包）。

## [1.14.0-panel] - 2026-10-11

### 新增

- **账号回收站（软删除）**：面板「移除账号」不再硬删凭证文件，改为移入 `data/trash/`（Docker 持久化卷内），可在回收站弹窗**恢复**（移回 `auths/` 并热加载进池）或**彻底删除**（二次确认）；超期自动清理（`trash.retention_days`，默认 30 天，`0` = 永久保留）。删除时刻记于文件 mtime，不引入额外元数据文件；同 uid 重复删除自动加时间戳共存；跨盘移动回退 copy+delete。未注入回收站目录的旧装配保持既有硬删行为（零回归）。
- **账号标签（tags）**：账号可打多个标签（面板「标签」弹窗，逗号分隔，归一化 trim/去重/限量 16×32 字符），落盘 auth 文件 `account.tags`（旧文件兼容），账号行徽章展示 + 列表按标签/关键字筛选；cockpit 导出/导入格式对齐 cockpit-tools 的 `tags` 字段（`Option<Vec<String>>`），双向迁移不丢分组信息。
- **余额预警（面板内横幅）**：账号积分低于「积分保底线」（`pool.credit_floor`）时，账号池页顶部横幅列出触底账号与剩余分值——与选号侧保底共用同一阈值，零新增配置、零上游请求（overview 现算缓存值）；积分与总额均为 0（从未查询）的账号不误报。
- **定时备份**：每日整点（`backup.hour`，默认凌晨 3 点）把 `auths/*.json` + `state.json` 打包为 zip 落 `data/backups/`（**不含 config.json**，避免 api_key 明文扩散），保留 `backup.keep` 份（默认 7）轮转；文件名毫秒精度 + 冲突自动外推重命名（手动连点 / CI 高速循环不会同名覆盖，且文件名字典序恒等于时间序）。回收站弹窗内提供「立即备份 / 备份列表 / 下载」（Content-Disposition 附件，便于把备份拿出容器；列表按完整 mtime 新→旧排序）。

### 变更

- **配置**：`config.example.json` 与配置页新增 `trash.retention_days`、`backup.enabled` / `backup.hour` / `backup.keep` 四项，均热生效（`panel.SetTrashRetention` / `SetBackupSettings`）；旧配置缺键走默认值，无感升级。
- **overview 接口**：`GET /panel/api/overview` 新增 `tags`（uid→标签）、`credit_floor`、`low_credit`（触底账号列表）三个纯增量字段。
- **新增 API**：`GET /panel/api/trash`、`POST /panel/api/trash/{name}/restore|purge`、`POST /panel/api/trash/purge_all`、`POST /panel/api/backup/run`、`GET /panel/api/backup/list`、`GET /panel/api/backup/{name}/download`、`POST /panel/api/accounts/{uid}/tags`。回收站/备份文件名均做白名单校验（`workbuddy-*.json` / `wb2api-backup-*.zip`，纯文件名，防路径穿越）。

### 验证

- `go test ./... -count=1` 全绿；新增 `internal/auth/tags_test.go`（归一化 / 落盘往返 / 旧文件兼容 / 变更检测）与 `internal/panel/trash_backup_test.go`（软删→恢复→彻底删全链路、同名冲突共存、过期清理、保留期热改、备份 zip 条目 / keep 轮转 / 下载与穿越拒绝、overview 预警与标签透出、旧装配回退硬删与 501）；`TestIndexHTMLUniqueIDs`（id 全局唯一门禁）与 `TestAppJSSyntax`（node --check）通过。
- 真实浏览器点击走查（chrome-devtools，真实账号数据）：预警横幅渲染与出池联动、标签编辑→徽章→双维筛选、移除（新确认文案）→回收站恢复（标签随文件往返）→彻底删除、手动备份→列表出现→下载按钮、配置页四项回填与保存持久化，全部通过。

## [1.13.4-panel] - 2026-10-11

### 修复

- **导出弹窗列表不可见（用户实测阻断级）——`id="expList"` 与既有元素重名**：面板账号页原本就有隐藏的 `#expBox` 盒子，内含 `#expList`（`index.html:669`）。导出弹窗（1.13.1 起）也用了 `id="expList"`，而 `getElementById` 恒取文档顺序第一个——于是弹窗的账号行被渲染进了**页面主区域那个旧容器**（被弹窗遮罩盖住）：用户看到空列表、点击无效，但计数/全选等所有逻辑"正常"（4/4），且所有基于 `getElementById` 的自动化断言全部假阳性。修复：弹窗内 `#expList` 改名 `#expAccList`（`index.html` + `app.js` 5 处引用），与既有元素彻底解耦。
- **回归防御**：`frontend_test.go` 新增 `TestIndexHTMLUniqueIDs`——解析 index.html 全部 `id="…"` 断言唯一，把"id 冲突导致 JS 静默写错容器"这类事故前移到 CI（本次事故的正确性教训：DOM 层级断言必须验证祖先链，仅凭 getElementById 内容断言会假阳性）。

### 排查记录

- 1.13.2 的缓存修复与 1.13.3 的 DOM API 渲染加固方向正确但未触及本根因；真网关 + 真数据 + 真实鼠标点击复现后，经 `elementsFromPoint` / 祖先链检查定位（点击被遮罩拦截、列表祖先链不含弹窗）。
- 真网关全流程实操验证（4 账号、真实点击）：弹窗内列表渲染（祖先链含 `#exportVeil`）、单选/双选计数联动（已选 2/4）、导出请求 `?uids=<2 个选中 uid> → 200` 浏览器下载触发。

## [1.13.3-panel] - 2026-10-11

### 修复

- **导出弹窗改用 DOM API 逐元素创建 + 关键样式内联**（终极兼容方案）：用户环境曾稳定复现"4 个账号已进 DOM（计数 4/4）但整行不可见"的渲染错乱，而同一份代码与数据在标准 Chrome 实例上完全正常——判定为浏览器环境（插件 / 扩展改写 DOM 或缓存层）与 `innerHTML` 字符串拼接的兼容性问题。改为：
  - `createElement` + `textContent` 逐元素构建账号行（账号昵称 / uid 不再经 HTML 字符串，顺带消除注入面）；
  - `display:flex` / 复选框尺寸 / 间距等**关键样式以内联 style 写入**，任何全局 CSS、浏览器插件、样式改写都无法覆盖（CSP 已允许内联样式）；
  - 行保留 `min-height:14px` 兜底，状态徽标为固定枚举 innerHTML。

### 验证

- `go test ./internal/panel/` 全绿（app.js 语法校验 / CSP 断言）。
- 4 账号实例浏览器实测：列表 139px、行内样式生效（computed style `display:flex`、行高 34px）、默认全选 4/4、单选取消 → 3/4、导出请求 `?uids=…→ 200`、console 无 JS 错误。

## [1.13.2-panel] - 2026-10-11

### 修复

- **面板静态资源增加 `Cache-Control: no-cache`**（`index.html` 与 `app.js` 响应）：此前无任何缓存头，浏览器会启发式强缓存 app.js；html 与 js 是同版本强耦合的两份独立文件（弹窗 DOM 与交互逻辑分属两处），升级网关后浏览器可能继续用旧脚本拼新页面，出现半新半旧的错乱界面（如导出弹窗列表空白）。改为 no-cache 协商校验后，网关更新前端即生效，无需用户强刷。
- **导出弹窗健壮性加固**（`app.js` `openExport`）：账号列表渲染全程 try/catch，任何异常都会在列表区显示明确错误文案（不再退化为空白条）；`uid` / `nickname` 全部防空（`String(... || '')`）；渲染失败时"导出选中"按钮保持禁用。
- **弹窗样式兜底**：加载/错误态固定最小高度与居中文案（`.exp-loading`），空列表不再是一条细线；复选框显式尺寸（`.exp-check` 14px + accent-color），防全局样式干扰；列表容器 `min-height` 兜底。

### 验证

- `go test ./internal/panel/` 全绿（app.js 语法校验 / CSP 断言 / 鉴权 / 回环）。
- 4 账号独立实例浏览器实测（禁缓存重载）：列表 4 行渲染、行高与复选框尺寸正常（computed style 断言）、单选导出请求 `?uids=…`（3 个 uid）→ 200、console 无 JS 错误（仅 favicon 404，无害）、响应头含 `Cache-Control: no-cache`。

## [1.13.1-panel] - 2026-10-11

### 新增

- **导出弹窗：账号勾选（单选 / 多选 / 全选）**
  - 面板顶部「导出」按钮由"直接全量导出"改为打开**导出选择弹窗**（`index.html` 新增 `exportVeil`，`app.js` 新增 `openExport` / `expSync` / `expSetAll` 交互）。
  - 弹窗内按账号逐行列出昵称、uid、积分与运行状态（可用 / 冷却中 / 已暂停 / 已禁用），复选框勾选，默认全选。
  - 头部「全选 / 全不选」开关与行勾选双向联动，实时显示「已选 N / M」计数。
  - 「导出选中」按钮：全选时走全量导出（不带 `uids` 参数），部分选中时按选中 uid 拼接 `?uids=` 过滤导出；成功后自动关闭弹窗并 toast 提示导出条数。
  - 后端无需改动：复用 1.13.0 的 `GET /panel/api/export/cockpit?uids=` 过滤语义。

### 验证

- `go test ./internal/panel/` 全绿（含 app.js 语法校验、CSP 断言）。
- 独立测试实例（临时端口 + 双假账号）浏览器实测：弹窗默认全选、行勾选联动计数、头部全选/全不选切换、单选导出请求 `?uids=test-uid-2 → 200`、全选导出请求不带 `uids → 200`、成功后弹窗自动关闭。

## [1.13.0-panel] - 2026-10-11

### 新增

- **面板「导出」按钮（cockpit tools 兼容账号导出）**
  - 新增接口 `GET /panel/api/export/cockpit`（`internal/panel/export.go`）：把账号池导出为 cockpit tools 的 WorkBuddy 账号 JSON 格式（**纯数组**，字段与 cockpit `WorkbuddyAccount` 的 serde snake_case 序列化一致：`id` / `uid` / `nickname` / `access_token` / `refresh_token` / `token_type` / `expires_at`（毫秒）/ `domain`）。
  - 支持 `?uids=uid1,uid2` 选择性导出；`uids` 全部未命中时返回空数组（不静默回落全量，防止「想导单个却拿到整池」的误操作）。
  - 过期时间自动做 **秒 → 毫秒** 转换（本面板落盘为 Unix 秒，cockpit 口径为毫秒）；无效值省略字段，两侧导入均有兜底。
  - 响应带 `Content-Disposition: attachment` 附件头；日志只记录导出条数，**凭据不落日志**。
  - 与既有 `POST /panel/api/import/cockpit` 互为逆操作：导出体可直接再导入本面板，也可经 cockpit tools 的 WorkBuddy「JSON 导入」回灌其账号库（已对照 cockpit-tools 源码 `import_from_json` 验证兼容）。
  - 前端：顶部工具栏新增「导出」按钮（`index.html` + `app.js`），点击后经鉴权请求接口并触发浏览器下载 `workbuddy_accounts_<日期>.json`，toast 提示导出条数。
- **测试**：`internal/panel/export_test.go` 新增 5 组测试——鉴权（无 key / 错 key 一律 401）、空池合法空数组、字段契约（秒→毫秒 / id 前缀 / token_type / omitempty）、`uids` 过滤（子集 / 未命中 / 全量）、导出→导入结构回环。

### 变更

- 版本号 `1.12.0-panel` → `1.13.0-panel`（`cmd/server/main.go`）。
- README：Web 面板功能表、架构表与「添加账号」章节补充 cockpit 兼容导入 / 导出说明。

### 安全

- 导出接口挂在 `withAuth` 之后（与导入接口同级鉴权，`api_key` 非空时必须 Bearer 认证）；测试覆盖 401 / 200 两态。
