# syntax=docker/dockerfile:1

# dev 分支在 VPS 上的部署镜像（离线二进制版）。
#
# 为什么不用仓库根目录那个多阶段 Dockerfile：
#   目标 VPS 只有 943MB 可用磁盘（/ 已用 86%），docker 可回收空间仅 89MB。
#   多阶段构建要拉 node:24-alpine + golang:1.26-alpine（合计 500MB 以上），
#   再加 npm 依赖、go 模块缓存与构建中间层，峰值需要 1.5GB 以上——空间不够，
#   而且写满根分区会连带影响同一台机器上正在跑的 5 个容器（含它们的 SQLite）。
#
#   这里改成：二进制在构建机上交叉编译好（CGO_ENABLED=0，纯静态），
#   运行时镜像只做一件事——把它拷进去。镜像约 25MB，构建几秒完成。
#
# 二进制与 Dockerfile 同目录，构建上下文就是本目录：
#     docker compose up -d --build
FROM alpine:3.21

# ca-certificates：调 Oracle 的 HTTPS 接口要验证证书链，没有它一律握手失败。
# tzdata：面板显示本地时间，容器默认 UTC，日志与「N 分钟前」会整体偏几小时。
# wget：HEALTHCHECK 用。
RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -g 10001 -S ocicore \
    && adduser -u 10001 -S -G ocicore -h /app ocicore

WORKDIR /app

# 构建机上交叉编译出来的 linux/arm64 静态二进制。
COPY ocicore /usr/local/bin/ocicore

RUN chmod 0755 /usr/local/bin/ocicore \
    && mkdir -p /app/data \
    && chown -R ocicore:ocicore /app

# master.key 与加密后的 OCI 私钥都在这里。丢了这个目录，所有已保存的
# 账号私钥都解不开，只能重新添加。
VOLUME ["/app/data"]

USER ocicore

# 容器内必须绑 0.0.0.0，否则 127.0.0.1 在容器里谁也连不上。
# 对外暴露与否由 compose 的 ports 决定，这里不影响。
ENV OCICORE_ADDR=0.0.0.0:8080 \
    OCICORE_DATA_DIR=/app/data \
    TZ=Asia/Shanghai

EXPOSE 8080

# /api/status 不需要登录，正好用来探活。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/api/status >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/ocicore"]
