#!/usr/bin/env bash
# gen-versioninfo.sh —— 生成 Windows PE 版本资源（治杀软启发式误报）。
#
# 背景：自编译 Go 产物（无签名、无版本资源、Go 运行时特征）反复被国产杀软
# 实时防护按 HEUR:VirTool/Obfuscator.a 之类启发式规则秒杀（先例：腾讯电脑
# 管家 2026-11 前 3 秒清源码目录产物；火绒 2026-11-11 判毒删除 A 机已部署
# 的 1.19.3/1.20.0 两个 build，见仓库 HANDOFF）。给 exe 注入 VERSIONINFO
# 资源（产品名/公司/版本/原文件名）可显著降低"无属性裸二进制"画像的误报率。
#
# 版本号从 cmd/server/main.go 的 appVersion 提取（单一事实来源，对齐 CI
# "二进制版本串 == 源码 appVersion" 发布纪律），不另行维护。
#
# 用法：bash scripts/gen-versioninfo.sh
# 产物：cmd/server/winres.json + cmd/server/rsrc_windows_amd64.syso
#       （syso 文件名带 _windows_amd64 平台限定：仅 GOOS=windows GOARCH=amd64
#        构建时自动链接，linux/darwin 交叉编译与本地 go build ./... 不受影响；
#        CI 五平台矩阵只有 windows job 需要跑本脚本）
# 依赖：go ≥1.17（go run pkg@version 免预装工具）。GOPROXY 直连 proxy.golang.org
#       不通时自动回落 goproxy.cn（国内网络默认可用）。
#
# 工具选型：曾试 goversioninfo@v1.2.0（底层 akavel/rsrc v0.8.0），其 COFF
# 重定位类型在 go1.27 内部链接器报 "unknown relocation type 7"，弃用；
# 改用 tc-hib/go-winres@v0.3.3（活跃维护、专为 Go linker 兼容设计）。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/cmd/server/main.go"
OUT="$ROOT/cmd/server"

APP_VERSION="$(sed -n 's/^const appVersion = "\(.*\)"$/\1/p' "$SRC" | head -1)"
if [ -z "$APP_VERSION" ]; then
  echo "ERROR: cannot extract appVersion from $SRC" >&2
  exit 1
fi
BASE="${APP_VERSION%-panel}"        # 1.20.0
IFS='.' read -r MAJ MIN PAT _ <<< "$BASE"
MAJ="${MAJ:-0}"; MIN="${MIN:-0}"; PAT="${PAT:-0}"

cat > "$OUT/winres.json" <<JSON
{
  "RT_VERSION": {
    "#1": {
      "0000": {
        "fixed": {
          "file_version": "$MAJ.$MIN.$PAT.0",
          "product_version": "$MAJ.$MIN.$PAT.0"
        },
        "info": {
          "0409": {
            "Comments": "WorkBuddy2API panel fork gateway - self-built binary",
            "CompanyName": "wjf1",
            "FileDescription": "WorkBuddy2API Panel Gateway (wb2api)",
            "FileVersion": "$APP_VERSION",
            "InternalName": "wb2api",
            "LegalCopyright": "Copyright (c) 2026 wjf1",
            "OriginalFilename": "wb2api.exe",
            "ProductName": "WorkBuddy2API Panel",
            "ProductVersion": "$APP_VERSION"
          }
        }
      }
    }
  }
}
JSON

WINRES="github.com/tc-hib/go-winres@v0.3.3"
if ! (cd "$ROOT" && go run "$WINRES" make --in "$OUT/winres.json" --out "$OUT/rsrc" --arch amd64); then
  # proxy.golang.org 直连不通（国内网络常态）时回落 goproxy.cn
  (cd "$ROOT" && GOPROXY=https://goproxy.cn,direct go run "$WINRES" make \
    --in "$OUT/winres.json" --out "$OUT/rsrc" --arch amd64)
fi

echo "OK: $OUT/rsrc_windows_amd64.syso (appVersion=$APP_VERSION -> FileVersion $MAJ.$MIN.$PAT)"
