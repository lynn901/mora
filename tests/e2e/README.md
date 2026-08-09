# E2E 验证套件

该套件通过 REST、WebSocket 和 MCP JSON-RPC 驱动完整运行栈，覆盖文档、版本、
权限、检索、RAG、MCP 和异常边界。测试使用 `e2e` build tag；未设置
`E2E_BASE_URL` 时会自动跳过。

## 运行

先启动完整栈，并通过覆盖文件暴露测试数据库端口：

```bash
docker compose -f deployments/docker-compose.yml \
  -f tests/e2e/docker-compose.e2e.override.yml up -d
```

然后执行：

```bash
E2E_BASE_URL=http://localhost:8990 \
E2E_MCP_URL=http://localhost:8081 \
DATABASE_URL=postgres://mora:mora@localhost:5432/mora \
INTERNAL_SERVICE_TOKEN=mora-internal-token \
go test -tags=e2e -v ./tests/e2e/...
```

只运行单个场景：

```bash
go test -tags=e2e -v -run TestCoreClosedLoop ./tests/e2e/...
```

## 配置

| 变量 | 默认值 | 用途 |
|---|---|---|
| `E2E_BASE_URL` | `http://localhost:8990` | Mora API 地址；未设置时跳过套件 |
| `E2E_MCP_URL` | `http://localhost:8081` | MCP Server 地址 |
| `DATABASE_URL` | 空 | 测试夹具使用的 PostgreSQL DSN |
| `INTERNAL_SERVICE_TOKEN` | `mora-internal-token` | 服务间调用凭证 |
| `E2E_ADMIN_EMAIL` | `admin@mora.local` | 演示管理员 |
| `E2E_ADMIN_PASSWORD` | `admin123` | 演示管理员密码 |
| `E2E_DEV_TOKEN` | `mora_dev_a1b2c3d4...` | 绑定管理员的 MCP Token |
| `E2E_INDEX_TIMEOUT` | `120s` | 等待索引完成的最长时间 |

需要数据库的用例会创建唯一工作区、用户和 Token，并在套件结束时清理。
