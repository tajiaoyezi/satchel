#!/bin/sh
# 构建网页并放进主控编进二进制的目录 internal/projection/web/dist/（master-web「网页编进主控二进制」）。
# CI、发布线与 Docker 镜像的构建都经这个脚本；主控仓库不提交前端产物，dist/ 里只留 .gitkeep。
#
#   tools/web/build.sh                       按 internal/projection/web/SATCHEL_WEB_REF 记的 tag 浅克隆 satchel-web 再构建
#   SATCHEL_WEB_DIR=../satchel-web tools/web/build.sh   用本地的一份 satchel-web 工作区（开发用，版本记成 local）
#
# 另外两个环境变量：SATCHEL_WEB_REPO 换克隆的地址（默认 GitHub 上的 satchel-web）；SATCHEL_WEB_KEEP 指定克隆放在哪个
# 目录并保留下来（CI 的端到端测试要用同一份 satchel-web），不给时克隆进临时目录、用完删掉。
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
webdir="$root/internal/projection/web"
ref=$(tr -d ' \r\n' < "$webdir/SATCHEL_WEB_REF")
repo=${SATCHEL_WEB_REPO:-https://github.com/tajiaoyezi/satchel-web.git}

if [ -n "${SATCHEL_WEB_DIR:-}" ]; then
    src=$SATCHEL_WEB_DIR
    version=local
else
    if [ -n "${SATCHEL_WEB_KEEP:-}" ]; then
        src=$SATCHEL_WEB_KEEP
    else
        src=$(mktemp -d)
        trap 'rm -rf "$src"' EXIT
    fi
    if ! git clone --quiet --depth 1 --branch "$ref" "$repo" "$src"; then
        echo "拉不到 satchel-web 的 $ref（$repo）：确认这个 tag 已经推送、网络通" >&2
        exit 1
    fi
    version=$ref
fi

(cd "$src" && npm ci --no-audit --no-fund && SATCHEL_WEB_VERSION="$version" npm run build)

if [ ! -f "$src/dist/index.html" ]; then
    echo "satchel-web 的构建没有产出 dist/index.html" >&2
    exit 1
fi
find "$webdir/dist" -mindepth 1 -maxdepth 1 ! -name .gitkeep -exec rm -rf {} +
cp -R "$src/dist/." "$webdir/dist/"
echo "已把 satchel-web $version 放进 $webdir/dist"
