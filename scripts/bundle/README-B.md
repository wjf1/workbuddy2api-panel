# wb2api-panel v1.16.0 —— B 机一键部署包（Windows amd64）

> 目标机器：B 机 `CHINAMI-DPNMO04`（也适用于任何 Windows 10/11 x64 全新部署）。
> A 机（`DESKTOP-DVFFB09`）为账号池主力；本包让 B 机随时具备同版本（1.16.0-panel）的部署与接管能力。

## 一键部署（三步）

1. 把整个 bundle 目录（或 zip 解压后）放到 B 机任意目录，建议 `F:\AI\wb2api-bundle\`。
2. 双击 **`install.cmd`**（自动请求管理员；也可在管理员 cmd 里运行并传自定义任务名：`install.cmd 任务名`）。
3. 看到面板地址 + API key 即部署完成。浏览器打开 `http://127.0.0.1:7863/panel/` 开始使用。

## install.cmd 会做什么

| 步骤 | 说明 |
|---|---|
| 注册计划任务 | 任务名默认 `wb2api`，以 `S-1-5-18`（SYSTEM）运行 `wb2api.exe -config config.json`，工作目录 = 部署目录；开机自启（延迟 30 秒）、失败重启 3 次（间隔 1 分钟）、无执行时长限制 |
| 覆盖同名旧任务 | 若已存在同名任务（如 B 机旧 1.15.7 实例）：先停止，并把旧任务定义备份为 `previous-task-<任务名>.xml`，再覆盖注册指向本目录 |
| 清理残留进程 | 按精确映像名 `wb2api.exe` 停止残留进程（**绝不通配**，避免误杀） |
| 首启配置 | 目录下无 `config.json` 时由程序自动生成（`listen :7863` + 随机 `api_key`），脚本最后打印该 key |
| 健康自检 | 等待 `/healthz` 返回 `workbuddy2api`（最多 20 秒），成功/失败都会显示面板地址与后续动作 |

## 账号怎么给 B 机（重要：一号一实例）

- **同一账号的凭证文件只能存在于一台机器**。同一份副本在两台机器上同时运行时，两边各自刷新 refresh token 会互相踩掉（RT 是轮换型的：刷新即作废旧值），上游的 rotation 检测会**连坐吊销整条令牌链**——2026-10-23 六个账号集体失效就是这个形态。
- 推荐做法：**A 机为主力持有全部账号**；B 机部署后保持空池待命，需要时从 A 机面板「导出」→ B 机面板「导入 JSON」只迁移真正要放过去的账号。
- B 机装好后若暂时不当主力：禁用其任务即可（管理员 cmd：`schtasks /change /tn wb2api /disable`，或任务计划程序 GUI），要用时再 `/enable` 并 `schtasks /run /tn wb2api`。
- 1.16.0 的防护会兜底：若两台机器真的同时在跑同一账号，A 机面板会出现「凭证双活」横幅（RT 轮换审计检测，点名具体 uid）——见到即按上述原则归并账号。

## 常用运维

| 动作 | 命令（管理员） |
|---|---|
| 启动 / 停止 | `schtasks /run /tn wb2api` / `schtasks /end /tn wb2api` |
| 开机自启开关 | `schtasks /change /tn wb2api /enable` / `/disable` |
| 卸载服务（保留文件） | 双击 `uninstall.cmd`（任务删除，exe/config/auths/data 全保留） |
| 查看版本 | 面板右上角，或 `curl http://127.0.0.1:7863/panel/api/overview`（带 Bearer） |

## 目录内容

```
wb2api.exe          1.16.0-panel（GitHub Release CI 构建）
install.cmd         一键安装（自提权、幂等可重跑）
uninstall.cmd       卸载服务（保留全部文件）
README-B.md         本说明
config.json         首启自动生成（也可从旧实例拷贝后改）
previous-task-*.xml 安装时自动生成：被替换旧任务的定义备份
```
