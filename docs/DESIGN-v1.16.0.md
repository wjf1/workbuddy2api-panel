# v1.16.0-panel 详细设计：会话失效恢复与防复发

> 状态：待评审 → 待实施。行号引用以工作区 `F:\AI\workbuddy2api-panel`（基线 v1.15.7-panel，git `2947308`）为准。
> 背景：2026-10-23 11:41 生产实例 6 个账号 RT 链被上游批量吊销（refresh 端点回 `code=12153 invalid_grant`），只能人工重走 OAuth；另 1 个账号 `11140 request illegal` 上游风控封号。用户"每天点保活"对死号无效：签到不刷 token、禁用号被所有保号任务跳过、无任何自动重登路径。本设计目标是**把"失效"从被动等待变为：秒级可见 → 一键恢复 → 机制性防复发**。

---

## 0. 威胁模型与防线总览

| # | 失效根因（按实证） | 防线（模块） |
|---|---|---|
| A | RT 双花连坐：同一 RT 被两个执行体（双机部署副本 / 独立 signin.sh 进程 / 旧版进程内并发）并发消费，撞上游 rotation/replay 检测，整条链吊销 | M4 租约锁（防同目录双实例）、M6 轮换审计 + 外部轮换检测（抓异地副本双活）、运维规约（一号一实例，README/HANDOFF 明示） |
| B | 上游风控批量吊销：17 号同机同出口 IP、每日 22:00 整点齐射刷新、9/21 点齐射签到，批量特征明显 | M5 时点抖动（消除齐射）、M7 智能保活（刷新量降 ~97%）、M2 批量判死告警（发生后 5 分钟内可见） |
| C | 会话已死后的恢复成本：只能翻菜单"添加账号"，死号混在普通禁用里不可辨识 | M1 死号分组 + 一键重登（复用现有 OAuth 流，后端近零改动） |
| D | 风控封号（11140）与 session dead 混淆 | M1 的 `dead_kind` 分类（session_dead / banned），banned 不参与 M8 探活 |
| E | 观测盲区：`session_dead_fails` API 已透出但前端不渲染；批量事件无通知 | M3 计数渲染、M2 横幅 + webhook |

模块清单与优先级：**M1/M2/M3 = P0**（可见性与恢复），**M4/M5/M6 = P1**（防复发机制），**M7/M8 = P2**（调度韧性，M8 默认关闭）。

---

## 1. M1 — 死号「待重登」分组与一键重登（P0）

### 1.1 后端：`dead_kind` 派生字段（唯一后端改动）

前端猜 `reason` 字符串不可靠（reason 是自由文本），由 pool 层收口分类：

- `internal/pool/entry.go`：`Status` 新增字段
  ```go
  DeadKind string `json:"dead_kind,omitempty"` // "session_dead" | "banned" | ""（非死号）
  ```
- 填充点：`state.go` 的 `statusOf`（state.go:520-582）内，`if e.disabled` 分支按 `e.reason` 分类：
  - `strings.Contains(reason, "session dead")` → `"session_dead"`（覆盖 `12153 session dead` 与 `refresh session dead` 两种 reason 常量，entry.go:532-538 / handler.go:941）
  - `strings.Contains(reason, "banned")` → `"banned"`（11140，entry.go 既有 reason 文案含 "banned by upstream"）
  - 其余 disabled（人工禁用 / 11140 之外的账号级故障）→ `""`（仍显示"已禁用"，不进重登分组）
- 常量化：把 `sessionDeadReason` 的判定收敛为 `IsSessionDeadReason(reason string) bool`（pool 包导出，M2 复用）。

### 1.2 一键重登：完全复用现有登录流，后端零新增端点

现有 `loginPoll`（`internal/panel/login.go:158-289`）在授权完成后：`SaveAtomic` 覆盖 `auths/workbuddy-<uid>.json`（同 uid 同文件名，upsert）→ `Pool.Add(a)`（pool.go:336 → `upsertLocked`，对已存在 uid 替换凭证引用）→ `Pool.Revive(acct.UID)`（state.go:78-98，全清禁用/冷却/计数）→ 顺带签到 + 余额刷新。**对死号重登同 uid 即"覆盖凭证 + 复活"，语义完全正确，后端无需任何改动。**

### 1.3 前端

**分组筛选**（`internal/panel/app.js`）：
- `accFilter`（app.js:358）扩第三维：`const accFilter = { q:'', tag:'', st:'' }`；`accApply`（app.js:359-367）追加：`st==='dead'` 时仅保留 `d.disabled && d.dead_kind`；`st==='ok'` 时排除之。
- `index.html` 工具栏在 `#accFilterTag` 旁新增 `#accFilterSt`（select：全部 / 待重登 / 可用正常），样式与既有下拉一致。

**死号行 UI**（`renderAccounts`，app.js:368-446）：
- 状态列（app.js:380-391）：`dead_kind` 非空时 tag 用醒目红底文案——`session_dead` →「待重登」、`banned` →「上游封号」；hint 小字保留 reason 原文。
- 操作列（app.js:435-443）：`dead_kind==='session_dead'` 的行追加「重登」按钮（`data-a="relogin"`，banned 行不给——封号重登大概率再被封，仅显示文案）；在 `#accBody` click 委托（app.js:548 起）新增 `relogin` 分支 → `openRelogin(uid)`。

**重登弹窗**（新增 `#reloginVeil`，结构与 `#addVeil`（index.html:1242-1287）同型）：
- 内容：目标账号昵称/uid、realm 预选（取该账号 `st.realm`，缺省 cn；global 单选仍可切换）、授权 URL 展示块（`<a target="_blank">` 打开 + 「复制」按钮）、状态行（轮询中 / 成功 / 失败 / 超时）、关闭按钮。
- 逻辑：`openRelogin(uid)` → 记录 `reloginTarget = uid`，打开 veil → `POST login/start {realm}` → 显示 URL → `setInterval(pollRelogin, 3000)` 复用 `api('login/poll?state=...')`：
  - `done:false` → 状态行显示 message；
  - `done:true` 且 `r.uid === reloginTarget` → 成功态 + toast「重登成功，账号已复活」→ 1600ms 后关 veil + `loadOverview(true)`；
  - `done:true` 但 uid 不同 → 警示态「登录的是另一个账号（昵称），已作为新账号加入池」，引导用户对目标账号重试；
  - 轮询 15 分钟（后端 `loginTTL`，panel.go:175）未完成 → 超时态提示重新发起。
- 复用 `pollLogin`（app.js:1189-1206）的轮询骨架，独立函数避免与"添加账号"的状态块互相污染。
- **id 唯一性门禁**：`#reloginVeil` 内所有新 id 提交前过 `TestIndexHTMLUniqueIDs`（grep 全文档查重）。

### 1.4 测试要点

- pool：`dead_kind` 分类单测（session dead 两种 reason / banned / 人工禁用 / 空）。
- 前端：`frontend_test.go` 既有语法 + CSP + UniqueIDs 断言覆盖新 id。
- 沙箱 + 真实点击走查：本机生产就有 6 个真死号——直接在生产演练「重登按钮 → 弹窗 → URL → 人工完成 OAuth → 复活」，恢复与验收一步到位（见 §9）。

---

## 2. M2 — 批量判死实时告警 + webhook 通知（P0）

### 2.1 事件源：pool 禁用 hook（单一收口）

所有禁用路径都经 `disableLocked`（`internal/pool/transition.go:51-57`）。给 Pool 加可选 hook：

```go
// Pool 新增（构造后由 main 装配，nil 时零开销）
SetOnDisabled(fn func(uid, nickname, reason string))
// disableLocked 末尾（持锁内只做拷贝，回调在锁外）：
if p.onDisabled != nil { go p.onDisabled(e.UID, e.nickname, reason) }
```

注意：锁内不直接调外部回调（防死锁/长阻塞），拷贝参数后 goroutine 外发。`NoteSessionDead` 满 3 禁用、chat 路径单发 Disable、banned 禁用全部自然经过此点。

### 2.2 告警器 `internal/alert`

```go
type Config struct {
    WebhookURL         string        // 空 = 不外发，仅横幅
    MassKillAccounts   int           // 窗口内判死账号数阈值，默认 3
    MassKillWindow     time.Duration // 默认 10m
    Cooldown           time.Duration // webhook 外发冷却，默认 30m
}
type Manager struct { /* mu、events []evt、lastSent time.Time、cfg atomic.Value、hc *http.Client(5s 超时) */ }
func (m *Manager) NoteDisabled(uid, nickname, reason string)  // 只记 IsSessionDeadReason 命中的
func (m *Manager) Payload() any                               // overview.mass_kill，见下
func (m *Manager) SetConfig(c Config)                         // 热应用
```

`NoteDisabled` 逻辑：追加事件 → 清理窗口外旧事件 → 若窗口内 **distinct uid 数 ≥ 阈值**且距上次外发 ≥ cooldown → goroutine 外发 webhook（失败静默重试 1 次，仍失败仅 ERROR 日志；外发失败不影响横幅）。**横幅不需要 cooldown**：`Payload()` 每次实时计算窗口内事件，事件老化后横幅自然消失。

webhook body（通用 JSON POST，可对接飞书自建应用 / Bark / 企业微信 webhook）：

```json
{ "event": "mass_session_dead", "host": "<hostname>", "version": "1.16.0-panel",
  "count": 3, "window_minutes": 10,
  "accounts": [ { "uid": "...", "nickname": "...", "reason": "12153 session dead", "at": "RFC3339" } ] }
```

M6 的外部轮换事件也走本 Manager 外发（`event: "external_token_rotation"`，同样受 cooldown 约束），见 §6。

### 2.3 overview 透出与前端横幅

- `panel.go` overview（panel.go:300-324）新增顶层字段 `mass_kill`：无事件为 `null`；有事件为 `{count, window_minutes, uids: [...], first_at, last_at}`。
- 前端：`index.html` 在 `#queueBox`（index.html:677-684）下方同型新增 `#killBox`（`.box` + `hidden` 切换）；`app.js` 新增 `renderKillBox(d)` 并在 `loadOverview()`（app.js:470-471）与 `renderQueueBox/renderAlertBox` 并列调用。文案：
  > ⚠ 最近 10 分钟内 3 个账号会话判死（待重登）——疑似上游批量吊销或凭证双活，请到「账号池 → 待重登」处理并检查是否有第二实例在共享凭证。

### 2.4 配置与热应用

`config.example.json` 新增段（`cmd/server/config.go` 新增 `Notify` struct + Default/normalize 兜底）：

```json
"notify": {
  "webhook_url": "",
  "mass_kill_accounts": 3,
  "mass_kill_window_minutes": 10,
  "cooldown_minutes": 30
}
```

热应用：`saveConfig`（main.go:486-573）热应用区新增 `alertmgr.SetConfig(...)`（仅 URL/阈值类，无需重启，**不进** `restartRequiredFields`）；配置页 `CFG_MAP`（app.js:996-1004）加对应表单项。

### 2.5 测试要点

- 单测：窗口计数（同 uid 重复禁用只算 1）、阈值边界、cooldown 抑制外发但横幅照常、webhook 失败静默（httptest 挂掉端点）、banned 事件不计入 mass_kill（只 session dead）。
- 代理注意：Go `net/http` 默认读 `HTTP_PROXY/HTTPS_PROXY` 环境变量；本机无持久化代理变量（仅 `NO_PROXY`），webhook 直连。README 注明如需走代理自行设环境变量。

---

## 3. M3 — `session_dead_fails` 前端渲染（P0）

API 已透出（`Status.SessionDeadFails`，JSON `session_dead_fails`，entry.go:114 无 omitempty），前端补渲染：

- `renderAccounts` 状态列（app.js:380-391）：`!disabled && sdf > 0` → 在主 tag 后追加橙色小 tag `session ${sdf}/3`（tooltip：离自动禁用还差 N 次连续判死）；`disabled` 时该行已由 M1 接管，不重复显示。
- 目的：余额 401 求证 / chat 裸 401 计数中的"亚健康"账号一眼可见，赶在满 3 禁用前人工干预（手动保活一次成功即清零）。

---

## 4. M4 — auths 目录租约锁（P1，防同目录双实例）

### 4.1 威胁与边界

防的是**同一个 auths 目录被两个执行体同时挂**（双进程同机、目录被网络盘/复制后原目录仍被另一台机使用）。防不了"凭证副本分处两个独立目录"（A/B 机各自 auths 的导出导入副本）——那由 M6 外部轮换检测抓。设计里两者互补，README 写清。

### 4.2 设计

新包 `internal/authlease`：

- 租约文件 `auths/.lease.json`（auth_dir 下；`auth.LoadDir` 的 glob 是 `workbuddy*.json`，`.lease.json` 不匹配，不会被当账号加载——已核实 auth.go:468）：
  ```json
  { "hostname": "DESKTOP-XXX", "pid": 5496, "started_at": "RFC3339", "heartbeat_at": "RFC3339" }
  ```
- `Acquire(dir string, allowStale bool) (release func(), err error)`，main.go 在加载 auths **之前**调用：
  1. 无文件 → 写入获取；
  2. 文件存在：`heartbeat_at` 距今 > 90s → 判 stale（崩溃遗留），WARN 后覆盖获取；
  3. hostname 与本机相同 → WARN「检测到同机实例租约（可能是双进程或残留），已接管」后覆盖获取（同机双进程由 M6 兜底；不阻断是为了不破坏计划任务重启路径）；
  4. hostname 不同且 heartbeat 新鲜 → **ERROR + 拒绝启动（exit 1）**；`auths.strict_lease=false` 时降级为 WARN 放行（逃生门）。
- 心跳：获取成功后 goroutine 每 30s 重写 `heartbeat_at`（与 main ctx 绑定）；release（main defer + ctx.Done）删除文件。写失败仅 WARN（租约是尽力而为的防线，不因它杀服务）。
- 时钟容忍：两端时钟偏差 90s 窗口内可接受（本方案不做跨机时钟同步假设，宁可放过不可误杀）。

### 4.3 配置

```json
"auths": { "strict_lease": true }
```
（`auth_dir` 已是顶层字段，本段只管租约策略；进 `restartRequiredFields`——main.go:577-601 清单追加。）

### 4.4 测试要点

单测（t.TempDir）：无文件获取 / stale 覆盖 / 同机覆盖 / 异机新鲜拒绝 / `strict_lease=false` 降级 / release 删除 / LoadDir 不吞 `.lease.json`（回归）。

---

## 5. M5 — 保号任务时点抖动（P1，消齐射特征）

### 5.1 设计

- 配置：`schedule.jitter_minutes`，**默认 20**（0 = 关闭，回到现状）。默认开启的理由：现状"22:00:00 十七号同一秒齐刷 refresh"正是被批量吊销的高危特征；抖动无任何功能代价。
- 每账号偏移（稳定可复现、跨天变化、跨号分散）：
  ```go
  // internal/scheduler/jitter.go
  func offset(uid string, day string, maxMin int) time.Duration {
      h := fnv32(uid + "|" + day)               // day = cst.Now() 的 YYYY-MM-DD
      return time.Duration(h % uint32(maxMin*60)) * time.Second
  }
  ```
- 调度侧拆分两条路径：
  - `runBatch`（scheduler.go:407-436）里五类任务（checkin/travel/activity/keepalive/blackcat）改调新的 `runXxxScheduled(ctx)`：对每账号 `time.AfterFunc(offset(uid, day, J), func(){ ctx 存活则执行单账号逻辑 })`，fire-and-forget（不 wg.Wait——runBatch 收尾本无强语义）。派发时打一行日志（如 `keepalive 546****2: 延迟 13m02s 后执行`）。
  - 手动按钮（`checkin_all` / `keepalive_all` 等）仍走现有 `RunXxxNow()`（同步、立即、无抖动）——面板点击语义是"现在就做"。
- 单账号逻辑从 `RunCheckinNow`/`RunKeepaliveNow` 等函数体中抽出为 `checkinOne(st)` / `keepaliveOne(st)` 供两条路径复用，避免复制逻辑。
- 实现细节：AfterFunc 回调里每号独立执行，天然并发，注意 `RunKeepaliveNow` 现有顺序循环即无锁依赖（依赖 Pool/Upstream 自身并发安全），抽函数无新增竞态；`-race` 不可用（本机无 gcc），靠单测 ×10 + 锁序审查。
- growth 任务（GrowthHook 队列）不抖（已是串行队列消费）。

### 5.2 测试要点

单测：offset 稳定性（同 uid+day 恒等）/ 范围 `[0, J*60s)` / 跨号分散度（17 uid 在 20 分钟桶上的分布不聚堆，断言最大重叠 ≤ 阈值）/ `jitter_minutes=0` 时 Scheduled 与 Now 等价路径。

---

## 6. M6 — RT 轮换审计 + 外部轮换检测（P1，抓异地双活）

### 6.1 设计

- **签名改造**：`upstream.Client` 的 `RefreshToken(a)` / `RefreshTokenIfNeeded(a, within)` 追加 `source string` 参数（调用点全仓库仅 5 处：scheduler.go:590 keepalive、handler.go:936-950 chat、balance_401.go:47 求证、cmd/signin/main.go:76、新增 probe）。`refresh()` 成功写回与失败（`ErrSessionDead`）两处调用注入的观察者：
  ```go
  c.SetRotationObserver(func(uid, oldRT8, newRT8, source string, err error))
  ```
- **审计器 `internal/rotaudit`**：
  - 播种：main 装配后 `rec.Seed(pool)`——对池内每号记录 `cur[uid] = sha256(rt)[:8]`（指纹，不落明文）。
  - `OnRotate(uid, old8, new8, source, err)`：
    1. 追加 JSONL `data/refresh-audit-<YYYYMMDD>.log`：`{"ts","uid","old_rt8","new_rt8","source","pid","host","err"}`；启动时清理 > 7 天的旧文件（复用 reqlog 按天切惯例）。
    2. **外部轮换检测**：`cur[uid]` 非空且 `old8 != cur[uid]` → 本进程持有的 RT 已被**别的执行体**换过（B 机副本 / signin.sh / 任何进程外刷新）→ ERROR 级日志「external token rotation detected」+ 调 `alertmgr`（M2）以 `event:"external_token_rotation"` 外发 webhook（受同一 cooldown 约束）+ 该事件也计入横幅（`mass_kill` 结构扩展 `external_rotations` 计数，或横幅独立第二行——取后者，避免语义混淆）。
    3. 更新 `cur[uid] = new8`。
  - 误报分析：进程内单飞已保证同进程只有一个 leader 消费同一 RT；播种后任何 old ≠ cur 必然来自进程外。两副本互检（双方都报）符合预期。
- 落盘失败（审计日志写不进）：WARN，不阻断刷新主流程。

### 6.2 测试要点

单测：JSONL 格式与按天切 / 播种后 old≠cur 触发外部检测 / 正常轮换不触发 / 指纹不含明文 RT（长度断言）/ 观察者 nil 时零开销（反射或行为断言）。

---

## 7. M7 — 智能保活 + 失败退避（P2）

### 7.1 动机与阈值

上游 RT 恒 60 天滚动（`expiresIn=5184000`，client.go:1082），现状每天全量强制刷一次 = 17 次/天，其中 ~97% 是不必要的上游调用，且正是齐射特征。改为按需：

- 配置：`schedule.keepalive_mode`: `"daily"`（默认，现状语义，兼容）| `"smart"`；`schedule.smart_keepalive_days`: 14（阈值）。
- smart 语义：每日 keepalive 时点触发时，仅对 `a.NeedsRefresh(14d)` 为真（即 `ExpiresAt - now < 14d`）的账号强制刷新，其余跳过（汇总一行日志：`smart keepalive: 3 刷新 / 14 跳过（RT 距过期 >14d）`）。60 天窗口 + 14 天阈值 ≈ 每号 46 天刷一次，17 号日均 0.4 次（降 ~97%），且各号 ExpiresAt 天然错峰。宁松勿紧：留 4 倍余量吸收失败退避链。
- **失败退避**（smart/daily 通用，修"一轮失败要等 24h"的断档）：刷新失败（`ErrSessionDead` 之外的网络/5xx 类）进进程内退避表 `kaBackoff[uid]{nextAt, tries}`，序列 30m → 2h → 6h → 次日兜底；12153 判死**不退避**（死号重试无意义，走既有 3 次计数）。驱动：并入 `StartBalanceRefresh` 循环（scheduler.go:645-679）每轮先 `driveKeepaliveBackoff()`（到期项逐号刷新，同样受 M5 抖动的单号偏移保护——退避项直接执行即可，本就零散）。
- 手动 `keepalive_all` 不受 smart 跳过与退避影响（全量强制，运维兜底）。
- 热应用：并入 `Reconfigure`（main.go:557-563 已热应用 schedule 段，扩展两个字段）。

### 7.2 测试要点

单测：`NeedsRefresh` 边界（14d±1s）/ smart 跳过与命中 / 退避序列与封顶 / 12153 不进退避 / daily 模式行为不变（回归）。

---

## 8. M8 — 禁用号低频探活（P2，默认关闭）

- 配置：`schedule.probe_dead_hours`: `[]`（默认空 = 关闭；`validateScheduleHours` 同校验）。
- `RunProbeDeadNow`：仅 `disabled && dead_kind=="session_dead"`（**排除 banned**）；逐号间 `sleep 5–11s` 随机间隔；`RefreshToken(a, "probe")`：
  - 成功（上游误吊销恢复 / 租约复活）→ `ClearSessionDead + ReviveDisabled + SaveAtomicRetry` + WARN「死号复活」+ M2 webhook `event:"dead_recovered"`；
  - 失败 → 静默（debug 级），次日再探。
- 新端点 `POST /panel/api/probe_dead_all`（手动触发，便于验收与"重登前先探一下"）；面板放「账号池」工具栏，仅存在死号时可见。
- 默认关闭理由：对已判死账号的周期性探测请求本身是上游可观测特征；由用户显式权衡开启（CHANGELOG/README 写明）。

---

## 9. 配置 / API / 前端变更汇总

### 9.1 `config.example.json` 增量（三段）

```json
"schedule": { "...现有...": 0,
  "jitter_minutes": 20,
  "keepalive_mode": "daily",
  "smart_keepalive_days": 14,
  "probe_dead_hours": []
},
"notify": { "webhook_url": "", "mass_kill_accounts": 3, "mass_kill_window_minutes": 10, "cooldown_minutes": 30 },
"auths": { "strict_lease": true }
```

热应用矩阵：schedule 全段热生效（Reconfigure 扩展）；notify 热生效（SetConfig）；`auths.strict_lease` 重启生效（进 restartRequiredFields）。

### 9.2 API 变更

| 变更 | 位置 |
|---|---|
| `overview.accounts[].dead_kind`（新增，omitempty） | pool Status / statusOf |
| `overview.mass_kill`（新增，null 或 {count, window_minutes, uids, first_at, last_at, external_rotations}） | panel.go overview |
| `POST /panel/api/probe_dead_all`（新增） | panel 路由表 |

无破坏性变更；`login/start`、`login/poll` 原样复用。

### 9.3 前端变更（index.html / app.js）

| 变更 | 类型 |
|---|---|
| `#killBox` 批量判死横幅 + `renderKillBox` | 横幅（同 #queueBox 型） |
| `#accFilterSt` 状态筛选下拉 + accFilter.st | 筛选 |
| 死号行「待重登/上游封号」tag + 「重登」按钮 + `#reloginVeil` 弹窗 + `openRelogin/pollRelogin` | 账号池 |
| 状态列 `session n/3` 橙 tag | 账号池 |
| 配置页：notify 段 / jitter / keepalive_mode / smart_days / probe_hours 表单项 | 配置页（CFG_MAP） |
| 「探测死号」按钮（仅有死号时显示） | 账号池工具栏 |

全部新 id 过 `TestIndexHTMLUniqueIDs`；app.js 过 `node --check`；CSP 断言不回归。

---

## 10. 实施顺序与工作量

按四个内聚提交推进（每个提交 `go build ./... && go vet ./... && go test ./... -count=1` 全绿）：

| 提交 | 内容 | 预估 |
|---|---|---|
| ① | M3 + M1（可见性 + 重登；后端仅 dead_kind） | 0.5–1 天 |
| ② | M2（禁用 hook + alert 包 + webhook + 横幅 + 配置页） | 1 天 |
| ③ | M6 + M4（refresh source 签名 + rotaudit + authlease） | 1–1.5 天 |
| ④ | M5 + M7 + M8（抖动 + smart 保活 + 探活 + probe_dead_all） | 1.5–2 天 |
| ⑤ | 沙箱验收 + UI 真实点击走查 + 发布（四文档 + tag + Release） | 1 天 |

发布版本：`1.16.0-panel`（`cmd/server/main.go` appVersion），tag `v1.16.0`，主题「会话失效恢复与防复发」。

## 11. 验收清单

1. **单测**：§1–§8 各模块测试要点全落地，`go test ./... -count=1` 全绿；`frontend_test`（语法/CSP/UniqueIDs）通过。
2. **沙箱**（惯例 `:7871`，17 号副本、调度全关、`include_disabled_in_tasks=true`）：
   - 死号副本走 `probe_dead_all` → 12153 静默；伪造"复活"（mock 上游 refresh 成功）→ Revive + webhook；
   - 手动触发三个号禁用 → `mass_kill` 横幅出现 + webhook 收到（httptest/本地接收端）；
   - smart 模式下 keepalive 只刷快过期号；抖动日志时间戳分散。
3. **生产演练 + UI 真实点击走查**（chrome-devtools 真实鼠标事件，非 JS click）：
   - 6 个真死号逐个：筛选「待重登」→ 点「重登」→ 弹窗打开授权 URL → 人工完成 OAuth → 面板复活为可用（`walk` 完整路径）；
   - `session n/3` tag 在亚健康号上可见；`#killBox` 触发与自然消退；`#reloginVeil` 中"登录了另一个账号"分支（用一个不同账号实测一次）。
4. **发布四文档门禁**：`CHANGELOG.md`（中文，按本设计逐模块）+ `README.md` 中英双语（新增：防双活部署规约"一号一实例"、notify 配置、smart 保活说明）+ `HANDOFF.md`（本次变更 + 待办）+ 版本常量；annotated tag `v1.16.0: 会话失效恢复与防复发（一键重登、批量判死告警、RT 轮换审计、智能保活）`；推送后核验 GitHub Release 标题与正文合规。

## 12. 风险与取舍记录

- **M5 默认开抖动**改变"整点完成"的运维体感（任务日志时间戳分散到 20 分钟内）——CHANGELOG 显著说明，`jitter_minutes:0` 可回退。
- **M7 默认 daily**（兼容优先），README 推荐生产开 smart；开 smart 后每日刷新日志行数骤减属预期。
- **M6 改 refresh 签名**（5 调用点）回归面小，但 `cmd/signin` 同步改；`signin.sh` 与服务并跑仍会被外部轮换检测点名（这正是目的）。
- **M4 租约对"同机双进程"只 WARN 不阻断**（保护计划任务重启路径），由 M6 兜底——两道防线职责在 README 写明。
- **M8 默认关**：探死请求本身是特征，交由用户权衡。
- **webhook 明文含 uid/昵称**：属本机自部署面板自有数据，与导出功能同敏感级；文档提示勿指向公网未加密端点。
