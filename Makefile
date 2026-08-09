# Makefile — Mora 平台一键部署命令
# 用法:  make build   — 构建所有镜像
#        make up      — 一键拉起（后台）
#        make down    — 停止服务
#        make logs    — 查看日志
#        make ps      — 查看容器状态
#        make restart — 重启全部
#        make reset   — 停止并清除数据卷（⚠ 清空所有数据）
#        make backup  — 备份数据
#        make restore — 恢复数据
#        make export  — 全量导出（迁移）
#        make verify  — 冒烟验证
#        make up-parser — 拉起 P2 多模态解析 sidecar（需先 make up）

COMPOSE_FILE = deployments/docker-compose.yml
COMPOSE_PROJECT = mora

.PHONY: build up up-parser down logs ps restart reset backup restore export verify config

build:
	docker compose -f $(COMPOSE_FILE) -p $(COMPOSE_PROJECT) build

up:
	docker compose -f $(COMPOSE_FILE) -p $(COMPOSE_PROJECT) up -d

# P2 多模态/复杂版式解析 sidecar（mora-parser：OCR/VLM/版式 PDF）。
# 启用后须在 .env 设 MORA_PARSER_URL=http://mora-parser:8000；详见 deployments/docker-compose.yml。
up-parser:
	docker compose -f $(COMPOSE_FILE) -p $(COMPOSE_PROJECT) --profile parser up -d

down:
	docker compose -f $(COMPOSE_FILE) -p $(COMPOSE_PROJECT) down

logs:
	docker compose -f $(COMPOSE_FILE) -p $(COMPOSE_PROJECT) logs -f

ps:
	docker compose -f $(COMPOSE_FILE) -p $(COMPOSE_PROJECT) ps

restart:
	docker compose -f $(COMPOSE_FILE) -p $(COMPOSE_PROJECT) restart

reset:
	@echo "⚠ 此操作将停止所有服务并删除所有数据卷！不可恢复！"
	@read -p "确认输入 yes: " CONFIRM; \
	if [ "$$CONFIRM" = "yes" ]; then \
		docker compose -f $(COMPOSE_FILE) -p $(COMPOSE_PROJECT) down -v; \
		echo "已清除所有数据"; \
	else \
		echo "已取消"; \
	fi

backup:
	./deployments/backup.sh

restore:
	@read -p "备份目录: " DIR; \
	./deployments/restore.sh "$$DIR"

export:
	./deployments/export.sh export

verify:
	./deployments/e2e-verify.sh

config:
	docker compose -f $(COMPOSE_FILE) -p $(COMPOSE_PROJECT) config
