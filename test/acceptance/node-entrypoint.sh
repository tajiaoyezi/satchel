#!/bin/sh
# 节点容器入口（m2-04 design 第 6 条）：
# 首次启动时从共享卷 /shared/join.env 读取配对参数执行 satchel-agent join，并把 kernel.json 与证书拷进 /etc/satchel-agent/；
# 重启基线里被 SIGKILL 后再次 docker start 时，配置文件已在命名卷 /etc/satchel-agent/ 里，直接 exec satchel-agent run。
set -eu

CONFIG_DIR="/etc/satchel-agent"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"
export SSL_CERT_FILE="/caddy-data/caddy/pki/authorities/local/root.crt"

if [ ! -f "${CONFIG_FILE}" ]; then
    if [ ! -f /shared/join.env ]; then
        echo "缺少 /shared/join.env" >&2
        exit 1
    fi
    # shellcheck disable=SC1091
    . /shared/join.env
    satchel-agent join \
        --master-url "${MASTER_URL:-https://master.test}" \
        --token "${NODE_TOKEN}" \
        --master-key "${MASTER_KEY}" \
        --mode websocket \
        --config "${CONFIG_FILE}"
    cp /etc/satchel-agent-seed/kernel.json "${CONFIG_DIR}/kernel.json"
    cp /etc/satchel-agent-seed/cert.pem "${CONFIG_DIR}/cert.pem"
    cp /etc/satchel-agent-seed/key.pem "${CONFIG_DIR}/key.pem"
    chmod 0600 "${CONFIG_DIR}/kernel.json" "${CONFIG_DIR}/cert.pem" "${CONFIG_DIR}/key.pem"
fi

exec satchel-agent run --config "${CONFIG_FILE}"
