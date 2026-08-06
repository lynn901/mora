# 协同编辑 CRDT 链路选型决策书（R2）

> Issue：YS-66（R2 CRDT 修复方案技术评审 A/B/C 选型）
> 上游：YS-42（协同编辑 CRDT 同步未集成 — 多端实时同步降级为 Local-only）
> 下游：YS-42 实施、YS-65（R4 通知）、R8（画板/白板）
> 评审角色：Mora 项目架构师（架构评审 / 技术选型决策，不参与编码实现）
> 状态：**已评审定稿** · 定稿日期 2026-08-03
> 关联文档：`design-docs/01-tech-selection-decision.md` §3.2.4、`design-docs/02-system-architecture.md` §6.1 / §7.1

---

## 1. 背景

### 1.1 现象（YS-42）

打开文档后协同状态短暂显示 "Live"（WebSocket 已连接），5 秒后降级为 "Local"。单端 autosave 落库正常，多端无法实时同步内容。

### 1.2 根因（已在 YS-42 定位，本评审复核代码确认）

前端 `MoraCollabProvider`（`web/src/lib/collab-provider.ts`）通过 WebSocket 连接 mora-api 的 `/api/v1/ws/collab/:document_id`，使用**自定义 JSON+Base64 文本协议**（`{type:"sync-step1", payload:"<base64>"}`）发起 Yjs `sync-step1`，期望 `sync-step2` 响应。

但 Go collab hub（`internal/module/mora/collab/hub.go` + `cmd/mora-api/wiring.go:serveCollab`）仅实现 `presence`/`cursor`/`update` 中继与并发准入降级，**不实现 sync-step1/2 交换**，收到 `sync-step1` 后无任何 case 分支处理。前端 `SYNC_TIMEOUT=5000ms` 内未收到 `sync-step2`，触发 `local-only` 降级（`collab-provider.ts:380-387`）。

yjs-server（`y-websocket@2.1.0`，标准 y-websocket-server，端口 1234）虽已运行（`deployments/Dockerfile.yjs`），但**前端不直连它**，Go hub 也未桥接——CRDT 状态没有任何组件在维护。

### 1.3 三方案（来自 YS-42）

- **方案 A**：Go hub 实现 sync-step1/2，Go 侧用 Yjs CRDT 管理文档状态。
- **方案 B**：前端直连 yjs-server 做 CRDT 同步，Go hub 仅做 presence/cursor。
- **方案 C**：Go hub 将 sync-step1/2 转发给 yjs-server（Go hub 作 proxy）。

---

## 2. 关键事实复核（代码 + 生态）

### 2.1 与既有架构设计的一致性（决定性依据）

mora 已定稿的架构文档**已经决定了目标拓扑**，当前代码是偏离而非未决：

| 文档 | 条目 | 已定结论 |
|---|---|---|
| `02-system-architecture.md` §6.1 通信矩阵 | `前端 → yjs-server` | WSS 443/8082，协同编辑**直连 yjs-server** |
| `02-system-architecture.md` §6.1 通信矩阵 | `前端 → Mora API` | **SSE**，实时通知（非 WebSocket） |
| `02-system-architecture.md` §7.1 扩展矩阵 | `yjs-server` | 按文档分片、粘性路由，awareness 走 Valkey 共享 |
| `02-system-architecture.md` §5.5 Valkey 用途 | 协同 awareness | yjs-server 利用 Valkey 跨实例同步 awareness |
| `01-tech-selection-decision.md` §3.2.4 | 协同编辑 | Yjs（CRDT）+ yjs-server 提供 awareness，**不自研 OT** |
| `01-tech-selection-decision.md` §8 决策表 | 协同编辑 | Yjs CRDT vs 自研 OT → 选 Yjs，"成熟库降低复杂度" |

> 即：**架构基线本就是「前端直连 yjs-server 做 CRDT，Go hub 做 presence/准入，通知走 SSE」**——这等价于方案 B。当前 bug 的本质是 R2 实现期把协同 WS 接到了 Go hub 的自定义协议上，与设计基线不一致。本评审的职责是把方案选型讲清楚并锁定，纠正这一偏离。

### 2.2 Go Yjs 生态（决定方案 A 不可行）

对 Go 侧实现 Yjs CRDT 的候选库做了穷尽检索，结论：**无生产可用实现**。

| 候选 | star | 末次活跃 | License | 评估 |
|---|---|---|---|---|
| `averyyan/YJS-GO` | 18 | 2023-08（3 年停滞） | Apache-2.0 | 仅 CRDT 类型，无 sync server 端点，测于 Yjs v13.4.14（现 v13.6.31）；实质弃坑 |
| `ralfbawg/YJS-GO` | 0 | 2026-05（仅 2 commit） | Apache-2.0 | 自称"85% 生产就绪"但 commit 史暴露为 fork 翻译；Y.Set 集成有已知缺陷 |
| `CivNode/yjs-go` | 0 | 2026-04 | MIT | 最诚实，README 明示 alpha：update-v2 未实现、XmlFragment 仅 `Len()`、content type 2/9 decode-only stub |
| `CFTL/YGS` | 1 | 2025-05（单 commit "初版"） | Apache-2.0 | 无 README、无测试 |

**关键区分**：解码/编码 Yjs update 字节 ≠ 能作为维护文档状态的同步端点。后者需要：按 room 维护 `Y.Doc`、`encodeStateVector`、`encodeStateAsUpdate(doc, remoteVector)` 算 diff、apply update、GC。仅 `CivNode` 实现了 sync 握手且有 interop 测试，但缺 update-v2、shared type 不全，0 star 单作者 alpha。

JS 参考实现 `yjs`（22.3k star，v13.6.31，2026-05-28 仍活跃）是事实标准。在 Go 里追平需 3–6 个月且**永久背负 CRDT 实现维护债**，与 §2.1 "不自研、复用成熟库" 的既定决策直接冲突。

### 2.3 y-websocket 协议与代理可行性（决定 B/C 可行）

- **协议**：每个 WS 帧首字节为顶层消息类型 varUint：`0`=sync、`1`=awareness、`2`=auth、`3`=query-awareness。sync 子协议：`SyncStep1=0`（携带 state vector）、`SyncStep2=1`（携带 diff update）、`Update=2`。
- **握手**：client 发 SyncStep1(本地 state vector) → server 回 SyncStep2(缺失 update) 并立即回发自己的 SyncStep1 → client 回 SyncStep2。后续增量走 Update。
- **awareness**：携带光标/在线状态等临时态，**与 sync 复用同一 WS 连接**（顶层 type=1），无需独立通道；y-websocket-server 在同一 room 内广播 awareness。这直接回答了"presence/cursor 是否需要单独链路"——**不需要**。
- **无状态代理可行**：sync-step1/2 帧自描述、双向对称，一个只做 WS 帧双向透传的 proxy（保留 room 名在 URL path）可透明转发 sync/update/awareness，**无需理解 CRDT 状态**。proxy 只有在要自己作为同步参与方（发自己的 SyncStep1）时才需要持有 `Y.Doc`——那正是方案 A。
- **反代支持**：y-websocket-server 是标准 Node `http.createServer` + `ws`，nginx WS upgrade 反代是其标准部署；upgrade 事件可读 cookie/查询参数做鉴权。

### 2.4 License（合规）

实际会用到的栈 **全部 MIT**，无传染：

| 包 | License | 风险 |
|---|---|---|
| yjs（核心 CRDT） | MIT | 无 |
| y-protocols（sync/awareness 线协议） | MIT | 无 |
| y-websocket（client provider） | MIT | 无 |
| y-websocket-server（basic Node 后端，当前在用） | MIT | 无 |
| hocuspocus（可选增强后端，2.5k star，活跃） | MIT | 无 |
| **yhub**（`yjs/yhub`，可扩展 Redis 后端） | **AGPL-3.0 / 双授权** | **传染** |

> **合规红线**：`yhub` 为 AGPL，私有化产品若 network-serve 会被要求开源整个后端。**禁止引入 yhub** 除非购买商业授权。当前 `Dockerfile.yjs` 用的是 `y-websocket@2.1.0`（MIT），合规。

### 2.5 持久化缺口（实施风险，必须在 YS-42 处理）

当前 `y-websocket@2.1.0` 是**纯内存**版本，无 `CALLBACK_URL` 持久化钩子。mora 的文档模型是不可改写版本历史（`domain.DocumentVersion`，`version` 包做 diff/快照，autosave 经 REST `PUT /documents/:id` 落库 + 发 `document.update` 事件触发 RAG）。CRDT 文档状态若不落库，**容器重启 / yjs-server 重启会丢失内存中的协同态**。这是方案 B/C 共有的实施前提，非选型分歧，见 §5 风险与缓解。

---

## 3. 三方案权衡对比

| 维度 | 方案 A（Go 自实现 Yjs） | 方案 B（前端直连 yjs-server） | 方案 C（Go hub proxy 转发） |
|---|---|---|---|
| **CRDT 一致性** | Go 侧自维护 `Y.Doc`，状态在 Go | yjs-server 维护，成熟实现 | yjs-server 维护，Go 透传不参与 |
| **与既有架构契合** | ❌ 违背"不自研 OT/CRDT"决策 | ✅ **正是 §6.1 通信矩阵基线** | ⚠️ Go hub 进数据路径，基线无此角色 |
| **技术可行性** | ❌ 无生产可用 Go 库（§2.2） | ✅ 今天即可，前端改协议+nginx 反代 | ✅ 今天即可，Go 加 ~200 行 WS relay |
| **实现复杂度** | 极高（3–6 月 + 永久维护债） | 低（前端切标准 y-websocket provider + nginx 加 location） | 中（Go 双向 WS relay + 生命周期管理） |
| **维护成本** | 极高（自维护 CRDT，跟随 Yjs 上游） | 低（复用上游 y-websocket 生态） | 中（多一层自研 proxy 需维护） |
| **鉴权/RBAC 落点** | Go hub 一次性校验 | yjs-server upgrade 处校验 token+RBAC | Go hub 边缘校验后透传 |
| **存在性不泄露** | Go hub 可强约束 | 需在 yjs-server upgrade 鉴权里复刻 RBAC | Go hub 可强约束 |
| **持久化钩子** | Go 直连 DB，最直接 | yjs-server 经回调/webhook 落 mora-api | 同 B，但 proxy 可拦截做旁路 |
| **可观测/审计** | Go 侧天然可见 | yjs-server 侧需补埋点 | Go proxy 侧可见帧元数据 |
| **License** | 自研无 License 问题 | MIT 栈，无传染 | 同 B |
| **下游通知复用** | — | 见 §4.1 | 见 §4.1 |
| **多副本扩展** | Go 自管状态，难 | §7.1 已规划分片+Valkey awareness | proxy 无状态，但 yjs-server 仍需分片 |

### 方案 A 不入选（否决）

- 违背 `01-tech-selection` §3.2.4 / §8 "不自研 OT/CRDT、复用成熟库"的既定决策。
- 无生产可用 Go Yjs 库（§2.2），自研 3–6 月且永久背负 CRDT 实现维护债，与 Yjs JS 上游（活跃维护）长期脱节。
- 即便建成，也使 mora 成为"自维护 CRDT"的孤岛，丧失 y-websocket 生态红利。

### 方案 B vs C 的本质区别

两者都让 yjs-server 承担 CRDT 状态，差别只在 **Go hub 是否在数据路径上**：

- **B**：Go hub 退化为 presence/准入 sidecar，协同 WS 流量不经 Go。鉴权在 yjs-server upgrade 处完成（token 查询参数 + 复用 mora-api RBAC）。
- **C**：Go hub 作为鉴权边缘 + 无状态 WS relay，校验 JWT/RBAC 后透传帧到 `ws://yjs-server:1234/<room>`。

### 选 B 还是 C——判定

**结论：选定方案 B，作为目标终态。** 理由：

1. **架构一致性最高**：§6.1 通信矩阵白纸黑字写明"前端 → yjs-server WSS，协同编辑"。方案 B 即该基线，方案 C 引入了基线未定义的 Go proxy 角色，是架构偏移。
2. **复杂度最低**：方案 B 只需「前端 collab-provider 改用标准 y-websocket 协议 + nginx 增加 yjs-server WS 反代 location + yjs-server upgrade 处接入鉴权」。方案 C 多一层自研 Go relay，多一份需长期维护的 I/O 代码与生命周期管理（双向 pump、半关闭、背压），收益仅为"鉴权在 Go"。
3. **鉴权可在 yjs-server 侧实现**：y-websocket-server upgrade 事件可读 URL 查询参数里的 JWT，调用 mora-api 的 RBAC（内部 service token，已有 `INTERNAL_SERVICE_TOKEN`）做文档权限校验。这满足 mora"显式优先、存在性不泄露"——无权文档在 upgrade 阶段即 401，不进入 room，目录树/检索本就过滤。Go hub 的 `serveCollab` 已有现成 `tm.Verify` + docID 解析逻辑可参考。
4. **通知链路本就走 SSE，不与协同 WS 复用**（§4.1），所以"Go hub 是否在协同路径"对通知无影响——C 的唯一潜在优势（统一 WS 入口）不成立。

**关于方案 C 的定位**：C 是合法的过渡/备选，**不否决其作为可选实现路径**。若研发评估"yjs-server 侧接入 RBAC 鉴权改造成本高于 Go 侧加 relay"，可采用 C 作为 M2 交付路径，但须明确：C 是实现选择，不改变"yjs-server 持有 CRDT 状态、通知走 SSE"的架构基线。选型结论锁定 B（目标态），允许 C（实现态备选）。

---

## 4. 下游影响结论

### 4.1 通知（YS-65 / R4）的 WebSocket 实时推送如何复用本方案选定的协同链路

**结论：不复用协同 WS，通知实时推送走 SSE，与 `02-system-architecture.md` §6.1 一致。**

- §6.1 通信矩阵已区分两条链路：协同编辑 = `前端 → yjs-server WSS`；实时通知 = `前端 → Mora API SSE`。两者职责不同，**本就不应复用同一连接**。
- 技术上协同 WS 承载的是 Yjs 二进制 CRDT 帧（type 0/1/3），与通知的业务 JSON 消息语义、QoS、生命周期都不同；复用会让 y-websocket provider 的帧解析与通知分发耦合，破坏 CRDT 链路的纯粹性。
- **SSE 完全满足通知场景**：单向推送、自动重连、走 mora-api 的 JWT 鉴权与 RBAC，无需双向 WS。通知中心（铃铛+未读计数）+ 历史通知查询走 REST，实时触达走 SSE。
- 因此 YS-65 的 WS 推送方案**不依赖 R2 的 A/B/C 选型**，本评审解锁后通知可直接定稿为 SSE。YS-65 描述里"若 R2 选 B 则通知牵头转前端"的前提不成立（B 不改变通知走 SSE、后端为主的归属）——**通知仍由后端牵头**。

### 4.2 R8（画板/白板）协同链路是否复用

**结论：复用同一 yjs-server CRDT 链路，分 room 隔离，不另起协同基础设施。**

- 画板的实时协同本质同样是多端状态同步，Yjs CRDT 同样适用（yjs 生态有 y-tldraw / tldraw-yjs 等白板协同绑定先例）。
- 复用方式：画板文档与文本文档用**不同的 room 命名空间**（如 `doc:<uuid>` vs `board:<uuid>`）隔离，共用 yjs-server 实例、同一套鉴权/反代/持久化基础设施。
- 前端画板 provider 同样采用标准 y-websocket 协议直连 yjs-server（方案 B 同款），与文档协同链路一致。
- **R8 的依赖点**：R8 依赖 R2 落地后的 yjs-server 链路（鉴权、反代、持久化、多副本分片）就绪。R2 选 B 即为 R8 铺好同一基座，R8 无需再选型。
- 风险提示：画板的 CRDT 状态体积与更新频率远高于文本文档（矢量图形对象多），需在 R8 立项时单独评估 yjs-server 单实例容量与分片策略，不在本次评审范围。

---

## 5. 风险与缓解

| 风险 | 等级 | 缓解 |
|---|---|---|
| **持久化缺口**：当前 `y-websocket@2.1.0` 纯内存，重启丢协同态 | 高 | YS-42 实施必须接入持久化：yjs-server 升级为支持 `CALLBACK_URL`/持久化的后端（自扩展 y-websocket-server 或评估 hocuspocus，均 MIT），文档更新经回调/webhook 落 mora-api 的 `PUT /documents` + 版本快照 + `document.update` 事件触发 RAG。**禁止用 yhub（AGPL）**。 |
| **鉴权落地**：yjs-server upgrade 处需复刻 mora RBAC | 中 | y-websocket-server upgrade 事件读 JWT（URL 查询参数），调 mora-api 内部 RBAC 接口（`INTERNAL_SERVICE_TOKEN`）校验文档权限；无权 401 不进 room。可参考 `serveCollab` 现有 `tm.Verify`+`docID` 逻辑。 |
| **存在性不泄露**：无权文档不可经协同链路探测 | 中 | RBAC 在 upgrade 阶段强约束（无权直接拒，不回 room 信息）；room 名用文档 UUID，不在前端枚举。与目录树/检索的过滤策略一致。 |
| **前端协议迁移**：从自定义 JSON+Base64 改为标准 y-websocket 二进制 | 中 | `collab-provider.ts` 当前自定义协议需替换为标准 `y-websocket` provider（`new WebsocketProvider(url, roomname, doc, opts)`）。awareness 已用 `y-protocols/awareness`，迁移后 presence/cursor 走同一连接的 awareness 通道，Go hub 的 presence 中继可逐步退役或保留作降级。 |
| **nginx 反代**：当前 nginx 仅反代 `/api/v1/ws/` 到 mora-api，未代理 yjs-server | 低 | 增加 `location /collab/`（或 `/yjs/`）反代到 `yjs-server:1234`，配 WS upgrade + `proxy_read_timeout 86400`。yjs-server 可仅绑内网、不直接暴露宿主端口（当前 compose 暴露 1234 可收敛）。 |
| **多副本**：单实例够 MVP，多副本需分片+粘性路由 | 低（M2 不涉及） | §7.1 已规划 yjs-server 按文档分片+粘性路由、awareness 走 Valkey。M2 单实例先跑通，多副本留 P1。 |
| **可观测**：yjs-server 侧缺埋点 | 低 | M2 补 yjs-server 连接数/room 数/消息量指标到 Prometheus；Go hub 保留 presence 指标。 |

---

## 6. 实施责任归属

**R2（YS-42）实施由后端牵头，前端协同。**

- 选定方案 B，但核心工作量跨前后端：
  - **后端（牵头）**：yjs-server 持久化接入（回调落库 + 版本快照 + RAG 事件）、upgrade 鉴权接入 mora RBAC、nginx 反代配置、yjs-server 容器端口收敛、可观测埋点。这是协同链路"能正确工作且安全"的关键，后端为主。
  - **前端（协同）**：`collab-provider.ts` 从自定义协议迁移到标准 y-websocket provider；`stores/collab.ts` 调整 serverUrl 与 room 命名；awareness/presence 走标准通道；验证多端 sync/cursor/reconnect。
- 牵头判据：方案 B 虽"前端改协议"看似前端为主，但**持久化与鉴权是协同能否上生产的硬约束且都在后端**，故后端牵头。这也纠正了路线图 YS-58 表格里"R2 实现当前误指派前端，待改派后端"的标注——本评审确认改派后端。
- 通知（YS-65）独立由后端牵头（SSE，§4.1），不随 R2 牵头角色变化。

---

## 7. 决策结论

1. **选定方案 B（前端直连 yjs-server，标准 y-websocket 协议）作为协同编辑 CRDT 链路目标终态**，与 `02-system-architecture.md` §6.1 / §7.1 既有架构基线一致。
2. **否决方案 A**（Go 自实现 Yjs CRDT）：违背"不自研 OT/CRDT"既定决策，无生产可用 Go 库，维护债不可接受。
3. **方案 C（Go hub proxy）作为实现态可选备选**，不改变架构基线；仅当研发评估 yjs-server 侧鉴权改造成本显著高于 Go relay 时采用。
4. **通知（R4）走 SSE，不复用协同 WS**；通知仍由后端牵头。
5. **R8 画板复用同一 yjs-server 链路**，分 room 命名空间隔离，R2 落地即为其铺好基座。
6. **R2 实施后端牵头、前端协同**；持久化与 RBAC 鉴权是硬约束，禁止引入 AGPL 的 yhub。
7. 本决策书可被 YS-42（实施）、YS-65（通知）、R8（画板）直接引用。

---

## 附录 A：协议要点速查（供研发实现参考）

```
y-websocket 顶层消息类型（每帧首字节 varUint）：
  0 = sync        1 = awareness     2 = auth     3 = query-awareness

sync 子协议（type=0 后的 payload）：
  SyncStep1 = 0   varBuffer(stateVector)      // 我方状态向量
  SyncStep2 = 1   varBuffer(documentUpdate)    // 相对对方向量的 diff
  Update     = 2   varBuffer(documentUpdate)    // 增量更新

握手：client SyncStep1 → server SyncStep2 + server SyncStep1 → client SyncStep2 → 之后增量 Update
awareness：与 sync 同连接（type=1），携带光标/在线状态，30s 未刷新剔除本地、每 15s 重播己方状态
room：URL path 携带 room 名（文档 UUID），y-websocket-server 按 room 广播
```

## 附录 B：参考代码与文档

- `web/src/lib/collab-provider.ts`（当前自定义协议 provider，需迁移）
- `web/src/stores/collab.ts:52-53`（当前 serverUrl 指向 Go hub）
- `internal/module/mora/collab/hub.go`（Go presence/准入 hub，保留）
- `cmd/mora-api/wiring.go:30 serveCollab`（Go WS handler，鉴权逻辑可参考）
- `deployments/Dockerfile.yjs`（`y-websocket@2.1.0`，需补持久化）
- `deployments/nginx.conf:59-68`（当前仅反代 `/api/v1/ws/`，需加 yjs-server location）
- `deployments/docker-compose.yml:207-221`（yjs-server 暴露 1234，可收敛为内网）
- `design-docs/02-system-architecture.md` §6.1 / §7.1 / §5.5
- `design-docs/01-tech-selection-decision.md` §3.2.4 / §8
- 上游 Yjs：`yjs/yjs`(MIT, 22.3k★)、`yjs/y-protocols`(MIT)、`yjs/y-websocket`(MIT)、`yjs/y-websocket-server`(MIT)；**`yjs/yhub`(AGPL，禁用)**
