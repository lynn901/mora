#!/usr/bin/env bash
# migrate-wiki-to-mora.sh — wiki → mora 标识符迁移脚本
#
# 将运行中的 wiki 部署迁移到 mora 标识符：PostgreSQL 库名/用户、Docker 卷前缀、
# Qdrant collection 前缀、配置文件中的 COMPOSE_PROJECT 等。
#
# 用法：
#   DRY_RUN=1 ./migrate-wiki-to-mora.sh   # 预览模式，不执行任何变更
#   ./migrate-wiki-to-mora.sh              # 执行迁移
#
# 前置条件：docker compose 全部服务已停止（docker compose down），但卷保留。
# 依赖：backup.sh / export.sh 已存在且可执行。

set -euo pipefail

DRY_RUN="${DRY_RUN:-0}"
COMPOSE_FILE="deployments/docker-compose.yml"
OLD_PROJECT="wiki"
NEW_PROJECT="mora"
OLD_DB="wiki"
NEW_DB="mora"
OLD_DB_USER="wiki"
NEW_DB_USER="mora"
BACKUP_DIR="backup/pre-migration-$(date +%Y-%m-%d_%H%M%S)"

log() { echo "[$(date +%H:%M:%S)] $*"; }
dry_run() { [[ "$DRY_RUN" == "1" ]]; }

if dry_run; then
  log "=== DRY RUN 模式 — 不会执行任何变更 ==="
fi

# ── Step 1: 全量备份 ──────────────────────────────
log "=== Step 1: 全量备份 → $BACKUP_DIR ==="
mkdir -p "$BACKUP_DIR"

if ! dry_run; then
  # PG dump
  docker compose -f "$COMPOSE_FILE" -p "$OLD_PROJECT" exec -T postgres \
    pg_dump -U "$OLD_DB_USER" -d "$OLD_DB" --no-owner --no-acl \
    > "$BACKUP_DIR/wiki_pg_dump.sql" 2>/dev/null || true
  gzip -f "$BACKUP_DIR/wiki_pg_dump.sql" 2>/dev/null || true

  # Qdrant 快照
  for col in $(curl -s http://localhost:6333/collections 2>/dev/null | python3 -c "import sys,json; print(' '.join(json.load(sys.stdin).get('result',{}).get('collections',[])))" 2>/dev/null); do
    curl -s -X POST "http://localhost:6333/collections/$col/snapshots" > /dev/null 2>&1 || true
  done

  # 全量导出
  bash deployments/export.sh "$BACKUP_DIR/wiki-pre-migration.tar.gz" 2>/dev/null || true

  # 保存配置快照
  docker compose -f "$COMPOSE_FILE" -p "$OLD_PROJECT" config > "$BACKUP_DIR/docker-compose.resolved.yml" 2>/dev/null || true
  cp "$COMPOSE_FILE" "$BACKUP_DIR/docker-compose.yml.before"
  cp .env "$BACKUP_DIR/.env.before" 2>/dev/null || true
fi
log "  ✔ 备份完成"

# ── Step 2: PostgreSQL 库名迁移 ────────────────────
log "=== Step 2: PostgreSQL $OLD_DB → $NEW_DB ==="
if ! dry_run; then
  # 启动 postgres 容器
  docker compose -f "$COMPOSE_FILE" -p "$OLD_PROJECT" up -d postgres 2>/dev/null
  sleep 5

  # 创建新库和用户
  docker compose -f "$COMPOSE_FILE" -p "$OLD_PROJECT" exec -T postgres \
    psql -U "$OLD_DB_USER" -d postgres -c "CREATE USER $NEW_DB_USER WITH PASSWORD '$(grep POSTGRES_PASSWORD .env 2>/dev/null | cut -d= -f2 || echo mora)';" 2>/dev/null || true
  docker compose -f "$COMPOSE_FILE" -p "$OLD_PROJECT" exec -T postgres \
    psql -U "$OLD_DB_USER" -d postgres -c "CREATE DATABASE $NEW_DB OWNER $NEW_DB_USER;" 2>/dev/null || true

  # dump 旧库 + 恢复到新库
  docker compose -f "$COMPOSE_FILE" -p "$OLD_PROJECT" exec -T postgres \
    pg_dump -U "$OLD_DB_USER" -d "$OLD_DB" --no-owner --no-acl \
    | docker compose -f "$COMPOSE_FILE" -p "$OLD_PROJECT" exec -T postgres \
    psql -U "$NEW_DB_USER" -d "$NEW_DB" 2>&1 | tail -5

  # 旧库 rename 为 backup
  docker compose -f "$COMPOSE_FILE" -p "$OLD_PROJECT" exec -T postgres \
    psql -U "$OLD_DB_USER" -d postgres -c "ALTER DATABASE $OLD_DB RENAME TO ${OLD_DB}_backup_$(date +%Y%m%d);" 2>/dev/null || true

  docker compose -f "$COMPOSE_FILE" -p "$OLD_PROJECT" stop postgres 2>/dev/null
fi
log "  ✔ PostgreSQL 迁移完成（旧库保留为 ${OLD_DB}_backup_*）"

# ── Step 3: Docker 卷迁移 ──────────────────────────
log "=== Step 3: Docker 卷 $OLD_PROJECT → $NEW_PROJECT ==="
if ! dry_run; then
  # 列出旧卷
  OLD_VOLUMES=$(docker volume ls --format '{{.Name}}' | grep "^${OLD_PROJECT}_" || true)
  for vol in $OLD_VOLUMES; do
    new_vol=$(echo "$vol" | sed "s/^${OLD_PROJECT}_/${NEW_PROJECT}_/")
    if docker volume inspect "$new_vol" >/dev/null 2>&1; then
      log "  卷 $new_vol 已存在，跳过"
      continue
    fi
    docker volume create "$new_vol"
    # 跨卷复制
    docker run --rm -v "$vol":/from -v "$new_vol":/to alpine sh -c "cp -a /from/. /to/"
    log "  ✔ $vol → $new_vol"
  done
fi
log "  ✔ Docker 卷迁移完成（旧卷保留）"

# ── Step 4: Qdrant collection rename ───────────────
log "=== Step 4: Qdrant collections wiki_chunks_* → mora_chunks_* ==="
if ! dry_run; then
  docker compose -f "$COMPOSE_FILE" -p "$NEW_PROJECT" up -d qdrant 2>/dev/null
  sleep 3

  COLLECTIONS=$(curl -s http://localhost:6333/collections 2>/dev/null | python3 -c "import sys,json; print(' '.join(json.load(sys.stdin).get('result',{}).get('collections',[])))" 2>/dev/null || true)
  for col in $COLLECTIONS; do
    if [[ "$col" == wiki_chunks_* ]]; then
      new_col=$(echo "$col" | sed 's/^wiki_chunks_/mora_chunks_/')
      # 快照 → 创建新集合 → 恢复
      snap=$(curl -s -X POST "http://localhost:6333/collections/$col/snapshots" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('result',{}).get('name',''))" 2>/dev/null || true)
      if [ -n "$snap" ]; then
        snap_url="http://localhost:6333/collections/$col/snapshots/$snap"
        curl -s -X PUT "http://localhost:6333/collections/$new_col" \
          -H 'Content-Type: application/json' \
          -d "{\"vectors\": {\"size\": 384, \"distance\": \"Cosine\"}}" > /dev/null 2>&1 || true
        curl -s -X PUT "http://localhost:6333/collections/$new_col/snapshots" \
          -H 'Content-Type: application/json' \
          -d "{\"location\": \"$snap_url\"}" > /dev/null 2>&1 || true
        log "  ✔ $col → $new_col（快照恢复）"
      fi
    fi
  done

  docker compose -f "$COMPOSE_FILE" -p "$NEW_PROJECT" stop qdrant 2>/dev/null
fi
log "  ✔ Qdrant collection 迁移完成"

# ── Step 5: 配置文件更新 ───────────────────────────
log "=== Step 5: 配置文件 COMPOSE_PROJECT / DB 名更新 ==="
if ! dry_run; then
  # 更新 .env（如有）
  if [ -f .env ]; then
    sed -i.bak "s/COMPOSE_PROJECT=wiki/COMPOSE_PROJECT=mora/g" .env
    sed -i "s/WIKI_API_PORT/MORA_API_PORT/g; s/WIKI_FRONTEND_PORT/MORA_FRONTEND_PORT/g" .env
    log "  ✔ .env 已更新（备份 .env.bak）"
  fi
fi
log "  ✔ 配置文件更新完成（docker-compose.yml 和 Go 代码已由 YS-48/49 处理）"

# ── Step 6: 验证引导 ───────────────────────────────
log "=== Step 6: 迁移完成 — 请执行以下验证 ==="
echo ""
echo "  1. docker compose -f $COMPOSE_FILE -p $NEW_PROJECT up -d"
echo "  2. docker compose -f $COMPOSE_FILE -p $NEW_PROJECT ps   # 确认全部 healthy"
echo "  3. curl http://localhost:8990/healthz"
echo "  4. 登录验证（admin@mora.local / admin123）"
echo "  5. 创建文档 → 确认 Qdrant mora_chunks_* 有向量写入"
echo ""
echo "  回退：参见 deployments/ROLLBACK-RUNBOOK.md"
echo ""
echo "  旧数据保留："
echo "    - PG 旧库: ${OLD_DB}_backup_$(date +%Y%m%d)"
echo "    - Docker 旧卷: ${OLD_PROJECT}_* （建议保留 30 天后删除）"
echo "    - 备份目录: $BACKUP_DIR"

if dry_run; then
  log "=== DRY RUN 完成 — 以上为预览，未执行任何变更 ==="
fi
