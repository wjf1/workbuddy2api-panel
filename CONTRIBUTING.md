# 贡献指南（CONTRIBUTING）

感谢你愿意为 `workbuddy2api-panel` 贡献代码。本文件说明开发环境、代码规范与提交流程；开始前请先读 [`README.md`](README.md) 了解项目定位与使用声明。

> ⚠️ **本项目的边界**：仅供个人自用账号（签到 / 保活 / 个人工具接入）。**不接受**以下方向的贡献——批量小号分发额度、卡密 / 收费售卖、二次加壳打包。详见 [使用声明](README.md#使用声明必读)。

---

## 1. 开发环境

| 项 | 要求 |
|---|---|
| Go | ≥ 1.27（与 `go.mod` 的 go directive 对齐） |
| Node | 可选，仅 `internal/panel` 的前端 DOM 守卫测试需要（`node --check`）；本机已装 node 时必须跑 |
| cgo | 不需要。Windows 本机无 gcc，`-race` 不可用（`CGO_ENABLED=1` 会因缺 gcc 失败）；CI / Linux 可跑 `go test -race ./internal/panel/` |
| 代理 | 首次拉依赖建议 `GOPROXY=https://goproxy.cn,direct` |

### 常用命令

```bash
go build ./...                                  # 编译全集
go test ./... -count=1                          # 全量测试（CI 同款）
go test ./internal/server/ -count=1             # 单包
gofmt -l ./cmd ./internal                       # 格式门禁（CI 会红）
bash scripts/gen-versioninfo.sh && go build -ldflags "-s -w" -o wb2api.exe ./cmd/server   # 生产构建（先注入 PE 版本资源，治国产杀软误报）
bash scripts/build-dev.sh                       # 开发期构建（输出到 build/，不剥符号，规避杀软误报）
```

> **杀软误报规避**：不要把临时构建产物输出到 `%TEMP%`（「临时目录新 exe」是杀软高危画像），也不要在开发构建里带 `-s -w`（剥符号是恶意软件典型对抗手法，误报权重最高）。统一走 `scripts/build-dev.sh`。

---

## 2. 代码规范

- **注释与文档用中文**。项目全部注释、CHANGELOG、HANDOFF 均为中文；新增代码保持同一口径。
- **`gofmt` 必须零输出**。CI 有 gofmt 检查；注意 go1.22 / 1.27 对同行注释的分歧——**注释必须独立成行**。
- **每个提交只做一件事**。大改拆成多个可独立回滚的提交。
- **错误处理不留静默失败**：吞错的地方必须有注释说明为什么可以吞，或至少 `logx.Warnf` 留痕。
- **日志走 `internal/logx`**（分级门面），不要直接用标准库 `log`（`internal/scheduler` 等既有例外保持原样即可）。
- **文件写入走 `internal/safefile`**（原子写 tmp + rename），不要裸 `os.WriteFile` 写状态文件。
- **时间按 CST（Asia/Shanghai）自然日**，用 `internal/cst`，不要用 `time.Now()` 直接比日期（UTC 容器会漂 8 小时）。
- **新增 API 端点**必须在 `README.md` 的 [API 端点](README.md#-api-端点)表登记，并补对应测试。
- **新增配置项**必须：① 进 `config.example.json` 与 `cmd/server/config.go` 的 `Default()`；② 在 README [配置说明](README.md#配置说明)表登记；③ 说明是否热生效。
- **禁止引入新的重量级依赖**。本项目刻意保持单二进制零外部服务依赖（Redis 为可选），新增依赖需在 PR 中说明必要性。

### 前端（`internal/panel`）

- 前端是 `go:embed` 单文件（`index.html` + `app.js`），无构建链。
- **HTML `id` 必须全局唯一**：`internal/panel/frontend_test.go` 的 `TestIndexHTMLUniqueIDs` 会强制校验。新增 `id` 前先 grep 全文查重。
- DOM 断言必须校验**祖先链**（`closest()` / `contains()`），不能仅凭 `getElementById` 存在性判定——元素重名时这类断言必然假阳性。
- 新增或修改用户可见交互时，需用真实浏览器走一遍关键路径（打开 → 选择 → 提交 → 结果），纯脚本 `click()` 不算验收。

---

## 3. 提交规范

采用 **Conventional Commits + 中文描述**：

```
<type>(<scope>): <中文摘要>

<可选正文：为什么这么改、根因是什么、验证数据>
```

**type**：`feat` / `fix` / `docs` / `chore` / `refactor` / `test` / `perf` / `ci`
**scope**（常用）：`panel` / `server` / `pool` / `scheduler` / `upstream` / `docs` / `ci` / `release`

示例：

```
fix(server): 上游 200 + error 帧不再把限流号记成健康
feat(panel): 账号池新增「需处理」筛选
docs(handoff): 更新 1.21.0 交付记录
```

---

## 4. 文档同步（强制）

代码变更必须同步文档，**PR 自检会逐项核对**：

| 变更类型 | 必须同步 |
|---|---|
| 任何代码变更 | `CHANGELOG.md` 的 `[Unreleased]`（Keep a Changelog 六类：新增 / 变更 / 弃用 / 移除 / 修复 / 安全） |
| 功能、界面、配置变更 | `README.md`（+ `README.en.md`）同分支修订 |
| 每次交付 | `HANDOFF.md` 的 §4（最近变更）与 §5（待办） |
| 架构级变更 | `HANDOFF.md` §1-§3 静态模块 |

---

## 5. 与参照上游的关系

原上游 `linguo2625469/workbuddy2api-panel` 已于 2026-10-21 停止维护。本仓库自 2026-11-10 起把
[`misakano7545/workbuddy2api-panel`](https://github.com/misakano7545/workbuddy2api-panel) 作为**事实参照上游**，规则见 [`docs/UPSTREAM-ALIGNMENT.md`](docs/UPSTREAM-ALIGNMENT.md)：

- **不是 git fork 追踪**：不 rebase、不 merge、**不 cherry-pick**（目录结构与职责切分已分化）。
- **抽取情报**：读它的 commit message，识别「上游接口变更 / 协议正确性」类修复（A/B 类），评估是否需要在本地等价实现。
- **借鉴实现**：确有价值的模块（如风控连坐保护）**按本地架构重写**，不直接复制。
- 判为必跟的修复落地后，在 `docs/UPSTREAM-ALIGNMENT.md` §6「已对齐记录」留一行。

---

## 6. PR 流程

1. Fork 并创建分支（`feat/xxx`、`fix/xxx`）。
2. 改代码 + 补测试 + 同步文档。
3. 本地过门禁：`go build ./...`、`go test ./... -count=1` 全绿、`gofmt -l ./cmd ./internal` 零输出、`gitleaks` 无泄漏。
4. 提 PR，按 `.github/PULL_REQUEST_TEMPLATE.md` 逐项填写自检。
5. 维护者审查；涉及行为变更的 PR 需附带验证数据（测试用例、真机验证记录或对账结果）。

**安全相关漏洞请勿开公开 issue**，按 [`SECURITY.md`](SECURITY.md) 的私密通道上报。

---

## 7. 免责与许可

贡献即表示同意以 [MIT License](LICENSE) 授权你的代码。请勿在 PR 中提交任何真实账号凭证、`auths/*.json`、token 或包含个人信息的日志。
