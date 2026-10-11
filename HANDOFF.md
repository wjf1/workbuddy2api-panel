# HANDOFF — 接力开发文档

> 本文档供跨会话 / 跨 Agent / 多人协同开发时快速建立上下文。新会话接手本项目时，**先读本文件**，再按需查阅 `README.md` 与 `CHANGELOG.md`。

## 1. 项目概况与当前状态

- **定位**：把腾讯 CodeBuddy（WorkBuddy）账号变成 OpenAI 兼容 API 的**多账号网关**，附 Web 管理面板。本项目是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的增强 fork（上游已删库，本分支独立演进）。
- **当前版本**：`1.14.0-panel`（`cmd/server/main.go` 的 `appVersion` 常量，透出到 `/panel/api/overview`）。
- **仓库**：`https://github.com/wjf1/workbuddy2api-panel`，本地工作区 `F:\AI\workbuddy2api-panel`（2026-10-11 新克隆），分支 `main`。历史工作区 `F:\AI\Qdor\review\workbuddy2api-panel` 已废弃。
- **最近 tag**：`v1.14.0`（`v1.14.0: 账号回收站、标签管理、余额预警与定时备份`），历史 tag 到 `v1.13.4`。
- **1.14.0 状态**：代码、测试、真实点击走查、四文档均已完成并推送 GitHub，Release 已发布。

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
  import.go            POST /panel/api/import/cockpit —— cockpit tools 账号导入（cockpitAccount 结构含 tags，毫秒→秒）
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

## 4. 最近一轮变更与交付成果（1.14.0-panel）

- **账号回收站（软删除）**：`accountRemove` 改为凭证移入 `data/trash/`（mtime=删除时刻），回收站弹窗恢复（规范名移回 auths/ + `Pool.Add`+`Revive`）/ 彻底删除 / 清空；`StartTrashLoop` 每小时按 `trash.retention_days`（默认 30，0=永久）清过期。同 uid 重复删除加时间戳共存；`moveFile` rename 跨盘失败回退 copy+delete；未注入 TrashDir 的装配回退硬删（测试覆盖）。
- **账号标签（tags）**：`auth.Auth` 加锁保护 `tags` 字段（`TagsValue/SetTags/NormalizeTags`，归一化 16×32 上限），落盘嵌套形 `account.tags`（空不写键，旧文件兼容；扁平形顶层 tags 也可读）；`POST /panel/api/accounts/{uid}/tags` 全量覆盖；overview 透出 `tags` 映射；导出/导入对齐 cockpit `tags` 字段；前端徽章 + 标签/关键字双维筛选（`accFilter`，仿 `mdFilter` 模式）。
- **余额预警**：overview 现算 `low_credit`（`credits < pool.credit_floor`，0/0 未查询账号跳过），前端 `#alertBox` 横幅（仿 `#expBox`），与选号保底共用阈值、零上游请求、零新增配置。
- **定时备份**：`RunBackupNow` 打包 `auths/*.json` + `state.json`（打包前 `Pool.Flush()`；**不含 config.json**）为毫秒文件名 zip；`StartBackupLoop` 10 分钟粒度检查「整点命中且当日未跑」；`SetBackupSettings` 热改（enabled/hour/keep）；`backup/list`、`backup/{name}/download`（Content-Disposition）；keep 轮转按文件名序删最旧。
- **配置**：`trash.retention_days`、`backup.enabled/hour/keep` 四键（Default+normalize+面板表单+热应用，`saveConfig` 增加 `pn *panel.Panel` 参数）。
- **验证**：`go test ./... -count=1` 全绿（新增 tags_test.go、trash_backup_test.go 共 10 个用例；TestIndexHTMLUniqueIDs / TestAppJSSyntax 通过）；chrome-devtools 真实点击走查全通过（预警横幅渲染与联动、标签编辑/徽章/筛选/往返、移除→恢复→彻底删除、手动备份→列表→下载、配置页四项保存持久化）。

## 5. 接力开发指引与待办（Next Steps / Backlog）

- **可演进项**：
  - 导出接口可加「仅导出健康账号 / 排除禁用号」过滤参数（当前按池内全量 + uids）。
  - cockpit 的 `plan_type` / `checkin_streak` 等运维字段面板仍无数据源，导出省略（`tags` 已于 1.14.0 补齐）；若后续新增这些维度，在 `cockpitExportAccount` 补 `omitempty` 字段即可。
  - README 已有顶部英文概览段；完整双语可参照根 `README.md` 章节新建 `README.en.md`。
  - `-race` 在 Windows 本机不可用，可考虑 CI 增加 Linux race job。
  - 备份恢复目前是手工解压覆盖（README/CHANGELOG 说明）；可考虑面板一键恢复（需处理覆盖前安全备份）。
  - 回收站可加「按标签批量恢复」等批量运维（数据结构已支持）。
- **新会话切入路径**：改回收站/备份 → 先看 `trash.go` / `backup.go` 头注释（目录推导、mtime 语义、毫秒文件名）→ 跑 `go test ./internal/panel/ -run 'Trash|Backup'`；改标签 → `auth.go` 的 `SetTags/NormalizeTags` 与 `export.go/import.go` 的对齐注释；改前端 → 注意 `frontend_test.go` 的 CSP、内联脚本与 id 唯一性断言。

## 6. 关键避坑与运行约束

- **auth.Auth 并发读**：token / domain / expires / tags 等字段会被刷新线程或面板写回，**必须**走带锁访问器（`AccessTokenValue` / `RefreshTokenValue` / `DomainValue` / `TagsValue`）或 `Lock()/Unlock()` 成组读；直读字段是数据竞争。`export.go` 的 `cockpitAccountFromAuth` 是标准示例。
- **毫秒 vs 秒**：cockpit tools 口径 `expires_at` 为**毫秒**；本面板落盘为 **Unix 秒**。导入（`import.go`）÷1000，导出（`export.go`）×1000，改任何一侧都要同步。
- **uids 过滤语义**：`export/cockpit` 的 `uids` 全部未命中返回**空数组**（非 4xx、非回落全量），前端据此 toast 提示；改动该语义需同步 `export_test.go`。
- **auth 文件扁平形字段是 camelCase**：手写 / 造测试账号时写 `accessToken` / `refreshToken` / `expiresAt` / `uid` / `nickname`（见 `auth.go` Parse）；写成 snake_case 会静默解析失败（启动日志 `loaded 0 account(s)`），排查先看这行。
- **pool.Add 不落盘**：`upsertLocked` 不标记 dirty——state.json 要等首次状态变更（签到/余额/选号）才写。依赖「Add 后 state.json 必存在」的测试或备份逻辑需先触发一次状态变更（如 `SetCreditsDetailed`）或接受 zip 里无 state（`writeBackupZip` 已容错）。
- **回收站/备份目录必须在持久化卷**：main 按 `state_file` 同目录推导（`data/trash`、`data/backups`），Docker compose 已挂 data/ 卷；若自定义 state_file 位置，确认同目录持久化，否则容器销毁丢回收站与备份。
- **备份刻意不含 config.json**（内含 api_key 明文）；恢复 = 手工解压覆盖 auths/ 与 state.json。改此设计前重新评估密钥泄露面。
- **app.js 是嵌入资源**：Go 编译器不检查其语法，`frontend_test.go` 用 `node --check` 兜底（无 node 时 skip）——改完前端务必让该测试跑一次（本机已装 node）。
- **CSP 严格策略**：面板禁止内联 `<script>` 与内联事件；新交互逻辑一律写进 `app.js`，按钮用 `id` + `addEventListener`（或 `.onclick`）绑定。
- **id 全局唯一是硬门禁**：新增 HTML `id` 前先查重（1.13.4 事故），`TestIndexHTMLUniqueIDs` 会拦但别依赖事后修。
- **测试环境**：本机（Windows）无 gcc → `-race` 不可用；`panel.New(Config{...})` 起面板测试无需网络，`Upstream` 为 nil 时**不可**调用会触达上游的 handler（如 `importCockpit` 的签到/激活分支），否则 nil panic；回收站/备份/overview 测试用 `trash_backup_test.go` 的 `newTrashPanel`（真池 + 真目录）。走查用临时服务进程注意：TaskStop 停后台 bash 包装不会杀掉它启动的 exe 子进程——先 `taskkill /F /IM wb2api*.exe` 再重启用同端口。
- **Windows 本机代理**：git 固化了 `http.proxy=127.0.0.1:7900`，代理未启动时 `git clone/fetch` 会失败，用 `git -c http.proxy= -c https.proxy= <cmd>` 临时直连；`curl` 直连 `api.github.com` / `raw.githubusercontent.com` 通常可用。
