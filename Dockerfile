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
RUN adduser -D -u 10001 gojob
WORKDIR /app
COPY --from=build /out/app /app/${BUILD_TARGET}
USER gojob
EXPOSE 7700 7799 7800
# 刻意不在镜像里写 HEALTHCHECK：三个 target 监听不同端口，镜像级健康检查会让 demo/agent
# 容器因为探不到 7700 而恒为 unhealthy。健康检查归编排层管——docker-compose.yml 里
# gojob-server 那节有针对 /healthz 的定义。
ENTRYPOINT ["/app/${BUILD_TARGET}"]
