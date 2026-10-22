# HANDOFF — 接力开发文档

> 本文档供跨会话 / 跨 Agent / 多人协同开发时快速建立上下文。新会话接手本项目时，**先读本文件**，再按需查阅 `README.md` 与 `CHANGELOG.md`。

## 1. 项目概况与当前状态

- **定位**：把腾讯 CodeBuddy（WorkBuddy）账号变成 OpenAI 兼容 API 的**多账号网关**，附 Web 管理面板。本项目是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的增强 fork（上游已删库，本分支独立演进）。
- **当前版本**：`1.15.6-panel`（`cmd/server/main.go` 的 `appVersion` 常量，透出到 `/panel/api/overview`）。生产已于 2026-10-22 15:11 切到该版本并验收通过。
- **仓库**：`https://github.com/wjf1/workbuddy2api-panel`（推送账号即 `wjf1`），分支 `main`。
  - **⚠️ 批注（2026-10-15 于 `CHINAMI-DPNMO04` 实测）**：原文此处只写一个工作区路径并断言「`F:\AI\wb2api-src` 在本机并不存在」——那句只对 `DESKTOP-DVFFB09` 成立，**在 `CHINAMI-DPNMO04` 恰好相反**（`F:\AI\wb2api-src` 存在且是活动检出，`F:\AI\workbuddy2api-panel` 不存在）。本仓库至少在两台机器上各有检出与生产实例，历史条目里的「本机」不指明机器会互相矛盾，机器标识见下方批注表。
  - **机器标识批注表（区分「本机」的唯一依据；MAC 取首行网卡）**

    | 机器 | 主机名 | 标识 MAC | 源码工作区 | 生产网关目录 | 计划任务名 |
    | --- | --- | --- | --- | --- | --- |
    | **B（2026-10-15 本次会话所在机）** | `CHINAMI-DPNMO04` | WLAN **`50-2E-91-F4-7E-74`**（在用网卡 Realtek 8852BE WiFi 6；有线 `1C-83-41-D6-A7-51` 未连接；蓝牙 `50-2E-91-F4-7E-75`） | `F:\AI\wb2api-src` | `F:\AI\wb2api`（`wb2api.exe` + `config.json` + `auths\` + `data\`，监听 `:7863`） | `wb2api`（Principal SYSTEM / RunLevel Highest，动作 = `F:\AI\wb2api\wb2api.exe -config config.json`，工作目录 `F:\AI\wb2api`；任务定义备份 = 同目录 `wb2api-task.xml`） |
    | **A（§1 旧条目与 §5 旧记录的路径来源机）** | `DESKTOP-DVFFB09` | **待补**——请在该机执行 `getmac /v` 或 `powershell "Get-NetAdapter \| Select Name,MacAddress"` 后回填本单元格 | `F:\AI\workbuddy2api-panel` | `F:\AI\wb2api-panel-v1.13.0-windows-amd64\`（内含 `start-gateway.cmd`） | `WorkBuddy2API` |

  - **三处同名不同实，用来判断自己在哪台机器上**：① 计划任务名 `wb2api`（B）/ `WorkBuddy2API`（A）；② 生产目录 `F:\AI\wb2api`（B，**无** `start-gateway.cmd` 与 `task-definition.xml`，任务直接调 exe）/ `F:\AI\wb2api-panel-v1.13.0-windows-amd64`（A，经 `start-gateway.cmd` 启动）；③ 源码工作区 `F:\AI\wb2api-src`（B）/ `F:\AI\workbuddy2api-panel`（A）。
- **最近 tag**：`v1.15.6`（上游 P0 四项同步：14003 短窗口 / safefile 原子写 / CST 时区 / 出站头族对齐，另含 traceparent 32 hex 修复）；上一版 `v1.15.5`（上游免费档排队 6020/6021 归类为上游整体过载，账号零惩罚 + 轮转 fail-fast）；历史 `v1.15.4`（上游 11133 模型参数拒绝归类为请求级错误并 fail-fast 返回 400）、`v1.15.3`（用量图悬停提示改中文三段式）、`v1.15.2`（Anthropic 兼容层支持对话中途 system）、`v1.15.1`（修复用量图悬停提示绑错元素）、`v1.15.0`（Anthropic Messages 兼容层与服务端 web_search）、`v1.14.2`（导入格式兼容升级）、`v1.14.1`（导出导入往返完整性修复）、`v1.14.0`（账号回收站、标签管理、余额预警与定时备份）。
- **本地生产网关（2026-10-13 本机重新核实并升级）** **【批注 2026-10-15：本段是机器 A `DESKTOP-DVFFB09` 的记录——目录 `F:\AI\wb2api-panel-v1.13.0-windows-amd64\`、任务名 `WorkBuddy2API`、`start-gateway.cmd`/`task-definition.xml` 在机器 B `CHINAMI-DPNMO04` 上**全部不存在**（B 机对应物 = 目录 `F:\AI\wb2api`、任务名 `wb2api`、任务定义备份 `wb2api-task.xml`、无 `start-gateway.cmd`）；B 机部署记录见 §5 首条】**：目录 **`F:\AI\wb2api-panel-v1.13.0-windows-amd64\`**（目录名是历史遗留，内容随版本推进；内有 `wb2api.exe` + `config.json` + `auths/` 17 个账号 + `start-gateway.cmd` + 历史 `wb2api.exe.*.bak`），监听 `:7863`；Windows 计划任务名 **`WorkBuddy2API`**（注册为 `S-1-5-18` SYSTEM，故**非提权读不到**——`schtasks /query /tn WorkBuddy2API` 报「拒绝访问」即任务存在，报「找不到」才是真没有；定义备份在该目录 `task-definition.xml`，动作 = `start-gateway.cmd`）。**当前已部署 `1.15.2-panel`**（2026-10-14 00:11 由 `1.15.1-panel` 升级，回滚备份 `wb2api.exe.pre-1.15.2.bak`，本轮升级过程见第 5 节；上一次 2026-10-13 20:47 从 `1.14.2-panel` 升级并验收：healthz 17/17、`/v1/models` 200、`/v1/chat/completions` 真实请求返回正常、`/v1/messages` 路由在位、服务端 web_search 已启用；升级前 exe 备份为 `wb2api.exe.pre-1.15.1.bak`）。更新惯例 = 停任务 → 备份成 `wb2api.exe.pre-<新版本>.bak` → 替换 exe（Release 的 `wb2api-panel-v<版本>-windows-amd64.zip` 内，或本机 `go build`）→ 起任务；**停机约 2–3 秒**，任务失败重启策略（`PT1M`×3）与开机自启（`PT20S`）保留。该实例的 `config.json` **没有 `search` 段**——加载时取 `Default()` 兜底，故 1.15.0 起服务端 web_search 默认即启用（想关再显式写 `"search":{"enabled":false}`）。
  - **本机部署避坑（2026-10-13 实测）**：① 计划任务注册为 SYSTEM，`schtasks /End|/Run`、`Get-ScheduledTask -TaskName`、`tasklist/taskkill` 均需**管理员**；本机 UAC 策略为 `ConsentPromptBehaviorAdmin=0`（管理员静默提权，不弹窗），`Start-Process -Verb RunAs` 可直接提权。② **无 BOM 的中文 `.ps1` 会被 Windows PowerShell 5.1 按 ANSI 读成乱码导致解析失败**（`Try 语句缺少 Catch/Finally`）——部署脚本改用**纯 ASCII 的 `.cmd` 批处理**最稳。③ Git Bash 里 `taskkill //F //IM x.exe` 会把 `//F` 原样传给程序报「无效参数」——用 `powershell Stop-Process -Name x` 或 `cmd /c` 代替。④ 清理测试进程**只能按精确镜像名**（如 `wb2api-smoke.exe`），**禁止 `wb2api*.exe` 通配**（会连带杀掉生产 `wb2api.exe`）。

## 2. 技术栈与运行基线

- **语言 / 运行时**：Go 1.22.5（见 `go.mod`）。前端为原生 JS 单文件（`internal/panel/app.js`）+ HTML，经 `go:embed` 打进二进制，**无 Node 构建链**（但 CI 用 `node --check` 做 app.js 语法校验，见 `frontend_test.go`）。
- **常用命令**（均在仓库根执行）：
  - 构建：`go build ./...`
  - 静态检查：`go vet ./internal/panel/`
  - 全量测试：`go test ./... -count=1`
  - panel 包测试：`go test ./internal/panel/ -count=1`
  - 本地运行：`go run ./cmd/server`（读根目录 `config.json`，`listen` 缺省 `:7863`，面板 `http://127.0.0.1:7863/panel/`）
- **测试环境注意**：Windows 本机无 gcc / cgo，`-race` 不可用（`CGO_ENABLED=1` 会因缺 gcc 失败）；CI 或 Linux 环境可跑 `go test -race ./internal/panel/`。

## 3. 核心架构与文件拓扑

```
cmd/server/            网关与面板装配入口（appVersion 常量、config 加载、依赖注入、saveConfig 热应用）
  config.go            Config 结构（trash/backup 段）、Default/normalize、WriteDefault
internal/panel/        Web 面板（单一职责：可视化运维）
  panel.go             Panel 结构、路由表 routes()、withAuth 鉴权、overview（含 tags/credit_floor/low_credit 现算）、accountRemove（软删）、accountTags
  trash.go             回收站：moveToTrash/moveFile（跨盘兜底）、trashList/Restore/Purge/PurgeAll、StartTrashLoop（每小时清过期）、validTrashName 白名单
  backup.go            定时备份：RunBackupNow（zip：auths+state，毫秒文件名）、StartBackupLoop（整点检查当日未跑）、SetBackupSettings、list/download、rotateBackups、validBackupName
  import.go            账号导入接口（1.14.2 起形状嗅探：cockpit 扁平数组/单对象 + 嵌套形/混合数组 + zip 整包 + 多文件 multipart；cockpitAccount 含 tags、毫秒口径；嵌套形经 auth.Parse 后秒→毫秒归一）
  export.go            GET  /panel/api/export/cockpit —— 账号池导出为 cockpit 兼容数组（秒→毫秒，含 tags），与本文件互为逆操作
  index.html / app.js  前端单页（go:embed）；回收站/标签弹窗（#trashVeil/#tagVeil）、预警横幅（#alertBox）、accFilter 双维筛选、备份列表
  *_test.go            含 frontend_test（app.js 语法校验、CSP 断言、TestIndexHTMLUniqueIDs）、security_test、export_test、trash_backup_test（回收站/备份/预警/标签全链路）
internal/pool/         账号池调度：Add/List/AuthByUID/CountsDetailed、熔断冷却、状态落盘（注意：Add 路径不标记 dirty，首次状态变更才落盘 state.json）
internal/auth/         凭据模型 Auth（落盘 auths/workbuddy-<uid>.json，嵌套形 account.tags）；token 字段与 tags 读取必须走带锁访问器
internal/upstream/     CodeBuddy/WorkBuddy 上游 API 客户端（签到/余额/任务/刷新）
internal/server/       OpenAI 兼容网关（/v1/chat/completions 等）+ Anthropic 兼容层
  anthropic.go         /v1/messages 请求侧：Anthropic 类型子集 → 内部 OpenAI chat body（system/文本/图片/tool_use/tool_result 映射、服务端工具剥离、tool_choice 翻译）；count_tokens 估算实现
  anthropic_response.go 响应侧：非流式聚合 → Anthropic message；流式「转换型 ResponseWriter」把 OpenAI SSE 逐帧改写为 Anthropic 事件序列（含错误帧与非 200 错误体转换）
  anthropic_websearch.go 服务端 web_search 编排：注入函数工具 → 检索执行 → server_tool_use/web_search_tool_result 块合成 → 多轮与 max_uses 预算 → 预算耗尽撤工具 + 限额提示 → Sources 兜底
  dsml.go              伪工具调用文本清洗（全角竖线 DSML 标记 + <tool_call> 伪 XML），StripDSMLText（整段）与 dsmlStreamFilter（流式增量、跨帧 holdback）
  wafip.go             WAF IP 级拦截 fail-fast 状态机（短窗多号 403 才激活，进程内状态）
  queuegate.go         上游排队（6020/6021）fail-fast 门控：首次命中即激活、账号零惩罚、门控期不发上游直接回 429、任一成功即清除（1.15.5 新增）
internal/websearch/    so.com 检索抓取解析（Searcher 接口、Result、PageAge 规范化；data-mdurl 取真实 URL）
internal/httpauth/     验签原语（VerifyBearer / VerifyAny：Bearer + x-api-key 双凭证、常量时间比较）
internal/scheduler/    定时任务（签到/保活/旅行/任务/余额刷新）
config.json            运行配置（listen / api_key / trash / backup / pool …），面板可在线改（热生效）
data/trash/            回收站（state 文件同目录推导；Docker 挂载卷内）
data/backups/          备份输出（同上）
```

- **数据流**：OAuth 登录或 cockpit 导入 → `auth.Auth` 落盘 `AuthDir` → `Pool.Add` 热加载 → 调度器 / 网关经 `Pool.AuthByUID` 取凭据；移除 → `Pool.Remove` + 凭证移入 `data/trash/`。
- **单一事实来源**：版本号 = `cmd/server/main.go appVersion`；面板字段契约 = `cockpitAccount`（导入口径）与 `cockpitExportAccount`（导出口径），两侧字段均对齐 cockpit-tools 的 `WorkbuddyAccount` serde 序列化（1.14.0 起含 `tags`）。

## 4. 最近一轮变更与交付成果（1.15.6-panel，2026-10-22）

> 完整细节见 `CHANGELOG.md` 的 `[1.15.6-panel]` 一节；此处只留接力要点。

- **性质**：**上游同步版**。从同源上游 `misakano7545/workbuddy2api-panel`（与本站同根，分叉点 `dbd7c68`=`v1.11.6`）**逐补丁移植 4 项 P0**，**不合并分支**（实测整分支合并会产生 64 个冲突文件、14 个 add/add，属"同一功能两套实现"的设计级冲突）。未引入上游的大块能力（`/v1/responses`、`/v1/search`、静态 API Key、预算双闸、AutoBot CI）。
- **14003 模型繁忙独立短窗口**（上游 `f728859`）：新增 `pool.CooldownModelBusy`（10s 起、封顶 1m，只写 `modelCooldowns[model]`）+ `upstream.IsModelBusy`；`handler` 仅在上游**未给恢复时刻**时走短窗口。**`6004` 判定与冷却参数逐字节未动**。
- **safefile 原子写**（上游 `4356a34`）：新增 `internal/safefile`（tmp→fsync→rename；`WriteKeep` 留 `.bak`，坏文件另存 `.bad-<时间>`；`Read` 坏正文回退 `.bak`）。接入 `pool/persist.go`、`auth/auth.go`、`cmd/server/config.go`、`cmd/server/main.go`、`usage`、`upstream/model_catalog.go`。**落盘路径与 JSON 结构一字未改**；`auth.LoadDir` 的 glob 仍是 `workbuddy*.json`，`.bak`/`.bad-*` 不会被当账号加载（已用临时用例验证）。
- **CST 固定时区**（上游 `025cc10`）：新增 `internal/cst`，收敛内联 `FixedZone`，签到日键 / 次日 04:00 / `nextFire` / 补签"昨天" / 夜间窗口 / 促销时段统一按 UTC+8。**`usage`（分桶）与 `reqlog`（切天）故意保留宿主本地时区**（操作者视角），迁移到 UTC 主机时需知悉这一点。
- **出站头族与标识对齐**（上游 `8f7366f` `d030e0f` `5605781` `edd0dba`）：补 `b3`/`traceparent`/`X-B3-ParentSpanId`/`x-stainless-*`/`acp-connection-id`；`X-Trace-ID` 不再回落 `conversationRequestId`；`X-Product` 恒 `SaaS`；`cache_key` 改 **UUID v4(36)** 且不含 uid 明文与项目缩写（`purpose==""` 时与旧 key 逐字节一致）；上报体清掉 `wb2api-*` 自证前缀；`conv_fields.go` 只给当前轮最后一条 user 消息补 `conversationRequestId`。本地 `sanitize.go` 与 `X-IDE-*` 头族**未被覆盖**；本地本就零注入 `X-Auth-Method`/`X-Userinfo`/`X-Id-Source`（上游对应补丁无净改动）。
- **本版额外修复（上游同源缺陷，独立 QA 用例抓到）**：`traceparent` 的 trace-id 只允许 32 hex，而 `X-B3-TraceId` 允许 16 位。客户端传 16 位 `X-Trace-ID` 时旧实现产出 `00-<16hex>-…` 非法头——比"头族不全"更明显的非官方指纹。修复 = 16 位左补零扩展到 128 位（链路关系不变），全零回落 `messageID`；`X-B3-TraceId` 保留原值。回归用例 `internal/upstream/headers_test.go`。
- **验证**：`go build ./...`、`go vet ./...` 干净；`go test ./... -count=1` **18 个包全绿**（基线 16 包 + 新增 `cst`/`safefile` 2 个）。沙箱实例（`:7871`，独立目录 `F:\AI\wb2api-e2e-p0`，17 账号副本、全调度关闭）实测：healthz 17/17、models 56、非流式/流式/`/v1/messages`（含对话中途 `system`）/工具调用全通、`state.json` 与 `.bak` 均可解析。
- **部署**：2026-10-22 15:11 切到生产，验收与回滚见 §5 首条。

## 4.1 上一轮（1.15.5-panel）

- **上游免费档排队被误判为账号级限流，106 秒内烧穿全池（本机实案，2026-10-15）**：上游服务端队列积压时返回 `HTTP 429` + 业务码 `6020 queue.waiting.title`（带 `data.queue_position` / `queue_size` / `retry_after` / `estimated_wait`）或 `6021 queue.full.title`，响应还有 `client_user_type:"free"`。这是**上游服务端队列水位**，与账号、模型都无关（实测同一时刻 `cn:kimi-k3-1` 与 `cn:deepseek-v4.1-flash` 回的都是排队码）。`Classify` 无该码 → 落 `status==429` 兜底 → 记成账号级软冷却。时间线：`11:18:46 → 11:20:32` 仅 6 条客户端请求、每条约 `attempts=3`，共 18 次上游调用，**16 个不同账号各吃一次 429 全进 600s/1200s/2400s 阶梯冷却**，面板一片「429 rate limit」。
- **修复**：新增 `ErrUpstreamQueue`（`ErrKind`，`String()`→`"upstream_queue"`，插在 `ErrModelParamInvalid` 之后、`ErrClient` 之前）。判定 `IsUpstreamQueue(status, body)`（非 2xx + `hasBusinessCode` 6020/6021，或 `queue.waiting.title` / `queue.full.title` 文案兜底）+ `ParseQueueInfo`（宽松解析 `data` 对象，JSON number 与数字字符串都接受）。`Classify` 中该分支**必须先于 `status==429` 兜底**（新第 5 层，注释里的层级编号已同步重排）。
- **进程级排队门控 `internal/server/queuegate.go`（新文件）**：`queueGate`（Handler 内嵌字段 `upstreamQueue`），与 `wafip.go` 的 `wafIPGate` 同风格（`mu + until + snapshot`），但**不做多号取证**——排队码单条响应即自证队列水位（有显式 `queue_position`），故首次命中即激活。`chatCompletions` 两处接线：① 命中处 `noteQueue(uid, info)` 后 `break`（终止轮转，不再换号放大请求）；② 轮转**第一跳之前** `snapshot()` 检查，门控激活则**不发上游请求**直接回 `429 upstream_queue_busy`。时长 `durationLocked`：`retry_after` → `estimated_wait` → 60s 默认，钳 `[15s, 2m]`（`queueGateCooldownMin/Max`），每次命中刷新但不超总上限。`clear()` 挂在流式与非流式两条**成功**路径（上游出完整响应即疏通）。进程内状态、重启清零。
- **末端映射独立化**（`handler.go` 末端错误透传段）：新增 `queueBusy` 分支 + `queueBusyMessage()`，回 `429` + `code=upstream_queue_busy`（不再与账号限流共用 `rate_limit_exceeded`），`message` 透传上游排队原文 + 摊开 `queue_position`/`queue_size`/`retry_after`，`gateway_hint` 明说「not an account problem; switching accounts or models will not help」（`hint.go` 新增 `ErrUpstreamQueue` case，带 `retry_after` 时给出具体秒数）。`applyErrorPolicy` 增补 `ErrUpstreamQueue` **零动作**分支（不冷却/不熔断/不 `NoteError`/不 `NoteFailures`）。
- **面板与 `/status` 可辨**：`/status` 与 `/panel/api/overview` 新增 `upstream_queue` 字段（激活给 `until`/`code`/`position`/`size`/`retry_after`，未激活 `null`）；`panel.Config` 无该字段（面板先于 handler 构造），改经 `Panel.SetUpstreamQueue(h.UpstreamQueueStatus)` 在 `cmd/server/main.go` 装配后注入（`Handler.UpstreamQueueStatus()` 暴露 `queueGate.snapshot()`）。前端 `index.html` 新增 `#queueBox` 横幅（放 `#alertBox` 之前）、`app.js` 新增 `renderQueueBox` + `fmtClock`（`overviewData` 刷新链里 `renderAlertBox` 之前调用），文案明写「与账号无关、换号换模型都不绕过、已暂停轮转、无需手动解冻」。
- **改动文件**：`internal/upstream/client.go`（ErrKind/String/IsUpstreamQueue/ParseQueueInfo/jsonInt/jsonString/Classify 分支 + 注释层级重排）、`internal/upstream/hint.go`（`ErrUpstreamQueue` case + `strconv` 导入）、`internal/server/queuegate.go`（新）、`internal/server/handler.go`（字段 + 轮转两处 + 末端映射 + applyErrorPolicy + `/status` 字段 + `UpstreamQueueStatus`/`queueBusyMessage`）、`internal/panel/panel.go`（`SetUpstreamQueue`/`upstreamQueueState`/`upstreamQueuePayload`）、`internal/panel/index.html`、`internal/panel/app.js`、`cmd/server/main.go`（注入 + 版本号）、`internal/upstream/client_test.go`、`internal/server/handler_test.go`、`CHANGELOG.md`、`README.md`、`HANDOFF.md`。
- **测试**：`TestClassify` +9 条（6021/6020 实测原文、字符串与空白形态、嵌套 `error.data`、文案兜底、`200+排队码`→`ErrNone`、`6004` 不被劫持）；`TestParseQueueInfo`（4 子例）、`TestIsUpstreamQueue`（10 例）；handler 侧 `TestChatUpstreamQueueFailsFastWithoutPenalty`（三号池：只打一次上游、回 `429 upstream_queue_busy`、透传原文、**0 处冷却/禁用/熔断/errTotal**）、`TestChatUpstreamQueueGateBlocksRotationWhileActive`（第二次请求**零上游调用**、响应带 `queue_position`）、`TestChatUpstreamQueueGateClearsOnSuccess`、`TestQueueGateActivationAndDuration`（5 子例含上下限钳制）。**负向验证**：移除 `Classify` 排队分支 → 报 `=soft_rate want upstream_queue`（复现全池误冷却分类）；移除轮转 `noteQueue` → 复现请求放大（`at-a1:1 at-a2:1 at-a3:1 want exactly 1`）+ 门控未激活；均已恢复。
- **真实二进制端到端**（沙箱 `:7873`，global 账号指向本地假上游 `:7872`，隔离 auths、全调度/备份禁用）：假上游回 6021 → 网关 `429 upstream_queue_busy`、`message` 原样带 `6021/queue.full.title/requestId`、**上游仅 1 次调用**；门控期第二条请求**零上游调用**、`message` 带 `queue_position=5399`；`/status` 与 `/panel/api/overview` 均透出 `upstream_queue`、账号 `cooling=false`；切假上游为成功模式且门控过期后回 **200**、`upstream_queue` 自动归 `null`。**注**：期间用真实上游 `:7863` 复测 3 条请求全部 200（上游队列当时已疏通），故队列路径改用本地假上游验证。
- `go build ./...`、`go vet ./internal/upstream/ ./internal/server/ ./internal/panel/ ./cmd/server/` 干净，`go test ./... -count=1` **18 包全绿**。

## 4.2 上一轮（1.15.4-panel）

- **上游 `11133 model_param_invalid` 误判为未知 4xx 导致请求长时间无响应（本机 ZCode + `cn:kimi-k3-1` 实案）**：上游模型提供方拒绝请求参数时返回 `HTTP 400` + 业务码 `11133`（`Invalid request parameters` / `extError.code=400001`）。`Classify`（`internal/upstream/client.go`）此前**无 11133 分支**，落进通用 4xx 兜底 `ErrClient`——按设计只换号不罚、但**继续轮转**并喂连败计数；同一 body 被连换 3 个账号重试全撞 11133（`payload.go` 已记录「schema 级拒绝换账号无用」），轮转烧满后末端落「其余保持 503」，ZCode 判定 503 可重试、指数退避最多重试 11 次（单次退避涨到约 28s）→ **会话长时间无响应**。
- **修复**：新增 `ErrModelParamInvalid`（`ErrKind`，`String()`→`"model_param_invalid"`），与 `ErrBadParams`(11101)/`ErrImageInvalid`(11135)/`ErrPromptTooLong`(11115) 同属**请求级错误**。`internal/server/handler.go` 的 `chatCompletions` 新增分支：命中即 **fail-fast 透传上游原文回 400、终止轮转**（不换号/不罚号/不喂熔断）；`applyErrorPolicy` 同步增补零动作分支（文档完备）。分类判定在 `Classify` 的 400 分支：`status==400 && isModelParamInvalid(body)`（复用 hint 层既有形态判定 `code 11133`/`extError.code=model_param_invalid`/精确文案），绑定 400 收紧（500 等不误归）。
- **改动文件**：`internal/upstream/client.go`（ErrKind + String + Classify 分支）、`internal/server/handler.go`（chatCompletions 分支 + applyErrorPolicy）、`internal/upstream/client_test.go`、`internal/server/handler_test.go`、`CHANGELOG.md`、`README.md`（错误分类表 11133 行 + 11101 行订正为「且不轮转」）、`cmd/server/main.go`（版本号）。
- **测试**：`TestClassify` 加 11133 三形态 + 500 防过宽；`TestChatModelParamInvalidFailsFastWithoutPenalty`（两账号池断言只打一次上游、零轮转、回 400、账号完好）。**负向验证**：临时改回 `ErrClient` 后两条用例如期失败（handler 侧复现原始 `503 no_healthy_account`）。
- **真实二进制端到端**：本机沙箱 `:7869` + 指向本地假上游的 `global` 账号（隔离 auths、全调度/备份禁用）；上游 11133 → 网关 `400 model_param_invalid`（`/v1/chat/completions` 与 `/v1/messages` 均验），请求日志**仅一次上游调用**。多分类不串味：11133/11101/11135/11102/内容拦截→400、限流→429、5xx/通用 4xx 语义不变。
- `go build ./...`、`go vet`、`go test ./... -count=1` **18 包全绿**。

## 4.2 上一轮（1.15.3-panel）

- **用量图悬停提示改为中文三段式（面板答疑发现）**：原文案 `2026-10-14T00  11.00M prompt / 22.9k completion / 47 次` —— 时间桶用原始 ISO 串（`T00` 是哪个时段靠猜）、`prompt`/`completion` 是 API 术语、且没给柱高总量（合计）。改为 **时间段 · 输入 + 输出 = 合计 · N 次**，新增 `fmtTokTimeRange(p)`（小时桶 `MM-DD HH:00–HH+1:00`、日桶 `MM-DD 全天`，23 点收尾写 `24:00` 防回绕歧义），改动在 `internal/panel/app.js` 的 `renderUsageChart` 柱子 `<g>` 循环里。
- **修复装饰元素截断柱子悬停**：网格 / 均值 / 基线 / 刻度文字都是叠加在柱子上的 SVG 元素，默认 `pointer-events:auto` 会抢命中——跨天分隔线恰好压在柱心时 `<title>` 不弹。修复 = `.uschart-body line, .uschart-body text { pointer-events: none; }`（`internal/panel/index.html`）。**这是本轮真实浏览器走查才暴露的**，单测看不到。
- **术语统一（用户可见标签）**：图例 `prompt / completion` → `输入 / 输出`、明细表头 `Prompt / Completion` → `输入 / 输出`、指标卡 `prompt / completion` → `输入 token / 输出 token`（`index.html` + `app.js`）；JSON 字段名不动。
- **测试**：`TestAppJSUsageChartBarClass` tooltip 断言升级为「逐点精确文案比对」+ 新增 `noLegacyTerms`；新增 `TestIndexHTMLChartPointerEvents`（装饰元素 `pointer-events:none` + `rect.usbar:hover` 仍在）。负向验证：术语改回 `prompt` 后断言如期失败（`noLegacyTerms:false`）。
- **真实浏览器走查**：沙箱 `:7867`（生产用量数据副本）；真实鼠标点击进「用量」→ 24 根柱逐根 `elementFromPoint` + 祖先链归属断言**0 处错配**。踩坑记录：`location.hash` 赋值**不触发**视图切换（视图靠 `<a href="#usage">` 的真实点击 + `hidden` 属性切换）；改 `index.html` 后须**硬刷新**（同 URL 强缓存，AGENTS.md 已知坑）。

## 4.3 上一轮（1.15.2-panel）

- **Anthropic 兼容层拒绝「对话中途 system」，ZCode 接入直接 400（本机接入实测 → 修复）**：ZCode / Claude Code 会把中途注入的系统提醒作为 `{"role":"system"}` 放进 `messages` 数组（Anthropic Messages API 的 mid-conversation system 形态），而 `internal/server/anthropic.go` 的 `convertAnthropicMessage` 的 role 白名单只放行 `user` / `assistant`，命中即回 `400 messages[n]: unsupported role "system"`——在 ZCode 侧显示为 `Provider rejected the model request / invalid_request status=400`（`messages[1]: unsupported role "system"`）。修复 = 增加 `system` 分支，透传为 OpenAI `role:"system"` 消息并保持其在消息流中的位置（不合并、不重排）；`content` 为字符串或文本块数组都归一为纯文本，空 content 直接丢弃；白名单外的 role（`developer` 等）仍照旧拒绝。
- **上游语义核对**（补齐 `internal/upstream/payload.go` 里「上游对多 system 的行为尚未实测」的空白）：经 `/v1/chat/completions` 实测上游接受 messages 中段的 system——单条中途 system、以及「顶层 system + 中途 system + user」组合均返回 `200`，故原位透传与上游语义一致。
- **测试**：`internal/server/anthropic_test.go` 新增 4 条（中途 system 字符串形态 / 文本块数组形态透传且位置不变、空 content 的中途 system 被丢弃、白名单外 role 仍报错）；`go test ./internal/server/ -run Anthropic` 20 条全过；`go build ./...` 干净；`go test ./... -count=1` 16 包全绿。
- **回归复现命令**：`curl -s http://127.0.0.1:7863/v1/messages -H "x-api-key: <key>" -H "content-type: application/json" -d '{"model":"cn:deepseek-v4.1-flash","max_tokens":8,"messages":[{"role":"user","content":"hi"},{"role":"system","content":"r"}]}'` —— 修复前 400 `unsupported role "system"`，修复后 200。

## 4.4 上一轮（1.15.1-panel）

- **用量时序图悬停提示绑错元素（真实面板报障 → 修复）**：`<title>` 写成了 `<svg>` 的直接子元素，而 SVG 的 `<title>` 只描述**父元素**，浏览器据此把它当作整张图的说明且只取第一个 → **悬停任意柱子都显示第一个时间桶的数字**。修复 = 每个点的「两根柱 + `<title>`」用 `<g>` 包住（`internal/panel/app.js` 的 `renderUsageChart`）。
- **断言固化**：`frontend_test.go` 的图表用例新增「`<svg>` 直接子元素不得含 `<title>`」「每点一个 `<g>`」「每 `<g>` 恰好一个 `<title>`」三项，修复前会失败。
- **验证**：`go test ./... -count=1` 16 包全绿；真实浏览器（IAB）逐柱 `elementFromPoint`/祖先链断言 + 真实鼠标移动 + `mouseover` 监听复核（09:00 → 4.62M/44.6k/65 次、14:00 → 23.97M/313.7k/149、15:00 → 24.82M/249.7k/193 各自命中）。
- **注意**：原生 `<title>` 提示由浏览器 UI 层绘制，**不会出现在页面截图里**——验收必须用「真实事件命中的归属」判定，别拿截图当证据。
- **本地生产网关同步升级（2026-10-13 20:47）**：本机 `F:\AI\wb2api-panel-v1.13.0-windows-amd64\` 实例由 `1.14.2-panel` 升级到 `1.15.1-panel`（跳过 1.15.0 首次部署，直接落到 1.15.1，同时带上 Anthropic `/v1/messages` 兼容层与服务端 web_search）。流程 = 停任务 `WorkBuddy2API` → 备份 `wb2api.exe.pre-1.15.1.bak` → 用 Release `wb2api-panel-v1.15.1-windows-amd64.zip`（sha256 `57ec8b5b…4470f`，与 `checksums.txt` 一致）内 exe 替换 → 起任务；停机约 2–3 秒。验收：隔离冒烟（版本 `1.15.1-panel`、`/v1/messages` 路由在位、`服务端 web_search 已启用`）+ 生产实例 healthz 17/17、`/v1/models` 200、`/v1/chat/completions` 真实返回 `OK`。

## 4.5 上一轮（1.15.0-panel）

- **Anthropic Messages API 兼容层**：新增 `POST /v1/messages` 与 `POST /v1/messages/count_tokens`（请求侧 `anthropic.go`、响应侧 `anthropic_response.go`）。核心取舍是**不复制调度逻辑**：Anthropic 请求转成内部 OpenAI chat body 后复用既有 `chatCompletions`（挑号 / 轮转 / 超时与 WAF 处置 / 记账），响应用「转换型 ResponseWriter」逐帧改写回 Anthropic 事件序列（流式）或 message 对象（非流式）；错误统一 Anthropic 错误体；非 `web_search` 的服务端工具（`web_fetch_*` / `code_execution_*`）出站前剥离（能力降级而非报错）。
- **服务端 `web_search` 由网关自执行**（`anthropic_websearch.go` + `internal/websearch/`）：客户端声明 `web_search_*` → 网关注入函数工具 → 模型给查询 → 网关抓 so.com 并解析 → 回吐 `server_tool_use` / `web_search_tool_result` 块，同时以 `role:"tool"` 回灌模型继续作答；一轮内多轮检索受 `max_uses`（缺省 8、钳 1..8）约束，预算耗尽**撤工具 + 追加限额提示**；正文没引用来源时补 `Sources:` 列表；非流式路径走多 pass 聚合同样支持。
- **伪工具调用清洗**（`dsml.go`）：两种形态（全角竖线 DSML 标记、`<tool_call>` 伪 XML 调用）在流式与非流式路径一致清洗；流式为增量过滤（字节级锚点 + 跨帧 holdback + 64KB 兜底），`<tool_call>` 只在块内是「工具调用形状 JSON」时丢弃（语法引用 / 普通 JSON 原样保留）。
- **本轮修掉的真实缺陷**（均以单测锁定）：① 整段清洗的未闭合尾规则误吃块后正文（`reDSMLTail` 改用只认起始标记的 `dsmlOpenPattern`）；② 流式锚点漏认闭合标签形态——实测把闭合 `calls` 标签当整段正文下发；③ 逐字节喂入时半截多字节竖线被放行 + `strings.ToLower` 对非法字节的替换导致索引错位（改 `asciiLower`）；④ `<tool_call>` 清洗漏写标签前的前缀，丢掉正文前半段；⑤ 挂起态流结束判定按整段缓冲而非**载荷**判断。
- **鉴权**：`httpauth.VerifyAny`（`x-api-key` 与 `Authorization: Bearer` 任一命中，Anthropic 端点专用；`withAuth` 对 `/v1/messages*` 走 Anthropic 错误体）。
- **配置**：`search.enabled`（缺省 true）/ `search.max_results`（钳 1..10）/ `search.timeout_seconds`（钳 1..60），`config.example.json` 同步补充。
- **验证**：`go build ./...`、`go vet`（server / httpauth / websearch / cmd/server）干净；`go test ./... -count=1` **16 包全绿**（`anthropic_test.go` 21 用例、httpauth `VerifyAny` 11 子用例、websearch 5 用例）。端到端在独立沙箱 `:7866`（隔离 `auths/`、全调度与备份禁用）跑完 6 项主矩阵 + 伪调用泄漏路径 6 采样，全部通过（具体数字见 CHANGELOG 1.15.0「验证」节）。

## 4.6 上一轮（1.14.2-panel）

- **导入格式兼容升级**：`POST /panel/api/import/cockpit` 从严格单形状（`[]cockpitAccount`）升级为**逐文件/逐元素形状嗅探**，直接兼容四类既有产物：嵌套形账号文件（`{"account":{...},"auth":{...}}`，即 auths 落盘口径，**秒**级 `expiresAt`；单对象与数组均支持）、扁平单对象、嵌套形+cockpit 扁平的混合数组、**zip 整包**（内部 `*.json` 自动解包，跳过 `__MACOSX`/`._`/非 json；单坏文件只记 errors 不阻断）；另支持多文件 multipart。实现分层：`parseImportPayload`（首字符 + probe 分派）→ `classifyAccountEntry`（含 `auth`/`accessToken` 键走 `auth.Parse` → `nestedToCockpit` 秒→毫秒映射；否则按 cockpitAccount 反序列化）→ `expandPayloadFile`（zip magic 探测与解包，条目 4MB LimitReader）。错误语义：单负载请求硬错误 400（保留既有文案），多负载降级 `errors` 条目。
- **前端**：「导入 JSON」页签 `accept=".json,.zip"` + `multiple`，提示文案更新；`d.errors` 前 3 条渲染进 `#importErr`（此前仅 console）。
- **验证**：新增 `roundtrip_test.go` 六用例（嵌套单对象/嵌套数组混合/数组坏元素/扁平单对象/zip 整包/形状无法识别 400），既有三用例回归全绿；`go vet` 干净、`go test ./... -count=1` 全绿（15 包）。真实浏览器走查（临时实例 :7864 + 独立 auths）：zip 导入成功 3 个 + 坏文件红字明细（祖先链含 `#addTabImport`）、非法 JSON 明确报错（`weird.json: unrecognized format`）、落盘嵌套形无损。注意：本次走查在 ZCode 内置浏览器（IAB）执行，该后端定位器/坐标点击在此页面持续超时（宿主工具限制），点击经页面内真实 handler 触发；其余链路真实执行。

## 4.7 再上一轮（1.14.1-panel）

- **导出 → 导入往返完整性修复**：边界账号端到端实测发现三缺口——device_token 往返丢失（导出/导入格式补 `device_token` 键 + `auth.DeviceTokenValue()` 访问器）、显式 realm 被 domain 推断搬家（导出补 `realm`，导入显式优先、空回落推断）、零过期回落 +365d（既有兜底设计，测试锁定）。导入的上游副作用加 `Upstream == nil` 守卫（裸装配不 panic）。
- **导出/导入字段契约（1.14.1 起完整清单）**：`id / uid / nickname / access_token / refresh_token / token_type / expires_at(毫秒) / domain / tags / realm(显式) / device_token`——往返全字段无损（正常账号）；`roundtrip_test.go` 锁定。

## 4.8 更早一轮（1.14.0-panel）

- 账号回收站（软删除）/ 账号标签（tags）/ 余额预警横幅 / 定时备份四件套 + `trash.retention_days`、`backup.enabled/hour/keep` 四配置键（详见 CHANGELOG 1.14.0 条目）。

## 5. 接力开发指引与待办（Next Steps / Backlog）

- **✅ 机器 B（`CHINAMI-DPNMO04`）生产网关已升级到 1.15.6-panel（2026-10-22 15:11，经用户明确确认后重启）**：`F:\AI\wb2api` 由 **`1.15.4-panel`**（注意：此前文档写的「已部署 1.15.5」不成立，exe 实际从未替换，面板 `overview.version` 实测为 `1.15.4-panel`）直接升到 `1.15.6-panel`，**一次跨越 1.15.5 与 1.15.6 两个版本**。流程 = **PowerShell `Stop-ScheduledTask -TaskName wb2api`**（本机 `schtasks.exe` 已被安全策略列入程序黑名单，**不要再用 `schtasks /End` 或 `taskkill` 通配**）→ 轮询确认 `wb2api` 进程真正退出（运行中的 exe 被 Windows 锁定，未强杀）→ 备份 `wb2api.exe.pre-1.15.6.bak`（sha256 `11C6360B…`，与原 exe 一致）→ 用分支自建二进制覆盖 `wb2api.exe`（sha256 `B357F1405C391CBC2FF7227CDBEA9D1A61C6B90C94A0F25D667EE23CF2455ADA`）→ `Start-ScheduledTask -TaskName wb2api`。**停机实测 5–8 秒**（未触发任务失败重启策略的 1 分钟窗口）。新进程 **PID 21108**。验收：面板 `overview.version = 1.15.6-panel`、**healthz 17/17**（cn/global 均 true）、`/v1/models`、`/v1/chat/completions`（`cn:deepseek-v4.1-flash`，非流式出 content「你好，很高兴见到你。」/ `finish=stop`、流式 SSE 正常）、`/v1/messages`（`stop_reason=end_turn`，正文「4」）、**`data/state.json.bak` 已生成（19878 字节）且与 `state.json`（19966 字节）均可解析、均含 17 账号**。回滚 = 停任务 → `Copy-Item wb2api.exe.pre-1.15.6.bak wb2api.exe -Force` → 起任务（回滚后 `state.json.bak` 不被旧版读取，无副作用）。
- **⚠️ 1.15.6 部署期间的账号池插曲（重要经验）**：切换前做 global 域 A/B 验证时，`global:gemini-3.8-flash` 返回上游 `11128 request illegal`，网关按「账号级故障」把**唯一的 global 账号自动禁用**，导致面板一度 `16/17`、`realm_servable.global=false`、模型数 56→22。**用旧版 1.15.4 发同一请求返回完全相同的 11128**，故属**上游侧既有状态**（该 global 号/该模型被风控拦截），与本次改动无关。处置 = `POST /panel/api/accounts/{uid}/revive`，healthz 恢复 17/17。**教训：不要对 global 域随手发探测请求**，会误伤账号池；要看 global 可用性读 `/v1/models` 即可，别真发请求。
- **机器 B（`CHINAMI-DPNMO04`，WLAN MAC `50-2E-91-F4-7E-74`）生产网关已升级到 1.15.4-panel（2026-10-15 09:40，经用户确认重启）**：`F:\AI\wb2api` 由 `1.15.2-panel` 直接升到 `1.15.4-panel`（**越过从未在此机部署的 1.15.3**，同时带上 11133 修复）。流程 = 提权 `schtasks /End /TN wb2api` → `taskkill /F /IM wb2api.exe`（**精确镜像名，禁 `wb2api*.exe` 通配**）→ 备份 `wb2api.exe.pre-1.15.4.bak`（旧 1.15.2，9225728 字节）→ 取 Release `wb2api-panel-v1.15.4-windows-amd64.zip`（sha256 `d4fa579e492bc860d6a95118d58ef28fa1631281963bf5b66800f95951d5fbfa`，与 `checksums.txt` 一致）内 exe **先落 `wb2api.exe.new` 再 `copy /Y` 替换** → `schtasks /Run /TN wb2api`；脚本与日志留档 `F:\AI\wb2api\deploy-1.15.4.cmd` / `deploy-1.15.4.log`。**停机实测 < 1 秒**（日志四步时间戳同为 09:40:36；注意该脚本里的 `timeout /t` 被 Git Bash 的 `timeout` 抢走而失效，等待步骤空跑——下次写 `.cmd` 批处理请用 `ping -n 3 127.0.0.1 >nul` 代替）。**重启前的安全检查**：确认只有 Chrome 面板页持有 `:7863` 连接、本会话模型请求不经该网关，避免自己把自己的会话掐断。验收：新进程 PID 13496（`F:\AI\wb2api\wb2api.exe`）、二进制内嵌版本串与面板 `overview` 均为 **`1.15.4-panel`**、healthz **17/17**（`realm_servable` cn/global 均 true）、`/v1/models` 200、`/v1/chat/completions`（`cn:deepseek-v4.1-flash`）真实出 content、`/v1/messages` 含中途 `system` 回归 HTTP 200（2.29s）。回滚 = `copy /Y wb2api.exe.pre-1.15.4.bak wb2api.exe` 后 `schtasks /Run /TN wb2api`（约 3 秒停机）。
- **【机器：A】本机生产网关已升级到 1.15.4-panel（2026-10-14 22:56，本轮交付，经用户确认重启）**：`F:\AI\wb2api-panel-v1.13.0-windows-amd64\` 由 `1.15.2-panel` 升到 `1.15.4-panel`（越过未部署的 1.15.3，直接带上本节 11133 修复）。流程 = 提权停任务 `WorkBuddy2API`（`schtasks /End`，wb2api.exe 随之退出）→ 备份 `wb2api.exe.pre-1.15.4.bak` → 新 exe 先落 `wb2api.exe.new` 再 `move /Y` 原子替换 → 提权起任务；停机约 10 秒。验收：进程重启（新 PID）、**面板 overview `version = 1.15.4-panel`**、healthz **17/17**、`/v1/models` 200、`/v1/chat/completions` 真实请求 `cn:deepseek-v4.1-flash` 与 `cn:kimi-k3-1` 均正常出 content（kimi 实测 `content='OK' / finish=stop`）。回滚 = 用 `wb2api.exe.pre-1.15.4.bak` 覆盖回 `wb2api.exe` 后重建任务（约 3 秒停机）。若上游再返回 11133，客户端将收到 **400 `model_param_invalid`**（而非 503），ZCode 不再长退避重试。
- **注：本机为 ZCode 自用网关，重启前须让用户确认。**
- **ZCode 侧口径（本次仍适用）**：指向 `http://127.0.0.1:7863/v1` 的 provider 可用 OpenAI（`openai-chat-completions`）或 Anthropic（`anthropic-messages`）两种格式，两条路径都复用同一 `chatCompletions` 管线、同样受本次排队修复覆盖。**上游排队时**客户端收到 `429` + `code=upstream_queue_busy`（非 `rate_limit_exceeded`），其 `gateway_hint` 明说「not an account problem」，故**不要试图换模型或换账号绕过**——排队是上游服务端队列水位，换了照旧；正确做法是等 `retry_after`（响应 message 里带）或等队列疏通（网关任一请求成功即自动解除门控）。
- **【机器：A】本机生产网关已升级到 1.15.2-panel（2026-10-14 00:11）**：`F:\AI\wb2api-panel-v1.13.0-windows-amd64\` 由 `1.15.1-panel` 升到 `1.15.2-panel`。流程 = 停任务 `WorkBuddy2API` → 备份 `wb2api.exe.pre-1.15.2.bak` → 取 Release `wb2api-panel-v1.15.2-windows-amd64.zip`（sha256 `2fda12cd…c77a`，与 `checksums.txt` 一致）内 exe **先落 `wb2api.exe.new` 再 `move /Y` 原子替换** → 起任务；停机约 3 秒。验收：隔离冒烟 A/B + 生产实例 `1.15.2-panel`、healthz 17/17、原报错形态 `messages[1]=system` 返回 200。回滚 = 用 `wb2api.exe.pre-1.15.2.bak` 覆盖回 `wb2api.exe` 后重建任务即可（约 3 秒停机）。
- **Anthropic 兼容层可演进项（1.15.0 新增）**：
  - **「模型只吐了伪调用」时正文会是空的**（模型想用未声明的工具）：当前**有意**保持空正文 + `end_turn`（宁缺勿造，且严格好于之前的乱码泄漏）。若要改善 UX，可在「清洗后正文为空 + 检测到被清洗的伪调用」时合成一句网关提示文本——改之前先想清楚语言选择与「助手没说过的话」的语义边界。
  - `count_tokens` 是无 tokenizer 的估算（请求 JSON 字节数 / 3）；要精确可接 tiktoken 类实现，注意依赖与本机无 cgo 的约束。
  - 检索后端目前只有 so.com（`internal/websearch`，`Searcher` 接口已就位）：可加 Bing / SearXNG 等实现并按 `search.*` 选择；`anthropicTool` 已解析 `allowed_domains` / `blocked_domains` 但**尚未落到过滤**（接了就能用）。
  - `web_fetch_*` / `code_execution_*` 服务端工具目前剥离（能力降级）；如需支持，可参照 websearch 包加 fetch 实现并复用同一编排骨架。
  - 客户端工具与检索同轮混用的语义已固定（本轮**不执行检索**、立即终局、交客户端；丢弃该轮的 web_search 调用）：`anthropic_websearch.go` 头注释 + 终局条件 `len(searchCalls)==0 || len(otherCalls)>0` 是唯一事实来源。**这块缺一个契约测试**——下次改动前建议先补：复用 `TestAnthropicWebSearchMultiRound` 的 SSE 夹具，加一条「同帧含 web_search 与客户端工具两个 tool_calls」的回复，断言 `searcher.calls` 为空、`stop_reason=tool_use` 且只有客户端工具块。
- **其他可演进项**：
  - 导出接口可加「仅导出健康账号 / 排除禁用号」过滤参数（当前按池内全量 + uids）。
  - cockpit 的 `plan_type` / `checkin_streak` 等运维字段面板仍无数据源，导出省略（`tags` 已于 1.14.0 补齐）；若后续新增这些维度，在 `cockpitExportAccount` 补 `omitempty` 字段即可。
  - README 已有顶部英文概览段；完整双语可参照根 `README.md` 章节新建 `README.en.md`。
  - `-race` 在 Windows 本机不可用，可考虑 CI 增加 Linux race job。
  - 备份恢复目前是手工解压覆盖（README/CHANGELOG 说明）；可考虑面板一键恢复（需处理覆盖前安全备份）。
  - 回收站可加「按标签批量恢复」等批量运维（数据结构已支持）。
  - 导入接口可再扩展：`{"accounts":[...]}` 等包裹格式（当前无现实产物佐证，暂不做）；zip 内嵌 zip 不递归解包。
  - CI 的 Release 标题/正文不合规范（见「关键避坑」），可考虑直接把 workflow 的 `name:` / `body:` 改成合规形态，省掉每次推 tag 后的手工 `gh release edit`。
- **新会话切入路径**：改 Anthropic 层 → 先读 `anthropic.go` / `anthropic_response.go` / `anthropic_websearch.go` / `dsml.go` 四个文件的头注释（设计取舍都写在里面）→ 跑 `go test ./internal/server/ -run 'Anthropic|DSML|ToolCall' -count=1`；改清洗规则 → `anthropic_test.go` 里 `TestStripDSMLText` / `TestDSMLStreamFilter*` 是行为契约，先加失败用例再改 `dsml.go`；改检索解析 → `internal/websearch` 的 `TestParseSoHTML*` fixture 即选择器契约。改回收站/备份 → 先看 `trash.go` / `backup.go` 头注释（目录推导、mtime 语义、毫秒文件名）→ 跑 `go test ./internal/panel/ -run 'Trash|Backup'`；改标签 → `auth.go` 的 `SetTags/NormalizeTags` 与 `export.go/import.go` 的对齐注释；改前端 → 注意 `frontend_test.go` 的 CSP、内联脚本与 id 唯一性断言。

## 6. 关键避坑与运行约束

- **auth.Auth 并发读**：token / domain / expires / tags 等字段会被刷新线程或面板写回，**必须**走带锁访问器（`AccessTokenValue` / `RefreshTokenValue` / `DomainValue` / `TagsValue`）或 `Lock()/Unlock()` 成组读；直读字段是数据竞争。`export.go` 的 `cockpitAccountFromAuth` 是标准示例。
- **毫秒 vs 秒**：cockpit tools 口径 `expires_at` 为**毫秒**；本面板落盘为 **Unix 秒**。导入（`import.go`）÷1000，导出（`export.go`）×1000，改任何一侧都要同步。**1.14.2 补充**：嵌套形条目经 `auth.Parse`（秒）→ `nestedToCockpit` ×1000（≥1e12 视为已是毫秒不再乘）→ 主循环 ÷1000，全部收敛到 cockpit 毫秒中枢口径。
- **导入形状嗅探的边界（1.14.2）**：`classifyAccountEntry` 判据 = 条目含 `auth` 或 `accessToken` 键 → 走 `auth.Parse`（camelCase 口径）；否则必须含 `uid` 或 `access_token` 键（cockpit snake_case 口径），都没有报 `unrecognized format`。单负载请求（非 zip 单文件 / 仅含一个条目的 zip）硬错误直接 400，多负载才降级 `errors`——改语义先看 `importCockpit` 的 `single` 分支与 `roundtrip_test.go` 断言。
- **uids 过滤语义**：`export/cockpit` 的 `uids` 全部未命中返回**空数组**（非 4xx、非回落全量），前端据此 toast 提示；改动该语义需同步 `export_test.go`。
- **auth 文件扁平形字段是 camelCase**：手写 / 造测试账号时写 `accessToken` / `refreshToken` / `expiresAt` / `uid` / `nickname`（见 `auth.go` Parse）；写成 snake_case 会静默解析失败（启动日志 `loaded 0 account(s)`），排查先看这行。
- **pool.Add 不落盘**：`upsertLocked` 不标记 dirty——state.json 要等首次状态变更（签到/余额/选号）才写。依赖「Add 后 state.json 必存在」的测试或备份逻辑需先触发一次状态变更（如 `SetCreditsDetailed`）或接受 zip 里无 state（`writeBackupZip` 已容错）。
- **回收站/备份目录必须在持久化卷**：main 按 `state_file` 同目录推导（`data/trash`、`data/backups`），Docker compose 已挂 data/ 卷；若自定义 state_file 位置，确认同目录持久化，否则容器销毁丢回收站与备份。
- **备份刻意不含 config.json**（内含 api_key 明文）；恢复 = 手工解压覆盖 auths/ 与 state.json。改此设计前重新评估密钥泄露面。
- **app.js 是嵌入资源**：Go 编译器不检查其语法，`frontend_test.go` 用 `node --check` 兜底（无 node 时 skip）——改完前端务必让该测试跑一次（本机已装 node）。
- **CSP 严格策略**：面板禁止内联 `<script>` 与内联事件；新交互逻辑一律写进 `app.js`，按钮用 `id` + `addEventListener`（或 `.onclick`）绑定。
- **id 全局唯一是硬门禁**：新增 HTML `id` 前先查重（1.13.4 事故），`TestIndexHTMLUniqueIDs` 会拦但别依赖事后修。
- **上游 429 不等于账号限流（1.15.5 教训）**：`HTTP 429` 至少有两族语义，判定必须靠**业务码**而非状态码——`6004` 是模型级限流（可切模型绕过）、`6020`/`6021` 是上游免费档排队（**与账号和模型都无关**，切哪个都没用）。排队码若落进 `status==429` 兜底会把全池逐个烧进冷却（实案 106 秒 16/17 号）。改 `Classify` 的 429 相关层级时，务必保持「`14018` 硬积分 → `6020/6021` 排队 → 通用 429 软限流」这个先后序；新增/调整任何 429 分支都要同步 `TestClassify` 的反例（`6004` 不得被排队分支劫持、`200+排队码` 不得判排队）。排队门控是**进程内**状态（重启清零），与 `wafip.go` 同风格但**不做多号取证**（排队码单条响应即自证队列水位）。
- **测试环境**：本机（Windows）无 gcc → `-race` 不可用；`panel.New(Config{...})` 起面板测试无需网络，`Upstream` 为 nil 时**不可**调用会触达上游的 handler（如 `importCockpit` 的签到/激活分支），否则 nil panic；回收站/备份/overview 测试用 `trash_backup_test.go` 的 `newTrashPanel`（真池 + 真目录）。走查用临时服务进程注意：TaskStop 停后台 bash 包装不会杀掉它启动的 exe 子进程——用**精确镜像名**清理（如 `taskkill /F /IM wb2api-test.exe`），**不要**用 `wb2api*.exe` 通配（会连带杀掉生产的 `wb2api.exe`）。
- **Windows 本机代理**：git 固化了 `http.proxy=127.0.0.1:7900`，代理未启动时 `git clone/fetch` 会失败，用 `git -c http.proxy= -c https.proxy= <cmd>` 临时直连；`curl` 直连 `api.github.com` / `raw.githubusercontent.com` 通常可用。**2026-10-15 实测**：该端口当日无监听（Clash 未运行），`git fetch` 报 `Failed to connect ... 127.0.0.1:7900`，直连可正常拉取；`gh` 同样不读系统代理、但读 `HTTP_PROXY`/`HTTPS_PROXY` 环境变量，本会话用 `export HTTPS_PROXY= HTTP_PROXY=` 后 `gh release download` 正常。另：`curl` 探测本机面板若不带 `-w`/`--noproxy`，401/无 body 时看起来像「服务没起」——先看 `:%{http_code}`（面板 API 需带 key，`/healthz` 免鉴权）。
- **Git Bash 会篡改 Windows 命令行参数（2026-10-15 部署时踩）**：`schtasks /query /tn xxx` 里的 `/tn` 被 MSYS 当成路径转换成 `C:/Users/.../git/query` 并报「无效参数」；同理 `.cmd` 批处理里的 `timeout /t 2` 会命中 Git Bash 的 `timeout` 而非 Windows 的 `timeout.exe`，报 `invalid time interval '/t'` 且**静默不等待**（部署脚本因此丢掉了等待步骤）。稳妥做法：查计划任务用 `powershell -NoProfile -Command "Get-ScheduledTask -TaskName x"`（免提权、不涉路径转换），跑批处理用 `cmd //c "x.cmd"`，或先 `export MSYS_NO_PATHCONV=1`。
- **DSML 标记会「毒化」客户端与文档（1.15.0 实测）**：正文里的全角竖线 DSML 标记会被 Anthropic 协议客户端（ZCode 客户端实测）当成**真实工具调用**解析并触发 `Tool not found: web_search` —— 这正是本层清洗存在的理由。写文档 / commit message / 测试夹具时**不要粘贴该标记原文**（Go 源码用 `dsmlToken = "\uFF5C\uFF5C"` 拼装，见 `anthropic_test.go`），否则自己的输出也会被解析成工具调用。
- **Anthropic E2E 沙箱（2026-10-13 状态：两个实例均已停止，目录与夹具保留）**：`:7865` → `F:\AI\wb2api-e2e\wb2api-test.exe`（上一轮遗留、旧二进制，已被 `:7866` 取代）；`:7866` → `F:\AI\wb2api-e2e-dev\wb2api-dev.exe`（开发用；key `sk-e2e-dev-7866`，`auths/` 是生产账号目录的复制，`config.json` 里全调度 / 备份禁用）。**重启方式**：`cd F:/AI/wb2api-e2e-dev && go build -o wb2api-dev.exe ./cmd/server`（在源码目录构建）+ 后台运行 `./wb2api-dev.exe`；请求样本与断言脚本都在该目录（`e2e-*.json` / `runner.sh` / `runner2.sh` / `check.js` / `check-dsml2.js`），改代码后重跑即可。测试账号可安全复制：token 刷新是 **rotate 不 revoke**（旧 token 仍有效），CN 账号约 27 天有效期。清理进程只按精确镜像名（`taskkill /F /IM wb2api-dev.exe`）或精确 PID，**不要用通配**（会连带杀掉生产的 `wb2api.exe`）。
- **中文请求体必须走 UTF-8 文件**：Git Bash 内联 `-d '中文...'` 会按 GBK 编码发出，上游可能解析成乱码或 400；一律写 `.json` 文件 + `--data-binary @file`。
- **Git Bash 的 `/tmp` 与 Node 不互通**：`curl -o /tmp/x.json` 之后 `node -e "readFileSync('/tmp/x.json')"` 会报 `ENOENT ... F:\tmp\x.json`（Node 按 Windows 语义解析）。要跨两者传文件就用相对路径（先 `cd` 到同一目录）或 Windows 绝对路径。
- **SVG `<title>` 只描述父元素**（面板用量图踩过）：想给多个图形分别配提示，必须把「图形 + `<title>`」一起放进各自的 `<g>`；挂成 `<svg>` 的并列子元素时，浏览器只认第一个，于是所有柱子共用同一个提示。判据用「真实事件命中的元素 → 祖先链里最近的带 `<title>` 的容器」，**不要用截图**（原生提示由浏览器 UI 层绘制，不进页面截图）。
- **用量图的读法（面板答疑）**：时序图只画**有流量的时间桶**，窗口内没请求的小时/日子没有柱子（横向按真实时间排布，所以中间留空白——不要误判为"数据缺失"）；「今天」范围下每点 = 一小时。顶部那句「**N 个分桶**」是窗口内命中的**存储桶条目数**（`internal/usage` 的 `matched`，每个「时段 × 账号 × 模型 × 倍率」组合算一条），**不是时间点数**。
- **CI 发布的 Release 标题 / 正文不合规范**：`.github/workflows/go-binaries.yml` 的 publish 任务用 `name: <tag>-panel` + 通用正文，**推 tag 后必须** `gh release edit <tag> --title "vX.Y.Z: 中文摘要" --notes-file <对齐 CHANGELOG 的正文>`（保留 CI 给的 compare 链接在正文里），这是本仓既有惯例。
