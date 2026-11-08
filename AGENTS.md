# AGENTS.md — workbuddy2api-panel

## 项目概览

CodeBuddy 账号池转 OpenAI 兼容 API 网关 + Web 管理面板（fork 增强分支，版本带 `-panel` 后缀）：
多账号调度、签到、积分、模型限流分类、用量统计，面板嵌入 Go 二进制（`internal/panel` 前端 + `app.js`）。
当前版本 **v1.19.2-panel**（2026-11-04；版本常量 `appVersion` 在 `cmd/server/main.go`，CI 由它提取）。

## 构建与测试命令

Go >= 1.22.5（go.mod），运行时依赖仅 `redis/go-redis/v9`。以下命令来自 `go.mod` 生态标准、HANDOFF 与 `.github/workflows/go-binaries.yml`：

| 命令 | 作用 |
|---|---|
| `go build -ldflags "-s -w" -o wb2api.exe ./cmd/server` | 构建生产二进制（CI 同款） |
| `go test ./...` | 全量测试（含 `internal/panel/frontend_test.go` 的 DOM 守卫：`node --check` + `TestIndexHTMLUniqueIDs`，无 node 时 skip——本机已装 node 必须跑） |
| `gofmt -l .` | 格式门禁（CI 有 gofmt 检查，go1.22/1.27 对同行注释分歧：注释必须独立行） |
| `docker-compose up` | Docker 部署（data/ 卷持久化）；另有 docker-ghcr.yml 出镜像 |

CI（go-binaries.yml）：多平台 zip/tar.gz + checksums + 自动 Release。注意其 Release 标题/正文不合规范，推 tag 后必须 `gh release edit <tag> --title "vX.Y.Z: 中文摘要" --notes-file <对齐 CHANGELOG 的正文>`（本仓既有惯例）。

## 代码风格约定

- Go 标准布局：`cmd/server`（入口）、`internal/`（auth/pool/panel/usage/anthropic 等）；面板 HTML/JS 是嵌入资源，交互逻辑一律写 `app.js`，CSP 禁内联 `<script>` 与内联事件。
- 中文注释与文档；CHANGELOG（Keep a Changelog，`-panel` 后缀语义）、HANDOFF（六模块）随代码同分支维护。
- 涉及账号结构的语义变更必须同步对应测试断言（import/export/classify 均有钉死用例）。
- 前端新增/修改 HTML `id` 前先 grep 全文档查重（1.13.4 事故教训，`TestIndexHTMLUniqueIDs` 会拦但别依赖事后修）。

## 提交与发布规范

- Conventional Commits + 中文描述，scope 常用 `panel:` / `docs:` / `docs(handoff):` / `chore(release): 1.19.2-panel` / `fix(ci):` / `ci:`。
- 发布走 annotated tag：`git tag -a vX.Y.Z -m "vX.Y.Z: <中文核心摘要>"`（tag 版本取 `-panel` 前的主版本号）；推送后按上述惯例修正 CI Release 标题与正文。

## 项目专属注意

1. **auth.Auth 并发读必须走带锁访问器**（`AccessTokenValue` / `DomainValue` / `TagsValue` 等）或 `Lock()/Unlock()` 成组读——token 会被刷新线程写回，直读字段是数据竞争（标准示例 `export.go` 的 `cockpitAccountFromAuth`）。
2. **前端三重守卫**：`app.js` 是嵌入资源，Go 编译器不查其语法（靠 frontend_test.go 的 `node --check` 兜底）；id 全局唯一；改前端后让该测试真实跑一次。DOM 排查顺序：祖先链 → `elementsFromPoint` 遮挡 → 最后才怀疑 CSS/脚本。
3. **上游 429 有两族语义，判定靠业务码不是状态码**：`6004` 模型级限流（可切模型）、`6020/6021` 免费档排队（与账号模型均无关）；排队码落进 429 兜底会烧穿全池（实案 106 秒 16/17 号）。改 `Classify` 必须保持「14018 硬积分 → 6020/6021 排队 → 通用 429」先后序并同步 TestClassify 反例。
4. **清理测试进程只按精确镜像名**（如 `taskkill /F /IM wb2api-test.exe`），**禁止 `wb2api*.exe` 通配**——会连带杀掉生产 `wb2api.exe`。

接力开发优先阅读 `HANDOFF.md`（§6 关键避坑约 20 条）与 `CHANGELOG.md`。
