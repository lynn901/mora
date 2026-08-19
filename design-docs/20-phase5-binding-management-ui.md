# Phase 5 配装管理 UI 设计稿（YS-165）

> 配套架构文档：`design-docs/19-phase5-skill-agent-binding.md`（§1.2 / §5.1 / §6.1 / §8 / §11.4）。
> 本稿为设计师主导交付。§6.1 REST 契约已由 YS-163（分支 `agent/mora/1f95a4dd-y163`，commit `8958253`）交付验收通过，本稿**已按真实 §6.1 路由与数据结构对齐校准**（路由、ETag、Idempotency-Key、`PinnedVersionBlocked` 阻断信号、severity 枚举等），前端研发可直接据此落地，不再有契约偏差需猜。
> 交付物聚焦设计：信息架构、交互规范、状态完备性、可达性、设计令牌映射。不含生产代码。

## 1. 设计目标与边界

### 1.1 要解决的设计问题

Phase 5 把可复用 Skill 作为受治理资产，**Mora 不执行 Skill**。配装管理 UI 要让工作空间管理员对"Agent 能用哪些资产、用哪个版本、以什么方式交付、显式排除什么"形成**可读、可改、可撤销、可审计**的可视化治理面，同时严格遵守三条架构红线：

1. **不执行**：UI 全程不触发任何 Skill 脚本执行；导入/预览/索引/校验在 UI 上表现为"只读展示结果"，不出现"运行/试跑"入口。
2. **不外传 Secret**：只展示 Secret 的需求声明（是否需要、作用域），永不展示值。
3. **存在性不泄露**：无权访问的资产在配装列表与选择器中不可见，不区分"无权"与"不存在"。

### 1.2 范围内 / 范围外

| 范围内（本稿覆盖） | 范围外（由其他交付负责） |
|---|---|
| Binding 列表 / 创建 / 撤销 / 批量配装 / 固定版本选择器 / delivery_mode 选择器 / 校验报告可视化 / 阻断态展示 / Secret 仅声明 | REST 契约实现（YS-163）、前端编码落地（前端研发）、`internal/module/skill/` 静态 Validator（YS-161）、生效集解析与缓存失效（YS-162）、MCP 工具（YS-164）、验收测试（YS-166） |

## 2. 信息架构

### 2.1 入口与导航

配装管理作为 Agent 治理的子页，挂在既有 Agent 管理域下，不新建顶层导航：

```
工作空间
└─ Agent 管理
   └─ <选中某 Agent>  → Agent 详情
      ├─ 概览（既有）
      ├─ 配装管理（Binding）  ← 本稿
      │   ├─ 生效集（只读解析结果，§5）
      │   └─ 配装列表（CRUD + 批量）
      └─ 审计（既有，配装变更走这里）
```

**理由**：Binding 的主体是 Agent，按 Agent 聚合而非全局扁平列表，避免跨 Agent 认知负担；与 §6.1 路由 `GET /agents/{id}/bindings` 一致。工作空间级"所有 Agent 的配装总览"作为二级筛选视图（见 §2.3）。

### 2.2 配装列表的列与分组

列表按"生效优先级 → scope_kind → effect"组织，默认折叠 deny（排除项）到分组末尾，避免排除项淹没 allow：

| 列 | 内容 | 说明 |
|---|---|---|
| 优先级 | `priority` 数字 + 拖拽手柄 | 数值越小越优先；deny 行固定置顶并禁用拖拽 |
| 作用域 | scope_kind 图标 + 标签 | `asset` / `workspace` / `asset_type` 三档 |
| 目标 | 资产名 / 类型名 / "全工作空间" | scope_kind=asset 显示资产名 + 版本锚点；workspace 显示"全工作空间"；asset_type 显示类型标签 |
| 版本策略 | `follow_published`（跟随最新）/ `pinned`（固定）| pinned 行额外展示固定版本号与撤权状态（见 §3.2） |
| 交付模式 | `tool` / `summary` / `inline` 三档徽标 | 见 §3.3 |
| 效果 | allow（允许，绿色）/ deny（排除，红色）| 见 §3.4 |
| 状态 | active / revoked（已撤销，灰色置灰）| revoked 行只读不可编辑 |
| 操作 | 撤销 / 编辑 | 撤销走二次确认（见 §4.2） |

**分组规则**：先按 effect 分两组（allow 在上、deny 在下，组间分隔线 + 组标题"允许 / 排除项"），组内按 priority 升序。deny 组标题带计数徽标，默认折叠。

### 2.3 工作空间级总览（次要视图）

切换"按 Agent 查看 / 按资产查看"：
- **按 Agent 查看**（默认）：见 §2.2。
- **按资产查看**：以资产为行，列展示"被哪些 Agent 绑定 / 多少 pinned / 多少 follow"。用于回答"撤掉这个资产会波及哪些 Agent"——这是批量撤销前的风险预览（见 §4.3）。

## 3. 关键组件设计

### 3.1 固定版本选择器（Pinned Version Selector）

**场景**：scope_kind=asset 且 version_policy=pinned 时，选择该资产的某个 ready 版本固定。

**交互**：

1. 仅当 `scope_kind=asset` 时启用版本策略切换；workspace / asset_type 下版本策略强制 `follow_published` 且选择器禁用（置灰 + tooltip："此作用域不支持固定版本"）。
2. 选 `pinned` → 版本选择器加载该资产的 ready 版本列表（来自 `GET /knowledge/assets/{id}/versions`，只取 ready）。
3. 版本下拉项展示：**版本号 + 内容摘要（content_hash 前 8 位）+ 创建时间 + 构建状态徽标**。非 ready 版本不出现在选项中（不让管理员选一个还没就绪的版本）。
4. 选中后表单下方出现"固定版本卡片"，显示版本号、hash、固定时间，并提供"取消固定（回到跟随最新）"次级操作。

**可达性**：版本选择器为 `Select`（shadcn），键盘可达；每个选项的版本号与 hash 为纯文本，可被屏幕阅读器读出。

### 3.2 撤权阻断态（核心红线，§5.1 / §11.4）

当一个已 pinned 的版本因**被撤销最终 use 授权**或**被管理员添加为 deny 排除项**而失效时，UI **必须显示阻断态，绝不静默回退到最新版**。

**阻断信号来源（§6.1 真实契约对齐）**：后端在批量 upsert 与列表返回中提供 `BindingResult.PinnedVersionBlocked`（布尔）作为唯一权威阻断标记——`true` 表示该 pinned 版本不可用、使用将被阻断、**无回退**（服务层注释原文："pinned version not usable → use will阻断, no fallback"）。批量响应还带 `BatchResult.Alerted`（被阻断条目的下标数组）供前端逐条高亮。列表层每条 `AgentBinding` 若 `version_policy=pinned` 且其 pinned 版本经 `PinnedVersionChecker.IsUsable` 判定不可用，UI 即渲染阻断态。前端**不可**靠"找不到版本就回退最新"自行推断——必须显式读 `PinnedVersionBlocked` / `Alerted`。

**阻断态视觉**（固定版本卡片）：

```
┌─────────────────────────────────────────────┐
│ ⛔ 固定版本已失效（阻断中）                  │
│ 版本 v3  · hash a1b2c3d4 · 固定于 08-15       │
│ 原因：该版本的 use 授权已被撤销（08-18）     │
│                                              │
│ 此 Agent 当前无法使用此资产。                │
│ 请选择处理方式：                             │
│  ○ 切换为「跟随最新」可用版本（follow_published）│
│  ○ 重新固定到另一个 ready 版本…             │
│  ○ 撤销此 Binding                           │
└─────────────────────────────────────────────┘
```

**设计要点**：

- 阻断态用 `--warning`（#FFB020）边框 + ⛔ 图标 + 文字标签"阻断中"，**状态不只靠颜色**（呼应设计系统 §3.5 状态不依赖颜色单一传达）。
- 阻断态行整行置灰不可直接编辑，但提供显式三选一处理动作，**强制管理员做一次明确决策**——这是"不静默回退"的可操作化。三个处理动作分别对应：`PATCH` 改 version_policy=`follow_published`、`PATCH` 改 `pinned_version_id`、`POST .../revoke`（见 §4.1/§4.2 路由）。
- 阻断原因来自 `agent_bindings.revoked_at` / 关联 `workspace_authz_revisions`；若后端不返回原因明细，显示"授权已失效，请联系工作空间管理员"（§11.4 存在性不泄露——不区分"无权"与"不存在"）。
- 列表层：阻断态行用左侧 4px `--warning` 色条 + 状态徽标"阻断中"，与 active / revoked 行视觉区分。列表行的阻断标记同样取自 `PinnedVersionBlocked`，前端不另行计算。

**与静默回退的对比（反面）**：若 UI 悄悄把 pinned 失效的版本回退到最新版，管理员会以为"还在用 v3"，实际已漂移——这是 §9 验收门禁"固定版本不可静默漂移"要防的。阻断态把漂移变成显式可见的卡点。

### 3.3 delivery_mode 选择器

三档语义提示，选择器为分段控件（ToggleGroup）+ 每档下方说明文案：

| 模式 | 标签 | 说明文案 | 适用 |
|---|---|---|---|
| `tool` | 工具调用 | 以 MCP 工具形式暴露，Agent 显式调用 | 需要参数化调用、有明确输入输出的 Skill |
| `summary` | 摘要注入 | 以摘要形式随上下文注入，不暴露调用接口 | 背景/参考类、被动提供的 Skill |
| `inline` | 内联展开 | 内容直接内联到上下文 | 短片段、声明类 Skill |

选择器下方固定一行小字："交付模式决定该资产如何对 Agent 可见，不改变其权限范围。"（呼应 §11.4 不变量：Binding 只缩小不扩大）。

### 3.4 effect 与 deny 排除项

- `allow`：默认绿色徽标。
- `deny`：红色徽标，列表内独立分组置顶或折叠（见 §2.2）。
- 创建/编辑表单中 effect 切换为 deny 时，版本策略与 delivery_mode 选择器**禁用置灰**（排除项不交付，无需这些字段），tooltip："排除项不交付，无需配置版本与交付模式"。

### 3.5 批量配装（Batch Binding）

**场景**：一次性给某 Agent 绑定多个资产，事务态展示。

**交互流程**：

1. 列表页"批量配装"按钮 → 打开批量配装抽屉（Sheet）。
2. 抽屉内：资产多选器（按类型筛选 + 搜索 + 复选），右侧"配装预览"实时展示已选项。
3. 对已选项**统一设置**：version_policy（全部 follow 或全部 pinned）、delivery_mode、priority 偏移、effect。
4. **事务态展示**：提交时显示进度条 + 逐条结果（成功/失败/阻断）。
   - 全成功 → 关闭抽屉，列表刷新，Toast"已绑定 N 项"。
   - 部分失败 → 抽屉不关，失败项标红 + 原因（阻断/无权/版本不可用），可"仅重试失败项"。
   - 后端如为单事务（全成或全败），UI 按"全败"展示并保留选择，不让用户以为部分已生效。
5. **Idempotency-Key**：提交前前端为每个批次生成一个 UUID 作 `Idempotency-Key` 请求头（见 §12.2 路由表）。重复点击/网络重试自动复用同一 Key，后端命中即返回原结果（`idempotent_hit=true`，`new_revision` 为首次返回的同一值，非 0）。UI 据此判"幂等命中"→提示"该批次已提交，显示原结果"，不产生副作用重复。
6. **阻断项展示**：响应体含 `results[].PinnedVersionBlocked` 与 `alerted[]`（被阻断条目下标）。阻断项不等于失败——它是"已写入但使用将被阻断"，UI 用 §3.2 黄色阻断态逐条标注，**不归入失败计数**，Toast 文案分列"成功 N / 阻断 M / 失败 K"。
7. **new_revision**：响应体含 `new_revision`（本次配装导致的 `workspace_authz_revisions` 自增值）。提交后列表头部刷新"当前授权版本 rev.{new_revision}"（呼应 §5 生效集快照基准），让管理员确认本次配装已落库生效。
8. **风险预览**：若已选项中存在当前 Agent 已绑定且 effect=allow 的资产，且本次设为 deny，预览区高亮"将覆盖现有允许 → 排除"，需二次确认。

**可达性**：批量多选用 `Checkbox` + 语义 `aria-label`（资产名）；进度条用 `Progress` + `role="status"` + `aria-live="polite"`。

## 4. 创建 / 撤销交互规范

### 4.1 创建 Binding（表单）

表单字段顺序与依赖：

1. **作用域** scope_kind（单选：asset / workspace / asset_type）→ 决定后续字段可见性。
2. **目标资产/类型**（scope_kind=asset 时为资产选择器；=asset_type 时为类型下拉；=workspace 时隐藏）。
3. **效果** effect（allow / deny）→ deny 时禁用 §4 步以后字段（见 §3.4）。
4. **版本策略** version_policy（仅 scope_kind=asset 且 effect=allow 可用）→ pinned 时展开 §3.1 选择器。
5. **交付模式** delivery_mode（仅 effect=allow 可用）。
6. **优先级** priority（数字输入，默认 100，deny 行禁用）。

**校验**（前端即时，最终以 §6.1 REST 返回为准）：

- scope_kind=asset 必须选资产；pinned 必须选 ready 版本。
- 同一 (agent, scope, target, effect) 不允许重复创建（前端预检 + 后端唯一约束）。
- 无权访问的资产不在选择器中可选（§1.1 红线 3）。

**提交路由对齐**：单条创建/编辑走 §6.1 真实契约——
- **新建**：`POST /agents/:id/bindings/batch`，body 为 `items: [单条]` + `workspace_id`，带 `Idempotency-Key` 头（前端生成 UUID）。后端返回 `results[0].binding`（含 `created_at` → 用作 ETag）。**无独立的 `POST /agents/:id/bindings` 单条创建路由**——单条与批量共用 batch 路由，单条即 items 长度为 1 的 batch。
- **编辑**（改 delivery_mode / priority）：`PATCH /agents/:id/bindings/:binding_id?workspace_id=...`，请求头 **必须带 `If-Match: <etag>`**（etag = 创建时返回的 `created_at` 毫秒时间戳）。无 If-Match → 400。后端实现是 revoke-old + create-new，新 binding 携带原 scope/effect/version_policy 不变字段；响应返回新 binding 并在响应头 `ETag` 回新 etag，前端据此更新本地行的 etag。ETag 不匹配（409）→ 冲突提示"该配装已被他人改动，请刷新后重试"。
- 404（列表/单查/PATCH/revoke）= 无权或不存在（§11.4），UI 一律走 §7 `ForbiddenEmpty`，不泄露。

### 4.2 撤销 Binding

- 列表行操作"撤销" → 二次确认对话框，文案明确："撤销后，下一次请求将拒绝该资产。"（呼应 §9 门禁"排除/撤权后下一请求拒绝"）。
- deny 行撤销 = 恢复允许，文案："撤销此排除项后，该资产将重新对 Agent 可见。"
- 撤销为软删除（`revoked_at`），行进入 revoked 置灰态，保留审计，不立即物理消失。
- **路由对齐**：撤销走 `POST /agents/:id/bindings/:binding_id/revoke`（非 DELETE，见 §6.1 真实契约——撤销是带权限校验的状态翻转，返回 `{ binding_id, revoked: true, new_revision }`）。响应的 `new_revision` 用于刷新列表头部"当前授权版本 rev.{new_revision}"。404 表示无权或不存在（§11.4，二者不可区分，UI 显示 §7 `ForbiddenEmpty`）。

### 4.3 撤销资产（资产侧，跨 Agent 影响）

当某资产被下线/删除时，触达所有 pinned 到其版本的 Binding。UI 提供"波及预览"：

- 资产下线确认对话框列出"将受影响的 Agent 配装（N 个 pinned、M 个 follow）"。
- 对 pinned 受影响项，提示"将进入阻断态，需逐一处理"（§3.2）。
- 不自动回退、不自动迁移版本——把决策留给管理员。

## 5. 生效集解析可视化（只读）

对应 §5 生效集解析（YS-162 实现）。配装管理页顶部 Tab 第二项"生效集"，只读展示后端解析的该 Agent 当前可用资产集：

- 按 asset_type 分组列表，每项显示：资产名 / 版本（pinned 版本号或"跟随最新"）/ 交付模式 / 来源（哪条 Binding 命中）。
- 提供搜索 + 按 delivery_mode 筛选。
- **不展示 Secret 值**：若资产声明了 Secret 需求，仅显示徽标"需要 Secret（作用域：xxx）"，无值、无掩码、无复制按钮。
- 生效集是解析后的"快照"，顶部显示"基于工作空间授权版本 rev.12345 解析"，与 `workspace_authz_revisions` 对齐，让管理员知道这份结果会随配装变更而失效重算。

## 6. 校验报告可视化

对应 §4 静态 Validator 的 `validation_report` 与 `compatibility_report`。在**资产/版本详情**与**配装创建表单的 pinned 选择**两处呈现。

### 6.1 validation_report.findings

每条 finding 结构对齐 §6.1 真实契约（`domain.ValidationFinding`）：`check`（检查项，如 "structure.skill_md"）+ `severity`（**block / warn / info**，非 error/warning——见下）+ `code`（稳定机器码，如 "SKILL_MD_MISSING"）+ `message` + `path`（可选，archive 相对路径）。用列表渲染：

| severity | 视觉 | 徽标 | 语义 |
|---|---|---|---|
| `block` | `--error` 左色条 + ✕ 图标 | 红色"阻断" | 强制 `validation_status=failed`，不可保存此版本 |
| `warn` | `--warning` 左色条 + ⚠ 图标 | 黄色"警告" | 可保存但需管理员知悉 |
| `info` | `--info` 左色条 + ℹ 图标 | 蓝色"提示" | 提示性信息 |

- 顶部汇总：`validation_status=passed` 时显示绿色"可保存交付"徽标 + 小字"仅表示可保存，不等于可执行"（§4 约束：**Mora 不执行 Skill**，passed ≠ 可执行）；`failed` 显示红色"不可保存"徽标（存在 block 级）；`pending` 显示灰色"校验中"徽标；`opaque` 显示"不透明包"徽标（opaque profile，无能力发现）。
- finding 的 `path` 可点（跳到包内对应文件，由前端在 §6.1 返回结构化定位时实现）。
- `block` 级 finding 存在时，"固定此版本"按钮禁用 + tooltip"存在阻断级校验项，版本不可用"。

### 6.2 compatibility_report.delivery

三档结果对齐 `domain.DeliveryVerdict` 枚举，以分段卡片展示：

| 结果 | 视觉 | 含义 | 衍生展示 |
|---|---|---|---|
| `lossless` | 绿色"无损" | agentskills.io profile，完全理解，无适配 | — |
| `runtime_adaptation_needed` | 黄色"需运行时适配" | hermes profile：未知合法字段已原样保留，运行时需适配 | 展示 `runtime_needs[]`（如 "runtime:claude-code"）+ `opaque_fields[]`（未理解字段路径） |
| `incompatible` | 红色"不兼容" | 不兼容，不可交付 | 列出阻断项；pinned 选择器禁用此版本 |

不兼容版本在 §3.1 版本下拉中标记并禁选，从源头阻断误配。`runtime_adaptation_needed` 版本可固定，但卡片显式列 `runtime_needs` 让管理员知悉"交付时运行时需自行适配"。

## 7. 状态完备性

遵循既有 `asset-primitives.tsx` 模式（状态不只靠颜色 + 图标 + 文本）：

| 状态 | 组件 | 说明 |
|---|---|---|
| 加载 | `LoadingState` / 骨架屏 | 列表与选择器加载用骨架行，不用裸 spinner |
| 空 | `EmptyState` | "该 Agent 暂无配装" + "新建配装"引导 |
| 错误 | `ErrorState` + 重试 | 网络错误显示重试；404/403 走 §11.4 空态 |
| 无权/不存在 | `ForbiddenEmpty` | "无权访问或资源不存在"，不泄露（既有组件复用） |
| 阻断 | §3.2 阻断卡片 | 黄色色条 + ⛔ + 文字 + 处理动作 |

## 8. Secret 处理规范（红线）

- **永不展示值**：不渲染、不掩码、不复制、不在 DOM 中出现 Secret 值。
- **只展示声明**：徽标"需要 Secret" + 作用域（如"作用域：环境变量 / 进程"），信息来自 manifest 的需求声明，不来自存储的值。
- **传输约束**：前端只请求"是否需要 Secret"，不请求值；若 §6.1 REST 意外返回值字段，前端在类型层忽略（`Omit<SecretValue>`），不渲染。这条写进前端实现 review checklist。
- **审计**：任何"复制 Secret"按钮一律不设计；粘贴态检测到疑似 Secret 值入表单时给出警告（防止管理员误粘 Secret 到优先级等无关字段）。

## 9. 可达性

- 全键盘可达：Tab 顺序 = 作用域 → 目标 → 效果 → 版本策略 → 版本选择 → 交付模式 → 优先级 → 提交。
- 焦点环用 `--primary`，遵守设计系统焦点规范。
- 所有图标按钮带 `aria-label`（如"撤销配装"而非裸图标）。
- 阻断态卡片用 `role="alert"` + `aria-live="assertive"`，屏幕阅读器立即播报。
- 表格行操作用 `aria-label` 区分"撤销""编辑"。
- 对比度遵循 WCAG AA（设计系统 §3 已保证语义色对比度）。

## 10. 响应式

- **桌面（≥1024px）**：列表全列展开 + 右侧抽屉表单。
- **平板（768–1024px）**：列表折叠"目标"列详情到次行，抽屉改全屏 Sheet。
- **移动（<768px）**：列表卡片化（每行变卡片），批量配装降级为"逐条添加"（移动端不开批量抽屉，避免误操作）。
- 弱网：列表与生效集支持分页 + 骨架；pinned 版本列表懒加载；撤销/批量操作乐观更新失败回滚 + Toast。

## 11. 设计令牌映射

全部复用 `design-docs/09-design-system.md`，不新增 token：

| 用途 | 令牌 |
|---|---|
| 主操作（新建/批量配装/提交） | `--primary` (#4F6BFF) |
| 允许 allow | `--success` (#34C759) |
| 排除 deny | `--error` (#FF5A5F) |
| 阻断态 | `--warning` (#FFB020) |
| 信息/交付模式提示 | `--info` (#5AC8FA) |
| AI/Skill 图标 | `--ai-purple` (#7A5AF8) |
| 焦点环 | `--primary` |
| 置灰/次要文本 | `--neutral-500` / `text-muted-foreground` |

组件复用：`Button` / `Badge` / `Select` / `Dialog` / `Sheet` / `Checkbox` / `Progress` / `Tooltip` / `ScrollArea` / `Separator`（均为既有 shadcn 原语，不新增）。

## 12. 交付清单与前端对接

### 12.1 设计师交付（本稿）

- [x] 配装管理信息架构与导航（§2）
- [x] 固定版本选择器 + 撤权阻断态（§3.1 / §3.2）
- [x] delivery_mode 选择器三档语义（§3.3）
- [x] effect / deny 分组与批量配装事务态（§3.4 / §3.5）
- [x] 创建 / 撤销 / 资产波及预览（§4）
- [x] 生效集只读可视化（§5）
- [x] 校验报告 + 兼容性报告可视化（§6）
- [x] 状态完备性 + Secret 不展示 + 可达性 + 响应式 + 令牌映射（§7–11）
- [x] **§6.1 真实契约对齐校准**：按 YS-163 交付的实际路由（`POST /batch`、`PATCH` + `If-Match`、`POST .../revoke` 非 DELETE）、数据结构（`BindingResult.PinnedVersionBlocked` / `BatchResult.Alerted`/`NewRevision`/`IdempotentHit`、`severity` 枚举 block/warn/info、`SkillValidationStatus` 四态、`DeliveryVerdict` 三档 + `runtime_needs`/`opaque_fields`）逐条校准（§3.2 / §3.5 / §4.1 / §4.2 / §6.1 / §6.2 / §12.2 / §12.3）。设计令牌与 shadcn 原语全部复用 `09-design-system.md`，无新增。

### 12.2 前端实现对接点（交前端研发，§6.1 已就绪）

- 组件建议位置：`web/src/components/agents/`（新建目录，与 `assets/` `rbac/` 平级），文件：`BindingListPage.tsx` / `BindingFormDialog.tsx` / `BatchBindingSheet.tsx` / `PinnedVersionSelector.tsx` / `ValidationReport.tsx` / `EffectiveSetPanel.tsx` / `BlockedBindingCard.tsx`（§3.2 阻断态卡片）。
- 状态原语复用 `assets/asset-primitives.tsx` 的 `AssetStatusBadge` 模式 + `ErrorState` / `ForbiddenEmpty`（§11.4 一致语义；该文件已导出 `ForbiddenEmpty` / `ErrorState`，缺 `LoadingState`/`EmptyState` 时由前端在 `asset-primitives.tsx` 内补齐，保持状态原语集中一处）。
- **REST 对接 §6.1（真实契约，分支 `agent/mora/1f95a4dd-y163` commit `8958253`，路由注册见 `cmd/mora-api/main.go:468-471`）**：

| 操作 | 方法 + 路由 | 关键请求 | 关键响应 | UI 落点 |
|---|---|---|---|---|
| 列表 | `GET /agents/:id/bindings?workspace_id=..&cursor=..&page_size=..` | cursor 游标分页 | `{ items: AgentBinding[], next_cursor }`，响应头 `X-Next-Cursor` | §2.2 列表 |
| 创建/批量 | `POST /agents/:id/bindings/batch` | body `{ workspace_id, items: BindingInput[] }`，头 `Idempotency-Key: <uuid>` | `{ results: BindingResult[], alerted: int[], new_revision, idempotent_hit }`；`results[i].PinnedVersionBlocked` 标阻断 | §3.5 批量、§4.1 新建 |
| 编辑 | `PATCH /agents/:id/bindings/:binding_id?workspace_id=..` | 头 `If-Match: <etag>`，body `{ delivery_mode?, priority? }` | 新 binding，响应头 `ETag`（新 etag） | §4.1 编辑（改 mode/priority） |
| 撤销 | `POST /agents/:id/bindings/:binding_id/revoke?workspace_id=..` | — | `{ binding_id, revoked: true, new_revision }` | §4.2 撤销 |
| 版本列表(pinned 选项) | `GET /knowledge/assets/:id/versions` | — | 版本数组（前端只取 ready 且 `compatibility_report.delivery != incompatible` 的） | §3.1 选择器 |
| 版本详情(校验报告) | `GET /knowledge/assets/:id/versions/:vid` | — | 含 `skill_packages`：`validation_status` / `validation_report` / `compatibility_report` / `manifest.content_hash` | §6 校验可视化 |
| 校验/重跑 | `POST /knowledge/assets/:id/versions/:vid/validate` | — | 更新后的 `validation_status` / `validation_report` | §6.1 刷新 |
| 无损导出 | `GET /knowledge/assets/:id/versions/:vid/export` | — | archive 流（前端不落 UI 入口，治理只读） | §1.1 红线 1 |

> 注意：生效集解析（`GET /agents/:id/effective-set`）由 YS-162 落地，本稿 §5 的 UI 已预留 Tab，待 YS-162 路由就绪后对接；当前若该路由未交付，前端把"生效集"Tab 置为"即将上线"置灰态，不影响配装 CRUD 主链路。
- 类型建议新增 `web/src/types/bindings.ts`，对齐 `domain.AgentBinding`（§2.2 列字段）与 `BindingResult`（含 `PinnedVersionBlocked`）、`BatchResult`（含 `Alerted`/`NewRevision`/`IdempotentHit`）、`ValidationFinding`（`severity: block|warn|info`）、`DeliveryVerdict`、`SkillValidationStatus`。枚举值严格对齐 domain 常量：`scope_kind ∈ {asset, workspace, asset_type}`、`effect ∈ {allow, deny}`、`version_policy ∈ {follow_published, pinned}`、`delivery_mode ∈ {tool, summary, inline}`。
- 类型建议新增 `web/src/types/bindings.ts`，对齐 §2.2 列字段与 `agent_bindings` 表。

### 12.3 走查 checklist（实现后）

- [ ] pinned 版本失效时 UI 显示阻断态，无静默回退——阻断判据取 `BindingResult.PinnedVersionBlocked` / `BatchResult.Alerted`，前端不自行推断（§3.2，对应 §9 门禁）。
- [ ] 全程无 Secret 值渲染、无掩码、无复制按钮（§8，对应 §1.2 不外传）。
- [ ] 无权资产不出现在选择器与列表（§1.1 红线 3，对应 §11.4；404 一律走 `ForbiddenEmpty`）。
- [ ] 批量配装事务态正确——据 `results`/`alerted` 分列成功/阻断/失败，`idempotent_hit=true` 时提示"幂等命中显示原结果"，不产生副作用重复；不让用户以为部分已生效（§3.5）。
- [ ] 编辑走 `PATCH` + `If-Match` ETag，无 ETag 或 ETag 不匹配（409）有明确提示并要求刷新（§4.1）；撤销走 `POST .../revoke` 非 DELETE（§4.2）。
- [ ] 校验 `block` 级 finding 存在时"固定此版本"禁用；`validation_status` 四态（passed/failed/pending/opaque）徽标齐全（§6.1）。
- [ ] `compatibility_report.delivery` 三档（lossless/runtime_adaptation_needed/incompatible）展示，incompatible 版本在 pinned 选择器禁选（§6.2）。
- [ ] 状态完备：加载/空/错误/无权四态均有（§7）。
- [ ] 键盘可达 + 对比度达标（§9）。
- [ ] 移动端列表卡片化、批量降级（§10）。

## 13. 与验收门禁的映射

| §9 门禁 | 本稿落点 |
|---|---|
| 绑定不复制资产 | §2.2 列表按引用展示，无复制语义 |
| 固定版本不可静默漂移 | §3.2 阻断态强制显式决策 |
| 样例包往返 hash/文件一致 | §6.1 校验报告展示 content_hash |
| 脚本执行次数为 0 | §1.1 红线 1 + 全程无"运行"入口 |
| 排除/撤权后下一请求拒绝 | §4.2 撤销文案 + §3.2 阻断 + §4.3 波及预览 |
