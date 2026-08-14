# Connector 安全测试矩阵 — 测试用例设计（YS-111）

> 对应设计文档 `design-docs/14-phase1-asset-registry-source.md` §10（29 条安全用例 + 5 条回归）。
> 本文是**用例设计**（前置条件 / 步骤 / 预期 / 断言 / 测试层），供评审与实现锚定；
> 自动化脚本待第 1–3 项（YS-108/109/110）出首个可测分支后，按本设计落地到 `tests/e2e/`（Go `e2e` build tag，沿用既有 `helpers_test.go` 夹具）与 `internal/platform/egress` 单测。

## 约定

- **测试层**：`unit` = Go 包内单测（egress / asset registry / connector）；`e2e` = `tests/e2e/` 黑盒（HTTP 驱动完整栈，`e2e` build tag）；`api` = 直连 mora-api REST（e2e 套件内以 `net/http` 驱动，等价于 pytest 的 requests 行为，沿用既有套件不引新依赖）。
- **夹具**：每个用例创建唯一 workspace + 用户 + Token（既有 `ensureWorkspace` / `seedUser` 模式），用例间数据隔离；SSRF 用例使用受控本地 HTTP 服务器（`httptest.Server`）与受控 DNS resolver 注入。
- **私网段覆盖**（§6.1 全量，用例 1/2/9 显式覆盖，其余在 egress 单测 `TestPolicy_IsPrivate` 全段枚举）：
  `127.0.0.0/8`、`10.0.0.0/8`、`172.16.0.0/12`、`192.168.0.0/16`、`169.254.0.0/16`、`::1`、`fc00::/7`、`fe80::/10`、云 metadata `169.254.169.254`。
- **审计断言**：写动作无权限 → 403 + `code=40300` + `audit_events` 落一条 `denied` 记录；只读无权或不存在 → 404 + `code=40400`，**不**落审计存在性记录（不泄露）。
- 优先级：P0 = 安全红线（SSRF 私网 / metadata / 凭据落库 / 版本覆盖）；P1 = 高；P2 = 中。

---

## 10.1 SSRF / 出网（14 条）

### 用例 1 — URL 来源指向 loopback（`http://127.0.0.1`）
- **优先级** P0 ｜ **测试层** unit + e2e ｜ **覆盖段** `127.0.0.0/8`
- **前置**：`url_api` 类型 Source，`uri=http://127.0.0.1:port/x`，`EGRESS_ALLOW_PRIVATE_RANGES=false`。
- **步骤**：调 Source 创建 → 触发 `Validate`（同步校验）。
- **预期**：`Validate` 返回 SSRF 错误（拒绝）；`source_sync_runs` 不进入 `fetching`。
- **断言**：创建/校验响应含 `code=ssrf_blocked`（或 4xx）；`audit_events` 有一条 egress deny 记录（redacted URL，无内嵌凭据）。

### 用例 2 — URL 来源指向云 metadata（`http://169.254.169.254`）
- **优先级** P0 ｜ **测试层** unit + e2e ｜ **覆盖段** `169.254.0.0/16` + metadata 显式
- **前置**：`uri=http://169.254.169.254/latest/meta-data/`。
- **步骤**：创建 Source 并触发一次 `Fetch`（绕过纯校验，验证 socket 层兜底）。
- **预期**：`Fetch` 拒绝；`DialHook` 在 connect 前阻断，**不**发起 TCP 连接（用受控 listener 断言无连接到达）。
- **断言**：`source_sync_runs.status='failed'`，`error_code` 为 ssrf/private/metadata 类；受控 listener 计数 = 0。

### 用例 3 — 302 重定向到内网（公网首次解析 → `http://10.0.0.1`）
- **优先级** P0 ｜ **测试层** unit + e2e ｜ **覆盖段** `10.0.0.0/8`
- **前置**：受控 `httptest.Server`（公网可达地址）返回 `302 Location: http://10.0.0.1/`；`AllowPrivateRanges=false`。
- **步骤**：`FetchURL(ctx, publicURL, pol)`。
- **预期**：重定向触发**重新解析 + 重新校验**目标 `10.0.0.1`，拒绝；不向 `10.0.0.1` 发起连接。
- **断言**：Fetch 返回 ssrf 错误；受控内网 listener 计数 = 0；审计记录 redirect-target=redacted。

### 用例 4 — DNS rebinding（首次 A 记录公网、第二次内网）
- **优先级** P0 ｜ **测试层** unit ｜ **覆盖段** TOCTOU / rebinding
- **前置**：注入受控 `net.Resolver`，第一次 `LookupHost` 返回公网 IP，第二次返回 `192.168.x`；`DialHook` 在 connect 时再次解析。
- **步骤**：`FetchURL`；观察校验阶段通过（首次公网）但 connect 阶段 `DialHook` 重新解析得内网。
- **预期**：`DialHook` 阻断内网连接；Fetch 失败。
- **断言**：无内网 TCP 连接建立；error 指示 dns_rebinding / private_destination。

### 用例 5 — 响应超过 `MaxResponseBytes`
- **优先级** P1 ｜ **测试层** unit + e2e
- **前置**：受控 Server 流式返回 > `MaxResponseBytes`（默认 100MB，测试用 1KB 阈值）。
- **步骤**：`FetchURL`。
- **预期**：流式读超额即断开连接。
- **断言**：`source_sync_runs.error_code='response_too_large'`；`status='failed'`；读取字节数 ≤ 阈值（未读完整正文）。

### 用例 6 — `Content-Type` 不在 allowlist
- **优先级** P1 ｜ **测试层** unit + e2e
- **前置**：受控 Server 返回 `Content-Type: application/x-shockwave-flash`（不在 `AllowedContentTypes`）。
- **步骤**：`FetchURL`。
- **预期**：拒绝；Run failed。
- **断言**：`source_sync_runs.error_code` 为 content_type_unallowed 类；不写入 manifest。

### 用例 7 — 重定向次数 > `MaxRedirects`
- **优先级** P2 ｜ **测试层** unit
- **前置**：受控 Server 链式 302 共 `MaxRedirects+1` 跳，每跳公网。
- **步骤**：`FetchURL`（`MaxRedirects=5`）。
- **预期**：第 6 跳拒绝。
- **断言**：error 为 too_many_redirects；每跳均经 egress 校验（审计 ≥6 条）。

### 用例 8 — `EGRESS_ALLOW_DOMAINS` 未包含目标主机
- **优先级** P1 ｜ **测试层** unit + e2e
- **前置**：`AllowDomains=['allow.example']`；`uri=http://deny.example/x`（公网解析）。
- **步骤**：`Validate` / `FetchURL`。
- **预期**：拒绝；审计。
- **断言**：error 为 domain_not_allowed；`audit_events` 有 deny 记录。

### 用例 9 — 私网段 `192.168.x`（`AllowPrivateRanges=false` → 拒绝；`internal` + 显式开启 → 放行）
- **优先级** P0 ｜ **测试层** unit + e2e ｜ **覆盖段** `192.168.0.0/16`
- **前置**：两个子场景：(a) `trust_level='untrusted'`、`AllowPrivateRanges=false`、`uri=http://192.168.1.5`；(b) `trust_level='internal'` + 治理 Profile 显式开启 `AllowPrivateRanges=true` + `AllowDomains` 包含该内网主机。
- **步骤**：分别 `Validate`/`Fetch`。
- **预期**：(a) 拒绝；(b) 放行，Fetch 成功。
- **断言**：(a) `status='failed'` + ssrf error；(b) `status='ready'`，manifest 有 entry；二者均审计。

### 用例 10 — Git 来源 `file://` 协议
- **优先级** P0 ｜ **测试层** unit + e2e
- **前置**：`source_type='git'`，`uri=file:///path/to/repo`。
- **步骤**：创建 Source / `Validate`。
- **预期**：拒绝（`file://` 禁止；本地文件来源走 file adapter）。
- **断言**：error 为 protocol_not_allowed；引导提示使用 file source_type。

### 用例 11 — Git 凭据不出现在 `.git/config` / 日志
- **优先级** P0 ｜ **测试层** e2e
- **前置**：`source_type='git'`，`https://user:token@host/repo`，`credential_ref` 指向测试 Secret；触发一次 Fetch 到隔离临时目录。
- **步骤**：Fetch 后读取 clone 目录的 `.git/config`、process env、audit/log 输出。
- **预期**：凭据明文不出现于 `.git/config`、remote URL、日志、trace。
- **断言**：`grep` clone 目录与日志输出不含 token 明文；`.git/config` 的 `remote.origin.url` 不带 userinfo；fetch 后 helper 已清理（无残留 credential store）。

### 用例 12 — 文件来源压缩炸弹（100:1）
- **优先级** P0 ｜ **测试层** unit + e2e
- **前置**：构造高压缩比文件（如 1KB gzip 解压为 >100KB，或 zip-bomb 样本），`source_type='file'`。
- **步骤**：`Fetch`/解析。
- **预期**：解压比超阈值（默认 100:1）或解压后总大小超限 → 拒绝。
- **断言**：error 为 decompression_bomb / size_limit；不落 manifest。

### 用例 13 — 文件来源路径穿越（`../etc/passwd`）
- **优先级** P0 ｜ **测试层** unit + e2e
- **前置**：`source_type='file'`，uri/路径含 `../etc/passwd` 或符号链接逃逸。
- **步骤**：`Validate` / `Fetch`。
- **预期**：`filepath.Clean` + 拒绝 `..` + 限制根目录 → 拒绝。
- **断言**：error 为 path_traversal；读取到的路径规范化后仍在 workspace 根目录下。

### 用例 14 — 文件来源 MIME/扩展名不匹配
- **优先级** P1 ｜ **测试层** unit + e2e
- **前置**：`a.exe` 改名 `a.pdf`（MIME sniff 与扩展名冲突）；`source_type='file'`。
- **步骤**：`Fetch`。
- **预期**：MIME + 扩展名双校验失败 → 拒绝。
- **断言**：error 为 mime_mismatch；不落 manifest。

---

## 10.2 凭据隔离（5 条）

### 用例 15 — `knowledge_sources.uri_normalized` 已移除内嵌凭据
- **优先级** P0 ｜ **测试层** e2e + 直查 DB
- **前置**：创建 Source `uri=https://user:pass@host/path`，`credential_ref` 指向 Secret。
- **步骤**：创建后直查 `knowledge_sources`。
- **预期**：`uri_normalized='https://host/path'`（userinfo 移除）；`credential_ref` 非空且不含明文。
- **断言**：DB 中 `uri_normalized` 不含 `user:pass`；`credential_ref` 列不含明文凭据；唯一索引 `uq_sources_workspace_uri` 防同源重复登记（重复创建返回 409/幂等）。

### 用例 16 — Run 读快照，Source 配置被改后不影响已排队 Run
- **优先级** P0 ｜ **测试层** e2e
- **前置**：创建 Source → 触发 sync Run（`status='queued'`）→ 在 Worker 消费前 PATCH 修改 Source `sync_policy`/`uri`。
- **步骤**：让 Worker 消费该 Run。
- **预期**：Run 使用创建时固化的 `source_config_snapshot` + `credential_version`，不受 PATCH 影响。
- **断言**：Run 完成后其 manifest / resolved_revision 与快照一致；DB `source_sync_runs.source_config_snapshot` 与被改后的 `knowledge_sources` 配置不同。

### 用例 17 — 凭据轮换后旧 Run 用旧版本、新 Run 用新版本
- **优先级** P1 ｜ **测试层** e2e
- **前置**：Source v1 凭据 → Run A `queued` → `PUT /credentials` 轮换到 v2 → 触发 Run B。
- **步骤**：两 Run 分别完成。
- **预期**：Run A 用 v1（`credential_version='v1'`），Run B 用 v2。
- **断言**：`source_sync_runs.credential_version` 分别为 v1/v2；轮换不导致 Run A 失败或重试。

### 用例 18 — `last_error` / 日志 / trace 凭据脱敏
- **优先级** P0 ｜ **测试层** unit + e2e
- **前置**：构造一次失败 Run（如 SSRF 拒绝 / 401），Source URI 含内嵌凭据 `https://user:secret@host`。
- **步骤**：读取 `source_sync_runs.error_detail_redacted`、`knowledge_sources.last_error`、审计/日志输出。
- **预期**：所有字段与输出中凭据明文打码（`https://host` 或 `***:***`）。
- **断言**：`grep` 全部输出不含 `secret` 明文；`last_error` / `error_detail_redacted` 已脱敏。

### 用例 19 — Connector 不接受 `allowed_asset_ids` 或用户 Token
- **优先级** P0 ｜ **测试层** unit（connector 契约）
- **前置**：`SourceConnector` 端口定义（§4.1）只接受 Run 快照（脱敏 Source 配置 + `credential_version`）。
- **步骤**：构造 `ValidateRequest`/`Fetch` 调用，注入 `allowed_asset_ids` 字段或 `user_token` 字段。
- **预期**：端口不接受该参数（编译期/契约层即无此字段）；运行期若传入则拒绝。
- **断言**：`ValidateRequest` 结构体无 `allowed_asset_ids`/`user_token` 字段（静态校验）；若 worker 误传，Connector 返回契约错误。

---

## 10.3 版本原子切换（5 条）

### 用例 20 — required projection 缺失时 `build_status` 不得转 `ready`
- **优先级** P0 ｜ **测试层** unit + e2e
- **前置**：某版本 required_projections=`['fts','vector']`；`fts` ready、`vector` 缺失/failed。
- **步骤**：触发 build 完成回调 / 对账扫描。
- **预期**：门禁拒绝；`build_status` 保持 `building`/`pending`，不转 `ready`。
- **断言**：`knowledge_asset_versions.build_status != 'ready'`；不写 CAS 激活。

### 用例 21 — 构建失败时 `current_version_id` 不被覆盖
- **优先级** P0 ｜ **测试层** e2e
- **前置**：Asset 已有 `current_version_id=V1`（ready+published）；新 V2 构建失败。
- **步骤**：V2 build failed。
- **预期**：`current_version_id` 仍指向 V1。
- **断言**：DB `knowledge_assets.current_version_id = V1`；查询返回 V1 内容 + `stale/building` 标记。

### 用例 22 — 旧版本晚完成（`latest_requested_version_no` 已前进）CAS 失败
- **优先级** P0 ｜ **测试层** unit + e2e
- **前置**：V1、V2、V3 顺序创建；V2 慢于 V3 完成；`latest_requested_version_no` 已到 3。
- **步骤**：V2 完成时尝试 CAS 激活（`expected_current=V1`, `latest_requested=2`）。
- **预期**：CAS 失败（条件不匹配）；只标记 V2 `build_status='ready'`，不切换 `current_version_id`。
- **断言**：`current_version_id` 未变为 V2；V2 行 `build_status='ready'` 但 `governance_status` 仍 candidate；SQL 影响 0 行（`latest_requested_version_no=3` ≠ 2）。

### 用例 23 — `governance_status='candidate'` 版本不作为查询结果
- **优先级** P0 ｜ **测试层** e2e
- **前置**：Asset 有 V1 published（current）、V2 candidate。
- **步骤**：`GET /knowledge/assets/{id}` 与 `GET /knowledge/assets/{id}/versions`。
- **预期**：详情返回 V1（最后 published）；versions 列表可含 V2 但标记 candidate；current 不指向 V2。
- **断言**：详情响应 `current_version_id=V1`；V2 非 published。

### 用例 24 — 人工回滚未显式 `expected_current` 被拒绝
- **优先级** P0 ｜ **测试层** e2e
- **前置**：Asset current=V2；尝试回滚到 V1。
- **步骤**：`POST /knowledge/reviews/{id}/decisions` decision=`promote`/`deprecate`，**不带** `expected_current`。
- **预期**：拒绝（4xx，`code=409xx` optimistic-lock）。
- **断言**：响应拒绝；`current_version_id` 未变；带正确 `expected_current` 时成功（对照用例）。

---

## 10.4 越权与存在性不泄露（5 条，Phase 1 扩展）

### 用例 25 — 无 `sync` 权限调 Source 创建/同步 → 403 + 审计
- **优先级** P0 ｜ **测试层** e2e
- **前置**：workspace 内 `read` 权限用户（无 `sync`）。
- **步骤**：`POST /workspaces/{ws}/knowledge/sources`、`POST /knowledge/sources/{id}/sync-runs`。
- **预期**：403 + `code=40300`；写审计 `denied`。
- **断言**：响应 403；`audit_events` 有 `action=sync, result=denied` 记录。

### 用例 26 — 无 `read` 权限调 `GET /knowledge/assets/{id}` → 404（不泄露存在）
- **优先级** P0 ｜ **测试层** e2e
- **前置**：Asset 存在于 workspace A；workspace B 的 `read` 用户调 A 的 asset。
- **步骤**：`GET /knowledge/assets/{id}`（用 B 的 token）。
- **预期**：404 + `code=40400`，响应体与"不存在"完全一致（无 403 泄露）。
- **断言**：响应 404；body 不含资产字段；无 `denied` 审计存在性记录（不泄露）。

### 用例 27 — 跨 workspace 引用 asset/source/relation → 404
- **优先级** P0 ｜ **测试层** e2e
- **前置**：wsA 的 source/asset/relation；wsB 的 `read` 用户。
- **步骤**：B 调 `GET /knowledge/sources/{id}`、`GET /knowledge/assets/{id}`、`GET /knowledge/assets/{id}/relations`。
- **预期**：均 404 + `code=40400`。
- **断言**：三端点均 404；body 一致；不泄露跨 workspace 存在性。

### 用例 28 — 撤权 Source（`enabled=false`）后下一次同步请求被拒绝
- **优先级** P1 ｜ **测试层** e2e
- **前置**：Source 创建并 `enabled=true` → 成功同步一次 → `PATCH enabled=false`（软删除）。
- **步骤**：再 `POST /knowledge/sources/{id}/sync-runs`。
- **预期**：同步拒绝（4xx，`code=409xx` source_disabled 或 404）。
- **断言**：不新建 `source_sync_runs` 行；`current_version_id` 冻结。

### 用例 29 — `review` 动作不在 `review_roles` 中的主体 → 403
- **优先级** P1 ｜ **测试层** e2e
- **前置**：治理 Profile `review_roles=[admin]`；`write` 权限（非 review 角色）用户。
- **步骤**：`POST /knowledge/reviews/{id}/decisions` decision=approve。
- **预期**：403 + `code=40300` + 审计。
- **断言**：响应 403；`review_decisions` 无新增行；`audit_events` 有 denied 记录。

---

## 10.5 回归（无退化，§8.3 扩展，5 条）

| # | 用例 | 测试层 | 预期 |
|---|---|---|---|
| R1 | `rbac/engine_test.go` 全绿 | unit | `go test ./internal/platform/rbac/...` PASS（**已验证 baseline 绿**，2026-08-11，Go 1.25） |
| R2 | `doc_events` RAG 索引链路不变 | e2e | 既有 `ac_rag_test.go` 文档写入 → 索引 → 检索链路 PASS |
| R3 | `rag-worker` 行为不变 | e2e | `rag-worker` 消费 `doc_events`，索引状态收敛 PASS |
| R4 | MCP `search_knowledge_base`/`get_document`/`list_documents` 行为不变 | e2e | `mcp_protocol_test.go` 三工具调用与返回结构不变 |
| R5 | `DocWriteSink.WriteDoc` 双写行为不变（存量文档双写新增 Asset 行不影响文档读/写/协同） | e2e + DB | 文档创建/编辑/协同路径 PASS；Phase 1 双写后额外校验 `knowledge_assets`+`knowledge_asset_versions` 与 `documents` 同事务一致（`current_version_id` = `documents.version_no`） |

---

## 实现映射（脚本落地计划，待 SUT 就绪）

| 测试层 | 目录 | 覆盖用例 |
|---|---|---|
| Go unit（egress） | `internal/platform/egress/*_test.go` | 1–9（SSRF 私网段全枚举）、5/6/7（大小/类型/跳转）、4（rebinding）、19（connector 契约静态校验） |
| Go unit（asset registry / connector） | `internal/module/knowledge/asset/*_test.go`、`source/connector/*_test.go` | 15（部分，快照脱敏）、20–22（CAS）、12–14（file adapter 安全） |
| Go e2e（`tests/e2e/`，`e2e` build tag） | `tests/e2e/source_security_test.go`（新增） | 1–3、8–11、15–18、21、23–29、R2–R5 |
| Go e2e（既有回归） | 既有 `ac_rag_test.go`/`mcp_protocol_test.go`/`core_closed_loop_test.go` | R2–R4（基线） |

**依赖**：第 3 项（YS-110：knowledge-worker + egress + 版本切换）出首个可测分支后，按本设计填充脚本；第 1、2 项（YS-108/109）提供表结构与管理 API。
