# 端到端验证用例覆盖矩阵（YS-16）

> 父需求：YS-4《团队智能 Mora 与向量知识库平台》 ｜ 本任务：YS-16 端到端验证用例编写
> 依据：YS-4 PRD §5 交互流程、§7 边界与异常、§8 AC-1~19；YS-5 设计文档 04/05/06/07
> 脚本位置：`tests/e2e/`（Go 黑盒 HTTP E2E，build tag `e2e`）
> 状态：用例/脚本已先行产出，待 YS-12 基础设施就绪后由 YS-10 一键执行闭环验证。

## 0. 执行方式与前置依赖

```bash
# 1) 拉起全组件栈（YS-12 交付）
docker compose -f deployments/docker-compose.yml -f tests/e2e/docker-compose.e2e.override.yml up -d
#    override 仅额外暴露 postgres:5432 供 E2E 夹具播种非管理员用户/Token

# 2) 执行 E2E（详见 tests/e2e/README.md）
E2E_BASE_URL=http://localhost:8990 \
E2E_MCP_URL=http://localhost:8081 \
DATABASE_URL=postgres://mora:mora@localhost:5432/mora \
INTERNAL_SERVICE_TOKEN=mora-internal-token \
go test -tags=e2e -v ./tests/e2e/...
```

**运行时依赖（已标注）**：
- mora-api / mcp-server / rag-worker 可达且健康（`/healthz`、`/mcp/health`）
- PG / Valkey / Qdrant / TEI（或 Ollama）已起，迁移 001~010 已执行
- `DATABASE_URL`：RBAC 跨层、MCP 越权/审计、存在性不泄露等用例需播种非管理员用户与自定义作用域 Token；未设置时这些用例自动 skip
- 种子数据（迁移 010）：`admin@mora.local/admin123`、演示工作区、激活的 embedding 模型、MCP dev token

**已知实现缺口（执行前排查：若以下用例失败，先确认是缺口而非脚本问题）**：
1. `GET /api/v1/documents/:id/versions` 返回桩（stub）而非版本历史 → AC-6 用 mounted 的 `diff`+`rollback`+`version_no` 路径验证
2. `/api/v1/admin/embedding-models*` 与 `/api/v1/documents/:id/index-status` 未在 mora-api 注册（handler 仅在 rag e2e 测试中挂载）→ AC-11 模型热切换/重建用例以 skip 标注，连通性通过 index_status=indexed 间接验证
3. `GET /api/v1/workspaces/:id/tags` 未注册 → AC-8 标签筛选用 `directory_id`+`created_by` 维度验证
4. MCP `get_document` 的 `format`/`version` 查询参数被 mora-api 忽略；返回存储的 Block 数组（无服务端 markdown 渲染）
5. MCP 401 返回 JSON-RPC 错误体（`code:-32001`），非 mora-api 的 `{code,message}` 信封
6. MCP `moraclient` 解码信封字段为 `msg` 而 mora-api 实际发 `message`（上游错误信息被静默丢弃）
7. MCP `create_draft`/`update_document` 经 `moraclient` 发送 `content` 为字符串，而 mora-api 期望 `markdown`（字符串）或 `content`（[]Block）→ MCP 写工具当前 400（contract drift）→ AC-17 MCP 路径 skip-with-note，草稿态在 mora 层验证；MCP 越权用例改断言安全结果（不可篡改+审计），精确 403 由 `TestBoundary_MoraWriteRBACDenied` 在 mora 层验证

---

## 1. AC-1~19 验证矩阵

| AC | 标题 | 优先级 | 前置条件 | 验证步骤 | 预期结果 | 脚本 | 依赖/备注 |
|----|------|--------|----------|----------|----------|------|-----------|
| AC-1 | Markdown/富文本双向可逆 + 代码块/图表/画板渲染 | P0 | mora-api 可达 | ①以 markdown（含标题+代码块）创建文档 ②GET 取回 ③以 Block 数组创建文档 ④GET 取回 | format 正确（markdown/blocks）；heading/codeBlock 经往返不丢失 | `TestAC1_ContentReversibility` | 富文本所见即所得渲染属前端；本用例验证内容基底可逆 |
| AC-2 | 批量导入 Markdown/PDF/Docx/HTML + 冲突处理 + 报告 | P1 | 导入接口 | 上传各格式文件，映射目录，预览冲突策略 | 目录结构建立；冲突按策略处理；产出导入报告 | — | 导入/导出为 P1（S1），本批 E2E 未覆盖；待 YS-10 联调后补 |
| AC-3 | 导出单文档/整空间为 Markdown/PDF/HTML | P1 | 导出接口 | 导出单文档与整空间，选择格式 | 生成对应格式文件流 | — | P1（S1），同上 |
| AC-4 | 无限极目录 + 多工作区数据/权限隔离 | P0 | mora-api + PG | ①建两个工作区 ②wsA 建 4 级嵌套目录 ③分别取目录树 | wsA 树深度≥4；两工作区目录互不泄露 | `TestAC4_WorkspaceIsolationAndTree` | — |
| AC-5 | ≥2 人实时协同编辑 + 光标/在线状态 + 评论锚定块/@提及/解决 | P0 | mora-api + WS | 两个 WS 客户端连 `/ws/collab/{doc}`，编辑同步；评论创建/解决 | 光标/在线状态可见；无数据丢失；评论锚定/解决 | — | 协同为前端 + WS；本批聚焦后端闭环，协同 WS 用例待 YS-14/YS-10 补 |
| AC-6 | 任意两版本 Diff + 回滚产生新版本 | P0 | mora-api + PG | ①建文档 v1 ②更新 v2 ③diff v1..v2 ④回滚到 v1 | diff 非空；回滚产生 v3（历史不改写）；内容回到 v1 | `TestAC6_VersionDiffAndRollback` | `GET .../versions` 为桩，用 diff+rollback+version_no 验证（缺口 1） |
| AC-7 | RBAC 工作区/目录/页面级读/写/管理；继承与覆盖；无权不在树与检索 | P0 | mora-api + PG + 非管理员用户 | ①默认拒绝 ②目录 allow 继承到文档 ③文档显式 deny 覆盖 ④撤销 deny 恢复 | 各态 GET/检索可见性正确；显式 deny>继承 allow>默认拒绝 | `TestAC7_RBACInheritanceAndOverride` | 需 DATABASE_URL |
| AC-8 | 全文检索多维筛选 + 高亮 + RBAC 过滤 | P0 | mora-api + PG(FTS) | ①无过滤检索 ②directory_id 过滤 ③created_by 过滤 ④snippet 校验 ⑤无权用户 0 命中 | 过滤命中正确；snippet 非空；无权 0 命中不泄露 | `TestAC8_FTSFiltersAndRBAC` | 标签筛选待 tags 路由挂载（缺口 3） |
| AC-9 | 文档 CRUD 自动触发流水线 + 状态徽标正确 | P0 | 全栈（含 rag-worker/TEI/Qdrant） | ①建文档（pending）②发布 ③轮询 index_status | 自动 pending→indexed，无需人工干预 | `TestAC9_AutoPipelineAndBadge` | index_status 经 `GET /documents/:id` 字段读取 |
| AC-10 | 更新/删除后旧 chunk 级联清理，检索不返回过期内容 | P0 | 全栈 | ①建+索引 ②更新替换内容 ③检索旧/新关键词 ④删除 ⑤检索 | 旧关键词不再命中；新关键词命中；删除后全不命中 | `TestAC10_CascadeCleanupOnUpdateAndDelete` | — |
| AC-11 | TEI/Ollama + Qwen3 连通性；模型切换重建 | P0 | 全栈 + admin 路由 | ①查 active 模型 ②建文档索引成功（连通性） ③模型切换/重建 | active 模型存在；索引成功=连通性通过 | `TestAC11_EmbeddingConnectivity` | 模型热切换/重建路由未挂载，skip 并备注（缺口 2） |
| AC-12 | Dense+BM25 混合检索 + 元数据过滤 + RBAC 硬约束 | P0 | 全栈 | ①admin 检索命中（含 dense/bm25 分） ②workspace 过滤排除 ③bob 无权不命中 | 命中含得分；过滤生效；越权不可见不可绕过 | `TestAC12_HybridSearchAndRBAC` | — |
| AC-13 | 流水线失败重试告警 + 幂等不重复 | P0 | 全栈 | ①建+索引 ②重复更新同内容 ③比较前后命中数 | 重复索引不产生重复向量（命中数不变） | `TestAC13_IdempotentReindex` | 失败重试×3/死信需断 TEI，infra 依赖，rag-worker 单测覆盖 |
| AC-14 | MCP initialize/capabilities 握手（HTTP/SSE）+ stdio | P0 | mcp-server | ①initialize ②校验 protocolVersion/capabilities | 返回 Mcp-Session-Id、protocolVersion、tools/resources 能力 | `TestMCP_InitializeHandshake` | stdio 为 P2，未在 docker 暴露；HTTP/SSE 验证 |
| AC-15 | Resources 返回工作区列表/目录树/文档元数据 | P0 | mcp-server + mora-api | ①resources/list ②read workspaces/tree/meta ③bob RBAC 范围 | admin 可见；bob 不可见（RBAC 范围） | `TestMCP_Resources` | — |
| AC-16 | Tools search_knowledge_base/get_document/(create_draft/update_document) 可调用返回结构化结果 | P0 | mcp-server + 全栈 | ①tools/list ②search_knowledge_base ③get_document ④list_documents | 工具存在；返回结构化命中/正文/列表 | `TestMCP_Tools` | create_draft/update_document 在 AC-17 验证 |
| AC-17 | 写操作默认进草稿/审阅态，不直接发布 | P1 | mcp-server + mora-api | ①mora 层建文档 status=draft ②alice（有 write）create_draft ③admin GET 校验状态 | mora 层默认 draft；MCP create_draft 返回草稿 ID，status=draft | `TestMCP_WriteToolsDraftState` | P1（S3），需 DATABASE_URL；MCP 路径因缺口 7 skip-with-note |
| AC-18 | Token 鉴权 + RBAC 约束 + 越权 403 + 全量审计 | P0 | mcp-server + PG | ①无效 token 401 ②过期 401 ③撤销 401 ④审计查询 | 401 JSON-RPC 错误；审计记录可查 | `TestMCP_TokenAuthAndAudit` | 越权写见 `TestBoundary_MCPOverPermissionDenied`+`TestBoundary_MoraWriteRBACDenied` |
| AC-19 | Token 作用域/有效期 + 即时吊销 | P0 | mcp-server + PG | ①readonly 写被拒（scope） ②过期 401 ③撤销 401 | 作用域拒绝；过期/撤销即时失效 | `TestMCP_TokenScope`、`TestMCP_TokenAuthAndAudit` | — |

---

## 2. 核心闭环验证用例（对齐 PRD §5.1，5 步）

| 步 | 环节 | 验证步骤 | 预期 | 脚本 |
|----|------|----------|------|------|
| 1 | 创建/编辑文档保存 → 产生 doc_event | 建文档并发布，校验 index_status=pending | 新文档 pending；发布触发事件 | `TestCoreClosedLoop` step1 |
| 2 | RAG Worker 消费 → 切块 → Embedding → 向量库 upsert → index_status 回执 | 轮询 `GET /documents/:id` 的 index_status | 自动到达 indexed（回执） | step2 |
| 3 | 全文检索 + 语义检索命中，RBAC 过滤生效 | admin/bob/alice 分别 FTS+RAG 检索 | admin 命中；bob 0；alice（已授权）命中 | step3 |
| 4 | Agent 经 MCP initialize → search_knowledge_base → get_document，越权 403/空 + 审计 | dev/alice/bob token 走 MCP 检索+取文档 | dev/alice 命中；bob 空集不报错；审计有记录 | step4 |
| 5 | 文档删除 → 级联清 chunk → 检索不再返回 | 删除后 FTS+RAG 检索 | 删除后不再返回（级联清理） | step5 |

---

## 3. RBAC 跨层一致性验证用例

| 用例 | 验证步骤 | 预期 | 脚本 |
|------|----------|------|------|
| 权限授予收敛 | 授 alice 目录读 → FTS 立即可见；MCP 在窗口内收敛可见 | Mora 层与检索/MCP 层同步可见 | `TestRBACCrossLayerConsistency` |
| 权限撤销收敛 | 撤销授予 → FTS 立即不可见；MCP 在窗口内收敛不可见 | 双向收敛；存在性不泄露 | 同上 |
| 双向收敛 | 再次授予 → 再次可见 | 收敛可复现 | 同上 |
| 存在性不泄露 | bob 无权：FTS total=0；GET 非 200；MCP get_document 空不报错 | 计数/内容/错误均不泄露存在 | `TestRBACExistenceNonLeak` |

> 跨层一致性依赖 `permission.change` 事件触发 chunk `visible_to` 重算（设计 05 §4.3.3）；FTS 走 PG 实时 RBAC 立即收敛，RAG/MCP 走 Qdrant payload + 防御性复核，在窗口内收敛。若超时不收敛 → 跨层一致性缺陷。

---

## 4. 边界与异常用例（对齐 PRD §7）

| 场景 | 验证步骤 | 预期 | 脚本 | 备注 |
|------|----------|------|------|------|
| MCP 越权 | alice 无 write 权限 update_document | 写不成功；内容未篡改；审计有记录 | `TestBoundary_MCPOverPermissionDenied` | 因缺口 7 改断言安全结果；精确 403 见下行 |
| mora 写 RBAC 拒绝 | alice 无 write 发合法 PATCH | 403/404；内容未改 | `TestBoundary_MoraWriteRBACDenied` | mora 层精确验证写 RBAC 否决 |
| Token 泄露 | 会话中撤销 token → 同会话后续调用 | 401，会话立即失效 | `TestBoundary_TokenRevocationInvalidatesSession` | — |
| 存在性不泄露 | 隐藏文档 vs 不存在文档 get_document/GET | 两者不可区分（同状态、空内容） | `TestBoundary_ExistenceIndistinguishable` | — |
| 超大文档 | ~80KB 文档建+索引+检索 | 批量切块不阻塞，indexed，可检索 | `TestBoundary_LargeDocumentIndexes` | — |
| 模型不可用降级 | （停 TEI）建文档 + 检索 | 流水线排队不丢事件；检索降级纯 BM25 | `TestBoundary_ModelUnavailableDegradation` | infra 依赖，skip+手动步骤 |
| 大量并发检索 | 限流突发 | 429 + Retry-After | `TestBoundary_RateLimited` | infra 依赖，skip+手动步骤 |
| 权限变更重算 | 见 §3 | 旧可见性保守生效，重算后收敛 | `TestRBACCrossLayerConsistency` | — |
| 删除级联对账 | 见核心闭环 step5 | 孤儿向量清理，检索剔除 | `TestCoreClosedLoop` step5、`TestAC10` | — |
| 流水线失败重试幂等 | 重复索引 | 不产生重复向量 | `TestAC13_IdempotentReindex` | 失败重试×3 由 rag-worker 单测覆盖 |

---

## 5. 覆盖汇总

- **AC 覆盖**：AC-1、4、6、7、8、9、10、11、12、13、14、15、16、17、18、19 共 16 条有自动化脚本；AC-2、3（导入导出 P1）、AC-5（协同 WS）待 YS-10/YS-14 联调后补。
- **核心闭环**：5 步全部覆盖（`TestCoreClosedLoop`）。
- **RBAC 跨层一致性**：双向收敛 + 存在性不泄露覆盖。
- **边界与异常**：PRD §7 可黑盒验证项全覆盖；模型降级/限流为 infra 依赖，提供手动步骤。
- **脚本可执行性**：build tag `e2e` + env 门控，infra 就绪后一键执行；依赖项与已知缺口已标注。
