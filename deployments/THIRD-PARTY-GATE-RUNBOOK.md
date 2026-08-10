# 第三方治理门禁 Runbook（D8）

> 决策书：`design-docs/13-phase0-contract-safety-baseline.md` §6（D8）
> Issue：YS-102

本 Runbook 描述 Mora 第三方治理门禁的资产、命令、CI 集成与违规处置流程。

## 1. 资产清单

| 路径 | 用途 |
|------|------|
| `third-party/lock.json` | 第三方组件锁定清单（name / source_url / commit_sha_or_digest / license / notice_path / capability） |
| `third-party/lock.schema.json` | lock.json 的 JSON Schema（结构校验依据） |
| `third-party/NOTICES/` | 各组件 NOTICE 文件副本 |
| `third-party/adr/0000-template.md` | 第三方引入 ADR 模板 |
| `third-party/adr/0001-codegraph-selection.md` | CodeGraph 引入决策（Phase 3 填充） |
| `third-party/adr/0002-skill-spec-baseline.md` | Skill 包格式基线（Hermes / agentskills.io） |
| `THIRD_PARTY_NOTICES.md` | 聚合后的 Third-Party Notices（由 `make notices` 生成） |
| `third-party/sbom/mora-sbom.json` | SBOM（CycloneDX-JSON，由 `make sbom` 生成，不入仓） |
| `deployments/scripts/third-party-check.sh` | 门禁校验脚本 |
| `deployments/scripts/generate-notices.sh` | 聚合生成脚本 |
| `.github/workflows/third-party-gate.yml` | CI 门禁流水线 |

## 2. 命令

| 命令 | 作用 | 退出码 |
|------|------|--------|
| `make third-party-check` | 校验 lock.json 结构 / digest / license 白名单 / NOTICE 与 ADR 存在 | 0 通过 / 1 fail closed |
| `make sbom` | 用 `syft` 生成 CycloneDX SBOM；syft 缺失则告警不阻断 dev | 0 |
| `make notices` | 从 lock.json + NOTICES/ 聚合生成 `THIRD_PARTY_NOTICES.md`（幂等） | 0 |
| `make third-party-all` | 上述三者串行（CI 门禁入口） | 任一失败则非 0 |

> 本地无 `syft` 时 `make sbom` 仅告警跳过；CI 由 `.github/workflows/third-party-gate.yml` 保证 `syft` 可用，缺失即失败。

## 3. CI 门禁集成（§6.4）

流水线 `.github/workflows/third-party-gate.yml` 在以下时机触发：

- `push` 到 `main`/`master`，且 `third-party/**`、门禁脚本、Makefile 或工作流本身有改动。
- 任意 `pull_request` 触及以上路径。
- `workflow_dispatch` 手动触发。

门禁步骤：

1. 安装 `syft` 与 `jq`。
2. `make third-party-check` —— fail closed，违反即退出 1。
3. `make sbom` —— 生成 CycloneDX SBOM。
4. `make notices` —— 重新聚合 `THIRD_PARTY_NOTICES.md`。
5. 校验 `THIRD_PARTY_NOTICES.md` 与 lock.json 一致（`git diff --exit-code`），不一致则失败，提示本地跑 `make notices` 并提交。
6. 上传 SBOM 为 artifact（保留 30 天）。

**门禁标准（§6.4）**：lock.json 无漂移、digest 一致、license 白名单通过、NOTICE 齐全。违反则 fail closed，阻断发布。

## 4. License 策略

`third-party/lock.json` `license_policy`：

- **allowlist**（默认通过）：Apache-2.0、MIT、BSD-2-Clause、BSD-3-Clause、ISC、MPL-2.0、0BSD、CC0-1.0、CC-BY-4.0、CC-BY-SA-4.0。
- **review_required**（需 ADR 评审）：LGPL-2.1-only、LGPL-3.0-only、EPL-2.0。
- **denylist**（fail closed，禁止）：GPL-2.0-only、GPL-2.0-or-later、GPL-3.0-only、GPL-3.0-or-later、AGPL-3.0-only、AGPL-3.0-or-later、SSPL、BUSL-1.1、Unlicense、`none`。

> copyleft / AGPL 触发传染性义务或商业限制，默认 fail closed。需引入者必须走 ADR 评审，并在评审中论证合规影响与缓解。

## 5. 新增第三方组件流程

1. 复制 `third-party/adr/0000-template.md`，编号递增，填实候选对比、决策、license 合规影响、固定基线。
2. 在 `third-party/lock.json` `components` 数组追加一条：必填 `name / source_url / commit_sha_or_digest / license / notice_path / capability`，建议填 `status / selection_phase / adr`。
3. 把组件的 NOTICE 文本放入 `third-party/NOTICES/<name>.<license>.txt`。
4. 本地跑 `make third-party-all`，确认全绿。
5. 提交 PR，CI 门禁自动复核；`THIRD_PARTY_NOTICES.md` 不一致会在 CI 失败，本地 `make notices` 后重新提交。

## 6. 违规处置

| 违规 | 表现 | 处置 |
|------|------|------|
| digest 为空或 `TBD` | `third-party-check` fail closed | 固定具体 commit/digest 后再发布 |
| license 在 denylist | `third-party-check` fail closed | 不得引入；如确需，走 ADR 评审并改 license 策略（需架构师批准） |
| license 不在 allowlist | `third-party-check` fail closed | 纳入白名单或改用合规替代 |
| NOTICE 文件缺失 | `third-party-check` fail closed | 补 `third-party/NOTICES/<name>.<license>.txt` |
| ADR 缺失（声明了 adr 字段） | `third-party-check` fail closed | 补 ADR 文件 |
| `THIRD_PARTY_NOTICES.md` 漂移 | CI 一致性检查失败 | 本地 `make notices` 重新生成并提交 |
| `syft` 缺失（CI） | CI 安装步骤失败 | 流水线自动安装；如失败检查网络与 install 脚本 |

## 7. Phase 0 基线说明

Phase 0 只固化**决策框架与门禁**，不做 CodeGraph 实际选型（那是 Phase 3 / YS-97）。
首批固定两条参考基线：

- **CodeGraph**（npm `codegraph` 1.5.0，MIT，commit `c6aaa20`）—— `reference_baseline_only`。
- **Hermes / Agent Skills 参考**（`agentskills.io/<spec-version>` spec，Hermes Agent commit `cd4317b4...`，MIT）—— `reference_baseline_only`，Mora 不承担 Runtime 执行。

后续 Phase（1/3）引入新依赖时，必须走「ADR + lockfile + digest + NOTICE」流程，不能跳过。
