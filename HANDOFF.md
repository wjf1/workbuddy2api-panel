# HANDOFF — 接力开发文档

> 本文档供跨会话 / 跨 Agent / 多人协同开发时快速建立上下文。新会话接手本项目时，**先读本文件**，再按需查阅 `README.md` 与 `CHANGELOG.md`。

## 1. 项目概况与当前状态

- **定位**：把腾讯 CodeBuddy（WorkBuddy）账号变成 OpenAI 兼容 API 的**多账号网关**，附 Web 管理面板。本项目是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的增强 fork（上游已删库，本分支独立演进）。
- **当前版本**：`1.15.0-panel`（`cmd/server/main.go` 的 `appVersion` 常量，透出到 `/panel/api/overview`）。
- **仓库**：`https://github.com/wjf1/workbuddy2api-panel`，本地工作区 `F:\AI\wb2api-src`（2026-10-13 当前开发目录；旧交接文档写的 `F:\AI\workbuddy2api-panel` 已不存在），分支 `main`。
- **最近 tag**：`v1.15.0`（`v1.15.0: Anthropic Messages 兼容层与服务端 web_search`）；历史 `v1.14.2`（导入格式兼容升级）、`v1.14.1`（导出导入往返完整性修复）、`v1.14.0`（账号回收站、标签管理、余额预警与定时备份）。
- **本地生产网关**：`F:\AI\wb2api-panel-v1.13.0-windows-amd64\`（目录名是历史遗留，实际内容随更新推进），Windows 计划任务 `WorkBuddy2API`（SYSTEM、开机自启、失败重启）跑 `start-gateway.cmd`；更新惯例 = 停任务 → `wb2api.exe.pre-<新版本>.bak` 备份 → 替换官方 Release exe → 起任务（停/启需管理员，走 PowerShell `-Verb RunAs` + UAC，`schtasks /End|/Run` 在 Git Bash 需 `MSYS_NO_PATHCONV=1`）。另有一处 `:7863` 实例（`F:\AI\wb2api`，含 `auths/` 14 个账号）在同机运行，改生产前先确认到底哪套在对外服务。

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
internal/websearch/    so.com 检索抓取解析（Searcher 接口、Result、PageAge 规范化；data-mdurl 取真实 URL）
internal/httpauth/     验签原语（VerifyBearer / VerifyAny：Bearer + x-api-key 双凭证、常量时间比较）
internal/scheduler/    定时任务（签到/保活/旅行/任务/余额刷新）
config.json            运行配置（listen / api_key / trash / backup / pool …），面板可在线改（热生效）
data/trash/            回收站（state 文件同目录推导；Docker 挂载卷内）
data/backups/          备份输出（同上）
```

- **数据流**：OAuth 登录或 cockpit 导入 → `auth.Auth` 落盘 `AuthDir` → `Pool.Add` 热加载 → 调度器 / 网关经 `Pool.AuthByUID` 取凭据；移除 → `Pool.Remove` + 凭证移入 `data/trash/`。
- **单一事实来源**：版本号 = `cmd/server/main.go appVersion`；面板字段契约 = `cockpitAccount`（导入口径）与 `cockpitExportAccount`（导出口径），两侧字段均对齐 cockpit-tools 的 `WorkbuddyAccount` serde 序列化（1.14.0 起含 `tags`）。

## 4. 最近一轮变更与交付成果（1.15.0-panel）

- **Anthropic Messages API 兼容层**：新增 `POST /v1/messages` 与 `POST /v1/messages/count_tokens`（请求侧 `anthropic.go`、响应侧 `anthropic_response.go`）。核心取舍是**不复制调度逻辑**：Anthropic 请求转成内部 OpenAI chat body 后复用既有 `chatCompletions`（挑号 / 轮转 / 超时与 WAF 处置 / 记账），响应用「转换型 ResponseWriter」逐帧改写回 Anthropic 事件序列（流式）或 message 对象（非流式）；错误统一 Anthropic 错误体；非 `web_search` 的服务端工具（`web_fetch_*` / `code_execution_*`）出站前剥离（能力降级而非报错）。
- **服务端 `web_search` 由网关自执行**（`anthropic_websearch.go` + `internal/websearch/`）：客户端声明 `web_search_*` → 网关注入函数工具 → 模型给查询 → 网关抓 so.com 并解析 → 回吐 `server_tool_use` / `web_search_tool_result` 块，同时以 `role:"tool"` 回灌模型继续作答；一轮内多轮检索受 `max_uses`（缺省 8、钳 1..8）约束，预算耗尽**撤工具 + 追加限额提示**；正文没引用来源时补 `Sources:` 列表；非流式路径走多 pass 聚合同样支持。
- **伪工具调用清洗**（`dsml.go`）：两种形态（全角竖线 DSML 标记、`<tool_call>` 伪 XML 调用）在流式与非流式路径一致清洗；流式为增量过滤（字节级锚点 + 跨帧 holdback + 64KB 兜底），`<tool_call>` 只在块内是「工具调用形状 JSON」时丢弃（语法引用 / 普通 JSON 原样保留）。
- **本轮修掉的真实缺陷**（均以单测锁定）：① 整段清洗的未闭合尾规则误吃块后正文（`reDSMLTail` 改用只认起始标记的 `dsmlOpenPattern`）；② 流式锚点漏认闭合标签形态——实测把闭合 `calls` 标签当整段正文下发；③ 逐字节喂入时半截多字节竖线被放行 + `strings.ToLower` 对非法字节的替换导致索引错位（改 `asciiLower`）；④ `<tool_call>` 清洗漏写标签前的前缀，丢掉正文前半段；⑤ 挂起态流结束判定按整段缓冲而非**载荷**判断。
- **鉴权**：`httpauth.VerifyAny`（`x-api-key` 与 `Authorization: Bearer` 任一命中，Anthropic 端点专用；`withAuth` 对 `/v1/messages*` 走 Anthropic 错误体）。
- **配置**：`search.enabled`（缺省 true）/ `search.max_results`（钳 1..10）/ `search.timeout_seconds`（钳 1..60），`config.example.json` 同步补充。
- **验证**：`go build ./...`、`go vet`（server / httpauth / websearch / cmd/server）干净；`go test ./... -count=1` **16 包全绿**（`anthropic_test.go` 21 用例、httpauth `VerifyAny` 11 子用例、websearch 5 用例）。端到端在独立沙箱 `:7866`（隔离 `auths/`、全调度与备份禁用）跑完 6 项主矩阵 + 伪调用泄漏路径 6 采样，全部通过（具体数字见 CHANGELOG 1.15.0「验证」节）。

## 4.1 上一轮（1.14.2-panel）

- **导入格式兼容升级**：`POST /panel/api/import/cockpit` 从严格单形状（`[]cockpitAccount`）升级为**逐文件/逐元素形状嗅探**，直接兼容四类既有产物：嵌套形账号文件（`{"account":{...},"auth":{...}}`，即 auths 落盘口径，**秒**级 `expiresAt`；单对象与数组均支持）、扁平单对象、嵌套形+cockpit 扁平的混合数组、**zip 整包**（内部 `*.json` 自动解包，跳过 `__MACOSX`/`._`/非 json；单坏文件只记 errors 不阻断）；另支持多文件 multipart。实现分层：`parseImportPayload`（首字符 + probe 分派）→ `classifyAccountEntry`（含 `auth`/`accessToken` 键走 `auth.Parse` → `nestedToCockpit` 秒→毫秒映射；否则按 cockpitAccount 反序列化）→ `expandPayloadFile`（zip magic 探测与解包，条目 4MB LimitReader）。错误语义：单负载请求硬错误 400（保留既有文案），多负载降级 `errors` 条目。
- **前端**：「导入 JSON」页签 `accept=".json,.zip"` + `multiple`，提示文案更新；`d.errors` 前 3 条渲染进 `#importErr`（此前仅 console）。
- **验证**：新增 `roundtrip_test.go` 六用例（嵌套单对象/嵌套数组混合/数组坏元素/扁平单对象/zip 整包/形状无法识别 400），既有三用例回归全绿；`go vet` 干净、`go test ./... -count=1` 全绿（15 包）。真实浏览器走查（临时实例 :7864 + 独立 auths）：zip 导入成功 3 个 + 坏文件红字明细（祖先链含 `#addTabImport`）、非法 JSON 明确报错（`weird.json: unrecognized format`）、落盘嵌套形无损。注意：本次走查在 ZCode 内置浏览器（IAB）执行，该后端定位器/坐标点击在此页面持续超时（宿主工具限制），点击经页面内真实 handler 触发；其余链路真实执行。

## 4.2 再上一轮（1.14.1-panel）

- **导出 → 导入往返完整性修复**：边界账号端到端实测发现三缺口——device_token 往返丢失（导出/导入格式补 `device_token` 键 + `auth.DeviceTokenValue()` 访问器）、显式 realm 被 domain 推断搬家（导出补 `realm`，导入显式优先、空回落推断）、零过期回落 +365d（既有兜底设计，测试锁定）。导入的上游副作用加 `Upstream == nil` 守卫（裸装配不 panic）。
- **导出/导入字段契约（1.14.1 起完整清单）**：`id / uid / nickname / access_token / refresh_token / token_type / expires_at(毫秒) / domain / tags / realm(显式) / device_token`——往返全字段无损（正常账号）；`roundtrip_test.go` 锁定。

## 4.3 更早一轮（1.14.0-panel）

- 账号回收站（软删除）/ 账号标签（tags）/ 余额预警横幅 / 定时备份四件套 + `trash.retention_days`、`backup.enabled/hour/keep` 四配置键（详见 CHANGELOG 1.14.0 条目）。

## 5. 接力开发指引与待办（Next Steps / Backlog）

- **Anthropic 兼容层可演进项（1.15.0 新增）**：
  - **「模型只吐了伪调用」时正文会是空的**（模型想用未声明的工具）：当前**有意**保持空正文 + `end_turn`（宁缺勿造，且严格好于之前的乱码泄漏）。若要改善 UX，可在「清洗后正文为空 + 检测到被清洗的伪调用」时合成一句网关提示文本——改之前先想清楚语言选择与「助手没说过的话」的语义边界。
  - `count_tokens` 是无 tokenizer 的估算（请求 JSON 字节数 / 3）；要精确可接 tiktoken 类实现，注意依赖与本机无 cgo 的约束。
  - 检索后端目前只有 so.com（`internal/websearch`，`Searcher` 接口已就位）：可加 Bing / SearXNG 等实现并按 `search.*` 选择；`anthropicTool` 已解析 `allowed_domains` / `blocked_domains` 但**尚未落到过滤**（接了就能用）。
  - `web_fetch_*` / `code_execution_*` 服务端工具目前剥离（能力降级）；如需支持，可参照 websearch 包加 fetch 实现并复用同一编排骨架。
  - 客户端工具与检索同轮混用的语义已固定（检索执行完立即终局、交客户端）：`anthropic_websearch.go` 头注释 + 轮次条件 `len(searchCalls)==0 || len(otherCalls)>0` 是唯一事实来源，改前先读。
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
- **测试环境**：本机（Windows）无 gcc → `-race` 不可用；`panel.New(Config{...})` 起面板测试无需网络，`Upstream` 为 nil 时**不可**调用会触达上游的 handler（如 `importCockpit` 的签到/激活分支），否则 nil panic；回收站/备份/overview 测试用 `trash_backup_test.go` 的 `newTrashPanel`（真池 + 真目录）。走查用临时服务进程注意：TaskStop 停后台 bash 包装不会杀掉它启动的 exe 子进程——用**精确镜像名**清理（如 `taskkill /F /IM wb2api-test.exe`），**不要**用 `wb2api*.exe` 通配（会连带杀掉生产的 `wb2api.exe`）。
- **Windows 本机代理**：git 固化了 `http.proxy=127.0.0.1:7900`，代理未启动时 `git clone/fetch` 会失败，用 `git -c http.proxy= -c https.proxy= <cmd>` 临时直连；`curl` 直连 `api.github.com` / `raw.githubusercontent.com` 通常可用。
- **DSML 标记会「毒化」客户端与文档（1.15.0 实测）**：正文里的全角竖线 DSML 标记会被 Anthropic 协议客户端（ZCode 客户端实测）当成**真实工具调用**解析并触发 `Tool not found: web_search` —— 这正是本层清洗存在的理由。写文档 / commit message / 测试夹具时**不要粘贴该标记原文**（Go 源码用 `dsmlToken = "\uFF5C\uFF5C"` 拼装，见 `anthropic_test.go`），否则自己的输出也会被解析成工具调用。
- **Anthropic E2E 沙箱**：`:7865`（`F:\AI\wb2api-e2e\wb2api-test.exe`，上一轮遗留的旧二进制进程）与 `:7866`（`F:\AI\wb2api-e2e-dev\wb2api-dev.exe`，当前开发用，key `sk-e2e-dev-7866`，`auths/` 是生产账号目录的复制，全调度 / 备份禁用）。测试账号可安全复制：token 刷新是 **rotate 不 revoke**（旧 token 仍有效），CN 账号约 27 天有效期。**改代码后跑 E2E = 重建二进制 + 重启后台任务**；清理进程只按精确镜像名（`taskkill /F /IM wb2api-dev.exe`），**不要用通配**（会连带杀掉生产的 `wb2api.exe`）。
- **中文请求体必须走 UTF-8 文件**：Git Bash 内联 `-d '中文...'` 会按 GBK 编码发出，上游可能解析成乱码或 400；一律写 `.json` 文件 + `--data-binary @file`。
- **CI 发布的 Release 标题 / 正文不合规范**：`.github/workflows/go-binaries.yml` 的 publish 任务用 `name: <tag>-panel` + 通用正文，**推 tag 后必须** `gh release edit <tag> --title "vX.Y.Z: 中文摘要" --notes-file <对齐 CHANGELOG 的正文>`（保留 CI 给的 compare 链接在正文里），这是本仓既有惯例。
