# WorkBuddy2API 开发交接文档

> **这份文档的用途**：任何人（或下一个 AI 会话）读完这一份就能直接开工，不需要重新翻代码摸底。
> **每次做完工作必须更新它** —— 更新规程见文末第 8 节，不更新等于这次工作没交付。

- 文档位置：`docs/DEV-HANDOFF.md`（已纳入版本库）
- 与根目录 `HANDOFF.md` 的区别：`HANDOFF.md` 是**发版/线上运维**的接力记录（83KB 历史堆积）；本文件只管**开发**：当前基线、待办、约定、坑。
- 最后更新：2026-10-25

---

## 1. 环境与运行

| 项 | 值 |
|---|---|
| 代码路径 | `F:\AI\workbuddy2api-panel`（**不在** WorkBuddy workspace 内，工具调用请用绝对路径） |
| 模块 | `github.com/linguo2625469/workbuddy2api-panel` |
| Go | 本机 go1.27.0 windows/amd64；`go.mod` 声明 `go 1.22.5`（CI 按 go.mod 取版本） |
| 依赖 | 唯一直接依赖 `github.com/redis/go-redis/v9`；Redis 未配置时自动降级为 Noop 纯内存模式 |
| 本机限制 | **无 gcc / cgo 不可用 → `go test -race` 跑不了**，竞态验证只能在 Linux CI 做 |
| 行尾 | `.gitattributes` 已固定 `*.go` 等为 LF；`*.cmd` 保持 CRLF |

```bash
# 编译
go build -o wb2api.exe ./cmd/server

# 运行（首次运行若 config.json 不存在，会自动生成一份含随机 api_key 的配置）
./wb2api.exe -config config.json

# 面板
#   http://localhost:7863/panel/
#   api_key 即登录凭证（Bearer / 面板登录框通用）
```

**关键约束：面板前端（`index.html` / `app.js`）经 `go:embed` 打进二进制。改前端不重新编译 = 改动不生效。**

---

## 2. 当前基线（每次更新本表）

| 指标 | 当前值 | 采集命令 |
|---|---|---|
| HEAD | `0b3c664` | `git log --oneline -1` |
| 版本常量 | `1.18.3-panel`（`cmd/server/main.go:39`） | `grep 'appVersion =' cmd/server/main.go` |
| 非测试代码 | 99 文件 / 32,626 行 | `wc -l` 汇总 `*.go`（排除 `_test.go`） |
| 测试代码 | 94 文件 / 24,590 行 / 837 个 Test 函数 | 同上 |
| `go test ./...` | **22 包全绿，约 10.6 秒** | `go test ./... -count=1` |
| `go vet ./...` | 0 告警 | `go vet ./...` |
| `gofmt -l ./cmd ./internal` | 空（CI 已设门禁） | `gofmt -l ./cmd ./internal` |
| 最大文件 | `server/handler.go` 1962 / `panel/autotask.go` 1264 / `panel/panel.go` 1083 / `scheduler/scheduler.go` 939 / `cmd/server/config.go` 855 / `pool/state.go` 818 | `find . -name '*.go' -not -name '*_test.go' \| xargs wc -l \| sort -rn` |
| 最慢包 | `internal/scheduler` 约 7.3s（其余均 < 2s） | 见测试输出 |

### 目录职责（18 个 internal 包）

```
cmd/server      入口：配置加载 / 装配 / HTTP Server（main.go 711 行，含 config.go 855 行）
cmd/login       账号扫码登录 CLI        cmd/credit  积分查询 CLI
cmd/signin      签到 CLI（无测试）      cmd/trial   试用/资格判定 CLI（无测试）
internal/
  server        HTTP 入口与协议层：OpenAI 兼容 /v1/* + Anthropic 兼容 /v1/messages
                handler.go(1962) 是主链路；anthropic*.go 是协议翻译；logging.go 请求流水
  panel         管理面板：panel.go 路由 + autotask.go 任务编排 + 内嵌 index.html/app.js
  pool          账号池：选号/熔断/冷却/降权/每日限额/持久化（state.go 三态之一）
  upstream      上游客户端：client.go 主链路 / classify.go 错误分类 / catalog.go 模型目录
                billing.go 计费 / refresh.go 凭据刷新 / paths.go 路径
  session       会话粘性（conversationId → 账号绑定）
  scheduler     定时任务：签到/旅行/保活/夜猫子/余额刷新
  usage         用量记账    reqlog 请求归档    rotaudit 轮换审计
  auth          账号凭证    authlease 目录租约   httpauth API Key 校验（恒定时间比较）
  livecfg       配置热加载（atomic 快照）        redisstore Redis 镜像（可 Noop）
  safefile      fsync+rename 原子落盘            logfmt 日志标签化
  alert         告警 webhook                     prompt 提示词改写    websearch 搜索
```

依赖方向：`cmd → {server, panel, scheduler} → pool → auth`；`upstream → {auth, session, safefile}`。**无循环依赖，改动时不要打破这个方向。**

---

## 3. 已完成的改造（2026-10-25 这一轮）

| commit | 内容 | 怎么验证 |
|---|---|---|
| `8ad641c` | 修测试顺序依赖（`dynamicModelsCache` 跨用例污染）；新增 `server.max_body_mb`（**默认 0 = 不限制**）；面板对比度达 WCAG AA；弹窗补 ARIA+焦点管理；57 处 `<th scope>`；浅色 FOUC；`docs/` 入库；README 四处纠偏 | `go test ./internal/server -shuffle=2`；对比度见 git message |
| `1494301` | **修 P0：SSE 心跳与错误路径并发写 header map 导致进程 fatal**。修法：`ensureSSEHeaders` 在 `headerWritten` 后直接返回、删除 `startWith` 里锁外调用、心跳改抓局部 stop channel | `go test ./internal/server -run TestAnthropicHeartbeat -v`；该用例可反证（回退修复立即失败） |
| `9ce24fe` | 14 处测试不再打真实网络（改毒桩 + `calls==0` 断言）；`TestMain` 补 `rotateBackoffBase = 0` | `go test ./internal/server` 耗时 11.3s → 0.7s |
| `1b7bcd7` | `upstream.SetBillingRetryDelayForTest` 注入点；`TestCheckinErrorDoesNotCrash` 从零断言改为 4 条真断言 | 单条 12.01s → 0.02s，scheduler 19.5s → 7.3s |
| `df97527` | `client.go` 2580 行按职责拆 6 文件（纯搬运） | 154 个顶层声明零缺失；行多重集比对丢失 0 行 |
| `0b3c664` | 25 个文件 gofmt + `.gitattributes`(LF) + CI 加 gofmt 门禁 | `gofmt -l ./cmd ./internal` 为空 |

---

## 4. 待办清单（按优先级，含落地要点）

> 每条都写了「改哪里 / 怎么改 / 风险 / 验收」，可直接照做。完成后请删除该条并记入第 8 节日志。

### P1 — 直接影响可用性、费用或后续开发效率

**4.1 面板字号令牌化**
- 现状：`internal/panel/index.html` 有 13 种字号散落（10px×1、11px×8、11.5px×17、12px×18、12.5px×16、13px×10、13.5px×5、14/15/15.5px 各若干），4 个半像素值。
- 改法：在 `:root` 定义 `--fs-xs:11px / --fs-sm:12px / --fs-base:13px / --fs-md:14px / --fs-lg:16px / --fs-xl:20px`，把 `font-size:` 与 `font:` 简写里的字号全部替换为 `var(--fs-*)`，`10px` / `10.5px` 一律并入 `--fs-xs`。
- 风险：**纯视觉回归**（字号收敛必然微调观感），必须人眼过一遍深浅两套主题。
- 验收：`grep -o 'font-size:[^;]*' index.html | sort -u` 只剩 6 种 + 变量引用；面板两套主题截图对比。

**4.2 账号主表吸附表头**
- 现状：`index.html:639` 只有请求表有 `.req-wrap thead th { position:sticky }`，账号表 `#accTable` 没有，长列表滚动后失去列名。
- 改法：给 `.acc thead th` 加 `position:sticky; top:0; background:var(--surface); z-index:1`，注意 `.topbar` 也是 `sticky top:0 z-index:20`，需给表头设置合适的 `top`（表头应吸在 topbar 下方）并避免 z-index 叠层错乱。
- 验收：账号列表滚动时表头常驻、不遮挡 topbar。

**4.3 拆分 `internal/server/handler.go`（1962 行）**
- 参考 `df97527` 的做法：**纯文件切分，零逻辑改动**。建议按端点族拆：`handler.go`(主链路/装配) / `handler_chat.go`(chatCompletions) / `handler_models.go`(/v1/models) / `handler_status.go`(/status,/healthz) / `handler_error.go`(错误写出与策略)。
- 必须用机械等价验证（见第 7 节），不要人工目测。
- 风险：低（编译期即可发现漏搬），但 diff 上千行，需单独 commit。

**4.4 请求体里 `metaData` 体积优化（opt-in）**
- 现状：客户端历史消息里每个 tool-call 都带 `tool.metaData._meta`（约 20 个字段：`traceId/traceparent/baggage/requestId/rawResponse/时间戳`），39 个 tool-call ≈ 55–70KB **纯追踪数据**；`payload.go` 的归一化链**不处理**它，原样透传上游 → 挤占上下文与费用。
- 改法：新增配置 `upstream.strip_tool_metadata`（默认 `false`），在 `payload.go` 归一化链加 `stripToolMetaData(obj)`：遍历 `messages[].content[]`，删 `type=="tool-call"` 的 `tool.metaData` 与 `tool.result.metaData`，保留 `id/name/args/result.text`。
- **先测量再改**：配 `upstream.dump_dir` 落一份真实出站请求（`dump.go` 落的就是发给上游的 body），对比开关前后字节数。
- 风险：上游是否依赖这些字段未知 → 必须单账号 A/B（同一请求开关对比响应与错误码）。
- 验收：单测断言「剥除后 id/name/args/result.text 完整、metaData 消失、reasoning 块不受影响」；dump 体积对比有数字。

**4.5 入站 per-key 限速**
- 现状：只有账号级 `max_in_flight`，**没有按 API Key 的入站限流**；配合默认不限制的请求体，单客户端可压满。
- 改法：在 `server` 层加令牌桶（按 `api_key` 或 `X-Forwarded-For`），配置 `server.rate_limit_rps` / `burst`，默认 `0 = 关闭`（保持零行为变更）。超限回 429 + `Retry-After`。
- 风险：限流粒度选错会误伤多客户端共享同一 key 的场景 → 默认关闭 + 文档说明。

**4.6 上游码 → HTTP 状态/错误体映射抽单一函数**
- 现状：映射散在 `handler.go` 8 处（`StatusTooManyRequests`/`StatusBadGateway` 直接赋值）。
- 注意：**v1 审计报告说映射分散在 `backoff.go`/`degrade.go`/`handler.go` 三处，这是错的** —— 前者是退避抖动单一来源，后者是降级状态机，都不做状态映射。
- 改法：抽 `mapUpstreamErr(kind upstream.ErrKind) (status int, typ string)`，handler 只调它。
- 风险：改变对外错误语义，属契约变更 → 单独发版。

**4.7 上游 `requestId` 关联进日志**
- 现状：网关自己的 `X-Request-Id`（`handler.go:246`）和日志 `rid` 都有；**缺的是上游返回的 `requestId` 没被记录**，跨端排查接不上。
- 改法：`Classify`/响应解析时把上游 `requestId` 取出，随流水行输出（如 `up_rid=...`）。

**4.8 开启 `-race` 门禁**
- 只能加在 Linux CI（本机无 gcc）。步骤：先在 CI 单独跑 `go test ./internal/pool ./internal/server ./internal/scheduler -race`，**确认全绿后再设为阻断**；项目 mutex+goroutine 密集，很可能暴露新问题，要有心理准备。

### P2 — 结构性债务

- **4.9 弹窗改原生 `<dialog>`**：现在是 `div.veil` + 自建焦点陷阱（已可用），缺 top-layer / 背景 `inert` / `::backdrop`；读屏仍能浏览背景内容。
- **4.10 三态一致性**：`pool`（内存）↔ `state.json`（快照）↔ `redisstore`（镜像）靠 `main.go` 的 `RestoreFromSnapshot()`「择新恢复」，并发写或时钟漂移可能回滚数据。需要单一真相源或版本号/租约。
- **4.11 日志结构化**：全仓 249 处 `log.Printf`（非测试），级别靠字符串前缀；`internal/logfmt` 已存在未全量采用。建议迁 `log/slog` 并保留面板 ring 的频道分类。
- **4.12 pprof / metrics 端点**：目前只有 `/status` 聚合计数，线上无法定位 CPU/内存热点。
- **4.13 文档拆分**：README 87KB / HANDOFF 83KB / CHANGELOG 82KB。建议 README 拆「索引 + 专题页」，HANDOFF 只留「当前状态 + 下一步 + 避坑」。
- **4.14 前端行为测试**：`app.js`(3449 行) 只有 `node --check` 语法检查 + CSP 断言，**无 node 时静默跳过**；73 处 `innerHTML` 无回归网。

### P3 — 低成本清理
圆角令牌化（13→3 档）｜骨架屏（当前 0 处）｜触控目标 27.7px → 44px（移动端）｜i18n（现硬编码中文）｜图标集（现 16 处内联 SVG）｜60 处内联 style 收敛｜OpenAPI 自动生成（40+ 端点靠 README 手工维护）｜`cmd/*` 冒烟测试（signin/trial/credit 零测试）｜`go.mod` 加 `toolchain`｜`panel/ring.go` 覆盖率 7.5% / `upstream/dump.go` 0%

---

## 5. 约定与坑（血泪，务必先读）

### 提交约定
- 提交信息：`type(scope): 中文描述`，type 用 `fix/feat/refactor/test/chore/docs`。参考 `git log` 现有风格。
- **一次提交只做一件事**。格式化、重构、行为变更、测试改造不混在一起。

### 测试铁律
- **不许削弱已有断言**。把"打到真上游拿 401"改成假桩时，要**新增** `calls == 0` 这类更强断言。
- **新测试必须可反证**：写完把修复回退，确认用例会失败，再恢复。参考 `anthropic_header_race_test.go` 的做法（用 Header() 调用计数锁不变量，绕开无 `-race` 的限制）。
- **测试不许打真实网络**。用 `newFakeUpstream(t, responder)` / `roundTripFunc` 打桩。
- 需要缩短等待时，用注入点（`TestMain` 里 `rotateBackoffBase = 0`、`upstream.SetBillingRetryDelayForTest`），**不要加 `time.Sleep`**。

### 已知的坑
1. **SSE 响应头只能设一次**：`anthropicStreamWriter.ensureSSEHeaders()` 在 `headerWritten` 后必须直接返回。它同时被当作 `ResponseWriter` 下传给 `chatCompletions`，其 `Header()` 直通底层 map 且**不持 `wmu`**；心跳在锁内改头 → 两个写者 → `fatal error: concurrent map writes`（进程级 abort，不可 recover）。**这是真实发生过的 P0。**
2. **本机 `gofmt` 曾误报 149 个文件**：那是 CRLF 造成的，`*.go` 真实不合规是 25 个，已清理。现在 `.gitattributes` 固定 LF，本地与 CI 判定一致。改完代码先 `gofmt -w` 再提交，否则 CI 红。
3. **面板改完必须重新编译**：资源是 `go:embed`，不重新 build 等于没改。
4. **仓库里可能挂着别人未提交的改动**（如 `internal/upstream/payload.go`、`payload_test.go` 的 `$ref` 解引用工作）。**提交前用 `git status` 确认，只 `git add` 你改的文件**，不要 `git add -A`。
5. **`/v1/models` 是纯动态无兜底**：拉不到就返回空列表 + 5min 负缓存。`dynamicModelsCache` 是包级全局，测试里必须用 `resetDynamicModelsCache(t)`，否则会污染后续用例（曾被误判为"偶发 flaky"）。
6. **`max_body_mb` 默认 0 = 不限制**：这是刻意的零行为变更设计，别"顺手"改成默认限制。
7. **部署前备份 `data/state.json`**：账号池状态、冷却、用量都在里面。

---

## 6. 常用命令

```bash
# —— 日常 ——
go build ./...                 # 编译全部
go vet ./...                   # 静态检查（CI 门禁）
gofmt -l ./cmd ./internal      # 格式检查（CI 门禁，应为空）
go test ./... -count=1         # 全量测试（约 10 秒）
go test ./internal/server -count=3 -shuffle=1   # 单包乱序重复（抓顺序依赖）

# —— 交付前必跑 ——
go build ./... && go vet ./... && gofmt -l ./cmd ./internal && go test ./... -count=1

# —— 排查 ——
go test ./internal/server -run <用例名> -v     # 单条 + 日志
curl -s localhost:7863/healthz                 # 探活（含 realm_servable）
curl -s -H "Authorization: Bearer $KEY" localhost:7863/status   # 池状态
```

---

## 7. 重构的机械等价验证（做了大范围搬运时必须用）

不许靠肉眼确认"只是搬动"。两套机械检查一起上：

```bash
# ① 符号集合：旧文件的每个顶层声明（含方法接收者、const/var 块内裸名）必须还在
git show HEAD:<旧文件> | grep -oP '^(func|type|var|const)\s+(\(\w+ \*?\w+\)\s*)?\K\w+' | sort -u
# 再逐个在目标包里 grep，缺失数必须为 0

# ② 行多重集：按行（去空白）做 Counter 差集
#    旧文件 vs 新文件集合，丢失行数必须为 0；新增行应当只有 package/import/文件头注释
```

`df97527`（client.go 拆 6 文件）就是用这套验证的：154 个声明缺失 0、行丢失 0、新增 56 行全是 import 骨架。

---

## 8. 更新规程（每次工作结束必做）

完成后**必须**回头改这份文档的三处，否则下次接手的人会按过期信息开工：

1. **第 2 节基线表** —— 更新 HEAD、测试耗时、包数、最大文件等（跑一遍第 6 节的"交付前必跑"拿到真实数字）。
2. **第 3 节已完成** —— 追加一行：`commit | 内容 | 怎么验证`。验证方式要能照抄执行。
3. **第 4 节待办** —— 删除已完成的条目；新发现的坑补进第 5 节；新发现的待办按优先级插入。

可选但推荐：如果这次改动引入了新的"反直觉设计"或踩到新坑，**补进第 5 节**——那一节的价值就在于让下一个人不重复踩。

### 更新日志

| 日期 | 做了什么 | HEAD |
|---|---|---|
| 2026-10-25 | 建立本文档；记录 6 个提交（测试顺序依赖、SSE fatal、测试去网络、scheduler 提速、client.go 拆分、gofmt+CI 门禁）与全部待办 | `0b3c664` |
