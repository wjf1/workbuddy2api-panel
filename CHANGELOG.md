# 更新日志（CHANGELOG）

本仓库从 `1.13.0-panel` 起维护本变更日志；更早版本的历史请查阅 Git tag（`v1.9.2` 及以前）与提交记录。

格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，版本号遵循语义化版本（`<主>.<次>.<修订>-panel` 后缀表示 fork 面板增强分支）。

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
