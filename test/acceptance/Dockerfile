# syntax=docker/dockerfile:1
# M2 正式验收的三个瘦镜像（design 第 5 条）：二进制与证书由 run.sh 在宿主机按 Docker 架构放入 .build/。

FROM debian:bookworm-slim AS base
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        bsdutils \
    && rm -rf /var/lib/apt/lists/*

FROM base AS master
COPY satchel /usr/local/bin/satchel
RUN chmod 0755 /usr/local/bin/satchel && mkdir -p /var/lib/satchel
ENV SATCHEL_DATA_DIR=/var/lib/satchel
ENV SATCHEL_LISTEN=0.0.0.0:12889
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/satchel"]
CMD ["serve"]

FROM base AS node
COPY satchel-agent /usr/local/bin/satchel-agent
COPY node-entrypoint.sh /usr/local/bin/node-entrypoint.sh
COPY kernel.json /etc/satchel-agent-seed/kernel.json
COPY cert.pem /etc/satchel-agent-seed/cert.pem
COPY key.pem /etc/satchel-agent-seed/key.pem
RUN chmod 0755 /usr/local/bin/satchel-agent /usr/local/bin/node-entrypoint.sh
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/node-entrypoint.sh"]

FROM base AS driver
COPY acceptance.test /usr/local/bin/acceptance.test
RUN chmod 0755 /usr/local/bin/acceptance.test
ENTRYPOINT ["/usr/local/bin/acceptance.test"]
