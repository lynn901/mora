# 17. Phase 3：CodeGraph（固定 commit 构建、graph_ref/source_tree 校验、只读代码工具）

> 对应设计文档 `design-docs/12-human-agent-knowledge-architecture.md` §7.4（Git 与 CodeGraph）、§10.1（远端 Provider 调用约束）、§10.2（CodeGraphProvider 契约）、§16.4（Phase 3 计划）。承接 YS-97。
>
> 本文是**架构层交付物**：定义 Provider 契约、sidecar 集成形态、snapshot/源码树物化与校验、只读 REST/MCP 工具、评测集与 capability 契约测试的架构边界与不变量。**不含** Go handler / DB migration / 业务逻辑实现——这些由研发（`[@mora后端研发]`）落地，交付部署工程师（`[@Mora 交付部署工程师]`）负责 sidecar profile 与凭据。实现路径与现有 Phase 0–2 代码 seam 一一对齐，研发可照此编码。

## 1. 范围与前置

| 项 | 说明 |
|---|---|
| 依赖 | Phase 1（YS-95）的 Source / Git 锚点、`knowledge_assets(codebase)` + commit revision 登记、`asset_projections` 表（已含 `codegraph` projection_kind）、`knowledge-worker` 进程与 job dispatch。Phase 1 明确"Git 的 CodeGraph 构建留给 Phase 3，只到登记 codebase Asset + commit revision"（`design-docs/14 §4.3`）。 |
| 不变 | 不改 Phase 1 的 `SourceConnector` 端口与 Git adapter 的 `Validate/ResolveRevision/Fetch`；只在 worker 侧新增"build graph"消费分支与 Provider 端口。 |
| 选型 | `codegraph` npm 1.5.0，MIT，commit `c6aaa20`（ADR-0001 已 `accepted`，见 §8 / `third-party/adr/0001-codegraph-selection.md`）。 |

## 2. 架构总览

```text
Source Sync（Phase 1，已落地）
  -> GitConnector.ResolveRevision pins commit_sha
  -> GitConnector.Fetch -> ContentSink -> MinIO 隔离前缀（codebase Asset version, content_hash, manifest）
  -> 登记 knowledge_asset_versions(build_status=pending, governance_status=candidate)
  -> Outbox KEAssetVersionRequested -> knowledge_events Stream

knowledge-worker（本 Phase 新增消费分支）
  mapKnowledgeEvent: KEAssetVersionRequested + asset_type=codebase
    -> enqueue codegraph_build job (dedupe = codegraph:{version_id}:{build_revision})
  CodeGraphBuildHandler.Run:
    -> 读 codebase Asset version 的 source locator（MinIO key 前缀 / commit_sha）
    -> 从 snapshot 物化只读工作树（复用受控 mirror / 浅克隆，egress 审计）
    -> 计算 source_tree_hash（工作树内容哈希，必须与 commit 一致）
    -> Provider.Build(cap, BuildRequest{snapshot_locator, commit, source_tree_hash})
       -> 返回 BuildResult{graph_ref, source_tree_ref, commit, source_tree_hash,
                         provider_version, provider_build_digest,
                         index_schema_version, extraction_version, capabilities_snapshot}
    -> 校验 BuildResult.commit == 输入 commit && source_tree_hash 匹配
    -> AssetRegistry.MarkProjectionReady(projection_kind=codegraph,
       locator={graph_ref, source_tree_ref, commit_sha, source_tree_hash,
                provider_version, provider_build_digest, ...})
    -> 投影就绪门禁翻转 build_status，CAS 激活 current_version（既有 §7 机制）
    -> 清理临时构建目录（保留 snapshot + active 源码树 / 可验证物化能力）

查询路径（只读，本 Phase 新增）
  REST:  /api/v1/knowledge/codebases/{id}/...   (§6.1)
  MCP:   code_explore / code_search / code_files / code_node
         / code_callers / code_callees / code_impact / code_status  (§6.2)
  -> mora-api 服务层 authorize（ResourceLocator 解析 codebase Asset，engine.Check read）
  -> 读 active codegraph projection 的 locator（graph_ref + source_tree_ref + commit）
  -> 查询前校验 capability.asset_version == graph_ref 绑定的 version && source_tree_hash 匹配
  -> Provider.Query{Explore|Search|Files|Node|Callers|Callees|Impact|Status}(cap, graph_ref, req)
  -> 结果携带 commit / 文件 / 行号 / 符号定位
  -> 失败 fail closed（source_snapshot_unavailable / capability_unavailable），不伪造结果
```

**关键边界**（蓝图 §"sidecar 不保存 Mora 凭据"）：
- Provider sidecar **不持有 Git 凭据**；`BuildRequest` 只携带只读 snapshot locator + commit，不携带 Secret。
- `graph_ref`、`source_tree_ref`、commit **一一绑定**；查询前校验三者一致 + `source_tree_hash`。
- 查询结果**必须**包含 commit、文件路径、行号、符号定位；无部署/运行时证据时只描述该 commit 的静态实现。

## 3. Provider 契约（端口与适配 seam）

### 3.1 端口定义（`internal/module/knowledge/codegraph/provider/provider.go`）

新增包，镜像 Wiki Provider 的"服务声明本地端口、worker 桥接具体 Provider"模式（参照 `internal/module/knowledge/wiki/provider/provider.go` + `internal/module/knowledge/worker/wiki_provider_adapter.go:27`）。包注释须声明：Provider 只接收 Mora 已授权并裁剪的 snapshot locator / 请求，不自行读 DB / 对象存储 / Git。

```go
// Package provider defines the CodeGraph provider port (12 §10.2).
// The provider receives an already-authorized, read-only snapshot locator
// and commit — never Git credentials. It returns graph artifacts, query
// results and diagnostics. Path normalization, source_tree_hash校验,
// projection registration, CAS activation and cleanup are Mora's job.
package provider

type Capability struct {
    // 复用 Phase 0 的签名 capability 形状（12 §10.1）：workspace / 动作 /
    // 资产范围 / 过期 / decision ID。组合 wiki 的 budget envelope 与
    // authz.DecisionCapability（internal/platform/authz/context.go:65
    // DecisionID/Token/AuthzRevision/ExpiresAt）。
    WorkspaceID    uuid.UUID
    AuthzRevision  int64
    DecisionID     uuid.UUID   // 来自 authz.Service.IssueDecision
    ExpiresAt      time.Time
    AllowedAssetIDs []uuid.UUID // 已裁剪的可见 codebase 资产集
    MaxReadBytes   int
    MaxReadFiles   int
    MaxResults     int
}

type CodeGraphCapabilities struct {
    Languages        []string // 声明支持语言
    Operations       []string // explore|search|files|node|callers|callees|impact|status
    MaxRepoSize      int64
    MaxFiles         int
    IncrementalSync  bool
    IndexSchemaVer   string
    ExtractionVer    string
}

type BuildRequest struct {
    SnapshotLocator  Locator  // 只读 MinIO key 前缀 / 临时路径（不可执行定位）
    Commit           string   // 固定 commit_sha
    SourceTreeHash   string   // 物化工作树哈希（Build 前由 Mora 计算并传入）
    Capability       Capability
}

type BuildResult struct {
    GraphRef            string  // Provider 侧 graph 句柄
    SourceTreeRef       string  // Provider 侧只读源码树句柄（与 graph_ref 同生命周期）
    Commit              string  // 必须等于 BuildRequest.Commit
    SourceTreeHash      string  // 必须等于 BuildRequest.SourceTreeHash
    ProviderVersion     string
    ProviderBuildDigest string
    IndexSchemaVersion  string
    ExtractionVersion   string
    CapabilitiesSnapshot CodeGraphCapabilities
    Stats               BuildStats // 文件数 / 符号数 / 调用边数
}

type CodeGraphProvider interface {
    Capabilities(ctx context.Context) (CodeGraphCapabilities, error)
    Build(ctx context.Context, req BuildRequest) (BuildResult, error)
    Explore(ctx context.Context, graphRef string, req ExploreRequest) (ExploreResult, error)
    Search(ctx context.Context, graphRef string, req CodeSearchRequest) ([]CodeHit, error)
    Files(ctx context.Context, graphRef string, req FilesRequest) (FileTree, error)
    Node(ctx context.Context, graphRef string, req NodeRequest) (CodeNode, error)
    Callers(ctx context.Context, graphRef string, req NodeRequest) ([]CodeEdge, error)
    Callees(ctx context.Context, graphRef string, req NodeRequest) ([]CodeEdge, error)
    Impact(ctx context.Context, graphRef string, req ImpactRequest) ([]CodeHit, error)
    Status(ctx context.Context, graphRef string) (GraphStatus, error)
    Delete(ctx context.Context, graphRef string) error
    Health(ctx context.Context) error
}
```

> **字段集对齐 §10.2**：`BuildResult` 必须返回 `graph_ref/source_tree_ref/commit/source_tree_hash/provider_version/provider_build_digest/index_schema_version/extraction_version/capabilities_snapshot`（设计文档原话）。`Explore` 可由 Provider 原生实现或由 adapter 编排窄接口，但 **MCP 不得绕过 Provider 直接调第三方 ToolHandler**（§10.2 红线）。

### 3.2 命中结果类型（携带定位）

```go
// CodeHit / CodeEdge / CodeNode 所有结果必须携带 commit / 文件 / 行号 / 符号定位。
type CodeLoc struct {
    Commit    string `json:"commit"`
    Path      string `json:"path"`
    StartLine int    `json:"start_line"`
    EndLine   int    `json:"end_line,omitempty"`
    Symbol    string `json:"symbol,omitempty"`   // 符号名（定义/调用方）
    Kind      string `json:"kind,omitempty"`    // function|method|class|...
}
type CodeHit  struct { Loc CodeLoc; Score float64; Snippet string `json:"snippet,omitempty"` }
type CodeEdge struct { From CodeLoc; To CodeLoc; Kind string } // calls|defines|implements
type CodeNode struct { Loc CodeLoc; Kind string; Signature string; Docstring string }
```

### 3.3 适配器（`internal/infra/codegraph/`，蓝图 §目录预留）

- `internal/infra/codegraph/sidecar.go`：`SidecarProvider` 实现 `provider.CodeGraphProvider`，封装 sidecar 的 HTTP/本地 RPC 调用。**不导入 `codegraph` 内部包路径**，只通过其公开 API/sidecar 协议交互。
- `internal/infra/codegraph/noop.go`：`NoopProvider`（nil/未配置时返回空结果 + `capability_unavailable`，保证未启用 CodeGraph 时文档/RAG/MCP 不退化，蓝图 §"未启用 CodeGraph 时文档能力继续工作"）。
- Worker 桥接：`internal/module/knowledge/worker/codegraph_provider_adapter.go`（参照 `wiki_provider_adapter.go`）把具体 Provider 桥接到服务端口。

### 3.4 远端调用约束（§10.1）

sidecar 调用必须具备：mTLS 或短期服务凭证；签名 capability（workspace/动作/资产范围/过期/decision ID）；deadline / trace ID / 幂等 key / provider API version；请求/响应大小限制；不记录正文/Token/仓库凭据/未脱敏证据；`Health` / `Capabilities` / 契约测试端点。

> **MCP delegated context 依赖说明**（架构提示，非本 Phase 必须落地）：`internal/module/mcp/moraclient/http.go:21-33` 注释指出 MCP→mora-api 的 delegated JWT 获取尚未实现（仍发 legacy `X-Identity-*` 头）。code_* MCP 工具遵循现有 `wiki.go` 形状即可工作（权限在 mora-api 服务层裁决）；delegated JWT 的全面切换是横切项，由后续 issue 统一收口，不阻塞本 Phase。

## 4. snapshot / 源码树物化与校验（fail closed 核心）

§7.4 原话："只保存 CodeGraph SQLite 索引不足以返回源码，因为索引仅保存文件 hash、符号位置和关系。Provider 可以持久化只读源码树，也可以在查询前从 `snapshot_ref` 物化，但必须先校验 `source_tree_hash`，且不能读取其他 revision 的工作树。"

### 4.1 物化与校验流程

```text
CodeGraphBuildHandler:
  1. 从 codebase Asset version 的 source locator 取 MinIO snapshot key + commit_sha
  2. 从 snapshot 物化构建期只读工作树（受控 mirror / 浅克隆复用，egress 审计每次出网）
  3. 计算 source_tree_hash = sha256(规范化文件清单 + 各文件内容 hash)
     -- 必须与 commit 对应的树一致；不一致则 fail closed，不构建
  4. Provider.Build(req{snapshot_locator, commit, source_tree_hash})
     -- Provider 内部从 snapshot 生成自己的源码树 + 索引
     -- 返回 graph_ref + source_tree_ref（与 graph_ref 同生命周期）
  5. 校验 BuildResult{Commit, SourceTreeHash} == 输入，否则丢弃 + fail
  6. MarkProjectionReady(locator={graph_ref, source_tree_ref, commit_sha,
     source_tree_hash, provider_version, provider_build_digest, ...})
  7. 清理临时 clone / 构建目录 / 凭据；保留 snapshot + active 源码树
```

### 4.2 查询期校验

```text
查询（REST/MCP -> mora-api 服务层）:
  1. 读 active codegraph projection 的 locator（graph_ref, source_tree_ref, commit, source_tree_hash）
  2. 校验 capability.asset_version == graph_ref 绑定的 version（§10.2 "Provider 必须验证 capability 中的 asset version 与 graph_ref 一致"）
  3. 校验 source_tree_hash 仍匹配（源码树未损坏 / 未错位）
     -- 不匹配 -> source_snapshot_unavailable，fail closed，不返回可能错位源码
  4. Provider.Query(graph_ref, req) -> 结果带 commit/文件/行号/符号
```

### 4.3 生命周期与清理（§7.4 + §15 故障表）

| 对象 | 保留策略 |
|---|---|
| snapshot（MinIO 不变快照） | 当前版本 + 固定 Binding 引用版本保留；旧版本保留期后可只留 snapshot |
| active graph 源码树 | **不可清理**（active 版本 + 固定 Binding 引用版本） |
| 临时构建目录 | build 完成即清理（验收门禁：删除临时构建目录后 active graph 仍能读取正确源码） |
| 未固定旧版本源码树 | 保留期后只留 snapshot，查询前受控物化 + hash 校验后恢复 |

**故障降级**（§15 表，直接对齐）：

| 故障 | 对外行为 | 恢复 |
|---|---|---|
| 新版本构建失败，查询服务可用 | 继续查上一个 active graph，标 stale | 从保留 snapshot 重试构建 |
| 索引存在但源码树缺失/hash 不符 | `source_snapshot_unavailable`，不返回错位源码 | 从 snapshot 重新物化 + 校验后恢复 |
| 查询服务不可用 | 只返回 graph 版本元数据 + `capability_unavailable`，不伪造结果 | Provider 恢复后用 active graph，缺失则从 snapshot 重建 |

> 降级状态必须出现在 API/MCP 响应元数据与运维指标中（§15）。系统不得把 Provider 故障、授权过滤后为空、真实无结果混为同一状态。

## 5. worker 集成（job dispatch，对齐现有代码）

现有 dispatch 表（`cmd/knowledge-worker/main.go:122-134`，`internal/module/knowledge/worker/runner.go:29-35`）：

| 现有 job_type | handler | 状态 |
|---|---|---|
| `source_sync` | `SourceSyncHandler` | Phase 1 skeleton（TODO Connector） |
| `projection_build` | `ProjectionBuildHandler` → `AssetRegistry.MarkProjectionReady` | 已落地 |
| `asset_activate` | `AssetActivateHandler` → CAS | 已落地 |

**本 Phase 新增**：

| job_type | handler | dedupe_key | 说明 |
|---|---|---|---|
| `codegraph_build` | `CodeGraphBuildHandler`（新） | `codegraph:{asset_version_id}:{build_revision}` | 物化 + Build + 校验 + MarkProjectionReady(kind=codegraph) |

- **Stream 路由**：复用 `knowledge_events` Stream（消费组 `knowledge_projection`），在 `mapKnowledgeEvent`（`cmd/knowledge-worker/main.go:384` 的 `enqueueProjectionJobs`）中，当 `asset_type=codebase` 时扇出一个 `codegraph_build` job（dedupe 保证幂等）。**不新增 Stream**——`knowledge_events` 已是投影扇出的统一通道。
- `CodeGraphBuildHandler` 完成后调既有 `AssetRegistry.MarkProjectionReady`（`internal/infra/postgres/asset_activation.go:60`），`kind=domain.ProjectionCodegraph`，`locator` 填 `graph_ref` 等。`allRequiredReady`（`:129`）翻转 `build_status`，`AssetActivateHandler` 做 CAS——**既有激活路径零改动**。
- 错误分类沿用 `runner.go` 的 `RetryClass`（transient/permanent）：snapshot 物化失败 / Provider 超时 = transient；commit/hash 不符 = permanent（fail closed，不重试错位源码）。

## 6. 只读 REST + MCP 工具

### 6.1 REST（§11.1/§11.2 子集，`cmd/mora-api/main.go` 既有 `authed` group 注册）

```text
GET  /api/v1/knowledge/codebases/{id}               # codebase 资产详情 + active graph 元数据
GET  /api/v1/knowledge/codebases/{id}/files         # FileTree（code_files）
GET  /api/v1/knowledge/codebases/{id}/status         # GraphStatus（code_status）
POST /api/v1/knowledge/codebases/{id}:search         # CodeSearch
POST /api/v1/knowledge/codebases/{id}:explore        # 组合查询
GET  /api/v1/knowledge/codebases/{id}/nodes/{nodeID} # CodeNode
GET  /api/v1/knowledge/codebases/{id}/nodes/{nodeID}/callers
GET  /api/v1/knowledge/codebases/{id}/nodes/{nodeID}/callees
POST /api/v1/knowledge/codebases/{id}:impact         # 影响面
```

- 鉴权：handler 层 `MustAuth(c)`（`internal/module/mora/handler/`），服务层 `authorize` 调 `engine.Check`（参照 `internal/module/knowledge/wiki/service/wiki_service.go:80-110`）。**不新增 RBAC target 类型**——codebase 已是 `knowledge_assets` 的 `asset_type`，既有 `codebase` Asset 的 ResourceLocator + asset 级 read 权限覆盖；无权/不存在 → 404 + `code=40400`，不泄露存在性（§11.4）。
- 内部 API（MCP 用）：`GET /internal/v1/codebases/{id}/...`（§11.2），服务身份 + delegated context。

### 6.2 MCP 工具（`internal/module/mcp/tool/code.go`，参照 `wiki.go` 形状）

| 工具 | 只读 | 对应 Provider 方法 |
|---|---|---|
| `code_explore` | ✓ | `Explore` |
| `code_search` | ✓ | `Search` |
| `code_files` | ✓ | `Files` |
| `code_node` | ✓ | `Node` |
| `code_callers` | ✓ | `Callers` |
| `code_callees` | ✓ | `Callees` |
| `code_impact` | ✓ | `Impact` |
| `code_status` | ✓ | `Status` |

- 每个工具：`struct{ base }`，`Definition()` 返回 `server.ToolDef`（Name/Description/InputSchema），`IsWrite()=false`，`Execute()` 调 `t.client.CodeXxx(...)`，`NotFound/Forbidden → emptyTextResult()`（§6.4 存在性不泄露，参照 `tool.go:41`）。
- `MoraClient` 新增方法（`internal/module/mcp/moraclient/client.go:231` interface + `http.go` 实现 + `mock.go`）：`CodeExplore/CodeSearch/CodeFiles/CodeNode/CodeCallers/CodeCallees/CodeImpact/CodeStatus`。
- **只暴露 Provider 声明且通过契约测试的能力**（§10.2）：某语言未通过契约测试则该语言的对应操作不进 `tools/list` 或在运行时返回 `capability_unavailable`。不假设所有语言调用解析覆盖率一致。
- 注册：`cmd/mcp-server/main.go:98-107` 现有 `srv.RegisterTool(...)` 列表追加 8 个工具。
- 管理型操作（build/activate/delete graph）**不进默认 Agent 工具集**（§11.3）。

## 7. 评测集与 capability 契约测试

### 7.1 评测集（验收门禁载体）

- 按 Provider 声明语言分层，每语言一个基准仓库子集 + 带预期答案的查询集。
- **硬门禁**：基准仓库的定义/调用查询 100% 命中；各语言影响候选召回率首版 ≥ 90%。
- 评测集存放：`internal/module/knowledge/codegraph/eval/`（预期答案 + 评测 runner）。研发实现时由测试工程师（`[@Mora知识库测试工程师]`）协同落地用例。
- 召回率分层报告，不聚合为单一数字（避免某语言拉高均值掩盖短板）。

### 7.2 capability 契约测试（§17.2）

覆盖：capability、build identity、graph/source-tree/commit 锚点、explore/files/status、引用、超时、删除。关键用例：

| 用例 | 预期 |
|---|---|
| Build 后 `BuildResult.Commit != 输入 commit` | 丢弃 + fail |
| `source_tree_hash` 不符 | `source_snapshot_unavailable`，不返回错位源码 |
| 删除临时构建目录后查 active graph | 仍读正确源码 |
| 过期 revision 查询 | 不伪装为当前结果（返回该 commit 静态实现 + commit 标注） |
| capability.asset_version ≠ graph_ref 绑定 version | 拒绝 |
| Provider 未启用 | `capability_unavailable`，文档/MCP 不退化 |

集成测试（§17.3）两条必须覆盖：
1. Git 同步 → CodeGraph build → current version 切换 → 删临时目录 → node/impact 查询。
2. CodeGraph 源码树损坏 → 查询 fail closed → 从 snapshot 物化 → hash 校验后恢复。

## 8. 第三方治理（ADR-0001 已 accepted）

| 资产 | 状态 | 位置 |
|---|---|---|
| ADR-0001 | `accepted` 2026-08-15 | `third-party/adr/0001-codegraph-selection.md` |
| lock.json 条目 | `selected-baseline-phase3`，commit `c6aaa20` pinned | `third-party/lock.json`（capability `code-symbol-graph`, ecosystem `reference`） |
| NOTICE | MIT 全文 + 集成说明 | `third-party/NOTICES/CodeGraph.NOTICE` |
| 门禁 | `make third-party-check` PASS（`✓ reference codegraph: baseline locked`） | `third-party/check.sh`（reference 分支已更新：校验 status + ADR accepted） |

**digest 字段**：sidecar 基线锁定到 commit `c6aaa20`（`digest_type=git-commit-pinned`），不 against lockfile 校验（codegraph 是 sidecar，非 go/web 运行时依赖）。npm tarball `dist.shasum` 在交付部署工程师落地 sidecar 时补录进 SBOM 追溯，不阻塞门禁。

## 9. 部署（交付部署工程师职责，`[@Mora 交付部署工程师]`）

- codegraph sidecar 独立 Compose profile（参照现有 `mora-*` 服务 profile），与 mora-api 同网络，**不暴露公网**（§"Qdrant/Valkey/MinIO/CodeGraph 不直接暴露公网"）。
- 独立服务凭据（mTLS 或短期 token），sidecar 不保存 Mora 凭据。
- 健康检查：`Health` + `Capabilities` 端点接入 Compose healthcheck / K8s probe。
- 默认不出网：sidecar 100% 本地构建与查询。

## 10. 可观测（§14 子集）

新增指标（Prometheus）：
- `knowledge_provider_calls_total{provider="codegraph",op,status}`（§14.1 模板）
- `codegraph_query_duration_seconds{op}` —— 目标 P95 ≤ 1.5s（§13.3 SLO）
- `codegraph_build_total{status}` / `codegraph_source_tree_hash_mismatch_total`（fail closed 计数）
- `knowledge_projection_age_seconds{kind="codegraph"}`（§14.1 投影新鲜度）

Trace 链路（§14.2）：`source sync → build → activate` 与 `MCP call → authz → Provider query → citation` 贯穿 `trace_id/job_id`。

## 11. 迁移（DB）

**无需新增 DB migration**——`asset_projections` 表（`migrations/014_phase1_asset_source.up.sql:141`）已含 `codegraph` projection_kind 与 `locator JSONB`。`graph_ref` / `source_tree_ref` / `commit_sha` / `source_tree_hash` 存入 `locator` JSONB，与 FTS/Qdrant 投影把 collection/prefix 存 `locator` 的既有约定一致（`design-docs/14 §2.2` "locator 只保存不可执行定位信息"）。`domain.AssetProjection`（`internal/domain/projection.go:39`）的 `Locator map[string]any` 直接承载。

> 这是 Phase 1 预留契约的直接兑现：Phase 1 建表时已把 `codegraph` 列入 `CHECK (projection_kind IN (...,'codegraph',...))`，本 Phase 只写入第一行 `codegraph` 数据，不改 schema。

## 12. 交付与角色分工

| 交付项 | 负责角色 |
|---|---|
| Provider 契约（§3）、sidecar 集成形态、snapshot/源码树物化与校验（§4）、REST/MCP 工具架构（§6）、评测集设计（§7）、本文档 | 架构师（已完成） |
| Go 实现：provider 包 + infra/codegraph adapter + worker handler + REST handler + MCP tool + MoraClient 方法 + 评测集 runner | `[@mora后端研发]` |
| codegraph sidecar Compose profile + 凭据 + healthcheck + npm tarball digest 补录 | `[@Mora 交付部署工程师]` |
| capability 契约测试 + 评测集用例 + lint/幂等/fail-closed 测试 | `[@Mora知识库测试工程师]` |

## 13. 验收门禁对齐（YS-97）

| 门禁 | 落地点 |
|---|---|
| 基准仓库定义/调用查询 100% 命中 | §7.1 评测集硬门禁 |
| 各语言影响候选召回率首版 ≥ 90% | §7.1 分层报告 |
| 所有结果携带 commit/文件/位置 | §3.2 CodeLoc + §4.2 查询期校验 |
| 过期 revision 不伪装为当前结果 | §4.2 + §7.2 契约用例 |
| 删临时构建目录后 active graph 仍读正确源码 | §4.3 清理策略 + §7.2 契约用例 |
| 源码树 hash 不符 fail closed | §4.1/§4.2 + §15 故障表 + §7.2 契约用例 |
