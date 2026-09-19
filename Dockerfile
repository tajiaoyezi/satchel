# syntax=docker/dockerfile:1
# 主控镜像（技术方案第 09 章）：Go 构建阶段 + 以 nginx 官方镜像为底的运行阶段。
# Satchel 用纯 Go 的 SQLite 驱动，CGO_ENABLED=0，构建阶段不装 gcc。
# 版本三元组由 --build-arg 传入，与二进制发布线的 ldflags 一致；本地 docker build 不传就是 0.0.0-dev。

FROM golang:1.26-bookworm AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0-dev
ARG COMMIT=unknown
ARG DATE=unknown
WORKDIR /src
# 先只拷 go.mod / go.sum 拉依赖，改源码不必重拉。
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -trimpath \
    -ldflags="-s -w -X github.com/satchel/satchel/internal/base/buildinfo.Version=${VERSION} -X github.com/satchel/satchel/internal/base/buildinfo.Commit=${COMMIT} -X github.com/satchel/satchel/internal/base/buildinfo.Date=${DATE}" \
    -o /out/satchel ./cmd/satchel

# 运行阶段：主控要装和管 nginx（同机节点复用这份 nginx），所以以 nginx 官方镜像为底；进程用 root 跑。
FROM nginx:mainline-bookworm
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        tzdata \
        wget \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/satchel /usr/local/bin/satchel
COPY docker-entrypoint.sh /usr/local/bin/satchel-entrypoint
RUN chmod 0755 /usr/local/bin/satchel /usr/local/bin/satchel-entrypoint

# 数据目录布局见 storage-dual-database：database.json、config.yaml、satchel.db、master.key、satchel.sock、subscribes/、rule_templates/、public/。
ENV SATCHEL_DATA_DIR=/var/lib/satchel
# 主控监听地址（master-serve）；compose 透传同名变量可改。健康检查从它取端口。
ENV SATCHEL_LISTEN=0.0.0.0:12889
VOLUME ["/var/lib/satchel"]

# 健康检查打无身份的 /api/v1/healthz；start-period 留给迁移与启动。
# 地址取 SATCHEL_LISTEN：监听全部地址（0.0.0.0、[::]、空）时探 127.0.0.1，绑了具体地址就探那个地址。
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD sh -c 'host="${SATCHEL_LISTEN%:*}"; case "$host" in ""|0.0.0.0|"[::]"|"::") host=127.0.0.1;; esac; wget -qO- "http://$host:${SATCHEL_LISTEN##*:}/api/v1/healthz" >/dev/null || exit 1'


# nginx 官方镜像把 STOPSIGNAL 设成 SIGQUIT（nginx 的优雅停止信号），Go 进程收到 SIGQUIT 会打印 goroutine 转储后退出 2；
# 主控的优雅停止认 SIGTERM，这里改回来，docker stop 才是优雅停止。
STOPSIGNAL SIGTERM

# 容器内一律不原地替换二进制，升级走换镜像 tag（m1-08 的自升级在容器里拒绝原地替换）。
ENTRYPOINT ["/usr/local/bin/satchel-entrypoint"]
CMD ["serve"]
