#!/usr/bin/env bash
# deployments/export.sh
# Wiki 全量数据导出与迁移工具（AC-23）。
# 导出全部：PG 数据 + Qdrant 向量索引（快照）+ 配置信息，打包为 tar.gz。
# 可在另一私有实例通过 import 子命令恢复。
#
# 用法:
#   ./deployments/export.sh export  [output.tar.gz]  — 导出全量数据
#   ./deployments/export.sh import  <input.tar.gz>   — 在目标实例恢复
set -euo pipefail

COMPOSE_FILE="deployments/docker-compose.yml"
COMPOSE_PROJECT="wiki"
TEMP_DIR="/tmp/wiki-export-$$"

cleanup() { rm -rf "$TEMP_DIR"; }
trap cleanup EXIT

export_data() {
  local OUTPUT="${1:-wiki-export-$(date +%Y%m%d_%H%M%S).tar.gz}"
  mkdir -p "$TEMP_DIR"

  echo "=== Wiki 全量导出 → $OUTPUT ==="

  # 1. PG 元数据
  echo "--- 导出 PostgreSQL 数据 ---"
  docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T postgres \
    pg_dump -U wiki -d wiki --no-owner --no-acl \
    > "$TEMP_DIR/wiki_pg_dump.sql"
  echo "  ✔ PostgreSQL dump saved"

  # 2. Qdrant 快照
  echo "--- 导出 Qdrant 向量索引 ---"
  QDRANT_CONTAINER=$(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" ps -q qdrant)
  if [ -n "$QDRANT_CONTAINER" ]; then
    COLLECTIONS=$(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T qdrant \
      curl -sf http://localhost:6333/collections 2>/dev/null | \
      python3 -c "import json,sys; r=json.load(sys.stdin); [print(c['name']) for c in r.get('result',{}).get('collections',[])]" 2>/dev/null || true)
    for col in $COLLECTIONS; do
      echo "  snapshot collection: $col"
      docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T qdrant \
        curl -sf -XPOST "http://localhost:6333/collections/$col/snapshots" > /dev/null 2>&1 || true
      docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T qdrant \
        bash -c "ls -t /qdrant/storage/snapshots/$col/*.snapshot 2>/dev/null | head -1" | \
        xargs -I {} sh -c "docker cp $QDRANT_CONTAINER:{} $TEMP_DIR/${col}_snapshot.snapshot" 2>/dev/null || true
    done
  fi

  # 3. 清单与版本
  {
    echo "export_time: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo "schema_version: 1"
    echo "collections:"
    for col in $COLLECTIONS; do echo "  - $col"; done
  } > "$TEMP_DIR/manifest.yaml"

  # 4. 打包
  tar czf "$OUTPUT" -C "$TEMP_DIR" .
  echo "  ✔ 导出完成: $(du -h "$OUTPUT" | cut -f1)"
}

import_data() {
  local INPUT="$1"
  if [ ! -f "$INPUT" ]; then
    echo "错误: 文件不存在: $INPUT"
    exit 1
  fi

  echo "=== Wiki 全量导入 ← $INPUT ==="

  # 确认
  echo "⚠ 导入将覆盖当前所有数据！确认继续？(yes/no)"
  read -r CONFIRM
  if [ "$CONFIRM" != "yes" ]; then
    echo "已取消"
    exit 0
  fi

  mkdir -p "$TEMP_DIR"
  tar xzf "$INPUT" -C "$TEMP_DIR"

  # 1. 恢复 PG
  if [ -f "$TEMP_DIR/wiki_pg_dump.sql" ]; then
    echo "--- 恢复 PostgreSQL ---"
    docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T postgres \
      psql -U wiki -d wiki -v ON_ERROR_STOP=1 < "$TEMP_DIR/wiki_pg_dump.sql"
    echo "  ✔ PostgreSQL 恢复完成"
  fi

  # 2. 恢复 Qdrant
  local QDRANT_CONTAINER
  QDRANT_CONTAINER=$(docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" ps -q qdrant)
  if [ -n "$QDRANT_CONTAINER" ]; then
    echo "--- 恢复 Qdrant 向量索引 ---"
    for snap in "$TEMP_DIR"/*_snapshot.snapshot; do
      [ -f "$snap" ] || continue
      local col
      col=$(basename "$snap" _snapshot.snapshot)
      local snap_name="${col}_import.snapshot"
      docker cp "$snap" "$QDRANT_CONTAINER:/qdrant/storage/${snap_name}"
      # 通过 REST API 恢复 snapshot
      docker compose -f "$COMPOSE_FILE" -p "$COMPOSE_PROJECT" exec -T qdrant \
        curl -sf -XPOST "http://localhost:6333/collections/$col/snapshots/upload?priority=snapshot" \
        -F "snapshot=@/qdrant/storage/${snap_name}" > /dev/null 2>&1 && \
        echo "  ✔ $col 恢复完成" || echo "  ⚠ $col 恢复失败（集合可能已存在）"
    done
  fi

  echo "=== 导入完成 ==="
  echo "建议重启 rag-worker 以重建索引状态: docker compose restart rag-worker"
}

case "${1:-help}" in
  export)
    export_data "${2:-}"
    ;;
  import)
    if [ -z "${2:-}" ]; then
      echo "用法: $0 import <export.tar.gz>"
      exit 1
    fi
    import_data "$2"
    ;;
  *)
    echo "Wiki 全量数据导出/迁移工具"
    echo ""
    echo "用法:"
    echo "  $0 export  [output.tar.gz]  导出当前实例全量数据"
    echo "  $0 import  <input.tar.gz>   在目标实例恢复数据"
    echo ""
    echo "示例:"
    echo "  $0 export wiki-backup-20260731.tar.gz"
    echo "  scp wiki-backup-20260731.tar.gz new-server:/data/"
    echo "  $0 import wiki-backup-20260731.tar.gz"
    ;;
esac
