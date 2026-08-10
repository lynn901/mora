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
#        make third-party-check — 第三方治理门禁：校验 lock.json / license / NOTICE（D8）
#        make sbom            — 生成 SBOM（CycloneDX，需 syft）
#        make notices         — 聚合生成 THIRD_PARTY_NOTICES.md
#        make third-party-all — third-party-check + sbom + notices（CI 门禁入口）

COMPOSE_FILE = deployments/docker-compose.yml
COMPOSE_PROJECT = mora

# 第三方治理门禁（决策书 §6 / D8）
THIRD_PARTY_SCRIPTS = deployments/scripts
SBOM_FORMAT = cyclonedx-json
SBOM_OUTPUT = third-party/sbom/mora-sbom.json

.PHONY: build up up-parser down logs ps restart reset backup restore export verify config \
        third-party-check sbom notices third-party-all

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

# ─── 第三方治理门禁（决策书 design-docs/13-phase0-contract-safety-baseline.md §6 / D8）───
# 校验 third-party/lock.json：结构完整、digest 固定、license 白名单通过、NOTICE 齐全。
# fail closed：违反门禁退出码 1，阻断发布。
third-party-check:
	@bash $(THIRD_PARTY_SCRIPTS)/third-party-check.sh

# 用 syft 生成 SBOM（CycloneDX-JSON）。syft 未安装时不阻断 dev（CI 中必须可用）。
sbom:
	@mkdir -p $(dir $(SBOM_OUTPUT))
	@if ! command -v syft >/dev/null 2>&1; then \
		echo "⚠ syft 未安装，跳过 SBOM 生成。"; \
		echo "  安装：https://github.com/anchore/syft#installation"; \
		echo "  CI 环境由 .github/workflows/third-party-gate.yml 保证 syft 可用。"; \
	else \
		syft dir:. -o $(SBOM_FORMAT) --file $(SBOM_OUTPUT); \
		echo "✓ SBOM 生成：$(SBOM_OUTPUT)"; \
	fi

# 从 lock.json + NOTICES/ 聚合生成 THIRD_PARTY_NOTICES.md（幂等）。
notices:
	@bash $(THIRD_PARTY_SCRIPTS)/generate-notices.sh

# CI 门禁入口：check → sbom → notices，全过才放行发布。
third-party-all: third-party-check sbom notices
	@echo "✓ 第三方治理门禁全部通过（lock / SBOM / NOTICE）"
