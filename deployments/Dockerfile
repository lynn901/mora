# syntax=docker/dockerfile:1
# Wiki API 后端镜像（Go 模块化单体）。
# 多阶段构建：builder 编译单二进制，runtime 用 distroless 最小化攻击面。

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/wiki-api ./cmd/wiki-api

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=builder /out/wiki-api /app/wiki-api
COPY migrations /app/migrations
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app/wiki-api"]
