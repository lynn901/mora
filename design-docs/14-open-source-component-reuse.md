# 文档编辑与查看：开源组件分层复用设计（方案 A）

> 文档版本：v1.0 ｜ 产出人：Mora 架构师 ｜ 对应任务：编辑与查看开源复用（Phase 0 落地）
> 依据：《技术选型与基座决策书》（01-tech-selection-decision.md，自研路线）、《第三方治理门禁》（13-phase0-contract-safety-baseline.md §6 D8）、《多格式文档解析设计》（10-document-parsing-design.md）、《设计系统》（09-design-system.md）

---

## 1. 摘要（结论先行）

**本设计在"自研路线"基座之上，把文档"编辑 + 查看"两块从"尽量自研"调整为"分层组件复用"：保留 Mora 现有骨架（Go 后端 + React + Tiptap v3 + Yjs），用白名单内（Apache-2.0 / MIT / BSD / MPL-2.0）的成熟开源组件替换/补齐自研成本最高的五处：块级交互 UI、Markdown 实时编辑、只读查看、版本 Diff、协同服务端。**

核心决策：

1. **编辑器内核不动**：继续使用 Tiptap v3 + ProseMirror + Yjs（MIT），不换内核、不整体 fork 平台。
2. **块级交互自研补全**：Tiptap v3 尚无白名单内的现成"Notion 风格块 UI"整体组件（BlockNote 基于 Tiptap v2，版本不匹配），故以 **La Suite Docs（MIT）** 的组件组合为蓝本，自研 slash menu / 拖拽手柄 / 块选择等 Tiptap v3 扩展，代码量可控（约 1–2 周）。
3. **Markdown 编辑模式升级**：把当前"纯 textarea"的 Markdown 模式改造为"所见即所得/即时渲染"，候选 **Milkdown（MIT）/ Vditor（MIT）**，保留 Block JSONB 单一真源。
4. **查看独立化**：新增基于 **react-markdown + remark/rehype + shiki + KaTeX + Mermaid**（全 MIT/Apache）的只读 DocumentViewer，与编辑器解耦，服务于分享、嵌入、导出。
5. **版本 Diff 复用化**：用 **jsdiff（MIT）/ diff-match-patch（Apache-2.0）+ prosemirror-changeset（MIT）** 替换自研行级 diff，升级为块级富文本 diff。
6. **协同服务端平移**：用 **Hocuspocus（MIT，Tiptap 官方）** 替换自研 y-websocket 部署与前端自研 `MoraCollabProvider`，接管鉴权 / 持久化 / 房间 / presence。
7. **附件预览（可选，Phase 2）**：轻量纯前端 **PDF.js + docx-preview + mammoth**（Apache/MIT/BSD）；重型 **kkFileView（Apache-2.0）** 独立服务。
8. **Office 在线编辑（远期，默认不做）**：仅 **Collabora Online（MPL-2.0）** 在白名单内；ONLYOFFICE（AGPL-3.0）被 Mora 门禁黑名单拦截，除非法务评估否则不引入。

**硬约束**：Mora 主许可证为 Apache-2.0，`third-party` 门禁黑名单为 AGPL/GPL/LGPL/SSPL/BUSL/commons-clause。因此 **Docmost（AGPL-3.0）、Outline（BSL-1.1）、AppFlowy（AGPL-3.0）、ONLYOFFICE（AGPL-3.0）、HedgeDoc/SiYuan（AGPL）全部只能作"功能对标"，不得复制代码、不得作为构建依赖**。任何新组件必须走 ADR + lock.json + NOTICE 门禁（§8.2）。

---

## 2. 背景与目标

### 2.1 背景

Mora 采用"自研基座"路线（01 号决策书），文档编辑与查看当前为**自研 + 少量开源库组合**，已实现的编辑/协同骨架投入不小，但距"Notion 级编辑体验"仍有明显差距，且越往后自研边际成本越高：

| 现状能力 | 实现方式 | 自研成本评估 |
|---|---|---|
| WYSIWYG 富文本编辑 | Tiptap v3 + StarterKit + 若干官方扩展 | 内核开源，已复用 ✅ |
| 块级交互（slash / 拖拽 / 块选择） | 无（仅官方默认交互） | 待自研，成本高 |
| Markdown 模式 | 纯 `<textarea>` 直编 `currentDocument.content`，无渲染/预览 | 体验简陋，需重做 |
| 实时协同 | 自研 `MoraCollabProvider`（手写 y-protocols 协议封装）+ `y-websocket` 服务 | **协议层自研，成本高、易出错** |
| 只读查看 | `editor.setEditable(false)`（编辑器内只读） | 无法独立分享/嵌入，需重做 |
| 版本 Diff | `VersionHistory.tsx` 自研逐行 diff | 行级且粗糙，需升级为块级富文本 diff |
| 附件预览（docx/pdf 等） | 无 | 待引入，自研成本极高 |

结论：**"编辑内核"已开源复用到位；高成本自研集中在"编辑器外围交互、协同协议封装、只读查看、Diff、附件预览"**——这些恰是开源生态已有成熟组件可覆盖的部分。方案 A 即针对这五处做组件化。

### 2.2 目标与范围（Do / Don't）

**做（本设计范围）**

- 编辑器层：补全块级交互（slash menu、拖拽手柄、块选择、气泡菜单、图片上传、表格）；升级 Markdown 模式为即时渲染。
- 查看层：新增独立 DocumentViewer（只读渲染 + KaTeX/Mermaid/代码高亮 + 目录），服务分享/嵌入/导出。
- 版本层：行级 diff 换用开源库，叠加块级富文本 diff。
- 协同层：Hocuspocus 替换 y-websocket + 自研 Provider。
- 附件层（可选）：纯前端或 kkFileView 在线预览上传附件。

**不做（本设计明确排除）**

- 不整体替换为 Docmost / Outline / AFFiNE / La Suite Docs 平台（License 黑名单或架构重构成本过高，见 §4.7）。
- 不引入 AGPL/GPL/LGPL/BSL/SSPL 组件（门禁黑名单）。
- 不改 Go 后端数据模型与 API 契约（`documents` / `document_versions` / `permissions` / `doc_events` 行为不变）。
- 不重写 Tiptap 内核；不引入第二个富文本内核与 Tiptap 并存（避免双内核维护）。

### 2.3 关键约束

1. **License**：主许可证 Apache-2.0；第三方白名单 Apache-2.0 / MIT / ISC / BSD-2 / BSD-3 / MPL-2.0；黑名单 AGPL-3.0* / GPL-2.0/3.0* / LGPL-* / SSPL / BUSL-1.1 / CC-BY-NC-* / commons-clause（13 号文档 §6）。
2. **私有化**：全部组件支持 Docker Compose / K8s 自托管，默认不出网。
3. **单一真源**：文档内容以 Block JSONB 为唯一真源；Markdown/HTML 均为派生视图，往返可逆（既有设计）。
4. **协同契约**：Yjs CRDT 协议不变；presence/cursor、并发降级（一人编辑他人只读）行为不回归。
5. **回归红线**：现有 `rbac.Engine.Check` / `VisibleDocuments` / 版本历史 / 审计行为不变。

### 2.4 术语

| 术语 | 含义 |
|---|---|
| Block JSONB | Mora 文档最小存储单位（`documents.content`），见 03 号数据模型 |
| Yjs / CRDT | 协同编辑的无冲突数据结构与同步协议 |
| Hocuspocus | Tiptap 官方 Yjs WebSocket 后端（Node.js） |
| DocumentViewer | 独立的只读文档渲染组件（本设计新增） |
| slash menu | 输入 `/` 弹出的块插入菜单 |
| drag handle | 行首拖拽手柄，用于选择/移动块 |

---

## 3. 现状盘点（As-Is）

### 3.1 前端编辑器（`web/src/components/editor/BlockEditor.tsx`）

- 技术栈：Tiptap v3 + `@tiptap/starter-kit` + CodeBlockLowlight + TaskList/TaskItem + Image + TextAlign + Placeholder + Collaboration + CollaborationCursor + `tiptap-markdown`（Markdown 扩展）。
- 工具栏：加粗/斜体/下划线/删除线/行内代码；H1–H3；无序/有序/任务列表/代码块/引用；左中右对齐；撤销/重做；WYSIWYG/Markdown 切换；保存（5s 防抖自动保存 + 手动 Save）。
- 只读：`editable: !isReadOnly`（协同降级或权限只读时）。
- **缺口**：无 slash menu、无拖拽手柄、无块选择、无表格、无图片上传对话框（仅官方 Image 节点）、无气泡菜单；Markdown 模式为 `<textarea>` 直编，无渲染预览。

### 3.2 实时协同（`web/src/lib/collab-provider.ts` + `deployments/Dockerfile.yjs`）

- 前端自研 `MoraCollabProvider`：基于 `y-protocols/sync` 与 `awareness`，在**自定义 JSON + Base64 WebSocket 协议**（消息类型 `update` / `sync-step1` / `sync-step2` / `awareness` / `presence`）之上手写了握手、同步、心跳、指数退避重连、5s 超时降级 `local-only`。
- 服务端：`deployments/Dockerfile.yjs` 直接 `npm install -g y-websocket@2.1.0` 起官方 y-websocket-server，**无鉴权、无持久化、无房间权限控制**；鉴权与并发准入由 mora-api 的 `/api/v1/ws/collab/:docId` 反向代理与 presence/cursor 中继承担。
- 现状问题：协议层是"重复造轮子"；y-websocket-server 能力弱（内存态、无持久化、重启丢文档状态）；鉴权钩子靠反向代理侧绕，扩展性差。

### 3.3 查看/只读

- 无独立查看组件。只读 = 同一编辑器 `setEditable(false)`。
- 后果：无法做轻量分享页、无法嵌入外部站点、移动端/低端设备加载整个编辑器成本高、无法按 Markdown 快速渲染导出。

### 3.4 版本 Diff（`web/src/components/history/VersionHistory.tsx`）

- 自研 `computeDiff`：按行拆分后逐行比较，输出 added/removed/unchanged 三段式文本 diff。
- 局限：仅文本行级；无富文本/块级对比；无语法高亮；中文长段落换行导致 diff 噪声大。

### 3.5 现状结论

高成本自研点 = ① 块级交互 UI ② Markdown 编辑体验 ③ 协同协议/服务端 ④ 独立查看 ⑤ 版本 Diff ⑥ 附件预览。**其中 ③ 应整体替换为开源实现，①②④⑤ 应"开源组件 + 少量自研胶水"，⑥ 按需引入现成服务。**

---

## 4. 候选开源组件调研与决策

> 调研时间：2026-08-11；star / License 以 GitHub API 实测为准（部分仓库 API 限流，数字以仓库页为准）。

### 4.1 编辑器层

| 候选 | Star | License | 技术栈 | 契合点 | 结论 |
|---|---|---|---|---|---|
| **Tiptap v3**（现用） | 38.0k | MIT | TS + ProseMirror | 已是 Mora 内核；headless、可扩展、Yjs 官方绑定 | ✅ 保留，不换 |
| **BlockNote** | 10.1k | MPL-2.0 | React + Tiptap **v2** + ProseMirror | Notion 风格块 UI 开箱即用；原生 Yjs 协同 | ⚠️ 暂不引入：内核 Tiptap v2 与 Mora v3 不匹配；引入需做兼容 spike（§8.1 记 MPL 义务） |
| **Milkdown** | 11.8k | MIT | ProseMirror + remark | Markdown-first WYSIWYG，Markdown AST 为源，与 Mora 双向可逆模型理念最契合 | ✅ 备选：用于 Markdown 即时渲染模式；注意单人维护 bus factor |
| **Vditor** | 11.2k | MIT | TS，框架无关 | 即时渲染（Typora 风格）+ 分屏预览，易嵌入 | ✅ 备选：Markdown 模式即时渲染；协同需自包 Yjs |
| **Novel** | 16.4k | Apache-2.0 | Tiptap v2 + Next.js | Notion 风格 + AI 补全 | ⚠️ 参考：内核 v2 + Next 绑定，不直接引入 |
| **La Suite Docs** | 16.7k | MIT | Django + React + BlockNote + Hocuspocus + Yjs | 与 Mora 目标形态几乎一致；MIT 可借鉴其组件组合 | ✅ **蓝本**：照其"骨架 + 开源组件"组合方式，不搬其代码 |

**决策 4.1**：编辑器内核保留 Tiptap v3；块级交互与 Markdown 模式**自研 Tiptap v3 扩展 + 借鉴 La Suite Docs（MIT）组件组合**；Milkdown / Vditor 作为 Markdown 即时渲染的候选组件，Phase 0 做 1 个 spike 后二选一（§7.2）。

### 4.2 查看/渲染层（DocumentViewer）

| 候选 | License | 用途 | 结论 |
|---|---|---|---|
| **react-markdown + remark-gfm + remark-toc** | MIT | Markdown 渲染主干 + GFM 表格/删除线 + 目录 | ✅ 引入 |
| **rehype-highlight / shiki** | MIT | 代码块语法高亮 | ✅ 引入 |
| **KaTeX / remark-math + rehype-katex** | MIT | 公式渲染 | ✅ 引入 |
| **Mermaid** | MIT | 流程图/时序图/甘特图 | ✅ 引入（按需懒加载） |
| **dompurify** | MIT | HTML 消毒（Markdown 允许 HTML 时） | ✅ 引入（安全基线） |
| PDF.js | Apache-2.0 | PDF 附件查看 | ✅ Phase 2 可选 |

**决策 4.2**：新增 DocumentViewer 渲染管线：Block JSONB →（`blocksToMarkdown` 已有）→ Markdown → react-markdown 管线。全组件 MIT/Apache，白名单内无争议。

### 4.3 版本 Diff 层

| 候选 | License | 说明 | 结论 |
|---|---|---|---|
| **jsdiff**（`diff` npm 包） | MIT | 成熟文本 diff，行/词/字符级，支持中文 | ✅ 引入（替换自研 computeDiff） |
| **diff-match-patch** | Apache-2.0 | Google 语义 diff，适合长文本段落 | ✅ 备选 |
| **prosemirror-changeset** | MIT | ProseMirror 官方变更集，块/节点级 diff | ✅ 引入（富文本块级 diff） |

**决策 4.3**：文本层用 jsdiff，块级用 prosemirror-changeset（与 Tiptap v3 同源，无版本风险）。

### 4.4 协同中间件

| 候选 | Star | License | 说明 | 结论 |
|---|---|---|---|---|
| **Hocuspocus** | 2.5k | MIT | Tiptap 官方 Yjs 后端：鉴权 hook、持久化（PostgreSQL 扩展可用）、房间、扩展体系、水平扩展 | ✅ **引入，替换 y-websocket** |
| y-websocket（现用） | — | MIT | 官方参考实现，无鉴权/持久化 | ❌ 替换 |
| 自研 MoraCollabProvider | — | — | 手写协议封装 | ❌ 替换为 `@hocuspocus/provider` |

**决策 4.4**：前端 `MoraCollabProvider` → `@hocuspocus/provider`（同一 Yjs 协议，改动集中在 `collab-provider.ts` / `collab.ts`）；服务端 `y-websocket` → Hocuspocus 独立 Node 服务，JWT 鉴权 + PostgreSQL 持久化（§7.5）。

### 4.5 附件/Office 预览（Phase 2 可选）

| 候选 | Star | License | 说明 | 结论 |
|---|---|---|---|---|
| **PDF.js** | 53.7k | Apache-2.0 | PDF 查看事实标准 | ✅ 引入（纯前端） |
| **docx-preview** | — | MIT | docx 前端渲染 | ✅ 引入（纯前端，.doc 不支持） |
| **mammoth.js** | 6.3k | BSD-2-Clause | docx → HTML（语义化，样式丢失） | ✅ 备选 |
| **kkFileView** | 10k+ | Apache-2.0 | Spring Boot 独立服务；LibreOffice 转 PDF 后预览 Office/PDF/WPS/CAD/3D 等超多格式 | ✅ 重型方案：独立容器 + REST 对接 |

**决策 4.5**：MVP 用纯前端（PDF.js + docx-preview + mammoth）覆盖 docx/pdf；若需全格式（xlsx/pptx/wps/ofd 等）版式保真预览，独立部署 kkFileView（Apache-2.0，与 Mora 白名单相容；作为外部服务，不进入前端构建）。

### 4.6 Office 在线编辑（远期，默认不做）

| 候选 | License | 说明 | 结论 |
|---|---|---|---|
| **Collabora Online** | MPL-2.0 | LibreOffice 内核在线编辑，白名单内 | ⚠️ 远期可选（部署重、资源高） |
| **ONLYOFFICE DocumentServer** | AGPL-3.0 | 更成熟的 Office 在线编辑 | ❌ 黑名单；除非法务评估服务边界，否则不引入 |

**决策 4.6**：Mora 主文档模型是 Markdown block，Office 二进制格式编辑不是核心场景；Phase 3 仅在业务明确要求"在线编辑 docx/xlsx"时启动，且只评估 Collabora。

### 4.7 平台级项目（仅对标，不引入）

| 项目 | Star | License | 不引入原因 |
|---|---|---|---|
| **Docmost** | 21.3k | AGPL-3.0 | 黑名单；后端 Node/TS，替换=废弃 Go 后端 + RBAC + RAG + MCP 全部投入 |
| **Outline** | 40.1k | BSL-1.1 | 黑名单（非 OSI 开源）；依赖 OIDC，私有化受限 |
| **AFFiNE / BlockSuite** | 71.4k / 6.0k | CE MIT / MPL-2.0 | local-first + Rust(OctoBase) 架构与 Mora 服务端模型差异大；仅借鉴块模型与协同设计 |
| **La Suite Docs** | 16.7k | MIT | Django 全家桶，后端不匹配；**最佳用法是组件组合蓝本（§4.1）** |
| **AppFlowy** | 75.2k | AGPL-3.0 | 黑名单；Dart/Flutter 非 Web 栈 |
| **HedgeDoc** | 7.4k | AGPL-3.0 | 黑名单 |

**决策 4.7**：不做平台整体替换；从 La Suite Docs / AFFiNE 吸收设计经验，从 MIT/MPL 组件中选型复用。

---

## 5. 目标架构（To-Be）

### 5.1 分层架构图

```
浏览器 (React SPA)
├── 编辑域
│   ├── BlockEditor（Tiptap v3 + 自研块级扩展 + Yjs）
│   │     ├── SlashMenu / DragHandle / BlockSelector / BubbleMenu
│   │     ├── ImageUpload / Table / Comments 锚点
│   │     └── Markdown 即时渲染模式（Milkdown 或 Vditor）
│   ├── CollabProvider（@hocuspocus/provider 替换自研 MoraCollabProvider）
│   └── VersionHistory（jsdiff 文本 diff + prosemirror-changeset 块级 diff）
├── 查看域
│   └── DocumentViewer（Block JSONB → Markdown → react-markdown + shiki + KaTeX + Mermaid）
└── 附件域（可选）
    ├── 纯前端：PDF.js / docx-preview / mammoth
    └── 重型：kkFileView（独立容器，REST 对接）

服务端（不变 + 替换）
├── mora-api（Go）：REST + RBAC + 版本 + 事件；WS 入口仅做鉴权/准入/降级中继
├── hocuspocus（Node，替换 y-websocket）：
│     ├── onAuthenticate（JWT + RBAC 校验）
│     ├── onStoreDocument（PostgreSQL 持久化）
│     └── 房间 = documentId；awareness = presence/cursor
├── kkFileView（可选，独立服务）
└── PostgreSQL：documents / document_versions / collab 持久化表（新增）
```

### 5.2 组件职责

| 组件 | 职责 | 新增/替换 |
|---|---|---|
| BlockEditor（Tiptap v3） | WYSIWYG 编辑、块级交互、协同编辑 | 增强（自研扩展） |
| Markdown 模式组件 | 即时渲染编辑 | 替换 textarea |
| DocumentViewer | 只读渲染、分享/嵌入/导出 | 新增 |
| CollabProvider | Yjs 同步 + awareness | 替换（Hocuspocus provider） |
| Hocuspocus | WS 协同服务端 + 鉴权 + 持久化 | 替换 y-websocket |
| VersionHistory | 版本对比与回滚 | 增强（开源 diff） |
| 附件预览（可选） | docx/pdf/xlsx 预览 | 新增 |

### 5.3 数据流

- **编辑链路**：输入 → Tiptap v3 → Yjs update → `@hocuspocus/provider` → Hocuspocus（鉴权/房间）→ 内存 + PostgreSQL 持久化 → mora-api 记录 `document_versions`（沿用现有"每次编辑产生新版本"契约）。
- **查看链路**：DocumentViewer 路由 → mora-api 读 `documents.content`（Block JSONB）→ `blocksToMarkdown` → react-markdown 渲染。
- **版本链路**：VersionHistory 读 `document_versions` → jsdiff 文本 diff + prosemirror-changeset 块级 diff → 可视化对比 → 回滚（沿用现有回滚语义：生成新版本不改写历史）。
- **附件预览链路**（可选）：mora-api 签发带签名 URL → kkFileView 拉取转换 → PDF.js 展示。

---

## 6. 分阶段落地设计

| 阶段 | 目标 | 关键产出 | 新增依赖（License） | 预估工作量 |
|---|---|---|---|---|
| **Phase 0-A** | 编辑器体验：块级交互 + Markdown 即时渲染 | SlashMenu / DragHandle / BlockSelector / BubbleMenu / ImageUpload / Table；Markdown 模式升级 | 自研 Tiptap v3 扩展；Milkdown 或 Vditor（MIT，spike 后二选一） | 1.5–2.5 周 |
| **Phase 0-B** | 独立只读查看 | DocumentViewer + 分享路由 + 导出 | react-markdown、remark-gfm、rehype-highlight/shiki、KaTeX、Mermaid、dompurify（全 MIT/Apache） | 0.5–1 周 |
| **Phase 0-C** | 版本 Diff 升级 | 行级 + 块级富文本 diff 可视化 | jsdiff（MIT）、prosemirror-changeset（MIT） | 0.5 周 |
| **Phase 1** | 协同服务端平移 | Hocuspocus 部署 + JWT 鉴权 + PostgreSQL 持久化 + 前端 provider 替换 | Hocuspocus、@hocuspocus/provider（MIT） | 1–2 周 |
| **Phase 2（可选）** | 附件在线预览 | 纯前端预览；或 kkFileView 独立服务 | PDF.js、docx-preview、mammoth（Apache/MIT/BSD）；kkFileView（Apache-2.0） | 0.5–1 周 / 1 周 |
| **Phase 3（远期）** | Office 在线编辑（默认不做） | 仅在业务明确要求时启动 | Collabora Online（MPL-2.0） | 2–3 周 |

> 各阶段独立可交付、可回滚；Phase 0 三线并行（A/B/C 互不阻塞），Phase 1 依赖 Phase 0 的协同契约稳定。

### 6.1 Phase 0-A：编辑器体验增强

**目标**：让 Mora 编辑体验达到"块级文档产品"基线，砍掉最大自研痛点。

**改动点**：
1. 自研 Tiptap v3 扩展（新增 `web/src/components/editor/extensions/`）：
   - `SlashCommand`：输入 `/` 弹出块插入菜单（标题/列表/任务/引用/代码块/表格/图片/分割线）。
   - `DragHandle`：行首拖拽手柄，支持选中块、拖拽排序。
   - `BlockSelector`：块类型快速切换（标题↔正文↔列表等）。
   - `BubbleMenu`：选中文字后的浮动格式菜单（加粗/斜体/链接/颜色）。
   - `ImageUpload`：本地图片 → mora-api 上传 → 插入 Image 节点（对接既有文件存储）。
   - `Table`：表格扩展（`@tiptap/extension-table`，MIT，官方）。
2. Markdown 模式升级（§7.2）：textarea → 即时渲染编辑器；WYSIWYG ↔ Markdown 双向同步仍走 `tiptap-markdown` + Block JSONB 单一真源。

**验收**：slash 插入 ≥8 种块；拖拽排序不破坏 Yjs 协同；Markdown 模式编辑后切回 WYSIWYG 无信息丢失（往返可逆回归测试全绿）。

### 6.2 Phase 0-B：DocumentViewer 只读查看

**目标**：把"查看"从编辑器解耦，支撑分享/嵌入/导出/移动端轻读。

**改动点**：
1. 新增 `web/src/components/viewer/DocumentViewer.tsx`：Block JSONB → `blocksToMarkdown` → react-markdown 管线。
2. 渲染扩展：GFM（表格/任务列表/删除线）、shiki 代码高亮、KaTeX 公式、Mermaid 图表（懒加载）、TOC 目录（`remark-toc`）、dompurify 消毒（Markdown 允许 HTML 时）。
3. 路由：`/doc/:id` 保留现有编辑入口；新增 `/s/:shareId` 只读分享页（无编辑器壳，纯查看）。
4. 导出：DocumentViewer 复用同一渲染管线输出 Markdown / HTML（打印友好）。

**验收**：分享页无 JS 编辑器依赖；公式/图表/代码高亮正确；与编辑视图渲染结果一致（同一真源）。

### 6.3 Phase 0-C：版本 Diff 升级

**改动点**：
1. `VersionHistory.tsx` 的 `computeDiff` 替换为 **jsdiff**（`diffLines` / `diffWords`，中文友好）。
2. 新增块级 diff：对两个版本的 Block JSONB 用 **prosemirror-changeset** 计算节点级变更，可视化"新增/删除/修改块"。
3. 保留现有回滚语义（生成新版本，不改写历史）。

**验收**：长段落 diff 噪声显著下降；块级变更（增删块/改标题）一眼可辨；回滚行为回归测试全绿。

### 6.4 Phase 1：Hocuspocus 替换协同服务端

**目标**：消灭"手写 y-protocols 协议 + 无能力 y-websocket 服务"，获得鉴权/持久化/扩展体系。

**改动点**（详见 §7.5）：
1. 服务端：新增 `deployments/` 下 Hocuspocus 服务（Node，Dockerfile + compose 段替换 `Dockerfile.yjs`）；接入 mora-api JWT 校验与 RBAC（房间 = `documentId`，按用户对文档权限放行/拒绝）。
2. 持久化：`onStoreDocument` 将 Y.Doc 快照写入 PostgreSQL（新增 `collab_documents` 表，或复用 `documents` 的 JSONB 列，二选一在实现时定）。
3. 前端：`MoraCollabProvider` → `HocuspocusProvider`（`@hocuspocus/provider`）；`collab.ts` 状态机（connecting/connected/disconnected/degraded/local-only/denied）保留，映射新 provider 事件。
4. presence/cursor：awareness 直连 Hocuspocus，mora-api 的 presence 中继保留为降级/准入旁路（或迁移为 Hocuspocus 扩展，实现时二选一）。
5. 并发降级契约不变：单文档并发超限 → 一人编辑他人只读（由 mora-api 准入逻辑判定，Hocuspocus `onAuthenticate` 返回 read-only 标记或由 mora-api 前置拦截）。

**验收**：多人协同、cursor、降级行为与现状一致（回归矩阵）；断线重连；服务重启后文档状态可从 PostgreSQL 恢复；JWT 过期/无权限连接被拒。

### 6.5 Phase 2（可选）：附件在线预览

**改动点**：
1. 纯前端轻量：docx/pdf 用 PDF.js + docx-preview + mammoth 渲染（新增 `AttachmentPreview` 组件，接入附件 URL）。
2. 全格式重型：独立部署 kkFileView 容器（Apache-2.0）；mora-api 对预览请求签发短时签名 URL，kkFileView 拉取转换，前端 iframe/PDF.js 展示；鉴权由签名 URL 保证，不外泄内部存储地址。

**验收**：docx/pdf 预览通过；xlsx/pptx 等全格式（重型方案）预览通过；无未授权访问。

### 6.6 Phase 3（远期，默认不做）：Office 在线编辑

仅在业务明确要求"在线编辑 docx/xlsx/pptx"时立项；只评估 **Collabora Online（MPL-2.0）**（对接 WOPI 协议，mora-api 实现 WOPI 端点）；ONLYOFFICE（AGPL）需法务先评估服务边界合规。本设计不展开。

---

## 7. 关键技术设计细节

### 7.1 块级 UI 扩展（Tiptap v3）

- 全部实现为 Tiptap v3 扩展 + React NodeView（`ReactNodeViewRenderer`），与现有 `BlockEditor.tsx` 的扩展数组并装。
- **与 Yjs 协同的兼容性**：NodeView 拖拽/插入必须通过 Tiptap 命令（`insertContent` / `setNode` / `splitBlock`）落盘到 ProseMirror state → Yjs 自动同步；禁止直接操作 DOM 造成 Yjs 与视图不一致。拖拽排序用 `@tiptap/extension` 的 transaction 原语实现，保证协同端一致收敛。
- **ImageUpload**：走 mora-api 上传接口（`POST /api/v1/files` 类），返回可访问 URL 后插入 Image 节点；上传中显示占位 NodeView（`isLoading` 状态），失败回滚事务。
- **Table**：直接用官方 `@tiptap/extension-table`（MIT）+ 样式对齐 09 号设计系统；注意表格节点在 Yjs 协同下为单节点更新（不细分单元格协同），可接受。

### 7.2 Markdown 模式改造

候选二选一（Phase 0-A 先做 spike）：

| 候选 | 优势 | 风险 | 接入方式 |
|---|---|---|---|
| **Milkdown（MIT）** | Markdown AST 为源，与 Mora 双向可逆理念一致；ProseMirror 系，与 Tiptap 同源 | 单人维护（bus factor）；与 Tiptap 双实例并存需隔离 | 独立 `<div>` 挂载，`onChange` 回写 Block JSONB；切模式时经 Markdown 中间态互转 |
| **Vditor（MIT）** | 即时渲染（Typora 风格）体验佳；框架无关 | 非 ProseMirror 系，协同需自包 Yjs（本模式默认单机编辑，可接受） | 独立挂载；`getValue/setValue` 与 Block JSONB 互转 |

> 原则：Markdown 模式是"编辑视图"之一，**不做协同**（协同只在 WYSIWYG 模式）；进入 Markdown 模式前 flush 当前 Yjs 状态，退出时经 Markdown 往返校验无信息丢失后写回。这避免了"双内核 + 协同"的复杂度爆炸。

### 7.3 DocumentViewer 渲染管线

```
Block JSONB ──blocksToMarkdown（已有 lib）──▶ Markdown 字符串
  ─▶ unified 管线: remark-parse → remark-gfm → remark-math → remark-toc
     → rehype-katex → rehype-highlight(shiki) → rehype-raw(消毒 via dompurify)
  ─▶ React 组件树: 标题锚点 / 表格 / 任务列表 / 代码高亮 / 公式 / Mermaid / TOC
```

- 懒加载：Mermaid 与 KaTeX 按需 `import()`，首屏只加载核心管线。
- 安全：`rehype-raw` 仅在显式允许 HTML 时启用，一律经 dompurify；默认关闭 HTML。
- 一致性：DocumentViewer 与 BlockEditor 共用 `blocksToMarkdown`，保证查看与编辑同源。

### 7.4 版本 Diff 可视化

- 文本层：jsdiff `diffLines` 输出行级 added/removed，替换现有 `computeDiff`。
- 块级层：两个版本 Block JSONB 对齐后，用 prosemirror-changeset 计算变更集，按块渲染"新增/删除/修改"徽标与高亮。
- UI：沿用现有 `VersionHistory` 对话框结构，新增"块级/文本"切换。

### 7.5 Hocuspocus 集成设计

```
浏览器 ──@hocuspocus/provider──▶ Hocuspocus (Node, :1234)
  ├─ onAuthenticate({token, documentName}) → mora-api 校验 JWT + RBAC(documentId)
  │     ├─ 通过 → { readOnly: false }
  │     ├─ 只读权限 → { readOnly: true }
  │     └─ 拒绝 → 抛错断连（前端 status=denied）
  ├─ onStoreDocument → PostgreSQL upsert Y.Doc 快照（collab_documents 表）
  ├─ onLoadDocument → 读库恢复 Y.Doc
  └─ awareness → presence/cursor 广播
```

- **鉴权**：WS URL 带 `token`（沿用现有 `?token=` 模式）或首包发送；`onAuthenticate` 同步调用 mora-api（内部 token）校验文档读/写权限；权限变化（显式拒绝优先）即时拒绝。
- **持久化**：新增表 `collab_documents (document_id uuid pk, y_doc bytea, updated_at)`；快照策略（每次编辑 debounce 落库，如 2s）；与 `document_versions` 的关系：Hocuspocus 存"协同工作态"，版本仍由 mora-api 按既有契约落 `document_versions`。
- **降级**：单文档并发准入由 mora-api 判定（沿用现有逻辑），Hocuspocus 通过 `onAuthenticate` 返回 `readOnly` 或由 mora-api 反向代理统一入口前置拦截；**两路任一实现，行为与现状一致即可**。
- **回退**：Hocuspocus 与 y-websocket 协议同为 Yjs，前端可快速切回旧 provider（保留 `collab-provider.ts` 一个 commit 的兼容壳，一个 PR 内完成替换，不跨版本并行）。

### 7.6 附件预览对接（可选）

- 纯前端：`AttachmentPreview` 按扩展名分发 → PDF.js（pdf）/ docx-preview（docx）/ mammoth（docx 备选）/ 图片原样。
- kkFileView 重型：compose 增加 `kk-fileview` 服务；mora-api 提供 `POST /api/v1/files/preview-token`（内部签名，短时有效）；前端 `iframe` 指向 kkFileView `/onlinePreview?url=<签名URL>`；安全：签名 URL 绑定文件 ID + 过期时间 + 用户 RBAC，kkFileView 出网白名单只允许 mora-api 内网地址。

---

## 8. License 合规与第三方门禁

### 8.1 License 合规矩阵（本设计全部新增组件）

| 组件 | SPDX | 白名单/黑名单 | ADR 要求 | 备注 |
|---|---|---|---|---|
| react-markdown / remark / rehype 家族 | MIT | 白名单 | 无 | — |
| shiki | MIT | 白名单 | 无 | — |
| KaTeX | MIT | 白名单 | 无 | — |
| Mermaid | MIT | 白名单 | 无 | 懒加载 |
| dompurify | MIT / Apache-2.0（双许可） | 白名单 | 无 | — |
| jsdiff | MIT | 白名单 | 无 | — |
| prosemirror-changeset | MIT | 白名单 | 无 | 与 Tiptap 同源 |
| Milkdown / Vditor（二选一） | MIT | 白名单 | 无 | 记录 bus factor 评估于 ADR |
| Hocuspocus / @hocuspocus/provider | MIT | 白名单 | 无 | 含 @hocuspocus/extension-* 各包 |
| @tiptap/extension-table 等官方扩展 | MIT | 白名单 | 无 | — |
| PDF.js | Apache-2.0 | 白名单 | 无 | — |
| docx-preview | MIT | 白名单 | 无 | — |
| mammoth.js | BSD-2-Clause | 白名单 | 无 | — |
| kkFileView | Apache-2.0 | 白名单（独立服务） | 需 ADR | 不进前端构建；服务边界说明 |
| BlockNote（若未来引入） | MPL-2.0 | 白名单但需人工评审 | **必须 ADR** | MPL 文件级 copyleft：改动其文件须保持 MPL；Tiptap v2 版本冲突先行 spike |
| Collabora Online（远期） | MPL-2.0 | 白名单但需人工评审 | **必须 ADR** | 独立服务；WOPI 对接 |

### 8.2 门禁流程（每个新增组件必须走）

1. 更新 `web/package.json` / `go.mod` 并安装。
2. 运行 `make third-party-sync` 重新生成 `third-party/lock.json`。
3. 新组件写 `third-party/adr/000X-*.md`（模板 `0000-template.md`），MPL 组件必填 License 合规影响与文件级 copyleft 说明。
4. LICENSE/NOTICE 副本放入 `third-party/NOTICES/`。
5. `make third-party-check sbom notices` 全绿后方可合入；CI publish 前自动执行，失败阻断发布。

### 8.3 禁止事项（硬性红线）

- ❌ 从 AGPL/GPL/LGPL/BSL/SSPL 仓库**复制代码**（含 Docmost 的 `@docmost/editor`、Outline 前端、ONLYOFFICE SDK）。
- ❌ 将黑名单组件以 npm/go 依赖形式引入构建（即使仅用其部分文件）。
- ❌ 绕过门禁直接手抄第三方源码片段（视觉/API 借鉴可，逐行复制不可；MPL 组件复制须保留 License 头并走 ADR）。

---

## 9. 风险与缓解

| 风险 | 等级 | 缓解 |
|---|---|---|
| BlockNote 与 Tiptap v3 版本不匹配，块 UI 只能自研 | 中 | Phase 0-A 以自研 Tiptap v3 扩展为主（官方原语 + 社区 MIT 示例），BlockNote 仅作 UX 参照；若未来 BlockNote 升级 v3 再做一次引入评估（§7.1） |
| 双编辑器实例（WYSIWYG + Markdown）状态漂移 | 中 | Markdown 模式不做协同；进出模式经 Markdown 中间态 + 往返一致性校验；以 Block JSONB 为唯一回写目标（§7.2） |
| Hocuspocus 替换引入协同回归 | 中 | 协议同源（Yjs），一个 PR 内完成替换 + 保留旧 provider 兼容壳；协同回归矩阵（并发/降级/cursor/断线重连）全绿再删旧代码（§7.5） |
| Hocuspocus 持久化与版本契约冲突 | 中 | 明确职责边界：Hocuspocus 管协同工作态，版本仍由 mora-api 落 `document_versions`；新增 `collab_documents` 表独立于版本表 |
| 误引入黑名单 License（人工粘贴代码） | 高 | CI 门禁 + 本设计 §8.3 红线 + 选型清单明示；代码评审把关 |
| 上游停更 / bus factor（Milkdown 单人维护） | 低 | 锁定 commit/digest（门禁已强制）；关键组件优先多维护者项目（Tiptap/Hocuspocus）；Milkdown/Vditor 二选一前做 spike |
| 查看与编辑双渲染源不一致 | 低 | 共用 `blocksToMarkdown` 单一派生函数；回归测试对比两视图输出 |
| 附件预览安全（kkFileView SSRF 历史漏洞） | 中 | 独立容器 + 网络隔离（只允许访问 mora-api 内网）；签名 URL 短时有效；升级到最新版（历史 CVE 已在 v5.0.1 修复） |

---

## 10. 验收与门禁清单

- [ ] 每个新组件完成 §8.2 门禁（lock.json / ADR / NOTICE / `make third-party-check sbom notices` 全绿）。
- [ ] Phase 0-A：slash/drag/块选择/气泡菜单/表格/图片上传可用；Markdown ↔ WYSIWYG 往返可逆回归全绿；协同下块操作一致收敛。
- [ ] Phase 0-B：DocumentViewer 与分享路由上线；公式/图表/代码高亮/目录正确；无编辑器依赖。
- [ ] Phase 0-C：块级 + 文本级 diff 上线；回滚行为与现有契约一致。
- [ ] Phase 1：Hocuspocus 部署；JWT/RBAC 鉴权；PostgreSQL 持久化与恢复；presence/cursor/降级行为与现状一致（回归矩阵全绿）；旧 y-websocket 与 `MoraCollabProvider` 移除。
- [ ] 现有 API 契约（documents / document_versions / permissions / audit_logs / doc_events）无破坏性变更。

---

## 11. 附录：候选项目全景表（2026-08-11 实测）

| 项目 | Star | License | 语言/栈 | 用途 | 结论 |
|---|---|---|---|---|---|
| [Tiptap](https://github.com/ueberdosis/tiptap) | 38.0k | MIT | TS + ProseMirror | 编辑器内核（现用 v3） | ✅ 保留 |
| [Hocuspocus](https://github.com/ueberdosis/hocuspocus) | 2.5k | MIT | Node/TS | Yjs 协同后端 | ✅ Phase 1 |
| [BlockNote](https://github.com/TypeCellOS/BlockNote) | 10.1k | MPL-2.0 | React + Tiptap v2 | 块级 UI 组件 | ⚠️ 评估引入（版本冲突） |
| [Milkdown](https://github.com/Milkdown/milkdown) | 11.8k | MIT | ProseMirror + remark | Markdown WYSIWYG | ✅ 候选 |
| [Vditor](https://github.com/Vanessa219/vditor) | 11.2k | MIT | TS 框架无关 | Markdown 即时渲染 | ✅ 候选 |
| [Novel](https://github.com/steven-tey/novel) | 16.4k | Apache-2.0 | Tiptap v2 + Next | Notion 风格 + AI | ⚠️ 参考 |
| [La Suite Docs](https://github.com/suitenumerique/docs) | 16.7k | MIT | Django + React | 组件组合蓝本 | ✅ 对标/借鉴 |
| [Docmost](https://github.com/docmost/docmost) | 21.3k | AGPL-3.0 | Node/TS | 平台替代候选 | ❌ 黑名单 |
| [Outline](https://github.com/outline/outline) | 40.1k | BSL-1.1 | Node/TS | 平台替代候选 | ❌ 黑名单 |
| [AFFiNE](https://github.com/toeverything/AFFiNE) | 71.4k | CE MIT / BlockSuite MPL-2.0 | TS + Rust | 平台替代候选 | ⚠️ 仅借鉴 |
| [AppFlowy](https://github.com/AppFlowy-IO/AppFlowy) | 75.2k | AGPL-3.0 | Dart | 平台替代候选 | ❌ 黑名单 |
| [HedgeDoc](https://github.com/hedgedoc/hedgedoc) | 7.4k | AGPL-3.0 | Node/TS | 平台替代候选 | ❌ 黑名单 |
| [PDF.js](https://github.com/mozilla/pdf.js) | 53.7k | Apache-2.0 | JS | PDF 查看 | ✅ Phase 2 |
| [mammoth.js](https://github.com/mwilliamson/mammoth.js) | 6.3k | BSD-2-Clause | JS | docx → HTML | ✅ Phase 2 备选 |
| [docx-preview](https://github.com/VolodymyrBaydalka/docx-preview) | — | MIT | JS | docx 前端渲染 | ✅ Phase 2 |
| [kkFileView](https://github.com/kekingcn/kkFileView) | 10k+ | Apache-2.0 | Spring Boot | 全格式在线预览 | ✅ Phase 2 重型 |
| [ONLYOFFICE DocumentServer](https://github.com/ONLYOFFICE/DocumentServer) | 6.8k | AGPL-3.0 | C++/JS | Office 在线编辑 | ❌ 黑名单（法务评估除外） |
| [Collabora Online](https://github.com/CollaboraOnline/online) | 3.3k | MPL-2.0 | C++ | Office 在线编辑 | ⚠️ Phase 3 远期 |

---

> 本设计为方案 A 的详细落地依据；各 Phase 实施时须以本文件 + 对应 ADR 为准，并保持与 13 号文档（第三方治理门禁）、10 号文档（解析层）边界清晰。
