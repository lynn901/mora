# syntax=docker/dockerfile:1
# Mora 平台后端镜像（多目标构建）。
# 通过 --build-arg TARGET=<mora-api|rag-worker|mcp-server|knowledge-worker> 选择构建哪个
# 二进制；四个服务共用同一 Go builder 阶段，runtime 用 distroless 最小化攻击面。
# 同时构建一个静态 healthcheck 探针：distroless 无 shell/curl，compose 的
# HEALTHCHECK 通过该探针发起 HTTP 健康检查。
# knowledge-worker（design-docs/14 §5.3）消费 source_events / knowledge_events Streams，
# 内嵌 Outbox Dispatcher；egress 出网防护为硬约束（§6），默认不出网。

ARG TARGET=mora-api

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
