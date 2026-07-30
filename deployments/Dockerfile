# syntax=docker/dockerfile:1
# Wiki 平台后端镜像（多目标构建）。
# 通过 --build-arg TARGET=<wiki-api|rag-worker|mcp-server> 选择构建哪个二进制；
# 三个服务共用同一 Go builder 阶段，runtime 用 distroless 最小化攻击面。
# 同时构建一个静态 healthcheck 探针：distroless 无 shell/curl，compose 的
# HEALTHCHECK 通过该探针发起 HTTP 健康检查。

ARG TARGET=wiki-api

FROM golang:1.25-alpine AS builder
ARG TARGET
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build the requested service binary + the shared healthcheck probe (both static).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${TARGET} \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/healthcheck ./deployments/healthcheck

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder /out/app /app/app
COPY --from=builder /out/healthcheck /app/healthcheck
COPY migrations /app/migrations
USER nonroot:nonroot
ENTRYPOINT ["/app/app"]
