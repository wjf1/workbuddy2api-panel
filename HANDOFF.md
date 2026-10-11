# HANDOFF — 接力开发文档

> 本文档供跨会话 / 跨 Agent / 多人协同开发时快速建立上下文。新会话接手本项目时，**先读本文件**，再按需查阅 `README.md` 与 `CHANGELOG.md`。

## 1. 项目概况与当前状态

- **定位**：把腾讯 CodeBuddy（WorkBuddy）账号变成 OpenAI 兼容 API 的**多账号网关**，附 Web 管理面板。本项目是 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 的增强 fork（上游已删库，本分支独立演进）。
- **当前版本**：`1.13.0-panel`（`cmd/server/main.go` 的 `appVersion` 常量，透出到 `/panel/api/overview`）。
- **仓库**：`https://github.com/linguo2625469/workbuddy2api-panel`，本地工作区 `F:\AI\Qdor\review\workbuddy2api-panel`，分支 `main`。
- **最近提交 / tag**：`9478287 chore: 版本号 1.12.0-panel`；历史 tag 到 `v1.9.2`（tag 命名与 `appVersion` 演进不同步，以 `appVersion` 为准）。
- **本工作区未推送**：`1.13.0-panel` 的导出功能为本地变更，尚未 commit / push（见第 4、5 节）。

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
cmd/server/            网关与面板装配入口（appVersion 常量、config 加载、依赖注入）
internal/panel/        Web 面板（单一职责：可视化运维）
  panel.go             Panel 结构、路由表 routes()、withAuth 鉴权中间件、overview
  import.go            POST /panel/api/import/cockpit —— cockpit tools 账号导入（cockpitAccount 结构，毫秒→秒）
  export.go            GET  /panel/api/export/cockpit —— 账号池导出为 cockpit 兼容数组（秒→毫秒），与本文件互为逆操作
  index.html / app.js  前端单页（go:embed）；按钮 id 约定：btnAdd / btnExport / btnRefresh
  *_test.go            含 frontend_test（app.js 语法校验、CSP 断言）、security_test（鉴权/响应头）、export_test
internal/pool/         账号池调度：Add/List/AuthByUID/CountsDetailed、熔断冷却、状态落盘
internal/auth/         凭据模型 Auth（落盘 auths/workbuddy-<uid>.json）；token 字段读取必须走带锁访问器
internal/upstream/     CodeBuddy/WorkBuddy 上游 API 客户端（签到/余额/任务/刷新）
internal/server/       OpenAI 兼容网关（/v1/chat/completions 等）
internal/scheduler/    定时任务（签到/保活/旅行/任务）
config.json            运行配置（listen / api_key / panel …），面板可在线改（热生效）
```

- **数据流**：OAuth 登录或 cockpit 导入 → `auth.Auth` 落盘 `AuthDir` → `Pool.Add` 热加载 → 调度器 / 网关经 `Pool.AuthByUID` 取凭据。
- **单一事实来源**：版本号 = `cmd/server/main.go appVersion`；面板字段契约 = `cockpitAccount`（导入口径）与 `cockpitExportAccount`（导出口径），两侧字段均对齐 cockpit-tools 的 `WorkbuddyAccount` serde 序列化。

## 4. 最近一轮变更与交付成果（1.13.0-panel）

- **新增账号池导出能力（cockpit tools 兼容）**：
  - 后端 `internal/panel/export.go`：`GET /panel/api/export/cockpit?uids=a,b`，字段契约见文件头注释；`uids` 未命中不回落全量；`Content-Disposition` 附件头；日志只记条数不落 token。
  - 路由注册于 `panel.go` routes()（紧邻 `import/cockpit`，同挂 `withAuth`）。
  - 前端 `index.html` 顶部「导出」按钮 + `app.js` 下载逻辑（Blob 下载 `workbuddy_accounts_<日期>.json`，401 时弹密钥框）。
  - 版本号 bump 至 `1.13.0-panel`；README 三处、CHANGELOG 新建。
- **测试验证数据**：`go build ./...` 通过、`go vet` 通过、新增 5 组导出测试全过（鉴权 401/200、空池数组、字段契约含毫秒转换、uids 过滤、导出→`[]cockpitAccount` 回环）、`go test ./... -count=1` 全绿（panel 包 14.7s，含 app.js 语法校验）。
- **外部兼容性依据**：cockpit-tools（jlcodes99/cockpit-tools，Rust/Tauri 开源）`crates/cockpit-core/src/modules/workbuddy_account.rs` 的 `export_accounts`（导出即 `Vec<WorkbuddyAccount>` 纯数组）与 `import_from_json`（第 2 分支直接吃账号数组）——本实现的导出格式与两者双向兼容。

## 5. 接力开发指引与待办（Next Steps / Backlog）

- **推送前待办**（本工作区尚未提交）：核对 `go.mod` 既有本地改动（非导出功能引入，内容为依赖调整）是否一并提交；按仓库流程打 annotated tag（建议 `v1.13.0: 面板 cockpit 兼容账号池导出`，subject 与 Release 标题一致）并走四文档核验 → push → GitHub Release 检查。
- **可演进项**：
  - 导出接口可加「仅导出健康账号 / 排除禁用号」过滤参数（当前按池内全量 + uids）。
  - cockpit 的 `tags` / `plan_type` / `checkin_streak` 等运维字段面板暂无数据源，导出省略；若后续面板新增这些维度，可在 `cockpitExportAccount` 补 `omitempty` 字段，cockpit 侧均为 Option 可直接吃。
  - README 目前仅中文；如需中英双语版，参照根 `README.md` 章节新建 `README.en.md`（仓库暂无英文版）。
  - `-race` 在 Windows 本机不可用，可考虑 CI 增加 Linux race job。
- **新会话切入路径**：改导出/导入字段 → 先看 `export.go` / `import.go` 头注释（与 cockpit `WorkbuddyAccount` 的对齐关系）→ 跑 `go test ./internal/panel/ -run Export`；改前端 → 注意 `frontend_test.go` 的 CSP 与内联脚本断言。

## 6. 关键避坑与运行约束

- **auth.Auth 并发读**：token / domain / expires 等字段会被刷新线程写回，**必须**走带锁访问器（`AccessTokenValue` / `RefreshTokenValue` / `DomainValue`）或 `Lock()/Unlock()` 成组读；直读字段是数据竞争（`auth.go` 头注释有 `-race` 实证说明）。`export.go` 的 `cockpitAccountFromAuth` 是标准示例。
- **毫秒 vs 秒**：cockpit tools 口径 `expires_at` 为**毫秒**；本面板落盘为 **Unix 秒**。导入（`import.go:94`）÷1000，导出（`export.go`）×1000，改任何一侧都要同步。
- **uids 过滤语义**：`export/cockpit` 的 `uids` 全部未命中返回**空数组**（非 4xx、非回落全量），前端据此 toast 提示；改动该语义需同步 `export_test.go`。
- **app.js 是嵌入资源**：Go 编译器不检查其语法，`frontend_test.go` 用 `node --check` 兜底（无 node 时 skip）——改完前端务必让该测试跑一次（本机已装 node）。
- **CSP 严格策略**：面板禁止内联 `<script>` 与内联事件；新交互逻辑一律写进 `app.js`，按钮用 `id` + `addEventListener`（或 `.onclick`）绑定。
- **测试环境**：本机（Windows）无 gcc → `-race` 不可用；`panel.New(Config{...})` 起面板测试无需网络，`Upstream` 为 nil 时**不可**调用会触达上游的 handler（如 `importCockpit` 的签到/激活分支），否则 nil panic——导出 handler 无上游依赖，可直接测。
- **Windows 本机代理**：git 固化了 `http.proxy=127.0.0.1:7900`，代理未启动时 `git clone/fetch` 会失败，用 `git -c http.proxy= -c https.proxy= <cmd>` 临时直连；`curl` 直连 `api.github.com` / `raw.githubusercontent.com` 通常可用。
