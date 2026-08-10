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
#
# 第三方治理门禁 (design-docs/13 §6, D8):
#        make third-party-check — 校验 lock.json 漂移 / license / NOTICE，fail-closed
#        make sbom               — 用 syft 生成 CycloneDX SBOM（容器化，无需本地装 syft）
#        make notices            — 聚合生成 THIRD_PARTY_NOTICES.md
#        make third-party-sync   — 从 go.sum / package-lock.json 重新生成 lock.json

COMPOSE_FILE = deployments/docker-compose.yml
COMPOSE_PROJECT = mora

# Third-party governance gate (design-docs/13 §6, D8). Image-pinned for reproducibility.
SYFT_IMAGE ?= anchore/syft:v1.27.1
SBOM_FILE ?= mora.sbom.cdx.json
SBOM_FORMAT ?= cyclonedx-json

.PHONY: build up up-parser down logs ps restart reset backup restore export verify config \
        third-party-check sbom notices third-party-sync

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

# ─── 第三方治理门禁 (design-docs/13 §6, D8) ────────────────────────────────────
# Fail-closed: returns non-zero on any drift / license violation / missing NOTICE,
# so CI can block publish. Does not require go / node / syft locally — only jq.
third-party-check:
	@./third-party/check.sh

# Regenerate the third-party lockfile from go.sum + web/package-lock.json.
# Re-run after any direct-dependency bump, review the diff, and commit alongside
# the ADR. Not gated by CI (it is a maintainer tool, not a publish check).
third-party-sync:
	@./third-party/sync-lock.sh

# Generate a CycloneDX SBOM with syft (pinned image — no local install needed).
# Scans go.mod + web/package.json. The repo is mounted read-only (scan only);
# syft writes to stdout and we redirect to the host-side artifact, so no writable
# volume is needed. The SBOM is a build artifact: git-ignored.
sbom:
	@echo "Generating SBOM ($(SBOM_FORMAT)) via $(SYFT_IMAGE) …"
	@mkdir -p .out
	@docker run --rm -v "$(CURDIR):/src:ro" -w /src $(SYFT_IMAGE) \
		'/src' -o $(SBOM_FORMAT) >.out/$(SBOM_FILE) 2>/tmp/syft-err \
		|| (echo "  ✗ syft failed (docker required, image $(SYFT_IMAGE)):"; cat /tmp/syft-err; exit 1)
	@echo "  ✓ wrote .out/$(SBOM_FILE) ($$(wc -c < .out/$(SBOM_FILE)) bytes, $$(jq '.components|length' .out/$(SBOM_FILE)) components)"

# Aggregate the redistribution-ready Third-Party Notices file.
# THIRD_PARTY_NOTICES.md is a build artifact: git-ignored; regenerate before release.
notices:
	@./third-party/generate-notices.sh

