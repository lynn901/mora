# Rollback Runbook — wiki → mora 迁移回退

> 当 wiki→mora 迁移失败或出现问题时，按本 Runbook 回退。
> 迁移脚本：`deployments/migrate-wiki-to-mora.sh`
> 备份目录：`backup/pre-migration-*`（迁移脚本 Step 1 自动创建）

## 前置条件

- 备份目录存在（`backup/pre-migration-<timestamp>/`）
- 旧 Docker 卷未被删除（`wiki_*` 前缀卷）
- 旧 PG 库未被删除（`wiki_backup_*`）

## 一键回退

```bash
# 在仓库根目录执行
bash deployments/rollback-migration.sh backup/pre-migration-<timestamp>
```

## 手动分步回退

### Step 1: 停止 mora 服务

```bash
docker compose -f deployments/docker-compose.yml -p mora down
```

### Step 2: PostgreSQL 回退

```bash
# 启动 postgres（用旧项目名）
docker compose -f deployments/docker-compose.yml -p wiki up -d postgres
sleep 5

# 恢复旧库名
docker compose -f deployments/docker-compose.yml -p wiki exec -T postgres \
  psql -U wiki -d postgres -c "ALTER DATABASE wiki_backup_<date> RENAME TO wiki;"

# 或从 dump 恢复
docker compose -f deployments/docker-compose.yml -p wiki exec -T postgres \
  psql -U wiki -d postgres -c "DROP DATABASE IF EXISTS wiki; CREATE DATABASE wiki OWNER wiki;"
gunzip -c backup/pre-migration-*/wiki_pg_dump.sql.gz | \
  docker compose -f deployments/docker-compose.yml -p wiki exec -T postgres psql -U wiki -d wiki

docker compose -f deployments/docker-compose.yml -p wiki stop postgres
```

### Step 3: Docker 卷回退

```bash
# 删除 mora 新卷（如已创建）
docker volume rm mora_pg_data mora_valkey_data mora_qdrant_data 2>/dev/null || true

# 旧卷 wiki_* 仍在，直接使用旧 COMPOSE_PROJECT=wiki 启动即可
```

### Step 4: Qdrant collection 回退

```bash
docker compose -f deployments/docker-compose.yml -p wiki up -d qdrant
sleep 3

# 从快照恢复 wiki_chunks_* 集合
for snap in $(curl -s http://localhost:6333/collections | python3 -c "import sys,json; [print(s['name']) for s in json.load(sys.stdin)['result']['collections']]" 2>/dev/null); do
  col=$(echo $snap | sed 's/_snapshot.*//')
  curl -s -X PUT "http://localhost:6333/collections/$col/snapshots" \
    -H 'Content-Type: application/json' \
    -d "{\"location\": \"http://localhost:6333/collections/$col/snapshots/$snap\"}"
done

docker compose -f deployments/docker-compose.yml -p wiki stop qdrant
```

### Step 5: 配置文件回退

```bash
# 恢复 .env
cp backup/pre-migration-*/.env.before .env

# 恢复 docker-compose.yml（如被修改）
cp backup/pre-migration-*/docker-compose.yml.before deployments/docker-compose.yml
```

### Step 6: 用旧标识重启

```bash
docker compose -f deployments/docker-compose.yml -p wiki up -d
docker compose -f deployments/docker-compose.yml -p wiki ps
curl http://localhost:8990/healthz
```

## 回退验证

- [ ] `docker ps` 全部 healthy
- [ ] `curl http://localhost:8990/healthz` → `{"status":"ok"}`
- [ ] 登录 admin@wiki.local / admin123 成功
- [ ] 文档列表正常加载
- [ ] 全文检索返回结果

## 注意事项

- 回退后代码也需切回迁移前的 commit（`git checkout <pre-migration-commit>`）
- 如 Go module path 已改（`github.com/wiki/wiki-backend` → `github.com/lynn901/mora`），需重新编译旧二进制
- 建议在维护窗口内执行回退，预留 30 分钟
