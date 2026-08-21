# Phase 7: 单镜像 = 内嵌 Web UI 的控制面。
# 1) node 阶段构建 web/dist（Vite 产物，Node 24 与本地/CI 一致）
# 2) go 阶段把产物拷入 server/web/dist → go:embed 进二进制
# 3) distroless static 运行（CGO_ENABLED=0，纯静态）
FROM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY web ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY proto ./proto
COPY server ./server
# 覆盖仓库占位（server/web/dist/index.html），embed 命中真实产物
COPY --from=web /web/dist ./server/web/dist
RUN cd server && CGO_ENABLED=0 go build -trimpath -o /out/xnc-server ./cmd/xnc-server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/xnc-server /xnc-server
ENTRYPOINT ["/xnc-server"]
