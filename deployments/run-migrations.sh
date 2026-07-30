#!/bin/sh
# deployments/run-migrations.sh
# 幂等执行 PostgreSQL 迁移：用 schema_migrations 表记录已应用版本，重复
# `docker compose up` 不会重复执行。按文件名顺序应用 0??_*.up.sql（001-010）。
# 在 postgres:16 镜像中运行（含 psql + sh）。
set -e

: "${PGHOST:=postgres}"
: "${PGPORT:=5432}"
: "${PGUSER:=wiki}"
: "${PGDATABASE:=wiki}"
export PGPASSWORD="${POSTGRES_PASSWORD:-wiki}"

PSQL="psql -h $PGHOST -p $PGPORT -U $PGUSER -d $PGDATABASE -v ON_ERROR_STOP=1"

# 等待 postgres 就绪
until $PSQL -c "SELECT 1" >/dev/null 2>&1; do
  echo "migrate: waiting for postgres..."
  sleep 1
done

$PSQL -c "CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());"

applied=0
for f in /migrations/0??_*.up.sql; do
  [ -f "$f" ] || continue
  v=$(basename "$f" .up.sql)
  if $PSQL -tAc "SELECT 1 FROM schema_migrations WHERE version='$v'" | grep -q 1; then
    echo "migrate: skip $v (already applied)"
    continue
  fi
  echo "migrate: applying $v"
  $PSQL -f "$f"
  $PSQL -c "INSERT INTO schema_migrations (version) VALUES ('$v') ON CONFLICT DO NOTHING;"
  applied=$((applied + 1))
done

echo "migrate: done ($applied newly applied, $(ls /migrations/0??_*.up.sql | wc -l) total)"
