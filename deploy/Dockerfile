# Phase 7: 单镜像 = 内嵌 Web UI 的控制面。
# 1) web/dist 由本地/CI 构建（Vite 产物，push 时打包上传）——服务器不跑
#    npm（registry 网络在阿里云国际区不可靠：npm ci ECONNRESET 曾使构建
#    全缓存的假象破裂，部署静默失败）
# 2) go 阶段把产物拷入 server/web/dist → go:embed 进二进制
# 3) distroless static 运行（CGO_ENABLED=0，纯静态）
FROM alpine:3.20 AS web
WORKDIR /web
COPY web/dist ./dist

FROM golang:1.26-alpine AS build
WORKDIR /src
# XNC_VERSION: server 构建版本（版本单一来源：构建时注入，与 agent bundle
# 版本一致；缺省回落 0.0.0-dev）。deploy_srv.py up 前在 .env 设 XNC_VERSION
# 即可让 compose build 透传。
ARG XNC_VERSION=0.0.0-dev
COPY proto ./proto
COPY rtv ./rtv
COPY server ./server
# 覆盖仓库占位（server/web/dist/index.html），embed 命中真实产物
COPY --from=web /web/dist ./server/web/dist
RUN cd server && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X xnc/server/internal/version.Version=${XNC_VERSION}" \
    -o /out/xnc-server ./cmd/xnc-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/xnc-server /xnc-server
ENTRYPOINT ["/xnc-server"]
