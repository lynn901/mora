# Mora MCP Server

The MCP server exposes Mora resources and tools over JSON-RPC 2.0 using HTTP
or stdio transport. It delegates content and permission checks to `mora-api`
and adds token authentication, per-token rate limits, sessions, and audit logs.

## Resources

| URI | Description |
|---|---|
| `mora://workspaces` | Workspaces visible to the caller |
| `mora://workspaces/{id}/tree` | Workspace directory tree |
| `mora://workspaces/{id}/tags` | Workspace tags |
| `mora://documents/{id}/meta` | Document metadata |
| `mora://documents/{id}/versions` | Document version history |

Reads without permission return empty contents so callers cannot infer whether
a resource exists. Write tools require write scope and create draft/review
content; they do not publish directly.

## Tools

| Tool | Access |
|---|---|
| `search_knowledge_base` | read |
| `get_document` | read |
| `list_documents` | read |
| `get_tags` | read |
| `create_draft` | write |
| `update_document` | write |

## Run

```bash
# In-memory development mode
MCP_USE_MOCK=1 go run ./cmd/mcp-server

# Backed by mora-api, PostgreSQL, and Valkey
DATABASE_URL=postgres://... \
VALKEY_URL=redis://localhost:6379 \
MORA_API_URL=http://localhost:8080 \
INTERNAL_SERVICE_TOKEN=... \
go run ./cmd/mcp-server

# stdio transport
go run ./cmd/mcp-server --transport stdio --api-token mora_...
```

## Test

```bash
go test ./internal/module/mcp/...
```
