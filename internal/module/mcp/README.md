# Wiki MCP Server

Model Context Protocol (MCP) integration for the Wiki knowledge base platform
(YS-9 / PRD §4 模块三, F3.1–F3.3). Exposes Wiki structure (Resources) and
capabilities (Tools) to external AI Agent ecosystems over the standard MCP
protocol, with token auth, RBAC reuse, audit, and rate limiting.

> Designed in [YS-5 `06-mcp-server-design.md`](../../design-docs). This module
> follows the "mock 先行" dependency strategy from YS-4: it builds against the
> YS-5 API contracts with an in-memory Wiki/RAG client, ready to swap in the
> real YS-6 (Wiki backend) / YS-8 (RAG) services when they land.

## Architecture

```
Agent ──HTTP/SSE (JSON-RPC 2.0)──▶ MCP Server (this module)
                                     │
                                     ├─ auth    : Bearer Token → SHA-256 lookup → AuthContext + scope gate
                                     ├─ ratelimit: per-token sliding window (read 100/min, write 20/min)
                                     ├─ server  : initialize/capabilities, tools/*, resources/*, session
                                     ├─ tool    : search_knowledge_base, get_document, list_documents,
                                     │            get_tags (MVP); create_draft, update_document (P1, draft/review)
                                     ├─ resource: wiki://workspaces, .../tree, documents/{id}/meta,
                                     │            .../tags, documents/{id}/versions
                                     ├─ audit   : append-only mcp_tool_calls + audit_logs
                                     │
                                     └─ wikiclient ──REST──▶ Wiki API (YS-6) + RAG search (YS-8)
                                                       (RBAC enforced server-side upstream)
```

**Key design rules (design doc 06):**
- RBAC is NOT re-implemented here — identity is propagated to the Wiki/RAG
  services which enforce it (06 §6.3). The MCP layer only enforces token-scope
  gating and existence-leak prevention.
- Read tools/resources return **empty results** (never 403/404) when the caller
  lacks permission — prevents document-existence inference (06 §6.4).
- Write tools return **403** on missing permission and always enter a
  **draft/review state** — never publish directly (06 §5.2.3/5.2.4, AC-17).
- Audit is append-only (INSERT never UPDATE/DELETE); forbidden calls bump
  `mcp_forbidden_total` (06 §7.1).

## Module layout (design doc 06 §8)

```
internal/module/mcp/
├── server/          # JSON-RPC 2.0, handshake, dispatch, session, transports
│   ├── protocol.go        # JSON-RPC + MCP types
│   ├── server.go          # method dispatch, rate limit, audit, error mapping
│   ├── session.go         # SessionStore (memory + postgres)
│   ├── transport_http.go  # Gin HTTP/SSE handlers
│   └── transport_stdio.go # stdio (newline-delimited JSON-RPC, P2)
├── tool/            # Tools (name → handler, calls WikiClient)
├── resource/        # Resources (wiki:// URI → handler)
├── auth/            # Token store, AuthContext, middleware, rate limiter
├── audit/           # Append-only audit store (memory + postgres)
└── wikiclient/      # Upstream Wiki+RAG client: interface + Mock + HTTP
cmd/mcp-server/main.go   # entry point (--transport http|stdio, --mock)
migrations/008_mcp.*.sql # api_tokens, mcp_sessions, mcp_tool_calls
```

## Resources

| URI | Description |
|---|---|
| `wiki://workspaces` | Workspaces visible to the caller |
| `wiki://workspaces/{id}/tree` | Directory tree (documents as metadata) |
| `wiki://workspaces/{id}/tags` | Workspace tag taxonomy |
| `wiki://documents/{id}/meta` | Document metadata |
| `wiki://documents/{id}/versions` | Document version history |

## Tools

| Tool | Type | MVP/P1 |
|---|---|---|
| `search_knowledge_base` | read | MVP (M7) |
| `get_document` | read | MVP (M7) |
| `list_documents` | read | MVP |
| `get_tags` | read | MVP |
| `create_draft` | write (draft/review) | P1 (S3) |
| `update_document` | write (draft/review) | P1 (S3) |

## Run

```bash
# Mock mode (in-memory Wiki+RAG, seeded; no Postgres/Valkey needed)
MCP_USE_MOCK=1 ./mcp-server
# → listens on :8081, prints a dev token to stderr

# Production (real Mora API + Postgres + Valkey)
DATABASE_URL=postgres://... VALKEY_URL=valkey:6379 \
  MORA_API_URL=http://mora-api:8080 INTERNAL_SERVICE_TOKEN=... \
  ./mcp-server

# stdio transport (local CLI/IDE Agent)
./mcp-server --transport stdio --api-token wki_...
```

## Quick protocol example

```bash
# initialize (returns Mcp-Session-Id header)
curl -X POST http://localhost:8081/mcp \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
       "params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"agent","version":"1.0"}}}'

# tools/list
curl -X POST http://localhost:8081/mcp -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}'

# tools/call search_knowledge_base
curl -X POST http://localhost:8081/mcp -H "Authorization: Bearer $TOKEN" \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call",
       "params":{"name":"search_knowledge_base","arguments":{"query":"分页","top_n":5}}}'
```

## Test

```bash
go test ./...            # all unit + integration tests
go test ./internal/module/mcp/server/   # protocol/auth/tools/RBAC/existence-leak
```

## Acceptance criteria (YS-9)

- AC-14: initialize/capabilities handshake over HTTP/SSE ✅
- AC-15: Resources (workspaces/tree/meta) ✅
- AC-16: search_knowledge_base / get_document return structured results ✅
- AC-17: write ops enter draft/review state, never publish directly ✅
- AC-18: token auth, RBAC, 403 on forbidden, full audit ✅
- AC-19: token scope/expiry/revocation ✅
