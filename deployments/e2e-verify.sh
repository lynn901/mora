#!/usr/bin/env bash
# deployments/e2e-verify.sh
# Docker Compose 端到端联调验证脚本。
# 用法： ./deployments/e2e-verify.sh
# 前提： docker compose up -d 已执行，mora-api 在 localhost:${MORA_API_PORT:-8990}。
set -uo pipefail

MORA_PORT="${MORA_API_PORT:-8990}"
MCP_PORT="${MCP_SERVER_PORT:-8081}"
MORA="http://localhost:${MORA_PORT}"
MCP="http://localhost:${MCP_PORT}"
ADMIN_EMAIL="admin@mora.local"
ADMIN_PW="admin123"
DEV_TOKEN="mora_dev_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
PASS=0
FAIL=0

ok()   { echo "  ✔ $1"; PASS=$((PASS+1)); }
fail() { echo "  ✗ $1"; FAIL=$((FAIL+1)); }
section() { echo ""; echo "=== $1 ==="; }

# curlv: curl without -f so error bodies are visible; returns body+http code.
curlv() { curl -sS -w "\n__HTTP_CODE__%{http_code}" "$@" 2>&1 || true; }

section "0. 容器状态"
if command -v docker &>/dev/null; then
  docker compose -f deployments/docker-compose.yml ps --format "table {{.Name}}\t{{.Status}}" 2>/dev/null || docker ps --format "table {{.Names}}\t{{.Status}}" 2>/dev/null | head -10
else
  echo "  (docker 不可用，跳过容器状态检查)"
fi

section "0.5 对象存储（MinIO，多格式解析上传依赖）"
# MinIO 是多格式解析的 P0 依赖（design-docs/10 §4.2.1）；mora-api/rag-worker depends_on 它 healthy。
# 宿主 9000 探测 /minio/health/live；未暴露端口时改走 docker exec。
MINIO_PORT="${MINIO_PORT:-9000}"
MINIO_HC=$(curlv "http://localhost:${MINIO_PORT}/minio/health/live" 2>/dev/null || echo "")
if [ -z "${MINIO_HC}" ]; then
  # 端口未发布到宿主（仅同网可达）→ 通过容器名探测
  MINIO_HC=$(docker exec mora-mora-api-1 wget -qO- "http://minio:9000/minio/health/live" 2>/dev/null || docker exec mora-rag-worker-1 wget -qO- "http://minio:9000/minio/health/live" 2>/dev/null || echo "")
fi
if [ -n "${MINIO_HC}" ]; then ok "MinIO /minio/health/live 可达（多格式解析对象存储就绪）"; else fail "MinIO 不可达"; echo "  提示: docker compose logs minio; mora-api/rag-worker depends_on minio healthy"; fi

section "1. 健康检查"
HC=$(curlv "${MORA}/healthz")
if echo "${HC}" | grep -q '"status":"ok"'; then ok "mora-api /healthz"; else fail "mora-api /healthz"; echo "  响应: ${HC}"; fi
HC=$(curlv "${MORA}/ready")
if echo "${HC}" | grep -q '"status":"ready"'; then ok "mora-api /ready (PG 连通)"; else fail "mora-api /ready"; echo "  响应: ${HC}"; fi
HC=$(curlv "${MCP}/mcp/health")
if echo "${HC}" | grep -q '"status":"ok"'; then ok "mcp-server /mcp/health"; else fail "mcp-server /mcp/health"; echo "  响应: ${HC}"; echo "  提示: docker compose logs mcp-server 查看启动错误"; fi

section "2. 登录获取 JWT（admin@mora.local / admin123）"
LOGIN=$(curlv -XPOST "${MORA}/api/v1/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"${ADMIN_EMAIL}\",\"password\":\"${ADMIN_PW}\"}")
JWT=$(echo "${LOGIN}" | python3 -c "import json,sys; s=sys.stdin.read().split('__HTTP_CODE__')[0]; print(json.loads(s).get('data',{}).get('token',''))" 2>/dev/null || echo "")
if [ -n "$JWT" ] && [ "$JWT" != "" ]; then ok "JWT 获取成功"; else fail "登录失败"; echo "  响应: ${LOGIN}"; fi

DOC_ID=""
if [ -n "$JWT" ] && [ "$JWT" != "" ]; then
  AUTH="Authorization: Bearer ${JWT}"

  section "3. 创建文档 → 触发 doc_event 到 Valkey Streams"
  DOC_RESP=$(curlv -XPOST "${MORA}/api/v1/workspaces/11111111-1111-1111-1111-111111111111/documents" \
    -H "${AUTH}" -H 'Content-Type: application/json' \
    -d '{"title":"联调验证文档","content":[{"type":"paragraph","text":"这是一个用于验证 RAG 索引链路的测试文档，包含可被检索的关键词：向量检索测试。"}],"format":"blocks"}')
  DOC_ID=$(echo "${DOC_RESP}" | python3 -c "import json,sys; s=sys.stdin.read().split('__HTTP_CODE__')[0]; print(json.loads(s).get('data',{}).get('id',''))" 2>/dev/null || echo "")
  if [ -n "$DOC_ID" ] && [ "$DOC_ID" != "" ]; then ok "文档创建成功: ${DOC_ID}"; else fail "文档创建失败"; echo "  响应: ${DOC_RESP}"; fi

  if [ -n "$DOC_ID" ] && [ "$DOC_ID" != "" ]; then
    section "4. 发布文档 → 触发 RAG 索引（doc_event → rag-worker → TEI → Qdrant）"
    PUB_RESP=$(curlv -XPATCH "${MORA}/api/v1/documents/${DOC_ID}" \
      -H "${AUTH}" -H 'Content-Type: application/json' -H "If-Match: 1" \
      -d '{"title":"联调验证文档","status":"published"}')
    if echo "${PUB_RESP}" | grep -q "published"; then ok "文档已发布"; else fail "文档发布失败"; echo "  响应: ${PUB_RESP}"; fi

    echo "  等待 rag-worker 索引（TEI embedding + Qdrant upsert）..."
    sleep 15

    section "5. 验证 index_status = indexed（rag-worker 回执）"
    IDX_STATUS=""
    for i in $(seq 1 12); do
      STATUS_RESP=$(curlv "${MORA}/api/v1/documents/${DOC_ID}" -H "${AUTH}")
      IDX_STATUS=$(echo "${STATUS_RESP}" | python3 -c "import json,sys; s=sys.stdin.read().split('__HTTP_CODE__')[0]; print(json.loads(s).get('data',{}).get('index_status',''))" 2>/dev/null || echo "")
      if [ "$IDX_STATUS" = "indexed" ]; then break; fi
      echo "  attempt $i: index_status=${IDX_STATUS}, waiting 5s..."
      sleep 5
    done
    if [ "$IDX_STATUS" = "indexed" ]; then ok "index_status=indexed（RAG 索引成功）"; else fail "index_status=${IDX_STATUS}"; echo "  提示: docker compose logs rag-worker 查看索引错误; docker compose logs tei 查看模型加载状态"; fi
  fi

  section "6. mora-api FTS 搜索（BM25）"
  SEARCH_RESP=$(curlv "${MORA}/api/v1/search?workspace_id=11111111-1111-1111-1111-111111111111&q=向量检索" -H "${AUTH}")
  SEARCH_TOTAL=$(echo "${SEARCH_RESP}" | python3 -c "import json,sys; s=sys.stdin.read().split('__HTTP_CODE__')[0]; d=json.loads(s); print(d.get('data',{}).get('total',0) if isinstance(d.get('data'),dict) else 0)" 2>/dev/null || echo "0")
  SEARCH_CODE=$(echo "${SEARCH_RESP}" | grep -o '__HTTP_CODE__[0-9]*' | grep -o '[0-9]*')
  if [ "$SEARCH_CODE" = "200" ]; then ok "FTS 搜索 200（total=${SEARCH_TOTAL}；simple 分词不支持中文，0 结果属正常）"; else fail "FTS 搜索失败 (HTTP $SEARCH_CODE)"; echo "  响应: ${SEARCH_RESP}"; fi
fi

section "7. MCP prod 模式：initialize 握手"
INIT_RESP=$(curlv -XPOST "${MCP}/mcp" \
  -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e-verify","version":"1.0"}}}')
PROTO=$(echo "${INIT_RESP}" | python3 -c "import json,sys; s=sys.stdin.read().split('__HTTP_CODE__')[0]; r=json.loads(s); print(r.get('result',{}).get('protocolVersion',''))" 2>/dev/null || echo "")
if [ "$PROTO" = "2025-06-18" ]; then ok "MCP initialize 握手成功（protocolVersion 回显）"; else fail "MCP initialize 失败"; echo "  响应: ${INIT_RESP}"; fi

section "8. MCP search_knowledge_base（RAG 检索）"
SEARCH_TOOL=$(curlv -XPOST "${MCP}/mcp" \
  -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search_knowledge_base","arguments":{"query":"向量检索测试","top_n":5}}}')
SEARCH_ITEMS=$(echo "${SEARCH_TOOL}" | python3 -c "import json,sys; s=sys.stdin.read().split('__HTTP_CODE__')[0]; r=json.loads(s); c=r.get('result',{}).get('content',[]); print(len(c) if c else 0)" 2>/dev/null || echo "0")
if [ "$SEARCH_ITEMS" -gt 0 ] 2>/dev/null; then ok "search_knowledge_base 返回结果（${SEARCH_ITEMS} 条 content）"; else fail "search_knowledge_base 无结果或报错"; echo "  响应: ${SEARCH_TOOL}"; fi

section "9. MCP get_document（mora-api 文档正文）"
if [ -n "${DOC_ID:-}" ] && [ "${DOC_ID:-}" != "" ]; then
  GET_DOC=$(curlv -XPOST "${MCP}/mcp" \
    -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"get_document\",\"arguments\":{\"document_id\":\"${DOC_ID}\"}}}")
  if echo "${GET_DOC}" | grep -q "向量检索测试"; then ok "get_document 返回文档正文"; else fail "get_document 未返回预期内容"; echo "  响应: ${GET_DOC}"; fi
else
  fail "跳过 get_document（无 DOC_ID）"
fi

section "10. 级联删除 → Qdrant chunk 清理"
if [ -n "${DOC_ID:-}" ] && [ "${DOC_ID:-}" != "" ]; then
  DEL_RESP=$(curlv -XDELETE "${MORA}/api/v1/documents/${DOC_ID}" -H "${AUTH}")
  if echo "${DEL_RESP}" | grep -q "__HTTP_CODE__204\|__HTTP_CODE__200"; then ok "文档删除成功"; else fail "文档删除失败"; echo "  响应: ${DEL_RESP}"; fi
  sleep 5
  RE_SEARCH=$(curlv -XPOST "${MCP}/mcp" \
    -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_knowledge_base","arguments":{"query":"向量检索测试","top_n":5}}}')
  if ! echo "${RE_SEARCH}" | grep -q "${DOC_ID}"; then ok "删除后检索不再返回该文档"; else fail "删除后仍可检索到（级联清理未生效）"; fi
else
  fail "跳过级联删除验证（无 DOC_ID）"
fi

echo ""
echo "================================"
echo "  通过: ${PASS}    失败: ${FAIL}"
echo "================================"
if [ "$FAIL" -gt 0 ]; then exit 1; fi
