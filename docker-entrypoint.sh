#!/bin/sh
# 主控容器入口：确保数据目录在、跑迁移、把控制权交给 satchel。
# 数据目录固定 /var/lib/satchel（ENV SATCHEL_DATA_DIR），compose 里用 ./data 挂过来，不能靠匿名卷。
set -eu

data_dir="${SATCHEL_DATA_DIR:-/var/lib/satchel}"
mkdir -p "$data_dir"
chmod 0700 "$data_dir"

# 迁移只向前；库结构与注册表不一致时这里会失败并列出差异，容器随之退出——不带着坏库起服务。
/usr/local/bin/satchel db migrate --data-dir "$data_dir"

exec /usr/local/bin/satchel "$@"
