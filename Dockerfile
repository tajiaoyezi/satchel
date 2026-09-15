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

# 数据目录布局见 storage-dual-database：database.json、satchel.db、master.key、subscribes/、rule_templates/。
ENV SATCHEL_DATA_DIR=/var/lib/satchel
VOLUME ["/var/lib/satchel"]

# 容器内一律不原地替换二进制，升级走换镜像 tag（M1 的自升级在容器里拒绝原地替换）。
ENTRYPOINT ["/usr/local/bin/satchel-entrypoint"]
# serve 随 M1 交付；M0 的镜像只能跑 version 与 db 子命令。
CMD ["serve"]
