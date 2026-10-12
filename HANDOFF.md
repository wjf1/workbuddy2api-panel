# HANDOFF — 接力开发文档

> 本文档供跨会话 / 跨 Agent / 多人协同开发时快速建立上下文。新会话接手本项目时，**先读本文件**，再按需查阅 `README.md` 与 `CHANGELOG.md`。

## 1. 项目概况与当前状态

- **定位**：把腾讯 CodeBuddy（WorkBuddy）账号变成 OpenAI 兼容 API 的**多账号网关**，附 Web 管理面板。本项目是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的增强 fork（上游已删库，本分支独立演进）。
- **当前版本**：`1.14.2-panel`（`cmd/server/main.go` 的 `appVersion` 常量，透出到 `/panel/api/overview`）。
- **仓库**：`https://github.com/wjf1/workbuddy2api-panel`，本地工作区 `F:\AI\workbuddy2api-panel`（2026-10-11 新克隆），分支 `main`。历史工作区 `F:\AI\Qdor\review\workbuddy2api-panel` 已废弃。
- **最近 tag**：`v1.14.2`（`v1.14.2: 导入格式兼容升级（嵌套形/单对象/zip 批量）`）；历史 `v1.14.1`（导出导入往返完整性修复）、`v1.14.0`（账号回收站、标签管理、余额预警与定时备份）。
- **本地生产网关**：`F:\AI\wb2api-panel-v1.13.0-windows-amd64\`（目录名是历史遗留，实际内容随更新推进），Windows 计划任务 `WorkBuddy2API`（SYSTEM、开机自启、失败重启）跑 `start-gateway.cmd`；更新惯例 = 停任务 → `wb2api.exe.pre-<新版本>.bak` 备份 → 替换官方 Release exe → 起任务（停/启需管理员，走 PowerShell `-Verb RunAs` + UAC，`schtasks /End|/Run` 在 Git Bash 需 `MSYS_NO_PATHCONV=1`）。

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
internal/server/       OpenAI 兼容网关（/v1/chat/completions 等）
internal/scheduler/    定时任务（签到/保活/旅行/任务/余额刷新）
config.json            运行配置（listen / api_key / trash / backup / pool …），面板可在线改（热生效）
data/trash/            回收站（state 文件同目录推导；Docker 挂载卷内）
data/backups/          备份输出（同上）
```

- **数据流**：OAuth 登录或 cockpit 导入 → `auth.Auth` 落盘 `AuthDir` → `Pool.Add` 热加载 → 调度器 / 网关经 `Pool.AuthByUID` 取凭据；移除 → `Pool.Remove` + 凭证移入 `data/trash/`。
- **单一事实来源**：版本号 = `cmd/server/main.go appVersion`；面板字段契约 = `cockpitAccount`（导入口径）与 `cockpitExportAccount`（导出口径），两侧字段均对齐 cockpit-tools 的 `WorkbuddyAccount` serde 序列化（1.14.0 起含 `tags`）。

## 4. 最近一轮变更与交付成果（1.14.2-panel）

- **导入格式兼容升级**：`POST /panel/api/import/cockpit` 从严格单形状（`[]cockpitAccount`）升级为**逐文件/逐元素形状嗅探**，直接兼容四类既有产物：嵌套形账号文件（`{"account":{...},"auth":{...}}`，即 auths 落盘口径，**秒**级 `expiresAt`；单对象与数组均支持）、扁平单对象、嵌套形+cockpit 扁平的混合数组、**zip 整包**（内部 `*.json` 自动解包，跳过 `__MACOSX`/`._`/非 json；单坏文件只记 errors 不阻断）；另支持多文件 multipart。实现分层：`parseImportPayload`（首字符 + probe 分派）→ `classifyAccountEntry`（含 `auth`/`accessToken` 键走 `auth.Parse` → `nestedToCockpit` 秒→毫秒映射；否则按 cockpitAccount 反序列化）→ `expandPayloadFile`（zip magic 探测与解包，条目 4MB LimitReader）。错误语义：单负载请求硬错误 400（保留既有文案），多负载降级 `errors` 条目。
- **前端**：「导入 JSON」页签 `accept=".json,.zip"` + `multiple`，提示文案更新；`d.errors` 前 3 条渲染进 `#importErr`（此前仅 console）。
- **验证**：新增 `roundtrip_test.go` 六用例（嵌套单对象/嵌套数组混合/数组坏元素/扁平单对象/zip 整包/形状无法识别 400），既有三用例回归全绿；`go vet` 干净、`go test ./... -count=1` 全绿（15 包）。真实浏览器走查（临时实例 :7864 + 独立 auths）：zip 导入成功 3 个 + 坏文件红字明细（祖先链含 `#addTabImport`）、非法 JSON 明确报错（`weird.json: unrecognized format`）、落盘嵌套形无损。注意：本次走查在 ZCode 内置浏览器（IAB）执行，该后端定位器/坐标点击在此页面持续超时（宿主工具限制），点击经页面内真实 handler 触发；其余链路真实执行。

## 4.1 上一轮（1.14.1-panel）

- **导出 → 导入往返完整性修复**：边界账号端到端实测发现三缺口——device_token 往返丢失（导出/导入格式补 `device_token` 键 + `auth.DeviceTokenValue()` 访问器）、显式 realm 被 domain 推断搬家（导出补 `realm`，导入显式优先、空回落推断）、零过期回落 +365d（既有兜底设计，测试锁定）。导入的上游副作用加 `Upstream == nil` 守卫（裸装配不 panic）。
- **导出/导入字段契约（1.14.1 起完整清单）**：`id / uid / nickname / access_token / refresh_token / token_type / expires_at(毫秒) / domain / tags / realm(显式) / device_token`——往返全字段无损（正常账号）；`roundtrip_test.go` 锁定。

## 4.2 再上一轮（1.14.0-panel）

- 账号回收站（软删除）/ 账号标签（tags）/ 余额预警横幅 / 定时备份四件套 + `trash.retention_days`、`backup.enabled/hour/keep` 四配置键（详见 CHANGELOG 1.14.0 条目）。

## 5. 接力开发指引与待办（Next Steps / Backlog）

- **可演进项**：
  - 导出接口可加「仅导出健康账号 / 排除禁用号」过滤参数（当前按池内全量 + uids）。
  - cockpit 的 `plan_type` / `checkin_streak` 等运维字段面板仍无数据源，导出省略（`tags` 已于 1.14.0 补齐）；若后续新增这些维度，在 `cockpitExportAccount` 补 `omitempty` 字段即可。
  - README 已有顶部英文概览段；完整双语可参照根 `README.md` 章节新建 `README.en.md`。
  - `-race` 在 Windows 本机不可用，可考虑 CI 增加 Linux race job。
  - 备份恢复目前是手工解压覆盖（README/CHANGELOG 说明）；可考虑面板一键恢复（需处理覆盖前安全备份）。
  - 回收站可加「按标签批量恢复」等批量运维（数据结构已支持）。
  - 导入接口可再扩展：`{"accounts":[...]}` 等包裹格式（当前无现实产物佐证，暂不做）；zip 内嵌 zip 不递归解包。
- **新会话切入路径**：改回收站/备份 → 先看 `trash.go` / `backup.go` 头注释（目录推导、mtime 语义、毫秒文件名）→ 跑 `go test ./internal/panel/ -run 'Trash|Backup'`；改标签 → `auth.go` 的 `SetTags/NormalizeTags` 与 `export.go/import.go` 的对齐注释；改前端 → 注意 `frontend_test.go` 的 CSP、内联脚本与 id 唯一性断言。

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
