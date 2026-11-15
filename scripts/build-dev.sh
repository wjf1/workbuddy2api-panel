#!/usr/bin/env bash
# build-dev.sh —— 开发期本地验证构建（固化杀软误报规避规范）。
#
# 背景：2026-11-12 晚 agent 临时验证构建
# `go build -ldflags "-s -w" -o %TEMP%/wb2api_verify.exe ./cmd/server`
# 被火绒实时防护按 HEUR:VirTool/Obfuscator.a 判毒删除——三个高危信号叠加：
# 剥符号（-s -w）、输出在 %TEMP%（恶意软件最爱的落地目录）、新 hash 零信誉。
# 仓库已有 PE 版本资源注入（scripts/gen-versioninfo.sh）治"无属性裸二进制"
# 画像，本脚本再把其余两个信号固化进开发期构建规范：
#   1. 输出到仓库 build/ 目录，不进 %TEMP%；
#   2. 不带 -s -w，保留符号表与调试信息（体积代价可接受，开发期不影响分发）；
#   3. 构建前无条件重新生成 PE 版本资源（syso 是生成物，appVersion 常量变更后
#      旧 syso 不会自动更新，曾出现构建 FileVersion 滞后于源码版本串的情况；
#      go run 工具首次下载后已入 module cache，重跑开销秒级）；
#   4. 构建后输出大小与 SHA256，供 VirusTotal 验证 / 火绒误报提交直接取用。
#
# 用法：bash scripts/build-dev.sh [输出文件名]   # 默认 wb2api-dev.exe
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_NAME="${1:-wb2api-dev.exe}"
OUT_DIR="$ROOT/build"
OUT="$OUT_DIR/$BIN_NAME"

mkdir -p "$OUT_DIR"

# go run pkg@version 每次都会经 GOPROXY 查询模块（即使已有缓存）——默认直连
# proxy.golang.org 在国内网络会 TCP 超时等待 20~30s 才落到 gen-versioninfo.sh
# 的回落逻辑（见其注释）。此处默认 goproxy.cn，外部可用环境变量覆盖。
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

echo "[build-dev] 重新生成 PE 版本资源（对齐 appVersion）..."
bash "$ROOT/scripts/gen-versioninfo.sh" >/dev/null

echo "[build-dev] 构建（不带 -s -w，保留符号表）..."
cd "$ROOT"
go build -o "$OUT" ./cmd/server

SHA="$(sha256sum "$OUT" | awk '{print toupper($1)}')"
SIZE="$(stat -c %s "$OUT")"
echo "[build-dev] 产物: $OUT"
echo "[build-dev] 大小: $SIZE bytes"
echo "[build-dev] SHA256: $SHA"
echo "[build-dev] 提示: 若仍被杀软拦截，取上述 SHA256 与文件提交火绒误报"
echo "[build-dev]       （bbs.huorong.cn 文件提交区）或上传 VirusTotal 验证。"
