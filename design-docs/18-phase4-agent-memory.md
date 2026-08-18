# 18. Phase 4：Agent 记忆沉淀（Evidence / Extraction / 去重冲突 / 召回，首版不自动发布）

> 对应设计文档 `design-docs/11-human-agent-knowledge-blueprint.md` §4.2（Agent 记忆沉淀）、§6.2（Agent 任务后的记忆沉淀）、§6.3（检索与使用）；`design-docs/12-human-agent-knowledge-architecture.md` §4.4（Memory 表）、§8（Agent 记忆架构）、§10.3（Extraction Provider）、§11.3（MCP 工具）、§12.2（删除矩阵）、§16.5（Phase 4 计划与门禁）。承接 YS-98。
>
> 本文是**架构层交付物**：定义 Evidence 存储/脱敏/ACL、提炼管线、去重与冲突建议、Candidate Inbox、结构化召回与反馈、保留周期与删除传播的架构边界与不变量，并固化四项决策先于实现（Evidence 保留/删除传播/密钥轮换/ACL 策略）。**不含** Go handler / DB migration / 业务逻辑实现——这些由研发（`[@mora后端研发]`）落地。实现路径与现有 Phase 0–3 代码 seam 一一对齐，研发可照此编码。

## 0. 决策摘要

| # | 决策 | 结论 | 依据 / 权衡 |
|---|---|---|---|
| D1 | 表归属 | 新增迁移 `018_phase4_agent_memory.up/down.sql`，建 `memory_evidence` / `memory_units` / `memory_evidence_links` 三表；Memory 不新增 `asset_type`，`memory_units` 以 `asset_id` 引用既有 `knowledge_assets(asset_type='memory')`（013 已预留该枚举） | §0 决策 11「不增加一级资产类型」；12 §4.4 表骨架；013 `knowledge_assets.asset_type` 含 `memory`；复用 014 的 `knowledge_relations`/`review_requests`/`review_decisions`/`asset_projections`/`governance_profiles` |
| D2 | Evidence ACL 独立于 Memory 发布 | Evidence 复用 Phase 0 已预留的 `permissions.target_type='evidence'`（`domain.TargetEvidence`，rbac.go:60）；新增 `EvidenceLocator` 实现 `ResourceLocator`，按 owner + 来源资产当前 ACL 二次校验。发布 Memory 不写 Evidence ACL，不扩大可见范围 | 12 §4.4「证据权限独立于 Memory 发布权限」；附录 A 不变量 8「发布 Memory 不扩大 Evidence ACL」；避免发布即泄漏原文 |
| D3 | 保留周期与删除传播 | 新增 `memory_retention_policies` 表（workspace + memory_type 维度）；Evidence 到期先置 `expires_at`/`pending_purge` 再进删除队列；删除传播路径：Evidence 内容擦除（保留不可逆 hash + 审计 ID）→ `memory_units.evidence_missing=true` 并退出高权威召回 → 级联删 FTS 行 / Qdrant point / 摘要缓存 → 审计保留不可逆摘要。**删除传播路径与状态先于发布流程实现**（12 §12.2） | 12 §8.4、§12.2 删除矩阵「Memory Evidence」行；开放决策 §19.6「默认保留期限、密钥轮换和删除证明」本文先固化策略框架，具体期限值由 PM 治理 |
| D4 | 加密与密钥轮换 | 小片段（≤ 64KiB 脱敏后）加密存 `memory_evidence.encrypted_content`（BYTEA，AES-256-GCM，DEK 由 envelope KEK 包装，KEK 引用 `credential_ref` 不内嵌明文）；大对象存 MinIO 隔离前缀 `mora-evidence/<workspace>/<evidence_id>`，DB 只存 `storage_key` + `content_hash` + `redacted_excerpt`。密钥轮换：KEK 版本化（`key_version`），轮换不改密文，读取时按 `key_version` 解包；新写入用当前 KEK。轮换凭据走既有 Secret 管理器 | 12 §8.4「小片段可加密存 PG；大对象存 MinIO」；§19.6 密钥轮换；§7 安全优先「不硬编码密钥」；egress 审计 `external_call`（07 §5.4） |
| D5 | 提炼管线编排 | 新增 `memory_events` Stream + `memory_distill` 消费组（12 §6.2 已预留），事件 `evidence.captured` / `evidence.extract` / `evidence.revalidate`；knowledge-worker 进程内新增 `memory_distill` Job 分支，复用 `knowledge_jobs`（013）幂等 + 租约 + dead letter | 12 §6.2 Stream 划分「拆 Stream 的原因是 LLM 提炼延迟特征不同，不能互相阻塞」；013 `knowledge_jobs` + outbox 事务模式 |
| D6 | Extraction Provider 契约 | 实现 12 §10.3 `ExtractionProvider` 端口（`ExtractMemory` / `ClassifyRelation` / `Summarize` / `Health`）；Provider 返回受 JSON Schema 约束的 `MemoryCandidate`（`memory_type`/`statement`/`scope`/`validity`/`confidence`/evidence locator）；解析失败保留 Evidence 并重试，不写半结构化 Memory。首版默认 adapter = local TEI/Ollama，capability 在 Mora adapter 终止校验 | 12 §8.2「Extraction Provider 必须返回受 JSON Schema 约束的候选」；§10.3「capability 必须绑定 Evidence ID 和 extract 动作」；§7.2 防 prompt injection |
| D7 | 去重与冲突 | 去重/冲突只产**建议**，不自动合并。结构过滤（workspace + memory_type + 有效期 + 实体键）→ FTS/向量召回候选 → Provider `ClassifyRelation` 输出 `duplicate`/`extends`/`contradicts`/`unrelated`。`duplicate`/`extends` 落 `memory_units.superseded_by` 候选建议；`contradicts` 落 `knowledge_relations(relation_type='contradicts')`（014 已有该枚举）。团队资产的 merge/supersede/conflict resolution 必须由 reviewer 决定 | 12 §8.3 五条；014 `knowledge_relations.relation_type` 含 `supersedes`/`contradicts`；附录 A 不变量 9「团队 Memory 首版不能自动发布」 |
| D8 | 召回与反馈 | 实现 12 §9.4 `MemoryQuery.Recall`，返回标准 `KnowledgeCandidate`；排序遵循 §9.5 权威策略（决策原因意图下「审核 Memory 与证据」为首要依据）。`useful`/`incorrect`/`stale` 反馈落 `memory_feedback` 表，反馈不改事实正文，只影响 authority/freshness 排序与 revalidate 触发。未经审核团队 Memory 不进默认召回；私有候选只 owner 显式请求或审核视图可读 | 12 §8.5、§9.2 步骤 7、§9.5；11 §4.2「记忆默认是私有候选」；附录 A 不变量 9 |
| D9 | 写入入口 | 首版两个显式入口：`memory_remember`（Agent 显式提交结论 + 最小证据引用）+ 会话导入（用户/管理员选择会话后提交）。工具结果只保存完成结论所需的脱敏片段。**不实现透明 Proxy 自动捕获**（首版明确排除） | 12 §8.1「首版不提供透明 Proxy 自动捕获」；11 §6.2「第一阶段使用显式提交，避免必须代理全部模型流量」 |
| D10 | 模块组织 | 新增 `internal/module/memory/{evidence,distill,dedup,recall}`（12 §3.1 已预留目录）；复用 Phase 1 `knowledge-worker` Job dispatch、`outbox`、`authz`、`egress`；新增 `internal/infra/extractor`（Provider client，12 §3.1 已预留） | 12 §3.1 目录预留；14 §5.2 job dispatch 扩展模式 |

---

## 1. 范围与依赖

### 1.1 本文档覆盖

落地设计文档 12 §16.5 的四项交付（与 YS-98 issue 描述一致）：

1. **决策先于实现**：固化 Evidence 保留周期、删除传播、加密密钥轮换与 ACL 策略（本文 §0 D2–D4、§4、§7）。
2. **写入入口 + 存储 + 提炼**：`memory_remember` 显式提交入口 + 会话导入；`memory_evidence` / `memory_units` / `memory_evidence_links` 表（§2）；Extraction Provider（受 JSON Schema 约束的候选，§5）；Candidate Inbox（§6）。
3. **去重 / 冲突 / 发布 / 反馈 / 召回**：去重冲突建议（duplicate/extends/contradicts/unrelated）、人工发布、合并/supersede、反馈（useful/incorrect/stale）、结构化召回（§5、§6、§8）。
4. **Evidence 独立 ACL + 最小脱敏 + 删除传播**（§4、§7）。

### 1.2 依赖（Phase 0–3，已落地）

本文档假设以下基线已存在（已核对 migrations/013、014、016；代码 `internal/domain/`、`internal/platform/authz/`）：

- **013 knowledge_core**：`knowledge_assets` / `knowledge_asset_versions`（`asset_type` 含 `memory`）、`governance_profiles`、`agents` / `agent_bindings`、`knowledge_jobs` / `outbox_events` / `outbox_deliveries`、`workspace_authz_revisions`、`authorization_decisions`。
- **014 phase1_asset_source**：`knowledge_sources` / `source_sync_runs` / `knowledge_source_targets` / `knowledge_relations`（`relation_type` 含 `supersedes`/`contradicts`）/ `review_requests` / `review_decisions` / `asset_projections`（`projection_kind` 含 `fts`/`vector`/`summary`/`relation`）。
- **005 rbac**：`permissions`（`target_type` VARCHAR(20)，Phase 0 已预留 `evidence` 枚举值，`domain.TargetEvidence`）。
- **Phase 0 authz**：`ResourceLocator` 端口（`internal/platform/authz/locator.go`）、`CompositeLocator`、`asset_agent_locator.go`、`AuthzContext`。
- **knowledge-worker**：Job dispatch（14 §5.2：`source_sync` / `projection_build` / `asset_activate` / `reconcile_scan` / `legacy_backfill`；16 扩展 `wiki_*`；17 扩展 `codegraph_build`）。
- **Qdrant**：`mora_chunks_*` 集合前缀 + RBAC payload 过滤（03 §3）。
- **MCP 工具层**：`internal/module/mcp/tool/`（code/wiki/document/search/list 已落地，leak-safe empty result 模式）。

### 1.3 非目标

- 不实现 Skill 与 Agent 配装（Phase 5，§16.6）。
- 不实现透明 Proxy 自动捕获（首版明确排除；11 §6.2、12 §8.1）。
- 不实现工作空间级低风险自动发布（首版明确排除；11 §4.2「首版不自动发布团队记忆」；附录 A 不变量 9）。
- 不引入新 `asset_type`；Memory 是 `asset_type='memory'` 的 Knowledge Asset。
- 不在本阶段定各 workspace/memory_type 的具体保留期限数值（属 PM 治理，§19.6）；本文只固化策略框架与表结构。
- 不实现透明 Proxy 自动捕获（首版明确排除，YS-98 范围；12 §8.1、11 §6.2）。doc 15 §17.1 将其修订为「可选 Context Proxy 层，当 Agent 经路径 Token 接入时被动捕获」——该层是**未来第三个写入入口**，走本文 §5.3 同一 Evidence→Extract→Review 管线，不绕过审核；Phase 4 只固化管线，不实现被动捕获触发。

---

## 2. 数据架构

### 2.1 新增表：`memory_evidence`

原始证据 L0，独立 ACL（D2）。来源分五类（12 §4.4 `source_kind`）。

```sql
-- 018_phase4_agent_memory.up.sql（节选）
CREATE TABLE memory_evidence (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    owner_type        VARCHAR(20) NOT NULL,          -- user|group|agent|service_account
    owner_id          UUID NOT NULL,
    source_kind       VARCHAR(20) NOT NULL,          -- session|message|tool_call|document|code
    source_ref        TEXT NOT NULL,                 -- 不可执行定位（session_id / message_id / tool_call_id / asset_version_id 引用）
    source_asset_id         UUID,                    -- 引用 knowledge_assets(id)，不加 FK：来源删除走删除传播而非级联
    source_asset_version_id UUID,
    visibility        VARCHAR(20) NOT NULL DEFAULT 'private',  -- private|restricted
    captured_authz_revision BIGINT NOT NULL,         -- 入库时 workspace_authz_revisions.revision，仅供审计，不作今后授权
    content_hash      TEXT NOT NULL,                 -- 脱敏后内容的 SHA-256，用于去重与删除证明
    encrypted_content BYTEA,                         -- 小片段：AES-256-GCM 密文（D4）；NULL 则用 storage_key
    storage_key       TEXT,                          -- 大对象：MinIO key mora-evidence/<ws>/<id>（D4）
    key_version       INTEGER,                       -- envelope KEK 版本（D4 密钥轮换）；encrypted_content 非空时必填
    redacted_excerpt  TEXT NOT NULL,                -- 最小脱敏片段，无权读原文时返回此列
    classification   VARCHAR(40),                    -- 自动敏感分类标签（secret|credential|pii|none）
    retention_policy_id UUID REFERENCES memory_retention_policies(id),
    state             VARCHAR(20) NOT NULL DEFAULT 'active',  -- active|pending_purge|purged
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at        TIMESTAMPTZ,                   -- 保留策略到期点；到期先置 pending_purge
    purged_at         TIMESTAMPTZ,                   -- 内容擦除时间；purged 后只保留 id/hash/审计元数据
    deleted_at        TIMESTAMPTZ,
    CHECK (source_kind IN ('session','message','tool_call','document','code')),
    CHECK (visibility IN ('private','restricted')),
    CHECK (state IN ('active','pending_purge','purged')),
    CHECK ((encrypted_content IS NOT NULL AND storage_key IS NULL AND key_version IS NOT NULL)
        OR (encrypted_content IS NULL     AND storage_key IS NOT NULL))
);
CREATE INDEX idx_evidence_workspace_owner ON memory_evidence(workspace_id, owner_type, owner_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_evidence_source_asset ON memory_evidence(source_asset_id) WHERE source_asset_id IS NOT NULL AND deleted_at IS NULL;
CREATE INDEX idx_evidence_purge_due ON memory_evidence(expires_at) WHERE state = 'active' AND expires_at IS NOT NULL;
```

不变量：
- `source_asset_id` 不加 FK —— 来源资产删除走 §7.2 删除传播（标 `evidence_missing`），不级联删 Evidence。
- `captured_authz_revision` 仅供审计回溯，**不能**作为今后访问授权（12 §4.4）。
- `purged` 后 `encrypted_content`/`storage_key` 清空，只留 `content_hash` + `redacted_excerpt` + 审计 ID（12 §8.4「审计记录只保留不可逆摘要与 ID」）。

### 2.2 新增表：`memory_units`

提炼后的结构化记忆单元，以 `asset_id` 挂到 `knowledge_assets(asset_type='memory')`（D1）。

```sql
CREATE TABLE memory_units (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    asset_id          UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    asset_version_id  UUID REFERENCES knowledge_asset_versions(id) ON DELETE SET NULL,
    memory_type      VARCHAR(20) NOT NULL,           -- fact|decision|constraint|preference|event
    statement        TEXT NOT NULL,                  -- 自然语言结论（脱敏后）
    structured_payload JSONB NOT NULL DEFAULT '{}',  -- 实体键/有效期/scope（受 JSON Schema 约束）
    confidence       NUMERIC(5,4),
    valid_from       TIMESTAMPTZ,
    expires_at       TIMESTAMPTZ,
    state            VARCHAR(20) NOT NULL DEFAULT 'candidate',  -- candidate|approved|published|rejected|deprecated
    superseded_by    UUID,                            -- merge/supersede 候选建议指向；发布前由 reviewer 确认
    evidence_missing BOOLEAN NOT NULL DEFAULT false,  -- Evidence 删除/不可定位后置 true（D3）
    authority        NUMERIC(5,4) NOT NULL DEFAULT 0.5,  -- 召回排序用，反馈 + 治理状态影响
    created_by_type  VARCHAR(20) NOT NULL,
    created_by_id    UUID NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (memory_type IN ('fact','decision','constraint','preference','event')),
    CHECK (state IN ('candidate','approved','published','rejected','deprecated')),
    CHECK (NOT (state = 'published' AND superseded_by IS NOT NULL))
);
CREATE INDEX idx_units_workspace_state ON memory_units(workspace_id, state) WHERE state IN ('candidate','published');
CREATE INDEX idx_units_asset ON memory_units(asset_id, created_at DESC);
CREATE INDEX idx_units_type_time ON memory_units(workspace_id, memory_type, valid_from DESC);
CREATE INDEX idx_units_supersede ON memory_units(superseded_by) WHERE superseded_by IS NOT NULL;
CREATE INDEX idx_units_evidence_missing ON memory_units(workspace_id) WHERE evidence_missing = true;
-- 结构化键精确召回（12 §8.5）——实体键索引，受 structured_payload schema 约束
CREATE INDEX idx_units_structured ON memory_units USING gin (structured_payload);
```

不变量：
- `state='published'` 必须经 review_decision（§6.2），**首版无自动发布**（附录 A 不变量 9）。
- `evidence_missing=true` 的单元**不作为高权威依据**召回（12 §8.4），但仍可读脱敏引用与校验状态。
- `superseded_by` 仅在 reviewer 确认 merge/supersede 后由 governance 写入；去重冲突建议先落 `memory_dedup_suggestions`（§5.2），不直接改 `memory_units`。

### 2.3 新增表：`memory_evidence_links`

Memory ↔ Evidence 多对多链接，记录引用定位与支撑/冲突类型（12 §4.4）。

```sql
CREATE TABLE memory_evidence_links (
    memory_unit_id UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    evidence_id    UUID NOT NULL REFERENCES memory_evidence(id) ON DELETE CASCADE,
    quote_locator  JSONB,                            -- 不可执行引用定位（offset/range/hash），不含原文
    support_type   VARCHAR(20) NOT NULL DEFAULT 'supports',  -- supports|contradicts
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (memory_unit_id, evidence_id),
    CHECK (support_type IN ('supports','contradicts'))
);
CREATE INDEX idx_evidence_links_evidence ON memory_evidence_links(evidence_id);
```

### 2.4 新增表：`memory_retention_policies`

保留策略（workspace + memory_type 维度，D3）。具体期限值由 PM 治理（§19.6 开放决策），本文只定结构。

```sql
CREATE TABLE memory_retention_policies (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    memory_type   VARCHAR(20),                       -- NULL=该 workspace 所有类型默认
    retain_for    INTERVAL NOT NULL,                 -- 保留期限；到期先 pending_purge
    purge_after   INTERVAL,                          -- pending_purge 后宽限至硬擦除（审计 hash 保留）
    is_system     BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, memory_type)
);
-- 每个 workspace 补一条系统默认策略（迁移时对已存在 workspace）
INSERT INTO memory_retention_policies (workspace_id, memory_type, retain_for, purge_after, is_system)
SELECT id, NULL, INTERVAL '365 days', INTERVAL '30 days', true
FROM workspaces w
WHERE NOT EXISTS (SELECT 1 FROM memory_retention_policies p WHERE p.workspace_id = w.id AND p.memory_type IS NULL);
```

### 2.5 新增表：`memory_feedback`

`useful`/`incorrect`/`stale` 反馈（D8）。反馈不改事实正文，只影响 authority/freshness 与 revalidate 触发。

```sql
CREATE TABLE memory_feedback (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    memory_unit_id  UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    feedback_type   VARCHAR(20) NOT NULL,            -- useful|incorrect|stale
    given_by_type   VARCHAR(20) NOT NULL,
    given_by_id     UUID NOT NULL,
    rationale_redacted TEXT,                         -- 脱敏理由
    revalidate_triggered BOOLEAN NOT NULL DEFAULT false,  -- stale/incorrect 是否触发 revalidate Job
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (feedback_type IN ('useful','incorrect','stale'))
);
CREATE INDEX idx_feedback_unit ON memory_feedback(memory_unit_id, created_at DESC);
```

### 2.6 新增表：`memory_dedup_suggestions`

去重/冲突**建议**（不自动合并，D7）。reviewer 处置后落 `memory_units.superseded_by` 或 `knowledge_relations`。

```sql
CREATE TABLE memory_dedup_suggestions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    unit_a_id       UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    unit_b_id       UUID NOT NULL REFERENCES memory_units(id) ON DELETE CASCADE,
    suggestion_type VARCHAR(20) NOT NULL,            -- duplicate|extends|contradicts|unrelated
    origin          VARCHAR(20) NOT NULL DEFAULT 'generated',  -- rule|generated
    confidence      NUMERIC(5,4),
    evidence_ref    JSONB,                            -- 建议依据（召回分数/规则命中），不含原文
    state           VARCHAR(20) NOT NULL DEFAULT 'pending',  -- pending|accepted|rejected
    resolved_by_type VARCHAR(20),
    resolved_by_id  UUID,
    resolved_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (suggestion_type IN ('duplicate','extends','contradicts','unrelated')),
    CHECK (origin IN ('rule','generated')),
    CHECK (state IN ('pending','accepted','rejected')),
    CHECK (unit_a_id <> unit_b_id)
);
CREATE INDEX idx_dedup_pending ON memory_dedup_suggestions(workspace_id, state) WHERE state = 'pending';
```

### 2.7 与现有表的关系图（ER 要点）

```text
workspaces ─< knowledge_assets(asset_type='memory') >─ memory_units
                    │                          │
                    │                          └─< memory_evidence_links >─ memory_evidence
                    │                                                          │
                    └─ knowledge_asset_versions                          memory_retention_policies
                    └─ knowledge_relations(supersedes/contradicts)        memory_feedback
                    └─ review_requests / review_decisions                 memory_dedup_suggestions
                    └─ asset_projections(fts/vector/summary/relation)

permissions(target_type='evidence') ── EvidenceLocator ── memory_evidence (独立 ACL)
memory_events Stream ── memory_distill 消费组 ── knowledge_jobs ── ExtractionProvider
```

### 2.8 回滚迁移

`018_phase4_agent_memory.down.sql` 按反序 `DROP TABLE`：`memory_dedup_suggestions` → `memory_feedback` → `memory_retention_policies` → `memory_evidence_links` → `memory_units` → `memory_evidence`。回滚不删 `knowledge_assets(asset_type='memory')` 行（属业务数据，迁移只建结构）。

---

## 3. 模块与代码组织

### 3.1 目标目录（12 §3.1 已预留）

```text
internal/module/memory/
  evidence/    # 证据存储、脱敏、ACL（EvidenceLocator + EvidenceRepo + redaction）
  distill/     # 提炼候选（ExtractionProvider adapter + CandidateInboxService）
  dedup/       # 去重、冲突和合并建议（DedupService + ConflictService）
  recall/      # 结构化召回与反馈（MemoryQuery.Recall + FeedbackService）
  handler/     # REST handler（§7.1）
internal/infra/
  extractor/   # ExtractionProvider client（local TEI/Ollama adapter + external 通用模型 adapter）
  postgres/
    memory_evidence.go    # EvidenceRepo
    memory_unit.go        # MemoryUnitRepo
    memory_recall.go      # 召回查询（FTS + 向量混合）
internal/platform/authz/
  evidence_locator.go     # 新增：TargetEvidence 的 ResourceLocator（D2）
```

### 3.2 依赖规则（12 §3.2 扩展）

- `internal/module/memory/*` 只依赖 `internal/domain`、`internal/platform/{authz,outbox,egress}`、`internal/infra/{postgres,extractor,qdrant,objstore}`、`internal/pkg/*`；不反向依赖 `module/knowledge`、`module/mora`。
- `internal/module/mora/handler/` 可调用 `module/memory` 的 service 端口（REST 路由注册），不跨层访问 repo。
- `EvidenceLocator` 注册到既有 `CompositeLocator`（`authz/locator.go:62`），与 `DocLocator`/`asset_agent_locator` 并列。

### 3.3 knowledge-worker Job dispatch 扩展（14 §5.2 模式）

`mapKnowledgeEvent` 新增 `memory_events` Stream 消费分支（D5）：

| Stream | Consumer group | 事件类型 | 扇出 Job（`knowledge_jobs.job_type`） |
|---|---|---|---|
| `memory_events` | `memory_distill` | `evidence.captured` | `memory_extract`（提取候选） |
| `memory_events` | `memory_distill` | `evidence.extract` | `memory_dedup`（去重冲突建议） |
| `memory_events` | `memory_distill` | `evidence.revalidate` | `memory_revalidate`（过期/反馈触发重验） |

Job `dedupe_key` 复用 013 模式：`memory:{workspace}:{evidence_id}:{stage}:{input_set_hash}`。幂等：重投同 `dedupe_key` 命中既有 Job，不产生新候选。

---

## 4. Evidence 存储、脱敏与 ACL（D2/D4）

### 4.1 入库前脱敏门禁

Evidence 入库前必须执行（12 §8.4、11 §8.6）：

1. **Secret/凭据检测**：正则 + 已知 secret 模式扫描（API key / token / `password=` / 私钥头），命中即拒入库并审计 `evidence.secret_detected`。
2. **PII 检测**：邮箱/手机号/身份证等模式脱敏（替换为 `[REDACTED:email]`），保留 `classification='pii'`。
3. **超范围上下文裁剪**：只保存完成结论所需的片段，不默认保存完整模型请求/响应（11 §8.6）。
4. 脱敏后计算 `content_hash`（SHA-256），作为去重与删除证明依据。

### 4.2 存储分流（D4）

```text
脱敏后内容 size?
  ├─ ≤ 64KiB → AES-256-GCM 加密 → memory_evidence.encrypted_content（BYTEA）
  │            DEK 由 envelope KEK(key_version) 包装；KEK 引用 credential_ref，不内嵌明文
  └─ > 64KiB → MinIO mora-evidence/<workspace>/<evidence_id> → memory_evidence.storage_key
```

密钥轮换：KEK 版本化（`key_version`）。轮换不改已存密文；读取时按 `key_version` 解包 DEK；新写入用当前 KEK。轮换凭据走既有 Secret 管理器（与 `knowledge_sources.credential_ref` 同机制，不硬编码）。

### 4.3 Evidence ACL（D2，独立于 Memory 发布）

`memory_evidence_read` 读取校验链（12 §4.4）：

```text
reader 请求展开 Evidence e
  → 校验 Memory use/read（reader 对引用该 Evidence 的 memory_unit 的权限）
  ∩ 校验 Evidence read（permissions.target_type='evidence'，reader 对 e 的 ACL）
  ∩ 校验 source_asset 当前 ACL（e.source_asset_id → knowledge_assets 当前权限）
  → 三者全过 → 返回最小脱敏片段（redacted_excerpt 或按 quote_locator 裁剪）
  → 任一不过 → 返回脱敏引用 + evidence_type + 校验状态，原文不可展开
```

- 会话/消息/工具证据默认 `visibility='private'`，只 owner 显式分享可读（11 §8.3）。
- 文档/代码证据还必须通过引用资产当前权限（`source_asset_id` 的 RBAC）。
- `captured_authz_revision` 仅供审计，**不能**作为今后访问授权（附录 A 不变量 8）。
- 来源删除/不可定位 → 原文默认不可展开（`evidence_missing`），但已脱敏引用、证据类型、校验状态仍可见。

### 4.4 EvidenceLocator 端口（D2）

```go
// internal/platform/authz/evidence_locator.go
// EvidenceLocator resolves TargetEvidence → Location{WorkspaceID, OwnerID, SourceAssetID}
// for the authz decision pipeline (D2). Evidence ACL is independent of Memory
// publish; publishing a Memory never writes an Evidence permission row.
type EvidenceLocator struct {
    db querier // pgx
}

func (l *EvidenceLocator) Locate(ctx context.Context, t TargetType, id uuid.UUID) (Location, error) {
    // SELECT workspace_id, owner_type, owner_id, source_asset_id, visibility
    // FROM memory_evidence WHERE id=$1 AND deleted_at IS NULL
    // 返回 chain: [{evidence,id},{source_asset,source_asset_id},{workspace,workspace_id}]
    // source_asset 缺失（已删）→ 返回 evidence + workspace 节点，不阻塞脱敏引用读取
}
```

注册：`CompositeLocator.Register(domain.TargetEvidence, &EvidenceLocator{db})`（与 `DocLocator` 并列）。

---

## 5. Extraction Provider 契约与提炼管线（D5/D6）

### 5.1 Provider 端口（12 §10.3）

```go
// internal/module/memory/distill/provider.go
type ExtractionProvider interface {
    ExtractMemory(ctx context.Context, cap Capability, req ExtractRequest) ([]MemoryCandidate, error)
    ClassifyRelation(ctx context.Context, cap Capability, req RelationRequest) (RelationSuggestion, error)
    Summarize(ctx context.Context, cap Capability, req SummaryRequest) (Summary, error)
    Health(ctx context.Context) error
}
```

- Provider 配置声明 `local/external`、模型、数据等级上限、允许的 workspace。
- 处理 Evidence 时 `capability` 必须绑定 Evidence ID + `extract` 动作（12 §10.3）。
- 上游不识别 Mora capability 的通用模型 API → capability 在 Mora `extractor` adapter 终止并校验，adapter 用独立上游凭据调用。任何外部调用前经 Egress Policy + 脱敏审计 `external_call`（07 §5.4）。

### 5.2 MemoryCandidate JSON Schema（D6 受约束候选）

Provider 必须返回受 JSON Schema 约束的候选（12 §8.2）。Schema 落 `internal/module/memory/distill/schema.json`，service 落库前双层校验（Provider adapter 校验 + service 落库前校验；失败 Run 标 `failed` 不落候选）。

```jsonc
// MemoryCandidate schema（节选）
{
  "memory_type": "fact|decision|constraint|preference|event",
  "statement": "<自然语言结论，已脱敏>",
  "scope":      "<适用范围，不可执行>",
  "validity":   { "valid_from": "rfc3339", "expires_at": "rfc3339|null" },
  "confidence": 0.0-1.0,
  "entity_keys": { /* 结构化键，用于精确召回 */ },
  "evidence_locator": { "evidence_id": "uuid", "quote_locator": {} }
}
```

解析失败 → 保留 Evidence 并重试（`memory_events` 重投），**不写半结构化 Memory**（12 §8.2）。

### 5.3 提炼管线（D5，对齐 12 §8.2 流程图）

```text
memory_remember / 会话导入
  → Capture（§4.1 脱敏门禁）→ memory_evidence（L0，独立 ACL，state=active）
  → Outbox KEEvidenceCaptured → memory_events Stream
memory_distill 消费组
  → evidence.captured → memory_extract Job → ExtractionProvider.ExtractMemory
      → []MemoryCandidate → memory_units(state=candidate) + memory_evidence_links
      → Outbox KEEvidenceExtracted → memory_events
  → evidence.extract → memory_dedup Job → DedupService（§6）
      → memory_dedup_suggestions(state=pending) + 可选 knowledge_relations(contradicts)
  → Candidate Inbox（§6.3）→ reviewer approve/merge/reject/supersede
      → memory_units(state=published|rejected|deprecated) + review_decisions
      → asset_projections(fts/vector/summary) 投影构建
```

---

## 6. 去重 / 冲突 / 发布 / Candidate Inbox（D7）

### 6.1 去重与冲突流程（12 §8.3 五条）

```text
新 memory_unit 入候选
  1. 结构过滤：workspace + memory_type + 有效期 + entity_keys 命中既有候选
  2. FTS/向量召回候选（不直接自动合并）
  3. ExtractionProvider.ClassifyRelation → duplicate|extends|contradicts|unrelated
  4. 落 memory_dedup_suggestions(state=pending)
  5. reviewer 处置：
     - duplicate/extends → reviewer 确认 → memory_units.superseded_by（被替代单元 deprecated）
     - contradicts       → reviewer 确认 → knowledge_relations(relation_type='contradicts')（014 枚举）
     - unrelated         → 关闭建议
```

- 团队资产的 merge/supersede/conflict resolution **必须由 reviewer 决定**（12 §8.3 第 4 条；附录 A 不变量 9）。
- 冲突关系进入 `knowledge_relations`，检索时同时返回，**不静默覆盖**（12 §8.3 第 5 条；11 §6.4「高权威资产互相冲突时返回冲突及各自引用」）。

### 6.2 发布门禁（首版无自动发布）

`memory_units.state` 转换：`candidate → approved → published`（或 `rejected`/`deprecated`）。

- `approved`/`published` 必须有对应 `review_requests` + `review_decisions`（014 已有表，复用治理流程）。
- 首版 `governance_profiles.auto_publish` 对 `memory` 类型 **强制空/false**（11 §4.2「首版不自动发布团队记忆」）。
- 发布触发投影构建（`asset_projections`：`fts`/`vector`/`summary`），复用 Phase 1 投影机制。

### 6.3 Candidate Inbox

```text
GET /api/v1/memory/inbox            # 列出 pending 候选 + dedup_suggestions（reviewer 视图）
POST /api/v1/memory/units/{id}:approve|reject|merge|supersede|promote
  → approve/reject/supersede：写 review_decision + 翻 state
  → merge：合并两 unit，superseded_by 链，保留 evidence_links
  → promote：提升为 Document Asset 候选（跨 asset_type，走 Document 治理流程）
```

- 私有候选只能由 owner 在明确请求或审核视图中读取（12 §8.5）。
- inbox 列表按 `evidence_missing`、`suggestion_type=contradicts` 优先排序。

---

## 7. API 契约（REST + MCP）

### 7.1 REST 控制面（§11.1 子集，`cmd/mora-api` 既有 `authed` group）

```yaml
# api/memory.yaml（节选，对齐 12 §11.1）
paths:
  /api/v1/memory/evidence:           # POST 提交证据（memory_remember / 会话导入入口）
  /api/v1/memory/evidence/{id}/read: # POST 展开最小脱敏片段（§4.3 ACL 链；{id}/read 而非 {id}:read，同 wiki-spaces {id}/lint，避 Gin v1.12 单段双通配符 panic）
  /api/v1/memory/inbox:              # GET candidate inbox（§6.3）
  /api/v1/memory/units:              # GET 结构化召回（§8，owner/reviewer 视图）
  /api/v1/memory/units/{id}:approve|reject|merge|supersede|promote:  # POST reviewer 处置
  /api/v1/memory/units/{id}/feedback:# POST useful/incorrect/stale（§8.4）
  /api/v1/memory/dedup-suggestions:  # GET pending 建议列表
```

- 所有写操作走 Outbox → `memory_events` Stream（事务一致性，12 §6.1）。
- 读操作 fail closed：无权 → 404/empty（存在性不泄露，附录 A 不变量；code.go leak-safe 模式）。

### 7.2 内部 API（§11.2，doc 15 §12.3 复用）

```text
POST /internal/v1/memory/candidates      # 提交 Memory Candidate（供未来 Context Proxy / 其他服务异步投递）
GET  /internal/v1/memory/evidence/{id}   # 内部服务读 Evidence 元数据（脱敏；正文展开仍走 §4.3 ACL 链）
```

- 内部 API 使用短期委托上下文（Phase 0 `delegated_sessions` + `authorization_decisions`），不用共享 `INTERNAL_SERVICE_TOKEN` 绕过授权（对齐 16 §7.2）。
- `POST /internal/v1/memory/candidates` 幂等（`Idempotency-Key`），重复提交不产生重复 Candidate（doc 15 §15.1 不变量 12）。
- 被动捕获的 Candidate 与主动提交走同一条治理管线，不绕过 Review（doc 15 §15.1 不变量 4）。

### 7.3 MCP 工具（§11.3，`internal/module/mcp/tool/memory.go`）

新增四工具（12 §11.3 命名），沿用 `code.go` leak-safe empty result 模式：

```text
memory_recall         # 结构化召回：workspace/owner/type/time/validity/asset 过滤 + 混合召回
memory_remember       # Agent 显式提交结论 + 最小证据引用（写入入口，D9）
memory_evidence_read  # 只返回通过 Evidence ACL 校验的最小脱敏片段（§4.3）
memory_feedback       # useful/incorrect/stale，不改事实正文（D8）
```

- `memory_recall` 默认**不返回**未经审核团队 Memory（`state != 'published'` 不进默认召回）；私有候选只 owner 显式请求可读（12 §8.5）。
- `memory_evidence_read` 无权时返回脱敏引用 + evidence_type + 校验状态，不返回原文，不报错（leak-safe）。
- 管理型操作（发布团队 Memory、删除投影）**不进**默认 Agent 工具集（12 §11.3「管理型操作不进入默认 Agent 工具集」）。

### 7.4 事件信封（§6.1 复用）

```jsonc
// Outbox KEEvidenceCaptured 事件 payload
{
  "event_type": "evidence.captured",
  "workspace_id": "uuid",
  "evidence_id": "uuid",
  "source_kind": "tool_call",
  "content_hash": "sha256",
  "captured_authz_revision": 42
}
```

`destinations: ["memory_events"]`，Dispatcher 投递后写 `outbox_deliveries`（013 模式）。

---

## 8. 召回与反馈（D8）

### 8.1 MemoryQuery.Recall（12 §9.4 类型端口）

```go
type MemoryQuery interface {
    Recall(ctx context.Context, authz AuthzContext, query KnowledgeQuery) ([]KnowledgeCandidate, error)
}
```

召回能力（12 §8.5）：
- 过滤：workspace / owner / memory_type / 时间 / 有效性 / 关联资产。
- 混合召回：结构化键精确召回（`structured_payload` GIN）+ FTS（`statement`）+ 向量（Qdrant `mora_chunks_memory_*`）。
- 排序：evidence state（`evidence_missing` 降权）+ confidence + freshness + authority（受反馈影响）。
- 返回标准 `KnowledgeCandidate`（12 §9.3），`ProjectionRef` 仅供内部诊断，不返回 Provider 凭据/存储地址。

### 8.2 权威策略对齐（12 §9.5）

「决策原因」意图下，审核 Memory 与证据为首要依据；低置信或被替代记忆须展示冲突（12 §9.5 表）。Memory 召回结果携带 `Relations`（含 `contradicts`），不静默选择一个答案（11 §6.4）。

### 8.3 反馈处理（D8）

```text
memory_feedback 提交（useful/incorrect/stale）
  → 落 memory_feedback 表（不改 memory_units.statement）
  → useful  → authority 微升
  → incorrect/stale → authority/freshness 降 + 若 revalidate_triggered=true → Outbox KEEvidenceRevalidate → memory_events
  → memory_revalidate Job → ExtractionProvider 重验/标过期
```

反馈不直接修改事实正文（12 §8.5）。

---

## 9. 安全架构

### 9.1 Prompt injection 防护（门禁）

- Evidence 内容**不**作为 prompt 直接拼入模型；提炼只提交必要片段（11 §8.6）。
- Extraction Provider 输出受 JSON Schema 双层校验，失败 fail closed（§5.2）。
- Provider 不触碰 DB/对象存储/URL/Git；只接收脱敏输入快照 + 不可执行 Schema（对齐 16 §4.1 D2）。

### 9.2 删除传播（D3，12 §12.2 删除矩阵「Memory Evidence」行）

```text
Evidence 到期 / 显式删除
  → state=active → pending_purge（先停止展开，原文仍可审计）
  → purge_after 到期 → state=purged：擦除 encrypted_content / storage_key（MinIO 对象删除）
      保留 id / content_hash / redacted_excerpt / 审计元数据
  → 级联：memory_evidence_links 该 evidence 的链接
      → memory_units.evidence_missing=true（若无其他独立证据）
      → 该 unit 退出高权威召回（authority 降权，不删 statement）
  → 级联投影：FTS 行删除 / Qdrant point 删除 / 摘要缓存失效
  → 审计：external_call 审计记录只保留不可逆摘要与 ID（12 §8.4）
```

- 删除传播路径与状态**先于发布流程实现**（12 §12.2「删除传播路径和状态必须先于 Phase 4 实现」）。
- `source_asset` 删除 → 该 asset 引用的 Evidence 标 `evidence_missing`（§4.3），不级联删 Evidence（`source_asset_id` 无 FK）。

### 9.3 存在性不泄露

- 无权 Memory/Evidence → leak-safe empty（不报 403/404 区分存在性）；`memory_recall` 默认不召回无权单元（附录 A 不变量；code.go 模式）。
- 私有候选只 owner 显式请求或审核视图可读（12 §8.5）。

### 9.4 审计事件（§13.4）

| action | 触发 | 记录（脱敏） |
|---|---|---|
| `evidence.captured` | 入库 | evidence_id, source_kind, content_hash, classification |
| `evidence.read` | 展开 | evidence_id, reader, decision（allow/deny） |
| `evidence.purged` | 擦除 | evidence_id, content_hash, purged_at |
| `memory.published` | 发布 | unit_id, reviewer, review_decision_id |
| `memory.feedback` | 反馈 | unit_id, feedback_type, given_by |
| `external_call` | Extraction Provider 调用 | 目标, 数据摘要（脱敏）, 结果 |

---

## 10. 迁移（DB）

`migrations/018_phase4_agent_memory.up.sql` —— 建 §2.1–2.6 六表 + 索引 + 系统默认保留策略行；`down.sql` 反序 DROP。

依赖：`013_knowledge_core`（`knowledge_assets`/`workspaces`）、`014_phase1_asset_source`（`knowledge_relations`/`review_requests`/`review_decisions`/`asset_projections`）、`005_rbac`（`permissions`，`target_type='evidence'` 已预留）。

迁移只建结构 + 补系统行（`memory_retention_policies` 默认行），不写业务数据（对齐 013/014 §2 原则）。

---

## 11. 验收门禁对应（§16.5）

| 门禁（12 §16.5 / YS-98） | 实现位置 | 验证方式 |
|---|---|---|
| 团队 Memory 无自动发布 | `governance_profiles.auto_publish` 对 memory 强制空（§6.2）+ `memory_units.state` 转换须 review_decision | 测试：注入候选 → 无 reviewer 决策 → `memory_recall` 默认不召回该 unit |
| 每条已发布 Memory 可回溯证据 | `memory_evidence_links`（PK memory_unit+evidence）+ `memory_units.asset_version_id` | 查询：给定 published unit → 列出全部 evidence_id + quote_locator + 校验状态 |
| 删除传播路径有自动化测试 | §9.2 传播链（pending_purge → purged → evidence_missing → 投影删除） | 测试：purge Evidence → 链接 unit 标 evidence_missing → FTS/Qdrant point 不再命中 |
| 过期路径有自动化测试 | `memory_retention_policies.retain_for` + `expires_at` + `pending_purge` | 测试：置 expires_at 过期 → 对账任务置 pending_purge → 原文不可展开 |
| 冲突路径有自动化测试 | `memory_dedup_suggestions(contradicts)` → `knowledge_relations(contradicts)` | 测试：注入冲突候选 → 建议落 contradicts → recall 同时返回双方 + Relations |
| 撤权路径有自动化测试 | `workspace_authz_revisions` +1 → EvidenceLocator/Binding 缓存失效 | 测试：撤权后 reader 读 evidence → 返回脱敏引用，原文不可展开 |
| Evidence ACL 独立 + 最小脱敏 | `permissions(target_type='evidence')` + EvidenceLocator + §4.3 校验链 | 测试：发布 Memory 不写 Evidence ACL；无权 reader 只得 redacted_excerpt |
| 私有候选只 owner 可读 | `memory_evidence.visibility='private'` + recall 默认过滤 | 测试：非 owner 查 private 候选 → empty（存在性不泄露） |

---

## 12. 交付清单与角色分工

### 12.1 交付物（架构层，本文档）

- 迁移脚本 `migrations/018_phase4_agent_memory.up/down.sql`（§2）。
- ExtractionProvider 端口 + MemoryCandidate JSON Schema（§5）。
- EvidenceLocator 端口（§4.4）。
- REST 契约 `api/memory.yaml` + MCP 工具契约 `memory_recall/memory_remember/memory_evidence_read/memory_feedback`（§7）。
- 事件信封与 `memory_events` Stream 划分（§3.3、§7.3）。
- 模块目录与 Job dispatch 扩展（§3）。

### 12.2 角色分工

- **架构师（本文档）**：表结构、Provider 契约、Evidence ACL/脱敏/删除传播策略、提炼管线、去重冲突建议、召回排序、API/MCP 契约、保留周期策略框架。
- **后端研发（`[@mora后端研发]`）**：实现 `module/memory/{evidence,distill,dedup,recall,handler}`、`infra/extractor`、`infra/postgres/memory_*`、`authz/evidence_locator`；接入 knowledge-worker `memory_distill` Job dispatch；实现 JSON Schema 双层校验、脱敏门禁、删除传播对账任务、leak-safe empty result。
- **产品经理（`[@项目管理助手]`）**：各 workspace/memory_type 默认保留期限数值、Candidate Inbox 审核 UX、记忆类型默认治理值（11 §4.2 表）、低风险自动发布的未来开启条件——开放决策 §19.6。
- **测试工程师（`[@Mora知识库测试工程师]`）**：§11 全部门禁自动化测试（删除传播、过期、冲突、撤权、Evidence ACL、私有候选存在性不泄露）。
- **交付部署工程师（`[@Mora 交付部署工程师]`）**：`extractor` Provider 的模型/凭据配置（local TEI/Ollama 默认）、MinIO `mora-evidence/` 前缀、KEK 密钥管理接入。

---

## 13. 风险与缓解

| 风险 | 缓解 |
|---|---|
| Extraction Provider 输出绕过 JSON Schema 注入污染记忆 | 双层校验（adapter + service 落库前）；失败 Run 标 failed 不落候选；候选态 + 审核 + 置信度门禁 |
| 发布 Memory 扩大 Evidence ACL 泄漏原文 | Evidence ACL 独立（D2）；发布只写 memory_units/review_decisions，不写 permissions(target_type='evidence')；附录 A 不变量 8 |
| Evidence 含 Secret | 入库前脱敏门禁（§4.1：Secret/凭据/PII 检测 + 超范围裁剪）；命中即拒入库 + 审计 |
| 删除传播不完整导致残留原文/投影 | 删除矩阵（§9.2）逐级传播 + 对账任务扫描孤儿；purged 后只留 hash/审计 ID |
| 密钥轮换中断读取 | KEK 版本化（`key_version`），轮换不改密文，按版本解包；新写入用当前 KEK |
| 去重冲突静默覆盖 | 只产建议不自动合并（D7）；contradicts 进 knowledge_relations 并在召回返回；reviewer 决定 |
| 私有候选泄漏给团队 | recall 默认过滤 private + leak-safe empty；inbox 只 owner/reviewer 可读 |
| 提炼 LLM 延迟阻塞投影构建 | 独立 `memory_events` Stream + `memory_distill` 消费组（D5，12 §6.2 拆 Stream 理由） |
| 自动捕获含 Secret（未来） | 首版不实现透明 Proxy（D9）；未来启用前须来源/隐私/误写策略成熟 + 工作空间显式开启 |

---

> 本文档为 YS-98 Phase 4 架构层交付物，与 `design-docs/12` §16.5 门禁及附录 A 不变量 8/9 一致。迁移脚本、Provider 契约、Evidence ACL/脱敏/删除传播策略可直接交付研发（`[@mora后端研发]`）实现。各 workspace/memory_type 具体保留期限数值、Candidate Inbox 审核 UX、记忆类型默认治理值属开放决策（§19.6），待 PM（`[@项目管理助手]`）与架构共同产出后补入 §2.4。
