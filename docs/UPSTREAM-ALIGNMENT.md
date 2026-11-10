# 上游对齐与迭代路线（UPSTREAM-ALIGNMENT）

> 本文档是 `workbuddy2api-panel` 在**上游停止维护后**的演进基线。
> 新会话接手时先读 `HANDOFF.md` 建立当前状态，再读本文档决定下一步做什么。

## 1. 背景：我们为什么需要一份「参照上游」

原上游 `linguo2625469/workbuddy2api-panel`（4771⭐）已于 **2026-10-21 停止维护**，
其停止维护声明写明三个原因：上游禁反代、维护者精力有限、非法转卖。这意味着：

- 本仓库**没有可合并的上游**，WorkBuddy 接口变更、错误码新增、协议行为变化
  全部要自己发现、自己修；
- 一旦上游改了某个响应形状而我们没跟上，故障是**静默的**（不报错、只是行为错）。

因此自 2026-11-10 起，把 **`misakano7545/workbuddy2api-panel`** 作为
**事实上的参照上游（reference upstream）**：

| 项 | 值 |
|---|---|
| 仓库 | `https://github.com/misakano7545/workbuddy2api-panel` |
| 定位 | 同源 Go fork，当前最活跃（日更多提交） |
| 本地参照检出 | `F:\AI\_upstream-ref\misakano`（`git clone --depth 50`） |
| 代码规模 | 177 个非测试 Go 文件 / ~62k 行 |
| Star | 1029（2026-11-10） |

**关系声明：不是 fork 追踪，而是「变动情报源 + 可借鉴实现」。**
我们不 rebase 到它上面（代码结构已分化），只做两件事：

1. **抽取情报**：读它的 commit message，识别「上游接口变更 / 协议正确性」类修复，
   评估是否需要在本地等价实现；
2. **借鉴实现**：对确有价值的模块（如风控连坐保护），按本地架构重写而非直接复制。

---

## 2. 上游 commit 归因表（判断「跟不跟」的唯一依据）

`misakano7545` 的 commit 大致四类。**只有前两类必跟——不跟就是静默失效。**

| 类型 | 典型 commit | 判断 | 理由 |
|---|---|---|---|
| **A. 上游接口变更修复** | `d78131ba` 资料接口空昵称不再静默成功<br>`f987bacc` 11151 归请求级<br>`afb5f224` 一个 data 行塞多个 JSON 时拆分 | 🔴 **必跟** | 上游改了形状，不跟=跑着跑着坏掉 |
| **B. 协议正确性** | `d7ffe424` 缺函数名不把已发工具调用算进去<br>`771c887f` `<think:SUF>` 拆回 thinking<br>`e8df35b6` DSML 文本不再触发续写 | 🔴 **必跟** | 影响所有客户端 |
| **C. 自家特性** | `592fb01a` 额度窗口探针、号池分区<br>`18a67761` 运行日志落盘+选号策略可配 | 🟡 按契合度 | 与本仓定位可能不同 |
| **D. 纯 UI 修补** | `cfac3703` 单选框不被撑开 | 🟢 可跳过 | 无功能影响 |

### 同步节奏与操作

**每两周一次**，按以下步骤抽样（不要全量读，成本不可控）：

```bash
cd /f/AI/_upstream-ref/misakano
git pull --depth 50 origin main          # 需代理
git log --oneline -60                     # 只看 subject，不读 diff
# 命中 A/B 两类的，再单独看：
git show <sha> --stat                     # 先看动了哪些文件
git show <sha> -- <相关文件>              # 再看具体实现
```

判定为本仓需要的，**按本地架构重写**（不直接 cherry-pick——目录结构与职责切分已不同），
并在本文档 §6 的「已对齐记录」留一行。

---

## 3. 能力差距清单（2026-11-10 实测）

### 3.1 上游有、本仓无的独立包

| 上游包 | 作用 | 本仓判断 |
|---|---|---|
| `proxypool` | 每账号出口代理 + 订阅导入 | ❌ 不做（用户已明确排除） |
| `media` | 图片去水印后处理 | ❌ 不做（无多模态计划） |
| `updater` | 自更新通道（代理/镜像/失败归因） | 🟡 可选项 |
| `balance` | 余额定时采样供面板趋势图 | ✅ 本仓已有等价实现 |
| `credithist` | 积分变动留痕 | ⚠️ 本仓 `usage.json` 部分覆盖 |
| `loginsite` | 登录站点表 / 协议常量 | ✅ 值得参考（对齐上游域名变化） |
| `jsonscan` | 原始 JSON 字节取字段，不解析全树 | ✅ **性能优化，可借鉴** |
| `debugcap` | 一次性取证落盘 | ⚠️ 诊断用 |
| `probe` | 真机测量工具 | ❌ 工具类，非运行时 |

### 3.2 上游有、本仓无的 server 层能力（按价值排序）

| 文件 | 行数 | 作用 | 优先级 |
|---|---|---|---|
| `restrict.go` | 83 | **11140 风控连坐保护** | 🔴 R1 |
| `responses.go` | 1763 | Codex Responses 入站适配 | 🔴 R2 |
| `continue.go` | 784 | 截断自动续写，多段流拼成连续 SSE | 🟠 R2 |
| `starved.go` | 198 | 思考吃光预算的救援重发 | 🟠 R2 |
| `toolonly.go` | 195 | 说一句就停但该动手时的安全续写 | 🟠 R2 |
| `narration.go` | 186 | 预告句收尾的救援续写 | 🟠 R2 |
| `upstreamhealth.go` | 167 | 上游「劣化但不报错」滑窗告警 | 🟡 R3 |
| `watchdog.go` | 123 | 上游目录改版监控 | 🟡 本仓已有类似 |
| `budget.go` | 198 | 积分闸（日累计+突发+低缓存率） | 🟡 本仓 `credit_floor` 部分覆盖 |
| `admin_audit.go` | 164 | `/admin` 操作 JSONL 审计 | 🟡 R3 |
| `admin_tasks.go` | 69 | 手动补跑排程端点 | 🟢 R3 |
| `busy.go` | 91 | 模型忙预检端点 | 🟢 R3 |
| `embeddings/images/videos/search/fetch/mcp` | 大 | 多模态与工具 | ❌ 明确不做 |

### 3.3 本仓有、上游无的独占能力（保持）

- **不安全绑定写保护**（`cmd/server/insecure_write_guard.go`，1.19.3）：监听非回环 +
  空 `api_key` 时写方法一律 405。国内外同类项目几乎无人做。
- `authlease` 跨机租约锁、`rotaudit` RT 轮换审计、`websearch` 服务端检索、
  `logx` 分级日志门面、`safefile` 原子写。

---

## 4. 迭代路线（四轮，按风险收益比排序）

> 用户已明确排除：**P2 多密钥发卡 + IP 白名单**、**P4 多后端存储**。
> 排除理由不写进待办，若将来需求变化再单独立项。

### R1 安全底线（最高优先，做完即可发版）

| 序 | 项 | 说明 | 验收 |
|---|---|---|---|
| R1-1 | ~~`restrict.go` 移植~~ ✅ **已完成 2026-11-10** | 11140 连坐保护：命中即停手（不换号）+ 窗口内 ≥2 号被限制→全池暂停 | 8 例行为钉子全过；全量 26 包回归绿 |
| R1-2 | ~~`jsonscan` 借鉴~~ ✅ **已完成 2026-11-10** | 请求体热路径只取少数几个字段，避免整树解析 | 基准 34 倍提速；等价性测试锁死语义一致；27 包回归绿 |

**R1-1 的紧迫性**：本仓现状（实测）为 `handler_error.go:149` 命中
`request illegal` 后 `Pool.Disable(uid)` 但**继续轮转**，`MaxRotate` 默认 3。
上游 `restrict.go` 注释记录了实测事故——hub #422「一次任务 7 个号全封」。
本仓最坏情况是单请求连撞 3 个号，方向一致、风险同源。

### R2 能力补齐

| 序 | 项 | 说明 |
|---|---|---|
| R2-1 | ~~`responses.go`~~ ✅ **已完成 2026-11-10** | `POST /v1/responses` 复用 chat 主链路；13 例测试 + 真实上游端到端（非流式/流式各验） |
| R2-2 | **续写救援**（部分完成） | ✅ `starved`（非流式路径，2026-11-10 完成，10 例测试 + 真实上游验证）<br>⬜ `continue` / `narration` / `toolonly`（需读取器级改造，见下） |

**R2-2 背景**：本仓 1.19.3 部署验收时实测到 `content='' / finish=length` /
`reasoning` 吃满预算。当时判断为 `max_tokens` 过小，但上游这四个文件说明
**这是系统性上游行为**，对 Codex / Claude Code 这类连续执行任务的客户端是致命的
（工具调用被吞 → agent 断在那里）。

**R2-2 施工判断（2026-11-10）**：四件套中 `continue`（784 行）/ `narration` /
`toolonly` 全部挂在参照上游的 `continueReader` 上——那是一个把多段上游流拼接成
单条客户端可见 SSE 的读取器，本仓的流式路径没有该结构，整体移植等于重写流式层，
风险与收益不成比例。因此本轮**只做 `starved`**（独立性强、走非流式聚合路径、
不触碰流式管线），并对它做了更严格的安全边界（每请求一次 / 失败即原结果 /
额度不放大 / 只认 deepseek 系）。

后续若要做流式续写，建议按「先建 `continueReader` 等价物 → 再接四件套」分两步，
而不是直接抄四件套（它们没有单独可用的形态）。

### R3 运维手感

| 序 | 项 | 说明 |
|---|---|---|
| R3-1 | ~~`admin_audit.go`~~ ✅ **已完成 2026-11-10** | 管理操作 JSONL 审计（`logging.admin_audit_path`，缺省关闭，7 例测试含"绝不落 key 明文"） |
| R3-2 | ~~`admin_tasks.go`~~ ✅ **已完成 2026-11-10** | 手动补跑 6 任务，202 + 并发 409 + panic 隔离（6 例测试） |
| R3-3 | ⬜ `upstreamhealth.go` | 上游「劣化但不报错」滑窗告警——**延后**（本仓已有 `alert` 包 + webhook 冷却，需求部分覆盖；等出现真实劣化场景再做） |
| R3-4 | ⬜ `busy.go` | 模型忙预检端点——**延后**（上游 6004/14003 已有分类与模型级冷却，独立预检端点收益低） |

### ❌ 明确不做（记录决策，避免反复讨论）

| 项 | 理由 |
|---|---|
| 多密钥发卡 + IP 白名单（P2） | 用户明确排除 |
| 多后端存储 file/sqlite/pg（P4） | 用户明确排除；现有原子写 + `.bak` + 租约锁已足够 |
| 生图 / 生视频 | 场景用不上，引入上游新端点逆向，长期负债 |
| 每账号出口代理 / 设备指纹派生 | 自用场景过度设计，上游行为持续变化 |
| 七平台矩阵（Trae/Qoder/豆包） | 广度换深度，非本仓定位 |
| Electron 桌面 App | 本仓定位是服务端网关 + Web 面板 |
| Playwright 破验证 | 投入产出比极差 |

---

## 5. 每轮的统一验收标准（不可跳过）

1. `go build ./...` / `go vet ./...` 干净；
2. `gofmt -l ./cmd ./internal` 为空（CI 有门禁）；
3. `go test ./... -count=1` 全绿（当前基线：26 包）；
4. 新增能力必须有**行为钉子测试**（不是覆盖率凑数，是锁死预期行为）；
5. 涉及用户可见交互的，做**真实浏览器点击走查**（脚本断言不算验收）；
6. 四文档同步：`CHANGELOG.md`（`[Unreleased]` 或新版本段）、`README.md`（若涉及功能/配置）、
   `HANDOFF.md`（§4 动态 + §5 待办）、`AGENTS.md`（若涉及构建/约定）；
7. 打 Tag 才触发四文档齐备门禁；日常推送不触发，但 README 需同分支修订。

---

## 6. 已对齐记录

| 日期 | 上游来源 | 本仓实现 | 说明 |
|---|---|---|---|
| 2026-11-10 | `misakano7545/internal/server/restrict.go` | `internal/server/restrict.go` + `upstream.IsRestrictionMarker` | **R1-1 完成**：命中即停手 + 窗内 2 号被限暂停全池；配置 `upstream.restriction_hold_seconds`（默认 1800，0=关闭）；8 例行为钉子 |
| 2026-11-10 | `misakano7545/internal/jsonscan/` | `internal/jsonscan/`（整包移植 + 等价性钉子 + 基准） | **R1-2 完成**：接入 `session.ResolveConversationID` 与 `upstream.InjectPromptCacheKey` 快路径。实测 200KB body：**5.7μs vs 195μs（34 倍）**，单请求省 ~190μs；未命中/畸形 body 回落整树解析，语义零变化 |
| 2026-11-10 | `misakano7545/internal/server/starved.go` | `internal/server/starved.go` | **R2-2（部分）完成**：思考吃光预算的救援重发。仅非流式路径；安全边界比参照更严（每请求一次、失败即原结果、额度不放大、仅 deepseek 系）。真实上游验证 100/200/400/800 全部救回 |
| 2026-11-10 | `misakano7545/internal/server/responses.go` | `internal/server/responses.go` + `responses_response.go` | **R2-1 完成**：`POST /v1/responses` 适配层。与 Anthropic 层同构（转换型 ResponseWriter 复用 chatCompletions，不复制调度）。支持 input 字符串/数组、instructions、function_call 历史项、内置工具剥离、流式具名事件。13 例测试 + 真实上游端到端（非流式 `text='正常'`、流式 12 事件顺序合规）。**本仓实现比参照更简洁**（参照 1763 行含工具/续写联动，本仓先做协议正确性子集） |
