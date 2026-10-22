# 更新日志（CHANGELOG）

本仓库从 `1.13.0-panel` 起维护本变更日志；更早版本的历史请查阅 Git tag（`v1.9.2` 及以前）与提交记录。

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循语义化版本（`<主>.<次>.<修订>-panel` 后缀表示 fork 面板增强分支）。

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

- **单测**：`internal/upstream/headers_test.go` 新增 `TestChatHeadersTraceparentAlways32Hex`（4 子例：32 位原样 / 16 位左补零且保留原值 / 空值现造 / 非法值回落）与 `TestChatHeadersTraceparentRejectsAllZero`；`cache_key_test.go` 断言升级为 UUID v4 形态 + 不含 `wb2a`/`wb2api`/`workbuddy2api` 与 uid 明文；`pool` 新增 14003 短窗口阶梯/不缩短更晚冷却、`safefile` 撕裂文件回退、`cst` 的 UTC 主机回归。全量 `go test ./... -count=1` **20 包全绿**（基线 18 包 + 新增 `cst`/`safefile`）。
- **独立 QA 用例**（临时文件，验证后删除）：16 条对抗性用例覆盖「config 缺失仍返回 `fs.ErrNotExist` 让 main 走自动生成」「`.bak`/`.bad-*`/`.tmp` 不被当账号加载」「`TZ=UTC` 下 `cst.Now()` 仍 +8」「连续 3 次 14003 不越过 1m 且账号 `cooling=false`」「`6004` 语义未变」「撕裂文件回退 `.bak`」——其中 **1 条抓到真实缺陷**（即上文 `traceparent`），其余 15 条通过。
- **未做**：本版未在真机上游联调即发布前验证；`go build ./...` / `go vet ./...` 干净。

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
