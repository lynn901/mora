#!/usr/bin/env bash
# deployments/restore.sh
# Wiki 平台数据恢复：PG 恢复 + Qdrant 快照恢复 + 向量库一致性校验。
# 用法: ./deployments/restore.sh <backup_dir>
# 前置条件：服务已 `docker compose up -d` 运行，postgres 和 qdrant healthy。
set -euo pipefail

if [ $# -lt 1 ]; then
  echo "用法: $0 <backup_dir>"
  echo "示例: $0 ./backup/2026-07-31_120000"
  exit 1
fi

BACKUP_DIR="$1"
COMPOSE_FILE="deployments/docker-compose.yml"
COMPOSE_PROJECT="wiki"

if [ ! -d "$BACKUP_DIR" ]; then
  echo "错误: 备份目录不存在: $BACKUP_DIR"
  exit 1
fi

echo "=== Wiki 数据恢复 ← $BACKUP_DIR ==="
echo "⚠ 恢复将覆盖当前数据库和向量数据！确认继续？(yes/no)"
read -r CONFIRM
if [ "$CONFIRM" != "yes" ]; then
  echo "已取消"
  exit 0
fi

# 0. 读取备份元数据
MANIFEST="$BACKUP_DIR/backup_manifest.yaml"
BACKUP_COLLECTIONS=""
if [ -f "$MANIFEST" ]; then
  echo "--- 备份信息 ---"
  cat "$MANIFEST"
  BACKUP_COLLECTIONS=$(sed -n 's/^  - //p' <(grep '  - ' "$MANIFEST" 2>/dev/null || true) || echo "")
fi

# 1. 恢复 PostgreSQL
PG_DUMP="$BACKUP_DIR/wiki_pg_dump.sql.gz"
if [ -f "$PG_DUMP" ]; then
  echo "--- PostgreSQL 恢复 ---"
  gunzip -c "$PG_DUMP" | docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T postgres \
    psql -U wiki -d wiki -v ON_ERROR_STOP=1
  echo "  ✔ PostgreSQL 恢复完成"
elif [ -f "$BACKUP_DIR/wiki_pg_dump.sql" ]; then
  docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T postgres \
    psql -U wiki -d wiki -v ON_ERROR_STOP=1 < "$BACKUP_DIR/wiki_pg_dump.sql"
  echo "  ✔ PostgreSQL 恢复完成"
else
  echo "  ⚠ 未找到 pg dump 文件，跳过"
fi

# 2. 恢复 Qdrant 快照
echo "--- Qdrant 快照恢复 ---"
QDRANT_CONTAINER=$(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" ps -q qdrant)
RESTORED_COLLECTIONS=""
if [ -n "$QDRANT_CONTAINER" ]; then
  for snap in "$BACKUP_DIR"/*_snapshot.snapshot; do
    [ -f "$snap" ] || continue
    col=$(basename "$snap" _snapshot.snapshot)
    RESTORED_COLLECTIONS="$RESTORED_COLLECTIONS $col"
    echo "  restoring collection: $col"
    docker cp "$snap" "$QDRANT_CONTAINER:/qdrant/storage/snapshots/${col}_snapshot.snapshot"
    docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T qdrant \
      curl -sf -XPOST "http://localhost:6333/collections/$col/snapshots/upload?priority=snapshot" \
      -H "Content-Type: multipart/form-data" \
      -F "snapshot=@/qdrant/storage/snapshots/${col}_snapshot.snapshot" || echo "  ⚠ $col 恢复失败"
    echo "  ✔ $col 快照恢复"
  done
fi

# 3. 向量库一致性校验
echo ""
echo "--- 一致性校验 ---"
VERIFY_PASS=true

# 3a. Qdrant 集合存在性检查
if [ -n "$BACKUP_COLLECTIONS" ]; then
  echo "  检查 Qdrant 集合状态..."
  CURRENT_COLS=$(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T qdrant \
    curl -sf http://localhost:6333/collections 2>/dev/null | \
    python3 -c "import json,sys; r=json.load(sys.stdin); [print(c['name']) for c in r.get('result',{}).get('collections',[])]" 2>/dev/null || true)
  for expected_col in $BACKUP_COLLECTIONS; do
    if echo "$CURRENT_COLS" | grep -q "$expected_col"; then
      echo "  ✔ 集合 $expected_col 存在"
    else
      echo "  ✗ 集合 $expected_col 缺失（快照恢复可能未完全生效）"
      VERIFY_PASS=false
    fi
  done
fi

# 3b. PG 文档数与 Qdrant chunk 计数交叉校验
if command -v python3 &>/dev/null; then
  echo "  交叉验证 PG 文档数与 Qdrant chunk 数（采样）..."
  DOC_COUNT=$(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T postgres \
    psql -U wiki -d wiki -tAc "SELECT COUNT(*) FROM documents WHERE status='published';" 2>/dev/null || echo "0")
  POINT_COUNT=0
  for col in $RESTORED_COLLECTIONS; do
    COUNT=$(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T qdrant \
      curl -sf "http://localhost:6333/collections/$col" 2>/dev/null | \
      python3 -c "import json,sys; r=json.load(sys.stdin); print(r.get('result',{}).get('points_count',0))" 2>/dev/null || echo "0")
    POINT_COUNT=$((POINT_COUNT + COUNT))
  done
  echo "  PG published 文档: $DOC_COUNT | Qdrant 向量点: $POINT_COUNT"
  if [ "$DOC_COUNT" -gt 0 ] && [ "$POINT_COUNT" -gt 0 ]; then
    echo "  ✔ 交叉校验通过（均有数据）"
  else
    echo "  ⚠ 交叉校验：文档或向量数为 0（空实例属正常；非空实例需排查）"
  fi
fi

# 3c. 校验结果摘要
echo ""
if [ "$VERIFY_PASS" = true ]; then
  echo "=== 一致性校验: ✅ 通过 ==="
else
  echo "=== 一致性校验: ❌ 异常（部分集合缺失） ==="
fi

echo ""
echo "=== 恢复完成 ==="
echo "建议: 重启 rag-worker 以重建索引状态: docker compose restart rag-worker"
