# gojob 服务端 / demo worker / 零代码脚本节点镜像（纯 Go sqlite 驱动，无需 CGO）
# docker build --build-arg BUILD_TARGET=gojob-server -t gojob/gojob-server:latest .
# docker build --build-arg BUILD_TARGET=gojob-demo   -t gojob/gojob-demo:latest .
# docker build --build-arg BUILD_TARGET=gojob-agent  -t gojob/gojob-agent:latest .
# 版本戳由发布脚本传进来（git tag），便于在容器里用 -version 核对跑的是哪一版：
#   --build-arg GOJOB_VERSION=v0.2.0
FROM golang:1.27-alpine AS build
ARG BUILD_TARGET=gojob-server
ARG GOJOB_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags="-s -w -X main.version=${GOJOB_VERSION}" \
      -o /out/app ./cmd/${BUILD_TARGET}

FROM alpine:3.20
ARG BUILD_TARGET=gojob-server
# 裸 alpine 没有 zoneinfo，Go 解析不了 TZ 就静默退回 UTC，注入 TZ=Asia/Shanghai 也无效。
# 装上 tzdata 让注入的 TZ 可解析；镜像本身不设默认 TZ，默认仍是 UTC（.cn 版才默认东八区）。
RUN apk add --no-cache tzdata
# 容器以非 root 运行：先建好可写目录，named volume 首次挂载时会继承这里的属主
RUN adduser -D -u 10001 gojob \
 && mkdir -p /app/data /var/lib/gojob/agent \
 && chown -R gojob:gojob /app /var/lib/gojob
WORKDIR /app
COPY --from=build /out/app /app/${BUILD_TARGET}
# exec 形式不展开变量，固定软链给 ENTRYPOINT 用；目标名保留便于排查
RUN ln -s /app/${BUILD_TARGET} /app/gojob
USER gojob
# 只有服务端监听端口：demo worker 与 gojob-agent 都不开任何端口
# （它们只出站访问 Server，优雅下线走 SDK 方法或控制台的「停止接单」开关）。
EXPOSE 7800
# 刻意不在镜像里写 HEALTHCHECK：三个 target 监听不同端口，镜像级健康检查会让 demo/agent
# 容器因为探不到 7800 而恒为 unhealthy。健康检查归编排层管——docker-compose.yml 里
# gojob-server 那节有针对 /healthz 的定义。
ENTRYPOINT ["/app/gojob"]
