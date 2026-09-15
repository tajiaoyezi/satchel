#!/bin/sh
# Satchel（百宝袋）主控一键安装脚本（技术方案第 09 章「主控部署」，照 mmwx 的骨架改名缩简）。POSIX sh，Alpine 的 ash 也能跑。
#
#   裸机：curl -fsSL https://raw.githubusercontent.com/tajiaoyezi/satchel/main/install.sh | sudo sh
#   Docker：… | sudo sh -s -- --docker
#
# 裸机路：检查 root 与架构 → 装依赖 → 探测 systemd / OpenRC → 从 GitHub Release 下载二进制与 .sig →
#         用本脚本内嵌的发布公钥（openssl）验签、验不过不落盘 → 装到 /usr/local/bin/satchel →
#         建 /var/lib/satchel → 写服务 → enable、start。
# Docker 路：没有 docker 就装 Docker Engine → 写 docker-compose.yml 与 .env → docker compose up -d。
#
# 验签的信任锚是这份脚本自己（你已经信任它了，才会 curl | sh），不是刚下载的二进制——让待装文件验它自己等于没验。
#
# 选项：
#   --docker            走 Docker 路（默认裸机）
#   --version <tag>     装指定版本（如 v0.1.0）；默认最新正式版。两条路都认
#   --prerelease        取最新预发布版。两条路都认
#   --binary <path>     从本地文件安装（开发与测试用），要求同目录有 <path>.sig，或加 --skip-verify
#   --skip-verify       跳过验签（只对 --binary 有效；会大字警告）
#   --no-start          只装不起（容器里没有 init 时用）
#   --install-dir <dir> Docker 路的安装目录（默认 /opt/satchel）
#   --help
#
# M0 状态：satchel serve 随 M1 交付，现在装完服务起不来是预期的；二进制、数据目录、服务文件的形状已定。
set -eu

GITHUB_REPO="tajiaoyezi/satchel"
GH_PROXY="https://gh-proxy.com/"          # 直连 GitHub 失败时的回退前缀，可改
IMAGE="ghcr.io/tajiaoyezi/satchel"
SERVICE_NAME="satchel"
BIN_DIR="/usr/local/bin"
DATA_DIR="/var/lib/satchel"
INSTALL_DIR="/opt/satchel"

# 发布公钥清单（base64 的 32 字节 Ed25519 公钥，一行一把，新钥在前），与 satchel 仓库 pkg/release 里的清单相同，
# 测试保证两处一致。轮换时先加新钥、下一版再去旧钥。
# BEGIN RELEASE PUBLIC KEYS
RELEASE_PUBLIC_KEYS="
Gp8PtBixDGF/pHfQM4LZ6mSJuE8cRJ2vJiTUYxrC7rI=
"
# END RELEASE PUBLIC KEYS

MODE="native"
VERSION=""
CHANNEL="stable"
LOCAL_BINARY=""
SKIP_VERIFY=0
NO_START=0
STAGING_DIR=""
SERVICE_MANAGER=""
SERVICE_MANAGER_RUNNING=1

info()  { printf '\033[0;32m[信息]\033[0m %s\n' "$*"; }
warn()  { printf '\033[1;33m[警告]\033[0m %s\n' "$*" >&2; }
error() { printf '\033[0;31m[错误]\033[0m %s\n' "$*" >&2; }
die()   { error "$@"; exit 1; }

usage() {
    cat <<'EOF'
用法：install.sh [--docker] [--version <tag>] [--prerelease] [--binary <path> [--skip-verify]] [--no-start] [--install-dir <dir>]

  --docker            走 Docker 路（默认裸机）
  --version <tag>     装指定版本（如 v0.1.0）；默认最新正式版
  --prerelease        取最新预发布版
  --binary <path>     从本地文件安装（开发与测试用），要求同目录有 <path>.sig，或加 --skip-verify
  --skip-verify       跳过验签（只对 --binary 有效）
  --no-start          只装不起
  --install-dir <dir> Docker 路的安装目录（默认 /opt/satchel）
EOF
}

cleanup() {
    case "$STAGING_DIR" in
        /tmp/satchel-install.*) rm -rf -- "$STAGING_DIR" ;;
    esac
}
trap cleanup EXIT

parse_args() {
    while [ $# -gt 0 ]; do
        case "$1" in
            --docker) MODE="docker" ;;
            --version) [ $# -ge 2 ] || die "--version 需要一个 tag"; VERSION="$2"; shift ;;
            --prerelease) CHANNEL="prerelease" ;;
            --binary) [ $# -ge 2 ] || die "--binary 需要一个路径"; LOCAL_BINARY="$2"; shift ;;
            --skip-verify) SKIP_VERIFY=1 ;;
            --no-start) NO_START=1 ;;
            --install-dir) [ $# -ge 2 ] || die "--install-dir 需要一个目录"; INSTALL_DIR="$2"; shift ;;
            -h|--help) usage; exit 0 ;;
            *) die "未知参数：$1（--help 查看用法）" ;;
        esac
        shift
    done
    if [ "$SKIP_VERIFY" = 1 ] && [ -z "$LOCAL_BINARY" ]; then
        die "--skip-verify 只能与 --binary 一起用；从 Release 下载的文件必须验签"
    fi
}

check_root() {
    [ "$(id -u)" -eq 0 ] || die "请用 root 运行：curl … | sudo sh"
}

check_architecture() {
    case "$(uname -m)" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) die "不支持的架构：$(uname -m)（支持 x86_64 / aarch64）" ;;
    esac
    ASSET="satchel-linux-${ARCH}"
    info "架构：${ARCH}"
}

# 包管理器探测：apt（Debian / Ubuntu）、dnf / yum（RHEL 系）、apk（Alpine）。
detect_package_manager() {
    if command -v apk >/dev/null 2>&1; then PKG="apk"
    elif command -v apt-get >/dev/null 2>&1; then PKG="apt"
    elif command -v dnf >/dev/null 2>&1; then PKG="dnf"
    elif command -v yum >/dev/null 2>&1; then PKG="yum"
    else die "不支持的发行版：找不到 apk / apt-get / dnf / yum"
    fi
    info "包管理器：${PKG}"
}

# 依赖：curl 下载、ca-certificates 走 HTTPS、openssl 验签；Alpine 另装 openrc（服务管理）。
install_dependencies() {
    info "安装依赖（curl、ca-certificates、openssl）…"
    case "$PKG" in
        apk) apk add --no-cache curl ca-certificates openssl openrc >/dev/null ;;
        apt) apt-get update -qq >/dev/null 2>&1 || true
             DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates openssl >/dev/null ;;
        dnf) dnf install -y -q curl ca-certificates openssl >/dev/null ;;
        yum) yum install -y -q curl ca-certificates openssl >/dev/null ;;
    esac
    command -v openssl >/dev/null 2>&1 || die "openssl 不可用，无法验签"
}

# 服务管理：systemd 或 OpenRC 正在跑就用它；都没在跑（容器、chroot）按发行版家族选一种，只写文件不启动。
detect_service_manager() {
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
        SERVICE_MANAGER="systemd"
    elif command -v rc-service >/dev/null 2>&1 && [ -e /run/openrc/softlevel ]; then
        SERVICE_MANAGER="openrc"
    else
        SERVICE_MANAGER_RUNNING=0
        os_id=""
        # shellcheck disable=SC1091
        [ -r /etc/os-release ] && os_id="$(. /etc/os-release && printf '%s %s' "${ID:-}" "${ID_LIKE:-}")"
        case " $os_id " in
            *" alpine "*) SERVICE_MANAGER="openrc" ;;
            *) SERVICE_MANAGER="systemd" ;;
        esac
        warn "没有在运行的 systemd / OpenRC（容器？）：按发行版写 ${SERVICE_MANAGER} 的服务文件，不启动"
        NO_START=1
    fi
    info "服务管理器：${SERVICE_MANAGER}"
}

prepare_staging() {
    if [ -z "$STAGING_DIR" ]; then
        STAGING_DIR="$(mktemp -d /tmp/satchel-install.XXXXXX)"
        chmod 0700 "$STAGING_DIR"
    fi
}

# 先直连 GitHub，失败再走 gh-proxy；下载到临时文件，成功才改名。
fetch() {
    url="$1"; dest="$2"
    rm -f -- "$dest.tmp"
    if curl --fail --location --silent --show-error --connect-timeout 15 --max-time 300 --retry 2 \
        "$url" -o "$dest.tmp" 2>/dev/null; then
        mv -f -- "$dest.tmp" "$dest"; return 0
    fi
    warn "直连失败，改走 ${GH_PROXY}"
    if curl --fail --location --silent --show-error --connect-timeout 15 --max-time 300 --retry 2 \
        "${GH_PROXY}${url}" -o "$dest.tmp"; then
        mv -f -- "$dest.tmp" "$dest"; return 0
    fi
    rm -f -- "$dest.tmp"
    return 1
}

# 取版本号：两条路都用。curl 或 grep 失败时留空，由下面统一报错（不让 set -e 吞掉提示）。
resolve_version() {
    [ -z "$VERSION" ] || return 0
    info "查询最新${CHANNEL}版本…"
    api="https://api.github.com/repos/${GITHUB_REPO}/releases"
    if [ "$CHANNEL" = "prerelease" ]; then
        # /releases 按发布时间倒序，第一个非 draft 的就是最新（预发布或正式版都算）。
        VERSION="$(curl -fsSL "${api}?per_page=20" 2>/dev/null | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/' || true)"
    else
        VERSION="$(curl -fsSL "${api}/latest" 2>/dev/null | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/' || true)"
    fi
    if [ -z "$VERSION" ]; then
        if [ "$CHANNEL" = "stable" ]; then
            die "无法取得最新正式版（可能还没有正式版，只有预发布：加 --prerelease；或网络 / GitHub API 限流：用 --version 指定）"
        fi
        die "无法取得版本号（网络或 GitHub API 限流），可用 --version 指定"
    fi
    info "版本：${VERSION}"
}

# 取得待安装的二进制与签名：下载或本地；随后验签。验不过就删掉临时文件、不落盘。
obtain_binary() {
    prepare_staging
    STAGED="${STAGING_DIR}/${ASSET}"
    if [ -n "$LOCAL_BINARY" ]; then
        [ -f "$LOCAL_BINARY" ] || die "找不到 --binary 指定的文件：${LOCAL_BINARY}"
        cp -- "$LOCAL_BINARY" "$STAGED"
        if [ -f "${LOCAL_BINARY}.sig" ]; then cp -- "${LOCAL_BINARY}.sig" "${STAGED}.sig"; fi
    else
        resolve_version
        base="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}"
        info "下载 ${ASSET}…"
        fetch "${base}/${ASSET}" "$STAGED" || die "下载 ${ASSET} 失败"
        fetch "${base}/${ASSET}.sig" "${STAGED}.sig" || die "下载 ${ASSET}.sig 失败"
    fi
    verify_binary
    chmod 0755 "$STAGED"
}

# 用脚本内嵌的公钥清单验 Ed25519 分离签名：任意一把通过即通过。openssl 要 PEM 的 SPKI 公钥，
# Ed25519 的 SPKI 头是固定的 12 字节，base64 后恰好 "MCowBQYDK2VwAyEA"，直接拼在原始公钥的 base64 前面。
verify_with_keys() {
    file="$1"; sig="$2"
    for key in $RELEASE_PUBLIC_KEYS; do
        [ -n "$key" ] || continue
        pem="${STAGING_DIR}/pub.pem"
        printf -- '-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA%s\n-----END PUBLIC KEY-----\n' "$key" > "$pem"
        if openssl pkeyutl -verify -pubin -inkey "$pem" -rawin -in "$file" -sigfile "$sig" >/dev/null 2>&1; then
            rm -f -- "$pem"
            return 0
        fi
    done
    rm -f -- "${STAGING_DIR}/pub.pem"
    return 1
}

verify_binary() {
    if [ "$SKIP_VERIFY" = 1 ]; then
        warn "================================================================"
        warn "  已跳过验签（--skip-verify）。只能用于开发与测试，绝不要在生产机器上这样装。"
        warn "================================================================"
        return 0
    fi
    [ -f "${STAGED}.sig" ] || die "缺少签名文件 ${ASSET}.sig；本地安装请把 .sig 放在二进制旁边，或明确加 --skip-verify"
    [ "$(wc -c < "${STAGED}.sig")" -eq 64 ] || die "${ASSET}.sig 不是 64 字节的 Ed25519 分离签名"
    info "验签（openssl，公钥内嵌在本脚本）…"
    if ! verify_with_keys "$STAGED" "${STAGED}.sig"; then
        rm -f -- "$STAGED" "${STAGED}.sig"
        die "签名与 Satchel 发布公钥不匹配，已丢弃下载的文件，未安装任何东西"
    fi
    info "验签通过"
}

install_binary() {
    mkdir -p "$BIN_DIR"
    install -m 0755 -- "$STAGED" "${BIN_DIR}/${SERVICE_NAME}"
    info "已安装 ${BIN_DIR}/${SERVICE_NAME}：$("${BIN_DIR}/${SERVICE_NAME}" version | head -n1)"
}

create_data_dir() {
    mkdir -p "$DATA_DIR"
    chmod 0700 "$DATA_DIR"
    info "数据目录：${DATA_DIR}"
}

write_service() {
    if [ "$SERVICE_MANAGER" = "systemd" ]; then
        cat > "/etc/systemd/system/${SERVICE_NAME}.service" <<EOF
[Unit]
Description=Satchel（百宝袋）主控
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Environment=SATCHEL_DATA_DIR=${DATA_DIR}
ExecStart=${BIN_DIR}/${SERVICE_NAME} serve
Restart=always
RestartSec=5
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
        info "已写 /etc/systemd/system/${SERVICE_NAME}.service"
    else
        cat > "/etc/init.d/${SERVICE_NAME}" <<EOF
#!/sbin/openrc-run
name="Satchel 主控"
command="${BIN_DIR}/${SERVICE_NAME}"
command_args="serve"
command_background=true
command_user="root"
pidfile="/run/${SERVICE_NAME}.pid"
output_log="/var/log/${SERVICE_NAME}.log"
error_log="/var/log/${SERVICE_NAME}.log"
export SATCHEL_DATA_DIR="${DATA_DIR}"

depend() {
    need net
    after firewall
}
EOF
        chmod 0755 "/etc/init.d/${SERVICE_NAME}"
        info "已写 /etc/init.d/${SERVICE_NAME}"
    fi
}

enable_service() {
    if [ "$SERVICE_MANAGER" = "systemd" ]; then
        if [ "$SERVICE_MANAGER_RUNNING" = 1 ]; then
            systemctl daemon-reload
            systemctl enable "${SERVICE_NAME}" >/dev/null 2>&1
        else
            # systemd 没在跑：手工建 enable 的软链，等机器正常启动时生效。
            mkdir -p /etc/systemd/system/multi-user.target.wants
            ln -sf "/etc/systemd/system/${SERVICE_NAME}.service" "/etc/systemd/system/multi-user.target.wants/${SERVICE_NAME}.service"
        fi
    elif command -v rc-update >/dev/null 2>&1; then
        rc-update add "${SERVICE_NAME}" default >/dev/null 2>&1 || true
    else
        mkdir -p /etc/runlevels/default
        ln -sf "/etc/init.d/${SERVICE_NAME}" "/etc/runlevels/default/${SERVICE_NAME}"
    fi
    info "服务已设为开机启动"
}

start_service() {
    if [ "$NO_START" = 1 ]; then
        info "按要求不启动服务（--no-start 或服务管理器未运行）"
        return 0
    fi
    info "启动服务…"
    if [ "$SERVICE_MANAGER" = "systemd" ]; then
        systemctl restart "${SERVICE_NAME}" || true
        sleep 2
        systemctl --no-pager --lines=5 status "${SERVICE_NAME}" || true
    else
        rc-service "${SERVICE_NAME}" restart || true
    fi
}

install_native() {
    check_architecture
    detect_package_manager
    install_dependencies
    detect_service_manager
    obtain_binary
    install_binary
    create_data_dir
    write_service
    enable_service
    start_service
    echo
    info "安装完成。二进制 ${BIN_DIR}/${SERVICE_NAME}，数据目录 ${DATA_DIR}，服务名 ${SERVICE_NAME}。"
    info "M0 阶段还没有 serve 子命令，服务要到 M1 才能真正启动（访问地址随 M1 给出）；现在可用：satchel version、satchel db status --data-dir ${DATA_DIR}"
}

# ---------- Docker 路 ----------

install_docker_engine() {
    if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
        return 0
    fi
    info "安装 Docker Engine 与 Compose 插件…"
    case "$PKG" in
        apt) apt-get update -qq >/dev/null 2>&1 || true
             DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io docker-compose-v2 >/dev/null 2>&1 \
               || DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker.io docker-compose-plugin >/dev/null ;;
        dnf) dnf install -y -q docker docker-compose-plugin >/dev/null ;;
        yum) yum install -y -q docker docker-compose-plugin >/dev/null ;;
        apk) apk add --no-cache docker docker-cli-compose >/dev/null ;;
    esac
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then systemctl enable --now docker; fi
    if command -v rc-service >/dev/null 2>&1 && [ -e /run/openrc/softlevel ]; then rc-update add docker default >/dev/null 2>&1 || true; rc-service docker start || true; fi
    docker compose version >/dev/null 2>&1 || die "Docker Compose v2 不可用，请先手工安装 Docker Engine 与 Compose 插件"
}

write_compose() {
    mkdir -p "$INSTALL_DIR"
    tag="${VERSION#v}"
    cat > "${INSTALL_DIR}/docker-compose.yml" <<EOF
services:
  satchel:
    image: \${SATCHEL_IMAGE:-${IMAGE}:${tag}}
    container_name: satchel
    # M0 没有 serve：容器跑完迁移就退出，所以只在失败时重试有限次；M1 交付 serve 后改回 unless-stopped。
    restart: on-failure:5
    network_mode: host
    environment:
      - SATCHEL_DATA_DIR=/var/lib/satchel
      - SATCHEL_DATABASE_DRIVER=\${SATCHEL_DATABASE_DRIVER:-}
      - SATCHEL_DATABASE_HOST=\${SATCHEL_DATABASE_HOST:-}
      - SATCHEL_DATABASE_PORT=\${SATCHEL_DATABASE_PORT:-}
      - SATCHEL_DATABASE_NAME=\${SATCHEL_DATABASE_NAME:-}
      - SATCHEL_DATABASE_USER=\${SATCHEL_DATABASE_USER:-}
      - SATCHEL_DATABASE_PASSWORD=\${SATCHEL_DATABASE_PASSWORD:-}
      - SATCHEL_DATABASE_SSLMODE=\${SATCHEL_DATABASE_SSLMODE:-}
    volumes:
      - ./data:/var/lib/satchel
      - /etc/localtime:/etc/localtime:ro

  postgres:
    image: postgres:18-alpine
    container_name: satchel-postgres
    restart: unless-stopped
    profiles: ["postgres"]
    environment:
      POSTGRES_DB: \${POSTGRES_DB:-satchel}
      POSTGRES_USER: \${POSTGRES_USER:-satchel}
      POSTGRES_PASSWORD: \${POSTGRES_PASSWORD:-change-me-before-start}
    ports:
      - "127.0.0.1:5432:5432"
    volumes:
      # PostgreSQL 18 起官方镜像的 PGDATA 在 /var/lib/postgresql/18/docker，声明的卷是父目录，必须挂父目录。
      - ./postgres-data:/var/lib/postgresql
EOF
    if [ ! -f "${INSTALL_DIR}/.env" ]; then
        cat > "${INSTALL_DIR}/.env" <<EOF
SATCHEL_IMAGE=${IMAGE}:${tag}
SATCHEL_DATABASE_DRIVER=
SATCHEL_DATABASE_HOST=
SATCHEL_DATABASE_PORT=
SATCHEL_DATABASE_NAME=
SATCHEL_DATABASE_USER=
SATCHEL_DATABASE_PASSWORD=
SATCHEL_DATABASE_SSLMODE=
POSTGRES_DB=satchel
POSTGRES_USER=satchel
POSTGRES_PASSWORD=$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 32)
EOF
        chmod 0600 "${INSTALL_DIR}/.env"
    fi
    info "已写 ${INSTALL_DIR}/docker-compose.yml 与 .env（镜像 ${IMAGE}:${tag}）"
}

install_docker() {
    check_architecture
    detect_package_manager
    install_dependencies
    # --no-start 只写文件：不在这一步装 Docker Engine（容器里装不上，也用不着）。
    [ "$NO_START" = 1 ] || install_docker_engine
    resolve_version
    write_compose
    if [ "$NO_START" = 1 ]; then
        info "按要求不启动（--no-start）：cd ${INSTALL_DIR} && docker compose up -d"
        return 0
    fi
    (cd "$INSTALL_DIR" && docker compose pull && docker compose up -d)
    info "容器已启动；数据在 ${INSTALL_DIR}/data。M0 阶段没有 serve，容器会在跑完迁移后退出，这是预期的。"
}

main() {
    parse_args "$@"
    check_root
    if [ "$MODE" = "docker" ]; then install_docker; else install_native; fi
}

main "$@"
