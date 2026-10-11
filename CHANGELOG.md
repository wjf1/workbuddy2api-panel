# 更新日志（CHANGELOG）

本仓库从 `1.13.0-panel` 起维护本变更日志；更早版本的历史请查阅 Git tag（`v1.9.2` 及以前）与提交记录。

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循语义化版本（`<主>.<次>.<修订>-panel` 后缀表示 fork 面板增强分支）。

## [1.14.0-panel] - 2026-10-11

### 新增

- **账号回收站（软删除）**：面板「移除账号」不再硬删凭证文件，改为移入 `data/trash/`（Docker 持久化卷内），可在回收站弹窗**恢复**（移回 `auths/` 并热加载进池）或**彻底删除**（二次确认）；超期自动清理（`trash.retention_days`，默认 30 天，`0` = 永久保留）。删除时刻记于文件 mtime，不引入额外元数据文件；同 uid 重复删除自动加时间戳共存；跨盘移动回退 copy+delete。未注入回收站目录的旧装配保持既有硬删行为（零回归）。
- **账号标签（tags）**：账号可打多个标签（面板「标签」弹窗，逗号分隔，归一化 trim/去重/限量 16×32 字符），落盘 auth 文件 `account.tags`（旧文件兼容），账号行徽章展示 + 列表按标签/关键字筛选；cockpit 导出/导入格式对齐 cockpit-tools 的 `tags` 字段（`Option<Vec<String>>`），双向迁移不丢分组信息。
- **余额预警（面板内横幅）**：账号积分低于「积分保底线」（`pool.credit_floor`）时，账号池页顶部横幅列出触底账号与剩余分值——与选号侧保底共用同一阈值，零新增配置、零上游请求（overview 现算缓存值）；积分与总额均为 0（从未查询）的账号不误报。
- **定时备份**：每日整点（`backup.hour`，默认凌晨 3 点）把 `auths/*.json` + `state.json` 打包为 zip 落 `data/backups/`（**不含 config.json**，避免 api_key 明文扩散），保留 `backup.keep` 份（默认 7）轮转；文件名毫秒精度 + 冲突自动外推重命名（手动连点 / CI 高速循环不会同名覆盖，且文件名字典序恒等于时间序）。回收站弹窗内提供「立即备份 / 备份列表 / 下载」（Content-Disposition 附件，便于把备份拿出容器；列表按完整 mtime 新→旧排序）。

### 变更

- **配置**：`config.example.json` 与配置页新增 `trash.retention_days`、`backup.enabled` / `backup.hour` / `backup.keep` 四项，均热生效（`panel.SetTrashRetention` / `SetBackupSettings`）；旧配置缺键走默认值，无感升级。
- **overview 接口**：`GET /panel/api/overview` 新增 `tags`（uid→标签）、`credit_floor`、`low_credit`（触底账号列表）三个纯增量字段。
- **新增 API**：`GET /panel/api/trash`、`POST /panel/api/trash/{name}/restore|purge`、`POST /panel/api/trash/purge_all`、`POST /panel/api/backup/run`、`GET /panel/api/backup/list`、`GET /panel/api/backup/{name}/download`、`POST /panel/api/accounts/{uid}/tags`。回收站/备份文件名均做白名单校验（`workbuddy-*.json` / `wb2api-backup-*.zip`，纯文件名，防路径穿越）。

### 验证

- `go test ./... -count=1` 全绿；新增 `internal/auth/tags_test.go`（归一化 / 落盘往返 / 旧文件兼容 / 变更检测）与 `internal/panel/trash_backup_test.go`（软删→恢复→彻底删全链路、同名冲突共存、过期清理、保留期热改、备份 zip 条目 / keep 轮转 / 下载与穿越拒绝、overview 预警与标签透出、旧装配回退硬删与 501）；`TestIndexHTMLUniqueIDs`（id 全局唯一门禁）与 `TestAppJSSyntax`（node --check）通过。
- 真实浏览器点击走查（chrome-devtools，真实账号数据）：预警横幅渲染与出池联动、标签编辑→徽章→双维筛选、移除（新确认文案）→回收站恢复（标签随文件往返）→彻底删除、手动备份→列表出现→下载按钮、配置页四项回填与保存持久化，全部通过。

## [1.13.4-panel] - 2026-10-11

### 修复

- **导出弹窗列表不可见（用户实测阻断级）——`id="expList"` 与既有元素重名**：面板账号页原本就有隐藏的 `#expBox` 盒子，内含 `#expList`（`index.html:669`）。导出弹窗（1.13.1 起）也用了 `id="expList"`，而 `getElementById` 恒取文档顺序第一个——于是弹窗的账号行被渲染进了**页面主区域那个旧容器**（被弹窗遮罩盖住）：用户看到空列表、点击无效，但计数/全选等所有逻辑"正常"（4/4），且所有基于 `getElementById` 的自动化断言全部假阳性。修复：弹窗内 `#expList` 改名 `#expAccList`（`index.html` + `app.js` 5 处引用），与既有元素彻底解耦。
- **回归防御**：`frontend_test.go` 新增 `TestIndexHTMLUniqueIDs`——解析 index.html 全部 `id="…"` 断言唯一，把"id 冲突导致 JS 静默写错容器"这类事故前移到 CI（本次事故的正确性教训：DOM 层级断言必须验证祖先链，仅凭 getElementById 内容断言会假阳性）。

### 排查记录

- 1.13.2 的缓存修复与 1.13.3 的 DOM API 渲染加固方向正确但未触及本根因；真网关 + 真数据 + 真实鼠标点击复现后，经 `elementsFromPoint` / 祖先链检查定位（点击被遮罩拦截、列表祖先链不含弹窗）。
- 真网关全流程实操验证（4 账号、真实点击）：弹窗内列表渲染（祖先链含 `#exportVeil`）、单选/双选计数联动（已选 2/4）、导出请求 `?uids=<2 个选中 uid> → 200` 浏览器下载触发。

## [1.13.3-panel] - 2026-10-11

### 修复

- **导出弹窗改用 DOM API 逐元素创建 + 关键样式内联**（终极兼容方案）：用户环境曾稳定复现"4 个账号已进 DOM（计数 4/4）但整行不可见"的渲染错乱，而同一份代码与数据在标准 Chrome 实例上完全正常——判定为浏览器环境（插件 / 扩展改写 DOM 或缓存层）与 `innerHTML` 字符串拼接的兼容性问题。改为：
  - `createElement` + `textContent` 逐元素构建账号行（账号昵称 / uid 不再经 HTML 字符串，顺带消除注入面）；
  - `display:flex` / 复选框尺寸 / 间距等**关键样式以内联 style 写入**，任何全局 CSS、浏览器插件、样式改写都无法覆盖（CSP 已允许内联样式）；
  - 行保留 `min-height:14px` 兜底，状态徽标为固定枚举 innerHTML。

### 验证

- `go test ./internal/panel/` 全绿（app.js 语法校验 / CSP 断言）。
- 4 账号实例浏览器实测：列表 139px、行内样式生效（computed style `display:flex`、行高 34px）、默认全选 4/4、单选取消 → 3/4、导出请求 `?uids=…→ 200`、console 无 JS 错误。

## [1.13.2-panel] - 2026-10-11

### 修复

- **面板静态资源增加 `Cache-Control: no-cache`**（`index.html` 与 `app.js` 响应）：此前无任何缓存头，浏览器会启发式强缓存 app.js；html 与 js 是同版本强耦合的两份独立文件（弹窗 DOM 与交互逻辑分属两处），升级网关后浏览器可能继续用旧脚本拼新页面，出现半新半旧的错乱界面（如导出弹窗列表空白）。改为 no-cache 协商校验后，网关更新前端即生效，无需用户强刷。
- **导出弹窗健壮性加固**（`app.js` `openExport`）：账号列表渲染全程 try/catch，任何异常都会在列表区显示明确错误文案（不再退化为空白条）；`uid` / `nickname` 全部防空（`String(... || '')`）；渲染失败时"导出选中"按钮保持禁用。
- **弹窗样式兜底**：加载/错误态固定最小高度与居中文案（`.exp-loading`），空列表不再是一条细线；复选框显式尺寸（`.exp-check` 14px + accent-color），防全局样式干扰；列表容器 `min-height` 兜底。

### 验证

- `go test ./internal/panel/` 全绿（app.js 语法校验 / CSP 断言 / 鉴权 / 回环）。
- 4 账号独立实例浏览器实测（禁缓存重载）：列表 4 行渲染、行高与复选框尺寸正常（computed style 断言）、单选导出请求 `?uids=…`（3 个 uid）→ 200、console 无 JS 错误（仅 favicon 404，无害）、响应头含 `Cache-Control: no-cache`。

## [1.13.1-panel] - 2026-10-11

### 新增

- **导出弹窗：账号勾选（单选 / 多选 / 全选）**
  - 面板顶部「导出」按钮由"直接全量导出"改为打开**导出选择弹窗**（`index.html` 新增 `exportVeil`，`app.js` 新增 `openExport` / `expSync` / `expSetAll` 交互）。
  - 弹窗内按账号逐行列出昵称、uid、积分与运行状态（可用 / 冷却中 / 已暂停 / 已禁用），复选框勾选，默认全选。
  - 头部「全选 / 全不选」开关与行勾选双向联动，实时显示「已选 N / M」计数。
  - 「导出选中」按钮：全选时走全量导出（不带 `uids` 参数），部分选中时按选中 uid 拼接 `?uids=` 过滤导出；成功后自动关闭弹窗并 toast 提示导出条数。
  - 后端无需改动：复用 1.13.0 的 `GET /panel/api/export/cockpit?uids=` 过滤语义。

### 验证

- `go test ./internal/panel/` 全绿（含 app.js 语法校验、CSP 断言）。
- 独立测试实例（临时端口 + 双假账号）浏览器实测：弹窗默认全选、行勾选联动计数、头部全选/全不选切换、单选导出请求 `?uids=test-uid-2 → 200`、全选导出请求不带 `uids → 200`、成功后弹窗自动关闭。

## [1.13.0-panel] - 2026-10-11

### 新增

- **面板「导出」按钮（cockpit tools 兼容账号导出）**
  - 新增接口 `GET /panel/api/export/cockpit`（`internal/panel/export.go`）：把账号池导出为 cockpit tools 的 WorkBuddy 账号 JSON 格式（**纯数组**，字段与 cockpit `WorkbuddyAccount` 的 serde snake_case 序列化一致：`id` / `uid` / `nickname` / `access_token` / `refresh_token` / `token_type` / `expires_at`（毫秒）/ `domain`）。
  - 支持 `?uids=uid1,uid2` 选择性导出；`uids` 全部未命中时返回空数组（不静默回落全量，防止「想导单个却拿到整池」的误操作）。
  - 过期时间自动做 **秒 → 毫秒** 转换（本面板落盘为 Unix 秒，cockpit 口径为毫秒）；无效值省略字段，两侧导入均有兜底。
  - 响应带 `Content-Disposition: attachment` 附件头；日志只记录导出条数，**凭据不落日志**。
  - 与既有 `POST /panel/api/import/cockpit` 互为逆操作：导出体可直接再导入本面板，也可经 cockpit tools 的 WorkBuddy「JSON 导入」回灌其账号库（已对照 cockpit-tools 源码 `import_from_json` 验证兼容）。
  - 前端：顶部工具栏新增「导出」按钮（`index.html` + `app.js`），点击后经鉴权请求接口并触发浏览器下载 `workbuddy_accounts_<日期>.json`，toast 提示导出条数。
- **测试**：`internal/panel/export_test.go` 新增 5 组测试——鉴权（无 key / 错 key 一律 401）、空池合法空数组、字段契约（秒→毫秒 / id 前缀 / token_type / omitempty）、`uids` 过滤（子集 / 未命中 / 全量）、导出→导入结构回环。

### 变更

- 版本号 `1.12.0-panel` → `1.13.0-panel`（`cmd/server/main.go`）。
- README：Web 面板功能表、架构表与「添加账号」章节补充 cockpit 兼容导入 / 导出说明。

### 安全

- 导出接口挂在 `withAuth` 之后（与导入接口同级鉴权，`api_key` 非空时必须 Bearer 认证）；测试覆盖 401 / 200 两态。
