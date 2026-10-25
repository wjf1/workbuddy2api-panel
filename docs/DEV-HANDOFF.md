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
| HEAD | `94799e5` | `git log --oneline -1` |
| 版本常量 | `1.18.3-panel`（`cmd/server/main.go:39`） | `grep 'appVersion =' cmd/server/main.go` |
| 非测试代码 | 107 文件 / 33,204 行 | `wc -l` 汇总 `*.go`（排除 `_test.go`） |
| 测试代码 | 101 文件 / 858 个 Test 函数 | 同上 |
| `go test ./...` | **22 包全绿，约 10–12 秒**（scheduler 占 7s，其余均 <2s） | `go test ./... -count=1` |
| `go vet ./...` | 0 告警 | `go vet ./...` |
| `gofmt -l ./cmd ./internal` | 空（CI 已设门禁，行尾已统一 LF） | `gofmt -l ./cmd ./internal` |
| 最大文件 | `panel/autotask.go` 1264 / `panel/panel.go` 1083 / `server/handler_chat.go` 976 / `scheduler/scheduler.go` 939 / `cmd/server/config.go` 858 / `pool/state.go` 818 | `find . -name '*.go' -not -name '*_test.go' \| xargs wc -l \| sort -rn` |
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
| `d8d70a0` | 面板字号 13 种 → 6 档语义令牌（`--fs-xs/sm/base/md/lg/num`）；账号表表头吸附（`--topbar-h`） | `grep -o 'font-size:[^;]*' internal/panel/index.html \| sort -u` 只剩令牌；滚动账号表看表头 |
| `bd4d969` | 补 `scheduler_wait_test.go` 文档注释缩进（Go 1.19+ gofmt 规则） | `gofmt -l ./cmd ./internal` |
| `9f0b9be` | 新增 `features.strip_tool_metadata`（默认 false）：出站剥除 tool-call 的追踪 `metaData`，无字段可删时字节不变 | `go test ./internal/upstream -run TestStripToolMetaData -v`（4 个用例） |
| `8160b3b` | `handler.go` 1962 行按端点族拆 5 文件（纯搬运） | 47 个声明零缺失、行多重集丢失 0 行 |
| `6deb1b5` | 入站限流 `server.rate_limit_rps`（默认关）：令牌桶按 TCP 对端 IP、429+`Retry-After`、豁免 `/status` 与 `/healthz` | `go test ./internal/server -run 'TestRateLimiter\|TestHandlerRateLimit\|TestHandlerRejectsWith429' -v`（9 个用例） |
| `3c1a87f` | 上游错误类别→HTTP 状态码收敛到 `errmap.go` 的单一表（此前散在 6 处）；附「每个 ErrKind 都必须表态」的完整性测试 | `go test ./internal/server -run TestHTTPStatusForKind -v`（2 个用例）。**注意：v1 审计说映射分散在 `backoff.go`/`degrade.go`/`handler.go` 三处是错的**，实际是 `handler_chat.go` 内 6 处 |
| `92011e4` | 上游 `requestId` 与本地 rid 同列落台账：`Error.RequestID` + `ParseUpstreamRequestID`（三种形态）+ `reqlog.Event.upstream_request_id` | `go test ./internal/upstream -run 'TestParseUpstreamRequestID\|TestChatStreamErrorCarries' -v`；`go test ./internal/server -run TestRequestEventCarries -v` |
| `be7747f` | 可选诊断监听 `server.debug_addr`（pprof + 低基数 `/metrics`），默认空关闭，**独立端口** | `go test ./cmd/server -run TestDebugServer -v`；`go test ./internal/server -run TestMainMuxHasNoDebug -v` |
| `d4410a5` | CI 增加 `-race` 作业（pool/server/scheduler 三个包，首轮 `continue-on-error`） | 看 Actions 里 race job；本机无 gcc 跑不了 |
| `94799e5` | 弹窗打开时给 `.shell` 加 `inert`（读屏不再能浏览遮罩背后内容） | `node --check internal/panel/app.js`；面板测试 |

---

## 4. 待办清单（按优先级，含落地要点）

> 每条都写了「改哪里 / 怎么改 / 风险 / 验收」，可直接照做。完成后请删除该条并记入第 8 节日志。

### P1 — 需要先设计，不能盲改

**4.1 状态三源一致性（pool 内存 ↔ `state.json` ↔ Redis 镜像）**
- 现状：`main.go` 的 `RestoreFromSnapshot()` 取"较新"的那份（比 `saved_at` / 文件 mtime），三处都能写，没有版本号也没有租约。并发写或时钟漂移时可能把**较旧的快照**覆盖上去。
- 为什么没做（重点）：这不是"抽个函数"能解决的，盲改的后果是**账号冷却状态回滚**——被冷却的号重新进入轮换，轻则继续被上游拒（白烧请求），重则触底号被打穿、账号被判死。改之前必须先有"冲突场景会怎样"的测试，而当前 `pool` 的持久化测试只覆盖单源读写。
- 建议路线（按顺序）：
  1. 给状态快照加单调整数 `revision`（每次 Flush +1），恢复时**只接受 revision 更大**的那份，`saved_at` 降级为观测字段；
  2. 补测试：本地旧 + Redis 新 → 采用 Redis；本地新 + Redis 旧 → 采用本地；revision 相同 → 采用本地（确定性，不靠时钟）；
  3. 再考虑多实例写冲突（租约或 CAS），那需要 `redisstore` 支持 WATCH/MULTI。
- 验收：上述三种组合的测试全绿 + 手工把 Redis 的 revision 调低后重启，状态不被回滚。

### P2 — 结构性债务

- **4.2 日志结构化（迁 `log/slog`）**：全仓 249 处 `log.Printf`（非测试），级别靠字符串前缀。**注意**：面板的 ring 与 `logfmt` 是按前缀/频道解析的，迁移必须保留输出形状（或同步改解析侧），否则面板会静默丢日志。建议一个包一个包迁，每迁一个包跑一次面板回归。
- **4.3 弹窗完整迁移原生 `<dialog>`**：`inert` 与 `role/aria-modal` 已补（`94799e5`），剩下 top-layer（`showModal()` 天然置顶，不受 z-index 影响）与 `::backdrop`。属于结构性重写 8 个弹窗，需要人眼验收，建议单独一轮做。
- **4.4 文档拆分**：README 87KB / HANDOFF 83KB / CHANGELOG 82KB。拆的时候注意仓库内外已有大量 `#锚点` 引用，改标题会断链；建议先加索引页再加专题页，最后才收缩主文档。
- **4.5 前端行为测试**：`app.js`(3449 行) 只有 `node --check` + CSP 断言，且**无 node 时静默跳过**。要写行为测试得先把可测函数（`esc` / `dur` / 时间格式化 / 模型名解析）抽成可导出模块，是一次结构性改动，不建议在功能迭代中途做。

### 明确不做的（及原因）

- **给 `/metrics` 加 per-model / per-account 标签**：基数会炸到几千，先崩的是 Prometheus。要按维度拆，先上 exemplar 或降采样。
- **全量 `-race` 阻断**：`-race` 作业目前是观察态（`continue-on-error`）。等它在 CI 里连续几轮全绿再转阻断——现在直接阻断大概率一次红到底。
- **把 `strip_tool_metadata` 并进 `payload.go` 归一化链**：纯性能优化（省一次 parse），等 `payload.go` 没有在途改动时再做，避免两个工作混进同一 diff（见坑 #9）。

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
8. **批量改行尾后 `git status` 会诈胡**：`.gitattributes` 改成 LF 后，把已在盘上的 CRLF 文件转成 LF，`git status` 会把上百个**内容没变**的文件标成 `M`（stat 缓存失配，`git diff` 却是空的）。别慌、更别 `git add -A`，执行：
   ```bash
   git status --porcelain | awk '$1=="M"{print $2}' | grep -v "upstream/payload" | tr '\n' '\0' | xargs -0 git add --
   ```
   这些文件内容与索引一致，`git add` 只刷新 stat 缓存、不会产生提交内容（用 `git diff --cached --name-only` 确认为空）。之后 `gofmt -l` 才会与 CI 结论一致。
9. **`strip_tool_metadata` 的技术债**：配置项落在 `features` 段（与 `sanitize_blacklist_fingerprints` 同段，复用其热加载路径），实现放在独立文件 `internal/upstream/strip_metadata.go`，调用点挂在 `client.go` 的 `prepareBody`，**没有并进 `payload.go` 的归一化链**。原因是当时 `payload.go` 有他人未提交改动，并进去会把两个工作搅在同一个 diff 里。代价：开关开启时多一次 parse+marshal（默认关闭时零成本）。后续若要优化，可把它合并进 `PrepareBodyOptWithEffortsAndDefault` 的同一次解析。
10. **限流键刻意不用 `X-Forwarded-For`**：`internal/server/ratelimit.go` 的 `rateKey()` 取的是 `RemoteAddr` 的主机部分。XFF / X-Real-IP 客户端可任意伪造，按它限流等于"每请求换一个伪造 IP 拿无限额度"，限流形同虚设。代价是反代后面所有请求对端都是代理地址，桶退化为全局限流——**这是有意的安全方向退化，别"顺手优化"成读 XFF**。同理也别改成按 `api_key` 限流：本网关只有一个 key，按 key 限等价于全局限流。

11. **诊断端点必须独立端口**：`server.debug_addr` 刻意不开在主 mux 上。Go 的 `net/http/pprof` 无鉴权，`profile` 能导出 CPU/heap，heap 里含请求体与账号 token；主端口通常绑 `0.0.0.0`。回归测试 `TestMainMuxHasNoDebugEndpoints` 钉住这一点——**别为了"顺手"把它加回主路由**。`/metrics` 也只放低基数指标（状态枚举 + goroutine 数），加 per-model 标签会先崩 Prometheus。

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
| 2026-10-25 | 完成 4 项 P1：面板字号 6 档令牌化、账号表表头吸附、`handler.go` 拆 5 文件、`strip_tool_metadata`（opt-in）；顺带统一工作区行尾为 LF（此前本地 gofmt 会误报 100+ 文件），补 1 处漏网格式问题 | `8160b3b` |
| 2026-10-25 | 完成入站限流 `server.rate_limit_rps`（opt-in，与 `max_body_mb` 配套）；待办缩到 9 项 | `6deb1b5` |
| 2026-10-25 | 上游错误类别→HTTP 状态码收敛到 `errmap.go` 单表（原散在 6 处），附完整性测试；待办缩到 8 项，P1 只剩 2 项 | `3c1a87f` |
| 2026-10-25 | 完成剩余可安全落地的 4 项：上游 requestId 落台账、pprof/metrics 独立诊断监听、CI `-race` 观察作业、弹窗 `inert`；其余 4 项因「需先设计/需人眼验收/会与在途改动冲突」明确保留（见第 4 节末尾） | `94799e5` |
