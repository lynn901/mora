#!/usr/bin/env bash
# deployments/e2e-verify.sh
# YS-12 端到端联调验证脚本：一键验证 docker compose up 后全部 AC。
# 用法： ./deployments/e2e-verify.sh
# 前提： docker compose up -d 已执行，wiki-api 在 localhost:${WIKI_API_PORT:-8990}。
set -euo pipefail

WIKI_PORT="${WIKI_API_PORT:-8990}"
MCP_PORT="${MCP_SERVER_PORT:-8081}"
WIKI="http://localhost:${WIKI_PORT}"
MCP="http://localhost:${MCP_PORT}"
ADMIN_EMAIL="admin@wiki.local"
ADMIN_PW="admin123"
DEV_TOKEN="wki_dev_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
PASS=0
FAIL=0

ok()   { echo "  ✔ $1"; PASS=$((PASS+1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL+1)); }
section() { echo ""; echo "=== $1 ==="; }

section "0. 健康检查"
if curl -sf "${WIKI}/healthz" | grep -q '"status":"ok"'; then ok "wiki-api /healthz"; else fail "wiki-api /healthz"; fi
if curl -sf "${WIKI}/ready" | grep -q '"status":"ready"'; then ok "wiki-api /ready (PG 连通)"; else fail "wiki-api /ready"; fi
if curl -sf "${MCP}/mcp/health" | grep -q '"status":"ok"'; then ok "mcp-server /mcp/health"; else fail "mcp-server /mcp/health"; fi

section "1. 登录获取 JWT（admin@wiki.local / admin123）"
LOGIN=$(curl -sf -XPOST "${WIKI}/api/v1/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"${ADMIN_EMAIL}\",\"password\":\"${ADMIN_PW}\"}" || echo "")
JWT=$(echo "${LOGIN}" | python3 -c "import json,sys; print(json.load(sys.stdin).get('data',{}).get('token',''))" 2>/dev/null || echo "")
if [ -n "$JWT" ] && [ "$JWT" != "" ]; then ok "JWT 获取成功"; else fail "登录失败（检查迁移 010 是否执行、admin 密码）"; echo "  响应: ${LOGIN}"; fi

if [ -n "$JWT" ]; then
  AUTH="Authorization: Bearer ${JWT}"

  section "2. 创建文档 → 触发 doc_event 到 Valkey Streams"
  DOC_RESP=$(curl -sf -XPOST "${WIKI}/api/v1/workspaces/11111111-1111-1111-1111-111111111111/documents" \
    -H "${AUTH}" -H 'Content-Type: application/json' \
    -d '{"title":"联调验证文档","content":[{"type":"paragraph","text":"这是一个用于验证 RAG 索引链路的测试文档，包含可被检索的关键词：向量检索测试。"}],"format":"blocks"}' || echo "")
  DOC_ID=$(echo "${DOC_RESP}" | python3 -c "import json,sys; print(json.load(sys.stdin).get('data',{}).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$DOC_ID" ]; then ok "文档创建成功: ${DOC_ID}"; else fail "文档创建失败"; echo "  响应: ${DOC_RESP}"; fi

  if [ -n "$DOC_ID" ]; then
    section "3. 发布文档 → 触发 RAG 索引（doc_event → rag-worker → TEI → Qdrant）"
    PUB_RESP=$(curl -sf -XPATCH "${WIKI}/api/v1/documents/${DOC_ID}" \
      -H "${AUTH}" -H 'Content-Type: application/json' -H "If-Match: 1" \
      -d '{"status":"published"}' || echo "")
    if echo "${PUB_RESP}" | grep -q "published"; then ok "文档已发布"; else fail "文档发布失败"; echo "  响应: ${PUB_RESP}"; fi

    echo "  等待 rag-worker 索引（TEI embedding + Qdrant upsert）..."
    sleep 15

    section "4. 验证 index_status = indexed（rag-worker 回执）"
    for i in $(seq 1 12); do
      STATUS_RESP=$(curl -sf "${WIKI}/api/v1/documents/${DOC_ID}" -H "${AUTH}" 2>/dev/null || echo "")
      IDX_STATUS=$(echo "${STATUS_RESP}" | python3 -c "import json,sys; print(json.load(sys.stdin).get('data',{}).get('index_status',''))" 2>/dev/null || echo "")
      if [ "$IDX_STATUS" = "indexed" ]; then break; fi
      echo "  attempt $i: index_status=${IDX_STATUS}, waiting 5s..."
      sleep 5
    done
    if [ "$IDX_STATUS" = "indexed" ]; then ok "index_status=indexed（RAG 索引成功）"; else fail "index_status=${IDX_STATUS}（可能 TEI 还在加载模型，或 rag-worker 报错）"; fi
  fi

  section "5. wiki-api FTS 搜索（BM25）"
  SEARCH_RESP=$(curl -sf "${WIKI}/api/v1/search?workspace_id=11111111-1111-1111-1111-111111111111&q=向量检索" -H "${AUTH}" 2>/dev/null || echo "")
  SEARCH_TOTAL=$(echo "${SEARCH_RESP}" | python3 -c "import json,sys; print(json.load(sys.stdin).get('data',{}).get('total',0))" 2>/dev/null || echo "0")
  if [ "$SEARCH_TOTAL" -gt 0 ] 2>/dev/null; then ok "FTS 搜索命中 ${SEARCH_TOTAL} 条"; else fail "FTS 搜索无结果"; fi
fi

section "6. MCP prod 模式：initialize 握手"
INIT_RESP=$(curl -sf -XPOST "${MCP}/mcp" \
  -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e-verify","version":"1.0"}}}' || echo "")
SESSION_ID=$(echo "${INIT_RESP}" | python3 -c "import json,sys; r=json.load(sys.stdin); print(r.get('result',{}).get('protocolVersion',''))" 2>/dev/null || echo "")
if [ "$SESSION_ID" = "2025-06-18" ]; then ok "MCP initialize 握手成功（protocolVersion 回显）"; else fail "MCP initialize 失败"; echo "  响应: ${INIT_RESP}"; fi

section "7. MCP search_knowledge_base（RAG 检索）"
SEARCH_TOOL=$(curl -sf -XPOST "${MCP}/mcp" \
  -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_knowledge_base","arguments":{"query":"向量检索测试","top_n":5}}}' || echo "")
SEARCH_ITEMS=$(echo "${SEARCH_TOOL}" | python3 -c "import json,sys; r=json.load(sys.stdin); c=r.get('result',{}).get('content',[]); print(len(c))" 2>/dev/null || echo "0")
if [ "$SEARCH_ITEMS" -gt 0 ] 2>/dev/null; then ok "search_knowledge_base 返回结果（${SEARCH_ITEMS} 条 content）"; else fail "search_knowledge_base 无结果或报错"; echo "  响应: ${SEARCH_TOOL}"; fi

section "8. MCP get_document（wiki-api 文档正文）"
if [ -n "${DOC_ID:-}" ]; then
  GET_DOC=$(curl -sf -XPOST "${MCP}/mcp" \
    -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"get_document\",\"arguments\":{\"document_id\":\"${DOC_ID}\"}}}" || echo "")
  if echo "${GET_DOC}" | grep -q "联调验证文档"; then ok "get_document 返回文档正文"; else fail "get_document 未返回预期内容"; echo "  响应: ${GET_DOC}"; fi
else
  fail "跳过 get_document（无 DOC_ID）"
fi

section "9. 级联删除 → Qdrant chunk 清理"
if [ -n "${DOC_ID:-}" ]; then
  curl -sf -XDELETE "${WIKI}/api/v1/documents/${DOC_ID}" -H "${AUTH}" >/dev/null 2>&1 && ok "文档删除成功" || fail "文档删除失败"
  sleep 5
  RE_SEARCH=$(curl -sf -XPOST "${MCP}/mcp" \
    -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_knowledge_base","arguments":{"query":"向量检索测试","top_n":5}}}' || echo "")
  if ! echo "${RE_SEARCH}" | grep -q "${DOC_ID}"; then ok "删除后检索不再返回该文档"; else fail "删除后仍可检索到（级联清理未生效）"; fi
else
  fail "跳过级联删除验证（无 DOC_ID）"
fi

echo ""
echo "================================"
echo "  通过: ${PASS}    失败: ${FAIL}"
echo "================================"
if [ "$FAIL" -gt 0 ]; then exit 1; fi
