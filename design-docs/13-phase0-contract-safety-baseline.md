# Phase 0 实施架构：契约与安全基线

> 对应 `design-docs/12-human-agent-knowledge-architecture.md` §16.1，蓝图 §12 Phase 0。
> 本文是 Phase 0 的**实施层架构决策书**，把 §19 中与本阶段相关的开放决策（#1 完整 SQL、#2 ResourceLocator 兼容、#3 delegated session 签名）落到研发可直接实现的契约。
> 架构师负责本文；研发同学按本文实现，测试 / 交付部署按 §8 / §9 设计矩阵与门禁。

## 0. 决策摘要

| # | 决策 | 结论 | 依据 / 权衡 |
|---|---|---|---|
| D1 | 核心表归属与编号 | 新增迁移 `013_knowledge_core.up/down.sql`，8 张控制面表，不 backfill 生产 | §16.1 明确"先用测试 Asset 验证授权，不做生产 backfill" |
| D2 | `ResourceLocator` 形态 | 引入 `platform/authz` 端口 + 多定位器组合，**不重写** `rbac.Engine`，现有 `engine.locate/targetChain` 改为定位器的一个适配 | §5.4 "先引入通用 ResourceLocator 端口，再增加资产和来源定位，避免继续扩大 switch" |
| D3 | 授权服务位置 | 新增 `platform/authz.Service`，RBAC Engine 退为其子策略；保留 `VisibleDocuments` 行为 | §5.3 决策流水线 + 不变量 4/5 |
| D4 | principal/target/action 扩展 | 原地扩 `domain/rbac.go` 枚举，新增动作语义不破坏 `read`/`write`/`admin` 蕴含链 | §5.4 "扩展现有类型，不另建平行 ACL" |
| D5 | delegated session 签名 | 复用 HS256 JWT（与 `auth.TokenManager` 同算法），自定义 claims；JTI 落 `delegated_sessions` 服务端记录 | §5.1/§5.6 + 复用现有 JWT 基座，不引入非对称密钥运维 |
| D6 | Outbox 与现有 Stream 关系 | 新增 `knowledge_events` Stream + 事务 Outbox；`doc_events` 暂不动，文档写事务双写 Knowledge Outbox 事件 | §6.3 "Phase 0 为新知识事件引入 Outbox…旧 RAG 发布器继续服务 doc_events" |
| D7 | MCP 内部调用鉴权 | 内部请求仍带服务身份（mTLS/共享 token 仅证明服务），**叠加**签名 delegated context；`X-Identity-Id` 头不再单独可信 | §11.2 "INTERNAL_SERVICE_TOKEN 不能单独代表最终用户权限" |
| D8 | 第三方治理门禁 | Makefile 目标 + CI 门禁；lockfile（`third-party-lock.json`）+ SBOM（syft）+ NOTICE 生成；首批固定 CodeGraph / Hermes 基线 | §16.1 第 7 项 |

## 1. 与现有代码的差距核对（基线）

依据 §1.2，并经代码核对确认：

| 差距 | 代码现状 | Phase 0 目标 |
|---|---|---|
| 主体类型 | `domain.SubjectType` 仅 `user`/`group`；`rbac_mcp.go` `IdentityType` 仅 `user`/`service_account` | 增 `agent`；统一主体模型 |
| 目标类型 | `domain.TargetType` 仅 `workspace`/`directory`/`document` | 增 `asset`/`source`/`agent`/`review`/`evidence` |
| 动作 | `Action` 仅 `read`/`write`/`admin` | 增 `use`/`assign`/`share`/`review`/`sync`，且 `read` 不蕴含 `use` |
| 授权定位 | `engine.locate`/`targetChain` 硬编码 3 种目标 switch | `ResourceLocator` 端口 + 多定位器 |
| MCP→API 鉴权 | `handler.AuthMiddleware` 信任 `X-Identity-Id` 头，仅靠共享 `INTERNAL_SERVICE_TOKEN` | 短期 delegated context，服务端按 JTI 校验 |
| 事件骨架 | `domain.DocEvent` + 单 Stream `doc_events`；无事务 Outbox | 新增统一 envelope + `knowledge_events` Stream + Outbox + `knowledge_jobs` |
| Agent / Binding | 无表、无域类型 | 8 张核心表 + `agents`/`agent_bindings`/delegated session |
| 第三方治理 | 无 lockfile / SBOM / NOTICE 门禁 | Makefile + CI 目标，首批基线固定 |

**回归红线**（§16.1 门禁"现有文档/RAG/MCP 文档工具行为回归无退化"）：现有 `documents`/`document_versions`/`permissions`/`audit_logs`/`doc_events` 行为不变；现有 `rbac.Engine.Check`/`VisibleDocuments` 的输入输出契约不变，只是其内部定位改为走 `ResourceLocator`。

## 2. 数据架构：核心表骨架（D1）

迁移文件：`migrations/013_knowledge_core.up.sql` / `013_knowledge_core.down.sql`。
原则：只建表 + 约束 + 索引，**不写数据**；存量文档迁移是 Phase 1 的事。

### 2.1 表结构

```sql
-- 013_knowledge_core.up.sql
-- Phase 0 控制面核心表（12 §4.2–4.3、§4.6、§5.6）
-- 依赖：001 users/service_accounts、002 workspaces、003 documents/document_versions、005 rbac

-- 工作区授权 revision（撤权线性化点，§5.6）
CREATE TABLE workspace_authz_revisions (
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    revision     BIGINT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id)
);
-- 每个工作区一行；revision 单调递增，由同一事务负责 +1

-- 治理 Profile（§4.2）
CREATE TABLE governance_profiles (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                VARCHAR(200) NOT NULL,
    asset_type          VARCHAR(20),              -- document|codebase|memory|skill|NULL=通用
    transition_rules    JSONB NOT NULL DEFAULT '{}',
    review_roles        JSONB NOT NULL DEFAULT '[]',
    auto_publish        JSONB NOT NULL DEFAULT '{}',
    default_validity    INTERVAL,
    evidence_required   BOOLEAN NOT NULL DEFAULT false,
    required_projections JSONB NOT NULL DEFAULT '[]', -- [fts|vector|summary|codegraph|relation]
    is_system           BOOLEAN NOT NULL DEFAULT false,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);
CREATE INDEX idx_gov_profiles_workspace ON governance_profiles(workspace_id);

-- 知识资产（§4.2）
CREATE TABLE knowledge_assets (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id             UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    asset_type              VARCHAR(20) NOT NULL,   -- document|codebase|memory|skill
    name                    VARCHAR(500) NOT NULL,
    description             TEXT,
    owner_type              VARCHAR(20) NOT NULL,   -- user|group|agent|service_account
    owner_id                UUID NOT NULL,
    status                  VARCHAR(20) NOT NULL DEFAULT 'draft',
    visibility              VARCHAR(20) NOT NULL DEFAULT 'private',
    governance_profile_id   UUID REFERENCES governance_profiles(id),
    native_document_id      UUID REFERENCES documents(id),  -- 仅 document 类型非空
    current_version_id      UUID,                  -- 自引用，建表后加 FK
    latest_requested_version_no BIGINT NOT NULL DEFAULT 0,
    confidence              NUMERIC(5,4),
    valid_from              TIMESTAMPTZ,
    expires_at              TIMESTAMPTZ,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (asset_type, native_document_id) WHERE native_document_id IS NOT NULL
);
CREATE INDEX idx_assets_workspace ON knowledge_assets(workspace_id, asset_type);
CREATE INDEX idx_assets_owner ON knowledge_assets(owner_type, owner_id);
CREATE INDEX idx_assets_status ON knowledge_assets(status) WHERE status NOT IN ('archived','rejected');
-- current_version_id 自引用 FK（表建完后追加）
ALTER TABLE knowledge_assets
  ADD CONSTRAINT fk_assets_current_version
  FOREIGN KEY (current_version_id) REFERENCES knowledge_asset_versions(id) DEFERRABLE INITIALLY DEFERRED;
-- DEFERRABLE: 版本激活 CAS 在同一事务内先写 versions 再写 assets.current_version_id

-- 资产版本（§4.2）
CREATE TABLE knowledge_asset_versions (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id                  UUID NOT NULL REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    version_no                BIGINT NOT NULL,
    source_id                 UUID,                  -- Phase 1 才有 source 表，先建列不加 FK
    source_revision           TEXT,
    native_document_version_id UUID REFERENCES document_versions(id),
    content_origin            VARCHAR(20) NOT NULL DEFAULT 'human', -- human|imported|generated|system
    generation_ref            JSONB,
    provider_ref              JSONB,
    content_hash              TEXT,
    dedupe_key                TEXT NOT NULL,
    build_status              VARCHAR(20) NOT NULL DEFAULT 'pending',
    governance_status        VARCHAR(20) NOT NULL DEFAULT 'candidate',
    activation_policy_snapshot JSONB,
    approved_by_type          VARCHAR(20),
    approved_by_id            UUID,
    approved_at               TIMESTAMPTZ,
    created_by_type           VARCHAR(20) NOT NULL,
    created_by_id             UUID NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (asset_id, version_no),
    UNIQUE (asset_id, dedupe_key),
    UNIQUE (native_document_version_id) WHERE native_document_version_id IS NOT NULL
);
CREATE INDEX idx_versions_asset ON knowledge_asset_versions(asset_id, version_no DESC);
CREATE INDEX idx_versions_build ON knowledge_asset_versions(build_status) WHERE build_status IN ('pending','building');
CREATE INDEX idx_versions_governance ON knowledge_asset_versions(governance_status);

-- Agent（§4.3）
CREATE TABLE agents (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name              VARCHAR(255) NOT NULL,
    description       TEXT,
    owner_id          UUID NOT NULL REFERENCES users(id),
    status            VARCHAR(20) NOT NULL DEFAULT 'active',  -- active|suspended|revoked
    runtime_type      TEXT,
    service_account_id UUID REFERENCES service_accounts(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_agents_workspace ON agents(workspace_id) WHERE status = 'active';
CREATE INDEX idx_agents_owner ON agents(owner_id);

-- Agent Binding（§4.3）
CREATE TABLE agent_bindings (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    agent_id          UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    workspace_id      UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    scope_kind        VARCHAR(20) NOT NULL,   -- asset|workspace|asset_type
    asset_id          UUID REFERENCES knowledge_assets(id),
    asset_type        VARCHAR(20),
    effect            VARCHAR(10) NOT NULL DEFAULT 'allow',  -- allow|deny
    version_policy    VARCHAR(20) NOT NULL DEFAULT 'follow_published', -- follow_published|pinned
    pinned_version_id UUID REFERENCES knowledge_asset_versions(id),
    delivery_mode     VARCHAR(20) NOT NULL DEFAULT 'tool',   -- tool|summary|inline
    priority          INTEGER NOT NULL DEFAULT 0,
    created_by        UUID REFERENCES users(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at        TIMESTAMPTZ,
    CHECK (
        (scope_kind = 'asset' AND asset_id IS NOT NULL)
        OR (scope_kind = 'asset_type' AND asset_type IS NOT NULL)
        OR (scope_kind = 'workspace')
    ),
    CHECK (NOT (version_policy = 'pinned' AND pinned_version_id IS NULL)),
    CHECK (NOT (version_policy = 'pinned' AND scope_kind <> 'asset'))
);
CREATE INDEX idx_bindings_agent ON agent_bindings(agent_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_bindings_workspace ON agent_bindings(workspace_id) WHERE revoked_at IS NULL;

-- 委托会话（§5.1/§5.6）——服务端可撤销记录，客户端只持 JTI
CREATE TABLE delegated_sessions (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_id              UUID REFERENCES api_tokens(id) ON DELETE CASCADE,
    agent_id              UUID REFERENCES agents(id) ON DELETE CASCADE,
    acting_user_id       UUID REFERENCES users(id),
    workspace_id          UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    allowed_actions       JSONB NOT NULL DEFAULT '[]',   -- 允许动作集合
    issued_authz_revision BIGINT NOT NULL,
    expires_at            TIMESTAMPTZ NOT NULL,
    revoked_at            TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_delegated_sessions_token ON delegated_sessions(token_id) WHERE revoked_at IS NULL;
CREATE INDEX idx_delegated_sessions_agent ON delegated_sessions(agent_id) WHERE revoked_at IS NULL;

-- 授权决策记录（§5.6，审计与 Provider capability 校验用）
CREATE TABLE authorization_decisions (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    authz_revision  BIGINT NOT NULL,
    principal_type  VARCHAR(20) NOT NULL,
    principal_id    UUID NOT NULL,
    acting_user_id  UUID REFERENCES users(id),
    agent_id        UUID REFERENCES agents(id),
    action          VARCHAR(20) NOT NULL,
    scope_hash      TEXT NOT NULL,      -- 规范化授权范围的 hash，防篡改
    audience        TEXT,               -- 目标 Provider/内部服务
    nonce_hash      TEXT,               -- 单次 nonce 的 hash
    expires_at      TIMESTAMPTZ NOT NULL,
    consumed_at     TIMESTAMPTZ,
    revoked_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_authz_decisions_workspace ON authorization_decisions(workspace_id, authz_revision);
CREATE INDEX idx_authz_decisions_lookup ON authorization_decisions(workspace_id, principal_type, principal_id) WHERE revoked_at IS NULL;

-- 事务 Outbox（§6.3）
CREATE TABLE outbox_events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregate_type  VARCHAR(40) NOT NULL,
    aggregate_id    UUID NOT NULL,
    event_type      VARCHAR(80) NOT NULL,
    event_version   INT NOT NULL DEFAULT 1,
    workspace_id    UUID,
    actor_type      VARCHAR(20),
    actor_id        UUID,
    destinations    TEXT[] NOT NULL DEFAULT '{}',
    payload         JSONB NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ,
    attempt         INT NOT NULL DEFAULT 0,
    last_error      TEXT
);
CREATE INDEX idx_outbox_unpublished ON outbox_events(occurred_at) WHERE published_at IS NULL;

CREATE TABLE outbox_deliveries (
    outbox_event_id   UUID NOT NULL REFERENCES outbox_events(id) ON DELETE CASCADE,
    stream            TEXT NOT NULL,
    delivery_attempt  INT NOT NULL DEFAULT 1,
    delivered_at      TIMESTAMPTZ,
    last_error        TEXT,
    PRIMARY KEY (outbox_event_id, stream)
);

-- 知识任务（§6.5，Phase 0 只建表与基础库）
CREATE TABLE knowledge_jobs (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_event_id   UUID,                  -- 触发本 Job 的 outbox_events.id（不加 FK，允许跨保留期）
    job_type          VARCHAR(60) NOT NULL,
    asset_id          UUID REFERENCES knowledge_assets(id) ON DELETE CASCADE,
    asset_version_id UUID REFERENCES knowledge_asset_versions(id) ON DELETE CASCADE,
    source_id         UUID,
    target_key        TEXT,
    build_revision    TEXT,
    dedupe_key        TEXT NOT NULL UNIQUE,
    status            VARCHAR(20) NOT NULL DEFAULT 'pending',
    attempt           INT NOT NULL DEFAULT 0,
    max_attempt       INT NOT NULL DEFAULT 5,
    lease_owner       TEXT,
    lease_until       TIMESTAMPTZ,
    progress          JSONB,
    error_code        VARCHAR(60),
    error_detail_redacted TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_jobs_status ON knowledge_jobs(status, lease_until) WHERE status IN ('pending','running');
```

down 迁移按反序 `DROP TABLE`。

### 2.2 关键不变量与约束说明

- **`workspace_authz_revisions`**：每个 workspace 恒有一行（首次建 workspace 时由触发器或应用初始化写入 `revision=0`）。Phase 0 给现有 `workspaces` 补一个 backfill 语句：`INSERT INTO workspace_authz_revisions(workspace_id, revision) SELECT id, 0 FROM workspaces ON CONFLICT DO NOTHING;`（这是元数据补齐，不是业务数据 backfill）。
- **`current_version_id` 自引用 + `DEFERRABLE INITIALLY DEFERRED`**：版本激活在同一事务内先 `INSERT knowledge_asset_versions`，再 `UPDATE knowledge_assets SET current_version_id=...`。不加 DEFERRABLE 会因引用顺序失败。
- **`latest_requested_version_no` 单调栅栏**：自动激活 SQL 固定用 `UPDATE knowledge_assets SET current_version_id=$1 WHERE id=$2 AND latest_requested_version_no=$3 AND current_version_id IS NOT DISTINCT FROM $4`（§6.4 CAS）。
- **`dedupe_key` 防重**：Document 用 `document_version:{version_id}`；Phase 0 测试 Asset 用 `test:{asset_id}:{vno}`。`NULL` 唯一语义问题靠 `WHERE ... IS NOT NULL` 偏索引解决。
- **物理删除策略**：所有控制面表默认软删除（`status`/`revoked_at`/`archived`）；`outbox_events`/`outbox_deliveries` 保留 30 天后由对账任务清理（§6.3）。

## 3. ResourceLocator 与统一 Authorization Service（D2/D3）

### 3.1 目标

把 `rbac.Engine` 中硬编码的 `locate`/`targetChain`（`internal/platform/rbac/engine.go:144-192`）抽象为端口，使新增 `asset`/`source`/`evidence` 等目标类型时不必继续扩大 switch，同时**不破坏**现有 `Check`/`VisibleDocuments` 契约。

### 3.2 ResourceLocator 端口

新增 `internal/platform/authz/locator.go`：

```go
// Package authz is the unified authorization layer (12 §3.1, §5).
// It owns the AuthzContext, the decision pipeline, and the ResourceLocator
// port. The legacy rbac.Engine becomes one strategy under this layer.
package authz

// ResourceLocator resolves a target (type+id) into an authoritative location
// within a workspace: the workspace it belongs to and an ordered chain of
// ancestor nodes from most-specific to least-specific, used by the decision
// pipeline for inheritance resolution.
//
// Implementations MUST be side-effect free (read-only) and MUST NOT leak
// the existence of a target the caller cannot see: resolving a non-existent
// or non-visible target returns ErrTargetNotFound, indistinguishable from
// "no permission" to the caller (存在性不泄露).
type ResourceLocator interface {
    Locate(ctx context.Context, targetType TargetType, targetID uuid.UUID) (loc Location, err error)
}

// Location is the resolved position of a target.
type Location struct {
    WorkspaceID uuid.UUID
    // Chain is the evaluation order, most-specific first. Each node carries
    // a target type+id that grants may attach to. For a document this is
    // [document, directory, ..., workspace-root, workspace]; for a workspace
    // it is [workspace]; for an asset it is [asset, workspace].
    Chain []Node
}

type Node struct {
    Type TargetType
    ID   uuid.UUID
}

var ErrTargetNotFound = errors.New("authz: target not found or not visible")
```

### 3.3 定位器组合（CompositeLocator）

```go
// CompositeLocator routes Locate by target type to a registered child locator.
// Adding a new target type = register a new child locator; no switch grows.
type CompositeLocator struct {
    children map[TargetType]ResourceLocator
}

func (c *CompositeLocator) Locate(ctx context.Context, t TargetType, id uuid.UUID) (Location, error) {
    child, ok := c.children[t]
    if !ok {
        return Location{}, fmt.Errorf("%w: unsupported target type %s", ErrTargetNotFound, t)
    }
    return child.Locate(ctx, t, id)
}
```

Phase 0 注册的子定位器：

| 子定位器 | 目标类型 | 链构造 | 复用现有实现 |
|---|---|---|---|
| `docLocator` | `workspace`/`directory`/`document` | 复刻 `engine.locate`+`targetChain` 的逻辑 | 是，迁移自 `rbac/engine.go` |
| `assetLocator` | `asset` | 查 `knowledge_assets.workspace_id`，链 `[asset, workspace]` | 新增 |
| `agentLocator` | `agent` | 查 `agents.workspace_id`，链 `[agent, workspace]` | 新增 |
| `evidenceLocator` | `evidence` | 解析 `memory_evidence` 的 workspace/owner/来源资产（Phase 4 表先空实现，Phase 0 仅占位） | 占位 |

`docLocator` 的 `Repository` 接口沿用 `rbac.Repository`（`DirectoryAncestors`/`DocumentLocation`/`DocumentsInDirectorySubtree`），保证现有 `internal/infra/postgres/rbac.go` 的 `GrantsFor`/`DocumentLocation` 实现零改动。

### 3.4 统一 Authorization Service

新增 `internal/platform/authz/service.go`：

```go
// Service is the authorization decision point (12 §5.3). It composes the
// decision pipeline: lifecycle gate -> RBAC/ACL -> Agent use -> Binding
// allow/deny -> optional task scope -> (Provider capability is issued
// separately as a signed decision, see §4).
type Service struct {
    locator  ResourceLocator
    rbac     *rbac.Engine       // legacy engine, now a sub-strategy for read/write/admin on doc/directory/workspace
    binding  BindingRepo        // agent_bindings 读取
    agents   AgentRepo          // agents 状态
    revisions RevisionRepo      // workspace_authz_revisions
    decisions DecisionRepo     // authorization_decisions 写入（用于 Provider capability）
}

// Authorize is the linearization point (12 §5.6): it reads the current
// workspace authz revision and decides in one DB snapshot.
func (s *Service) Authorize(ctx context.Context, req AuthzRequest) (AuthzContext, error)

// VisibleAssets filters a set of asset IDs down to those the principal may use,
// for search/listing (存在性不泄露). Analogous to rbac.VisibleDocuments.
func (s *Service) VisibleAssets(ctx context.Context, req ListScope) (visibilitySet, error)

// IssueDecision records an authorization_decision and returns a signed
// short-lived capability for a Provider to validate (audience, nonce, expiry).
func (s *Service) IssueDecision(ctx context.Context, req AuthzRequest, audience string) (DecisionCapability, error)
```

决策流水线（对应 §5.3 mermaid，落地为代码顺序）：

```text
1. lifecycle gate:  资产/版本 status 允许 action？(asset 表 status; version governance_status)
2. RBAC/ACL:        principal 在 workspace 内对目标链的 read/write/admin/use 授权？
3. Agent use:       若 principal_type=agent，agent_bindings 含目标 asset 且 effect=allow？
4. Binding deny:    agent_bindings 显式 deny 优先于所有 allow
5. task scope:      Phase 0 不启用（§19 #12 未决），预留接口位
6. (Provider capability 由 IssueDecision 单独签发，§4)
```

不变量：`Binding 只能缩小 Agent 可用范围，不能赋予 acting principal 原本没有的权限`（附录 A #4）。即第 3 步 Agent use 必须与第 2 步 RBAC 求交：`agent` 主体自主调用时，其 `service_account_id` 的 RBAC 权限 ∩ Binding allow；代表用户调用时，`acting_user_id` 的 RBAC ∩ Binding allow。

### 3.5 迁移路径与回归保证

- `internal/platform/rbac/engine.go` 的 `Check`/`VisibleDocuments` **签名与行为不变**；内部把 `locate`+`targetChain` 委托给注入的 `ResourceLocator`（默认 `docLocator`）。现有 `engine_test.go` 必须全绿——这是回归门禁。
- `handler`/`service`/`search` 各处调用 `rbac.Engine.Check` 的代码**不需要改动**；新代码（asset/agent 路径）直接调 `authz.Service`。
- 迁移分两步提 PR（研发执行）：① 引入 `authz` 包 + `CompositeLocator` + `docLocator`，`rbac.Engine` 内部改委托，跑全量回归；② 接入 `assetLocator`/`agentLocator` 与 `authz.Service` 的 asset/agent 分支。

## 4. 身份与授权扩展（D4/D5）

### 4.1 domain 类型扩展

`internal/domain/rbac.go` 原地扩展（不新建文件，不破坏旧枚举）：

```go
// Action 扩展
const (
    ActionRead   Action = "read"
    ActionWrite  Action = "write"
    ActionAdmin  Action = "admin"
    // 新增：Phase 0
    ActionUse    Action = "use"      // Agent 调用资产
    ActionAssign  Action = "assign"  // 绑定资产到 Agent
    ActionShare  Action = "share"    // 分享证据/资产
    ActionReview Action = "review"   // 治理审核
    ActionSync   Action = "sync"     // 来源同步
)

// SubjectType 扩展
const (
    SubjectUser          SubjectType = "user"
    SubjectGroup         SubjectType = "group"
    SubjectAgent         SubjectType = "agent"           // 新增
    SubjectServiceAccount SubjectType = "service_account" // 新增
)

// TargetType 扩展
const (
    TargetWorkspace TargetType = "workspace"
    TargetDirectory TargetType = "directory"
    TargetDocument  TargetType = "document"
    // 新增：Phase 0
    TargetAsset    TargetType = "asset"
    TargetSource   TargetType = "source"
    TargetAgent    TargetType = "agent"
    TargetReview   TargetType = "review"
    TargetEvidence TargetType = "evidence"
)
```

动作语义规则（写入 `hasAction` 逻辑，§5.4）：

- `admin` 蕴含 `read`+`write`（不变）。
- `write` 蕴含 `read`（不变）。
- **`read` 不蕴含 `use`**（新增硬规则）：`hasAction(use)` 必须显式匹配 `use` 或 `admin`（governance profile 可决定 admin 是否蕴含敏感证据 `use`，默认不）。
- `api_tokens.identity_type` 枚举值新增 `'agent'`，此时 `identity_id=agents.id`（§4.3）。`migrations/008_mcp.up.sql` 已用 `VARCHAR(20)` 无 CHECK 约束，无需改表结构；只需应用层 `IdentityType` 增加 `IdentityAgent`。

### 4.2 AuthzContext（§5.2 落地）

`internal/platform/authz/context.go`，字段与 §5.2 一致；`AllowedAssetIDs` 为空时表示"workspace 级可见，由服务端 decision_id 引用"，避免大集合传输。

### 4.3 Delegated Session（D5）

**签名格式**：复用 `auth.TokenManager` 的 HS256（同一 `JWT_SECRET`），自定义 claims，避免引入非对称密钥的密钥分发运维。新增 `internal/platform/authz/delegated.go`：

```go
// DelegatedClaims is the JWT payload for a short-lived delegated context (12 §5.1).
// The client (MCP Server) holds ONLY the JTI-bearing JWT; the authoritative
// revocable record lives server-side in delegated_sessions.
type DelegatedClaims struct {
    SessionID    string   `json:"sid"`   // delegated_sessions.id (JTI 来源)
    AgentID      string   `json:"aid,omitempty"`
    ActingUserID string   `json:"uid,omitempty"`
    WorkspaceID  string   `json:"wsid"`
    Actions      []string `json:"act"`   // allowed_actions
    AuthzRevision int64   `json:"rev"`   // issued_authz_revision
    Audience     string   `json:"aud"`   // 目标 Provider/内部服务
    jwt.RegisteredClaims
}

// IssueDelegated signs a delegated JWT AND inserts the server-side
// delegated_sessions row in the same transaction. Expiry <= 30s (§5.6).
func (m *DelegatedManager) IssueDelegated(ctx context.Context, req DelegatedRequest) (token string, err error)

// VerifyDelegated validates signature+expiry, THEN loads delegated_sessions
// by SessionID to confirm not revoked and authz_revision still current.
// It does NOT trust the JWT's own claims alone — the server-side row is authoritative.
func (m *DelegatedManager) VerifyDelegated(ctx context.Context, token string) (*DelegatedClaims, error)
```

**关键不变量**：
- `acting_user_id` 不能由请求头声明；中间件校验签名后仍按 JTI 读 `delegated_sessions`（§5.1）。
- 有效期 ≤ 30 秒（§5.6），与 Provider capability 一致。
- 撤销：`UPDATE delegated_sessions SET revoked_at=now()` + 同事务 `workspace_authz_revisions.revision+1`；其后新请求读到新 revision 即拒绝（§5.6）。

### 4.4 MCP 内部调用改造（D7）

**现状问题**（`internal/module/mora/handler/middleware.go:36`）：`INTERNAL_SERVICE_TOKEN` 匹配即信任，`X-Identity-Id` 头可伪造，缺失时默认 admin。

**改造**：

1. `mcp-server` 调用 mora-api 前先调 `POST /internal/v1/authz/delegated`（新增内部端点）获取 delegated JWT，携带 `Authorization: Bearer <delegated_jwt>` + `X-Service-Identity: mcp-server`（服务身份证明，仍可用 mTLS 或共享 token，但仅证明"这是 mcp-server 服务"，不代表最终用户）。
2. `mora-api` 的 `AuthMiddleware`：
   - 收到 delegated JWT → `DelegatedManager.VerifyDelegated` → 校验服务端 session 未撤销 + revision 当前 → 构造 `AuthState`（UserID=acting_user_id，AgentID，Actions）。
   - 仍保留 `INTERNAL_SERVICE_TOKEN` 作为**服务身份**的鉴权（证明调用方是受信内部服务），但**不再单独代表最终用户权限**：缺 delegated context 的内部服务调用只能以 service account 自身权限行动（capability 受限），不再 fallback admin。
3. 兼容期：`X-Identity-Id` 头**废弃**，读取后忽略并写 deprecation 审计；一个发布周期后移除。

**接口契约**（`design-docs/04-api-contract.md` 补充，研发实现）：

```text
POST /internal/v1/authz/delegated
  Auth: Bearer <mcp-service-token>   # 服务身份
  Body: { agent_id, acting_user_id?, workspace_id, actions[], audience }
  Resp: { delegated_token, expires_at, session_id }
  幂等: Idempotency-Key 支持
```

## 5. 事件骨架：Outbox + 统一 Envelope + knowledge_jobs（D6）

### 5.1 统一事件信封

新增 `internal/domain/knowledge_event.go`（与现有 `DocEvent` 并存，不互相污染）：

```go
// KnowledgeEvent is the unified event envelope (12 §6.1). Carries only IDs,
// revisions, actions and necessary params — never content, credentials,
// full sessions or Skill packages.
type KnowledgeEvent struct {
    EventID       string         `json:"event_id"`       // 全局幂等键
    EventType     string         `json:"event_type"`     // e.g. "asset.version.requested"
    EventVersion  int            `json:"event_version"`
    AggregateType string         `json:"aggregate_type"` // "knowledge_asset" | "agent" | ...
    AggregateID   uuid.UUID      `json:"aggregate_id"`
    WorkspaceID   *uuid.UUID     `json:"workspace_id,omitempty"`
    Actor         EventActor     `json:"actor"`          // {type, id}
    CorrelationID *uuid.UUID     `json:"correlation_id,omitempty"`
    CausationID   *uuid.UUID     `json:"causation_id,omitempty"`
    OccurredAt    time.Time      `json:"occurred_at"`
    Payload       map[string]any `json:"payload,omitempty"`
}
```

事件类型枚举（Phase 0 首批，资产/版本/权限/治理/Agent）：

```text
asset.created / asset.version.requested / asset.version.activated / asset.deprecated
permission.changed / governance.decision
agent.created / agent.suspended / agent.binding_changed / agent.use_denied
authz.revision_changed
```

### 5.2 Stream 划分

Phase 0 只启用 `knowledge_events`（§6.2），其余 Stream 在对应 Phase 接入。`knowledge_events` consumer group = `knowledge_projection`。复用 `internal/infra/mq/valkey.go` 的 `ValkeyQueue` 模式，新增 `knowledgeEventsQueue`（不重写 `doc_events` 逻辑）。

### 5.3 事务 Outbox（§6.3 落地）

新增 `internal/platform/outbox/dispatcher.go` + `internal/platform/outbox/store.go`：

```go
// Package outbox implements the transactional outbox (12 §6.3).
// Producers write outbox_events IN THE SAME TX as aggregate state changes;
// the Dispatcher polls unpublished events with FOR UPDATE SKIP LOCKED and
// publishes to target Streams, recording outbox_deliveries.
type Dispatcher struct {
    db      *postgres.DB
    streams map[string]StreamPublisher  // stream name -> publisher
    batch   int
    interval time.Duration
}

// Record is called inside the producer's transaction: it inserts an
// outbox_events row using the SAME pgx.Tx. No separate publish call.
func (s *Store) Record(tx pgx.Tx, ev KnowledgeEvent, destinations []string) error

// Poll claims unpublished events and publishes them. All required streams
// delivered -> writes published_at. Uses FOR UPDATE SKIP LOCKED.
func (d *Dispatcher) Poll(ctx context.Context) error
```

**双写协议**（§6.3，Phase 0 落地点）：文档写事务（`documents` create/update/delete/permission_change）在现有 `doc_events` 发布之外，**同事务**写一条 Knowledge Outbox 事件 `asset.*`（destinations 仅 `knowledge_events`）。旧 RAG 发布器继续服务 `doc_events`，互不阻塞。Phase 1 backfill 对账后，再把 RAG 文档事件迁到同一 Dispatcher。

> ⚠️ Phase 0 不实现 backfill；双写事件在 Phase 0 期会进入 `knowledge_events` 但没有 consumer 消费资产视图（consumer 在 Phase 1 接入）。这是预期行为：先把骨架跑通，事件不丢即可。

### 5.4 knowledge_jobs 基础库

新增 `internal/module/knowledge/worker/job.go` + `store.go`（Phase 0 只建表 + 基础 CRUD + 租约领取/续约/释放，不实现具体 job_type 处理逻辑）：

- 幂等键：`job_type + asset_version_id + target_key + build_revision`（§6.5），DB `dedupe_key UNIQUE`。
- 租约：`SELECT ... FOR UPDATE SKIP LOCKED WHERE lease_until < now()` 领取；`UPDATE lease_until` 续约；超时回收。
- 重试分类：`transient`（退避重试）/`permanent`（不重试）/`policy_denied`（不重试，写审计）。
- dead：`attempt >= max_attempt` → `status='dead'`，人工重投。

## 6. 第三方治理门禁（D8）

### 6.1 资产

新增 `third-party/` 目录与 Makefile/CI 目标：

| 文件 | 用途 |
|---|---|
| `third-party/lock.json` | 第三方组件锁定清单：`name / source_url / commit_sha_or_digest / license / notice_path / capability` |
| `third-party/NOTICES/` | 各组件 NOTICE 文件副本 |
| `third-party/adr/` | 第三方引入 ADR 模板与实例 |
| `Makefile` 目标 `third-party-check` | 校验 lock.json 完整性、digest 一致、license 合规、NOTICE 存在 |
| `Makefile` 目标 `sbom` | 用 `syft` 生成 SBOM（CycloneDX） |
| `Makefile` 目标 `notices` | 聚合生成 `THIRD_PARTY_NOTICES.md` |

### 6.2 首批固定基线

| 组件 | 来源 | commit/digest | license | capability | ADR |
|---|---|---|---|---|---|
| CodeGraph | （Phase 3 选型时固定，Phase 0 先留 ADR 模板与决策框架，不预选） | TBD | TBD | 代码符号/调用/影响查询 | `third-party/adr/0001-codegraph-selection.md`（模板） |
| Hermes / Agent Skills 参考 | `agentskills.io/<spec-version>` spec | spec 版本号 | spec 声明 | Skill 包格式 profile | `third-party/adr/0002-skill-spec-baseline.md` |

> Phase 0 不做 CodeGraph 实际选型（那是 Phase 3 / YS-97）；Phase 0 只固化**决策框架与门禁**，确保 Phase 3 选型时必须走 ADR + lockfile + digest 流程，不能跳过。

### 6.3 ADR 模板

`third-party/adr/0000-template.md`：

```markdown
# ADR-XXXX: <组件> 引入决策
- Status: proposed | accepted | superseded
- Date: YYYY-MM-DD
## 背景
## 候选对比
| 候选 | 语言 | License | 活跃度 | 维护成本 | 性能 | 合规影响 |
## 决策
## License 合规影响（AGPL/GPL 传染性义务 / 商业限制）
## 固定基线
- source_url / commit_sha_or_digest / license / notice
## 风险与缓解
## 升级与回退策略
```

### 6.4 门禁集成

CI（`deployments/` 下现有流水线配置补充）在 publish 前跑 `make third-party-check sbom notices`，失败阻断发布。门禁标准：lock.json 无漂移、digest 一致、license 白名单通过、NOTICE 齐全。

## 7. 模块与目录组织（§3.1 Phase 0 子集）

Phase 0 只落地 §3.1 中本阶段必需的部分，其余在对应 Phase 接入：

```text
internal/
  domain/
    rbac.go                  # 扩展（§4.1）
    knowledge_event.go       # 新增（§5.1）
    knowledge_asset.go       # 新增：Asset/Version/Agent/Binding 值对象
  platform/
    authz/                   # 新增：locator.go / service.go / context.go / delegated.go
    outbox/                  # 新增：store.go / dispatcher.go
    rbac/                    # 改造：engine.go 内部委托 ResourceLocator（行为不变）
  infra/
    postgres/
      knowledge_core.go      # 新增：8 表 repository
      outbox.go              # 新增：outbox store 实现
  module/
    knowledge/               # 新增（Phase 0 子集）
      worker/                # 新增：job.go / store.go（基础库）
      handler/               # 新增：/internal/v1/authz/delegated 等
migrations/
  013_knowledge_core.up.sql / .down.sql
third-party/                 # 新增：lock.json / NOTICES/ / adr/
Makefile                     # 新增目标
```

## 8. 授权测试矩阵规范（交付测试工程师）

§16.1 第 5 项"主体 × 资产类型 × 动作 × 访问路径"矩阵，Phase 0 范围（asset=测试 Asset，document 用现有文档回归）：

### 8.1 矩阵维度

| 维度 | 取值 |
|---|---|
| 主体 | user / agent(代表用户) / agent(自主,service_account) / service_account |
| 资产类型 | document（回归）/ asset（测试 Asset） |
| 动作 | read / write / admin / use / assign / share / review / sync |
| 访问路径 | 直接 ID 查询 / 列表 / 搜索(FTS) / MCP 工具 / 内部 Provider 调用 / 异步索引路径 |

### 8.2 必须覆盖的越权用例（100% 拒绝）

1. user 无 `use` 权限调 asset_read → 拒绝，返回 not_found（不泄露存在性）。
2. agent(代表用户) 的 acting_user 无该文档 read，即使 binding allow → 拒绝（求交失败）。
3. agent(自主) 的 service_account 无权限，binding allow → 拒绝（binding 不扩大权限）。
4. binding effect=deny，即使 principal 有 RBAC allow → 拒绝（deny 优先）。
5. pinned binding 的 version 被撤权 → 阻断，不自动回退最新版（§11.4）。
6. 撤权后**下一次请求**同步拒绝（revision+1 同事务提交，新请求读新 revision）。
7. 缓存与投影视图 60 秒内收敛：撤权后 Qdrant/FTS 投影尚未同步时，以 Mora batch check 为准，最终 60s 内投影不再可见。
8. MCP 内部调用缺 delegated context，仅有服务身份 → 降级为 service account 受限权限，不 fallback admin。
9. delegated JWT 过期（>30s）或 session revoked → 拒绝。
10. 跨 workspace 资产引用 → 拒绝（`GrantsFor` 已按 workspace 隔离，新增 asset 路径保持同约束）。

### 8.3 回归用例（无退化）

- 现有 `rbac/engine_test.go` 全绿。
- 现有文档 `read`/`write`/`admin` 经 `rbac.Engine.Check` 行为不变（定位改委托后）。
- `doc_events` RAG 索引链路不变。
- MCP `search_knowledge_base`/`get_document`/`list_documents` 行为不变。

## 9. 交付清单与角色分工

| 交付项 | 主导 | 协同 | 验收 |
|---|---|---|---|
| 1. 核心表 013 迁移 | 架构师（本文 §2） | 后端研发实现 | 迁移 apply 成功；约束/索引齐 |
| 2. ResourceLocator + authz.Service | 架构师（本文 §3） | 后端研发实现 | `rbac/engine_test.go` 全绿 + asset/agent 定位用例通过 |
| 3. principal/target/action 扩展 + Agent/Binding/delegated | 架构师（本文 §4） | 后端研发实现 | §8 矩阵越权用例 100% 拒绝 |
| 4. Outbox + envelope + knowledge_jobs | 架构师（本文 §5） | 后端研发实现 | 双写事件入 `knowledge_events`；outbox 不丢；job 幂等 |
| 5. 授权测试矩阵 | **测试工程师** | 架构师提供本文 §8 | §8 全部用例自动化通过 |
| 6. MCP delegated context | 架构师（本文 §4.4） | 后端研发实现 | 缺 delegated 不 fallback admin；revoked/过期拒绝 |
| 7. 第三方治理门禁 | **交付部署工程师** | 架构师提供本文 §6 | `make third-party-check sbom notices` 通过 |

## 10. 风险与缓解

| 风险 | 缓解 |
|---|---|
| `rbac.Engine` 内部改委托破坏现有文档授权 | 分两步 PR；第一步仅委托 docLocator，全量回归门禁；现有 `engine_test.go` 不动 |
| DEFERRABLE 自引用 FK 在低版本 pgx 行为差异 | 迁移在 PG16 验证；CAS 语句固定 §2.2 版本 |
| delegated JWT 复用 JWT_SECRET 密钥泄露面扩大 | delegated TTL ≤30s；服务端 session 为权威；revoked 即拒；密钥轮换由部署侧 K8s Secret 管理 |
| Outbox 双写在文档写事务增加延迟 | outbox insert 同事务轻量（单行 JSONB）；dispatcher 异步；监控 outbox 未发布积压 |
| 第三方门禁误阻发布 | lock.json 由架构师评审；CI 失败仅阻断 publish，不阻断 dev |

## 11. 验收门禁（§16.1）

- [ ] 越权用例 100% 拒绝（§8.2 全部）。
- [ ] 撤权后下一次请求同步拒绝；缓存/投影视图 60 秒内收敛。
- [ ] 发布构建不存在漂移依赖，许可证/NOTICE 检查通过（§6.4）。
- [ ] 现有文档/RAG/MCP 文档工具行为回归无退化（§8.3）。

---

## 附录 A：与 §19 开放决策的对应

| §19 决策 | 本文落点 | 结论 |
|---|---|---|
| #1 完整 SQL/索引/迁移编号 | §2 | `013_knowledge_core`，8 表 DDL + 约束 + 索引 |
| #2 ResourceLocator 兼容现有目录继承与文档查询性能 | §3 | CompositeLocator + docLocator 复用现有 Repository；engine 行为不变 |
| #3 delegated session 签名/有效期/MCP 接入 | §4.3/§4.4 | HS256 复用 JWT_SECRET，TTL ≤30s，JTI 服务端权威，/internal/v1/authz/delegated 端点 |

其余 §19 决策（#4–#12）属于后续 Phase，本文不涉及。
