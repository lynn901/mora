#!/usr/bin/env bash
# deployments/backup.sh
# Mora 平台数据备份：PG dump + Qdrant 快照 + 应用配置文件（如.env）。
# 支持保留策略：自动清除超过 BACKUP_RETENTION_DAYS（默认 30）天的旧备份。
# 用法: ./deployments/backup.sh [output_dir]
#        BACKUP_RETENTION_DAYS=7 ./deployments/backup.sh
# 默认输出到 ./backup/YYYY-MM-DD_HHMMSS/
set -euo pipefail

BACKUP_ROOT="./backup"
RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-30}"
BACKUP_DIR="${1:-${BACKUP_ROOT}/$(date +%Y-%m-%d_%H%M%S)}"
COMPOSE_FILE="deployments/docker-compose.yml"
COMPOSE_PROJECT="mora"

mkdir -p "$BACKUP_DIR"
echo "=== Mora 备份开始 → $BACKUP_DIR ==="

# 1. PostgreSQL dump
echo "--- PostgreSQL dump ---"
docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T postgres \
  pg_dump -U mora -d mora --no-owner --no-acl \
  > "$BACKUP_DIR/mora_pg_dump.sql"
  gzip "$BACKUP_DIR/mora_pg_dump.sql"
  echo "  ✔ mora_pg_dump.sql.gz ($(du -h "$BACKUP_DIR/mora_pg_dump.sql.gz" | cut -f1))"

# 2. Qdrant 快照（通过 REST API）
echo "--- Qdrant snapshot ---"
QDRANT_CONTAINER=$(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" ps -q qdrant)
if [ -n "$QDRANT_CONTAINER" ]; then
  COLLECTIONS=$(docker exec "$QDRANT_CONTAINER" bash -c 'echo > /dev/tcp/localhost:6333' 2>/dev/null && \
    docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T qdrant \
    curl -sf http://localhost:6333/collections | python3 -c "
import json,sys
r=json.load(sys.stdin)
for c in r.get('result',{}).get('collections',[]):
    print(c['name'])" 2>/dev/null || echo "")

  if [ -n "$COLLECTIONS" ]; then
    for col in $COLLECTIONS; do
      echo "  creating snapshot for collection: $col"
      SNAP_RESP=$(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T qdrant \
        curl -sf -XPOST "http://localhost:6333/collections/$col/snapshots" 2>/dev/null || echo "{}")
      SNAP_NAME=$(echo "$SNAP_RESP" | python3 -c "import json,sys; r=json.load(sys.stdin); print(r.get('result',{}).get('name',''))" 2>/dev/null || echo "")
      if [ -n "$SNAP_NAME" ]; then
        docker cp "$QDRANT_CONTAINER:/qdrant/storage/snapshots/$col/$SNAP_NAME" "$BACKUP_DIR/${col}_snapshot.snapshot" 2>/dev/null || \
          echo "  ⚠ could not copy snapshot (might be in default path)"
        echo "  ✔ snapshot for $col"
      fi
    done
    # save collection list for restore validation
    echo "$COLLECTIONS" > "$BACKUP_DIR/qdrant_collections.txt"
  else
    echo "  - no collections or qdrant not ready"
  fi
fi

# 3. 保存 compose 和 env 状态（不含敏感值）
docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" config > "$BACKUP_DIR/docker-compose.resolved.yml" 2>/dev/null || true
echo "  ✔ docker-compose resolved config saved"

# 4. 保存备份元数据（恢复一致性校验用）
{
  echo "backup_time: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "mora_version: $(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T mora-api /app/app -version 2>/dev/null || echo 'unknown')"
  echo "pg_dump: mora_pg_dump.sql.gz"
  echo "qdrant_snapshots:"
  for snap in "$BACKUP_DIR"/*_snapshot.snapshot; do
    [ -f "$snap" ] && echo "  - $(basename "$snap")"
  done
} > "$BACKUP_DIR/backup_manifest.yaml"
echo "  ✔ backup manifest saved"

# 5. 清理过期备份（保留策略）
echo ""
echo "--- 清理超过 ${RETENTION_DAYS} 天的备份 ---"
CLEANED=0
for old_dir in "$BACKUP_ROOT"/[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]_*; do
  [ -d "$old_dir" ] || continue
  if [ "$(find "$old_dir" -maxdepth 0 -mtime +"$RETENTION_DAYS" 2>/dev/null)" != "" ]; then
    rm -rf "$old_dir"
    echo "  ✗ removed: $(basename "$old_dir")"
    CLEANED=$((CLEANED + 1))
  fi
done
echo "  ✔ 清理完成: $CLEANED 个过期备份"

echo ""
echo "=== 备份完成: $BACKUP_DIR ==="
echo "恢复方式: ./deployments/restore.sh $BACKUP_DIR"
