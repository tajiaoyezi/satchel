#!/bin/sh
# M2 代理内核与节点通道正式验收脚本（test/acceptance/run.sh）：
# 构建四个容器（master / https / node / driver），完成初始化、节点配对与五项黑盒正式验收，退出时一律清理容器与卷。
# 用法：
#   ./test/acceptance/run.sh                     # 正式口径（含 180 秒稳态单流/多流限速）
#   ./test/acceptance/run.sh --quick             # 开发快速回归（限速采样缩短至 15 秒）
#   ./test/acceptance/run.sh --quick -test.run TestAcceptanceKick
set -eu

DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
ROOT="$(CDPATH='' cd -- "$DIR/../.." && pwd)"
BUILD_DIR="$DIR/.build"
COMPOSE_FILE="$DIR/compose.yml"

QUICK="${ACCEPTANCE_QUICK:-}"
TEST_ARGS="${ACCEPTANCE_TEST_ARGS:-}"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --quick)
      QUICK="1"
      shift
      ;;
    *)
      if [ -z "$TEST_ARGS" ]; then
        TEST_ARGS="$1"
      else
        TEST_ARGS="$TEST_ARGS $1"
      fi
      shift
      ;;
  esac
done
export ACCEPTANCE_QUICK="$QUICK"
export ACCEPTANCE_TEST_ARGS="$TEST_ARGS"

cleanup() {
  docker compose -f "$COMPOSE_FILE" --profile driver down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

AGENT_REF="$(tr -d ' \r\n' < "$DIR/SATCHEL_AGENT_REF")"
if [ -z "$AGENT_REF" ]; then
  echo "错误：$DIR/SATCHEL_AGENT_REF 为空" >&2
  exit 1
fi

rm -rf "$BUILD_DIR"
mkdir -p "$BUILD_DIR"

AGENT_DIR="${SATCHEL_AGENT_DIR:-}"
if [ -z "$AGENT_DIR" ]; then
  if [ -d "$ROOT/../satchel-agent" ]; then
    AGENT_DIR="$(CDPATH='' cd -- "$ROOT/../satchel-agent" && pwd)"
  else
    AGENT_DIR="$BUILD_DIR/satchel-agent-src"
    echo "==> 检出 satchel-agent ($AGENT_REF)"
    git -c advice.detachedHead=false clone --depth 1 --branch "$AGENT_REF" \
      https://github.com/tajiaoyezi/satchel-agent.git "$AGENT_DIR"
  fi
fi

RAW_ARCH="$(docker version --format '{{.Server.Arch}}' 2>/dev/null || echo amd64)"
case "$RAW_ARCH" in
  x86_64|amd64) GOARCH="amd64" ;;
  aarch64|arm64) GOARCH="arm64" ;;
  *) GOARCH="$RAW_ARCH" ;;
esac

echo "==> 交叉编译 Linux/$GOARCH 二进制（satchel / satchel-agent / acceptance.test）"
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
  go -C "$ROOT" build -trimpath -o "$BUILD_DIR/satchel" ./cmd/satchel
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
  go -C "$AGENT_DIR" build -trimpath -tags with_quic -o "$BUILD_DIR/satchel-agent" ./cmd/satchel-agent
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" \
  go -C "$DIR" test -c -tags with_quic -o "$BUILD_DIR/acceptance.test" .

cp "$DIR/kernel.json" "$BUILD_DIR/kernel.json"
cp "$DIR/node-entrypoint.sh" "$BUILD_DIR/node-entrypoint.sh"
chmod +x "$BUILD_DIR/node-entrypoint.sh"

openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
  -nodes -days 1 -subj "/CN=localhost" \
  -keyout "$BUILD_DIR/key.pem" -out "$BUILD_DIR/cert.pem" >/dev/null 2>&1

cleanup
echo "==> 构建并启动 master 与 https 容器"
docker compose -f "$COMPOSE_FILE" --profile driver build
docker compose -f "$COMPOSE_FILE" up -d master https

echo "==> 等待 master 就绪与 Caddy 内部 CA 根证书生成"
READY=0
for _ in $(seq 1 30); do
  if docker compose -f "$COMPOSE_FILE" exec -T master curl -fsS http://127.0.0.1:12889/api/v1/healthz >/dev/null 2>&1 \
    && docker compose -f "$COMPOSE_FILE" exec -T https test -s /data/caddy/pki/authorities/local/root.crt >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 1
done
if [ "$READY" -ne 1 ]; then
  echo "错误：master 或 https 未在 30 秒内就绪" >&2
  docker compose -f "$COMPOSE_FILE" logs master https >&2 || true
  exit 1
fi

ADMIN_PASS="AcceptPass123!"
echo "==> 初始化 master、设置主控地址与 10 秒流量采集间隔、签发令牌与添加节点 hk-1"
printf '%s\n%s\n' "$ADMIN_PASS" "$ADMIN_PASS" | \
  docker compose -f "$COMPOSE_FILE" exec -T master \
  script -qec "satchel setup init --username admin --json" /dev/null >/dev/null

printf '%s\n\n' "$ADMIN_PASS" | \
  docker compose -f "$COMPOSE_FILE" exec -T master \
  script -qec "satchel settings master-url set --url https://master.test --resource-version 1 --verify-user admin --json" /dev/null >/dev/null

docker compose -f "$COMPOSE_FILE" exec -T master \
  satchel settings set --set traffic_collect_interval=10 --resource-version 2 --json >/dev/null

TOKEN_OUT="$(printf '%s\n\n' "$ADMIN_PASS" | \
  docker compose -f "$COMPOSE_FILE" exec -T master \
  script -qec "satchel token create --name driver --preset ops --danger restart --verify-user admin --json" /dev/null)"
DRIVER_TOKEN="$(printf '%s\n' "$TOKEN_OUT" | tr -d '\r' | grep -o 'sat_[A-Za-z0-9_-]*' | head -n 1)"
if [ -z "$DRIVER_TOKEN" ]; then
  echo "错误：未能从 token create 输出中提取 API 令牌" >&2
  printf '%s\n' "$TOKEN_OUT" >&2
  exit 1
fi
docker compose -f "$COMPOSE_FILE" exec -T master sh -ceu "printf '%s' '$DRIVER_TOKEN' > /shared/token"

docker compose -f "$COMPOSE_FILE" exec -T master satchel server add hk-1 --json >/dev/null
INSTALL_JSON="$(docker compose -f "$COMPOSE_FILE" exec -T master satchel server install-command hk-1 --json)"
NODE_TOKEN="$(printf '%s\n' "$INSTALL_JSON" | tr -d '\r' | sed -n 's/.*--token \([^ "\\]*\).*/\1/p' | head -n 1)"
MASTER_KEY="$(printf '%s\n' "$INSTALL_JSON" | tr -d '\r' | sed -n 's/.*--master-key \([^ "\\]*\).*/\1/p' | head -n 1)"
if [ -z "$NODE_TOKEN" ] || [ -z "$MASTER_KEY" ]; then
  echo "错误：未能从 server install-command 输出中提取节点入网凭据" >&2
  printf '%s\n' "$INSTALL_JSON" >&2
  exit 1
fi
docker compose -f "$COMPOSE_FILE" exec -T master sh -ceu \
  "printf 'MASTER_URL=https://master.test\nNODE_TOKEN=%s\nMASTER_KEY=%s\n' '$NODE_TOKEN' '$MASTER_KEY' > /shared/join.env"

echo "==> 启动 node 容器并等待 hk-1 在线"
docker compose -f "$COMPOSE_FILE" up -d node

CONNECTED=0
for _ in $(seq 1 30); do
  if docker compose -f "$COMPOSE_FILE" exec -T master satchel server get hk-1 --json 2>/dev/null | grep -q '"status": *"connected"'; then
    CONNECTED=1
    break
  fi
  sleep 1
done
if [ "$CONNECTED" -ne 1 ]; then
  echo "错误：节点 hk-1 未在 30 秒内连上主控" >&2
  docker compose -f "$COMPOSE_FILE" logs master https node >&2 || true
  exit 1
fi

echo "==> 启动 driver 容器运行正式验收测试"
docker compose -f "$COMPOSE_FILE" --profile driver up -d driver
docker logs -f satchel-acceptance-driver &
LOG_PID=$!
CODE="$(docker wait satchel-acceptance-driver)"
wait "$LOG_PID" 2>/dev/null || true

if [ "$CODE" != "0" ]; then
  echo "错误：正式验收测试失败（退出码 $CODE），打印 master / node 日志：" >&2
  docker compose -f "$COMPOSE_FILE" logs master node >&2 || true
  exit "$CODE"
fi
echo "==> M2 正式验收全部通过"
