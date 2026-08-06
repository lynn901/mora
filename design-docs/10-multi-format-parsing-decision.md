# 多格式解析技术选型决策书

> 文档版本：v1.0 ｜ 产出人：Mora项目架构师 ｜ 对应任务：YS-70《分析 WeKnora 功能并制定 Mora 整体规划》
> 父决策：01-tech-selection-decision.md（自研路线 + 全组件宽松 License 基线）
> 产品基线：YS-70 PM 初版规划 §1.1 / 阶段一 P0「多格式导入」
> 评审状态：**部分定稿**——§1–§7 现在可定稿；§8 table/Block 模型扩展**挂起待补**，待 PM 定 P0 表格保真口径后补入

---

## 1. 决策背景与范围

### 1.1 背景

PM 初版规划将「多格式解析」列为 P0，目标是闭合 `design-docs/04-api-contract.md §12` 已定义但未实现的 `POST /workspaces/{workspace_id}/import`（Markdown/PDF/Docx/HTML/ZIP，异步 task_id）与 `GET /documents/{id}/export`。当前 `internal/module/mora/handler/document.go` 仅实现 Create/Update/Get/List/Versions/Diff/Rollback，**import/export 路由缺失**；RAG 抽取 `internal/module/rag/pipeline/extract.go:22-26` 注释明示依赖上游预抽取的 `DocumentSnapshot.ContentText`——即格式解析是未闭合的上游环节。

本决策书回答「用什么库、怎么解析、保真到什么程度」，是 P0 编码前置门禁。

### 1.2 决策原则（继承 01-tech-selection §1.2）

1. **私有化优先**：默认不出网，解析须在本地完成。
2. **License 合规优先**：继承 01 §5「全组件宽松 License」基线，不得引入 AGPL/GPL 或专有商业授权依赖。
3. **结构感知优先**：解析产物须能转为 `domain.Block`（TipTap/ProseMirror 节点），而非压成纯文本——区别于 WeKnora 的 chunk-only 策略。
4. **可插拔**：Parser 抽象为接口，按格式插拔；OCR/VLM 作可选增强，不进 P0 主路径。
5. **复用既有链路**：解析产物接入现有 `MarkdownToBlocks`（`content/converter.go:89`）→ Valkey Streams → rag-worker → Qdrant 链路，chunk 的 `visible_to` RBAC payload 复用。

### 1.3 P0 范围

PDF（文本层）/ DOCX / XLSX / HTML / Markdown-ZIP 五格式。PPT/EPUB/图片/CSV/JSON 列 P2；扫描件 OCR / VLM 图片描述 / ASR 列可选增强（挂 Ollama 之后）。

---

## 2. 候选库评估

### 2.1 候选总表

| 格式 | 候选 | License | 活跃度 | 选型定位 |
|---|---|---|---|---|
| DOCX | **自研极简 reader**（`archive/zip`+`encoding/xml`） | Go 标准库（BSD-3） | N/A | **选用**：P0 覆盖 `word/document.xml` 主流结构 |
| DOCX | `unidoc/unioffice` | **专有 UniDoc EULA**（商业授权，需 license code） | 4.9k star | **否决**：非自由 License，破坏 01 §5 基线 |
| XLSX | **`xuri/excelize`** | BSD-3-Clause | 20.8k star，活跃 | **选用**：事实标准，纯 Go |
| PDF(文本层) | **`ledongthuc/pdf`** | BSD-3-Clause | 614 star，纯 Go | **选用**：文本层抽取，轻量 |
| HTML | **`golang.org/x/net/html`** | BSD-3-Clause | Go 官方扩展 | **选用**：DOM 遍历，标准 |
| MD/ZIP | `archive/zip` + 现有 `MarkdownToBlocks` | 标准库 + 已有 | N/A | **选用**：原生保真 |

### 2.2 关键否决理由：`unidoc/unioffice`

> ⚠️ **更正**：本 issue 上一轮架构评估曾将 `unidoc/unioffice` 标注为「AGPLv3/商业双授权」。经核实其当前 License 为**专有 UniDoc EULA**——「commercial product and requires a license code to operate」，**非 AGPL**。早期版本曾以 AGPLv3 发布，现版已转为专有商业 EULA。无论哪种，结论一致：**非自由 License**，不符合 01-tech-selection-decision.md §5「全组件宽松 License，支持闭源私有化商用交付」的基线，**否决**。此处理由以 License 合规为准，不取「AGPL 传染」的旧表述。

### 2.3 评分表（满分 5 分）

| 维度（权重） | 自研 DOCX reader | excelize | ledongthuc/pdf | x/net/html |
|---|---|---|---|---|
| License 合规（30%） | 5（标准库 BSD） | 5（BSD-3） | 5（BSD-3） | 5（BSD-3） |
| 结构感知（20%） | 5（直读 OOXML 结构） | 5（行列/单元格） | 3（文本层，版式弱） | 5（DOM） |
| 维护成本（20%，分高=易） | 4（自维护，但 OOXML 主流子集稳定） | 5（社区维护） | 4 | 5（官方） |
| 私有化（20%） | 5（纯本地） | 5 | 5 | 5 |
| 性能（10%） | 5 | 5 | 4 | 5 |
| **加权总分** | **4.8** | **5.0** | **4.3** | **5.0** |

---

## 3. 决策结论

| 格式 | 选型 | License |
|---|---|---|
| DOCX | 自研极简 reader（`archive/zip`+`encoding/xml`） | Go 标准库 BSD-3 |
| XLSX | `xuri/excelize` v2 | BSD-3-Clause |
| PDF(文本层) | `ledongthuc/pdf` | BSD-3-Clause |
| HTML | `golang.org/x/net/html` | BSD-3-Clause |
| MD/ZIP | `archive/zip` + 现有 `MarkdownToBlocks` | 标准库 + 已有 |
| DOCX（备选，**否决**） | `unidoc/unioffice` | 专有 EULA — 不引入 |

---

## 4. 决策理由

1. **License 全合规**：选用项均为 BSD-3/标准库，继承 01 §5 宽松 License 基线，支持闭源私有化商用交付。`unidoc/unioffice` 的专有 EULA 是唯一硬否决项。
2. **DOCX 自研可行且合规最优**：P0 所需的 DOCX 保真依赖 `word/document.xml` 的主流结构（段落/标题 `pStyle`/列表 `numPr`/表格 `w:tbl`），用标准库 `archive/zip`+`encoding/xml` 即可覆盖，避免引入任何第三方 DOCX 依赖；OOXML 主流子集稳定，自维护成本可控。`unidoc/unioffice` 功能更全但 License 不合规，`excelize` 不处理 DOCX。
3. **excelize 是 XLSX 事实标准**：20.8k star、纯 Go、BSD-3、活跃维护，无可争议。
4. **ledongthuc/pdf 轻量够用**：PDF 文本层抽取 P0 够用；版式/扫描件能力弱是已知降级，扫描件 OCR 挂 Ollama 之后列 P1，不进 P0 主路径。其 star 数（614）低于 excelize 但 PDF 文本层抽取是成熟稳定场景，风险可控。
5. **x/net/html 官方背书**：HTML DOM 遍历 → Block 映射，标准且零维护负担。
6. **MD/ZIP 零新依赖**：`MarkdownToBlocks` 已在，ZIP 批量解包后逐文件走现成路径。

---

## 5. Parser 接口设计（结构感知解析策略）

### 5.1 接口定义

解析统一漏斗：各 Parser 产出 `[]domain.Block` 或 Markdown 文本（再走 `MarkdownToBlocks`），落 `internal/module/mora/content/parser`（新模块）。接口草案：

```go
// ParseResult is the structured output of a format parser. Blocks is the
// preferred form (structure-preserving); Markdown is the fallback when a
// format maps cleanly to Markdown (e.g. HTML/PDF text). Title and
// Attachments (embedded images) are extracted alongside.
type ParseResult struct {
    Blocks      []domain.Block
    Markdown    string // used when Blocks is nil (routed via MarkdownToBlocks)
    Title       string
    Attachments []domain.Attachment
    SourceMeta  map[string]string // e.g. source format, page count
}

type Parser interface {
    // Formats returns the MIME/types this parser handles (e.g. "docx","xlsx").
    Formats() []string
    // Parse reads a document stream and produces structured blocks.
    Parse(ctx context.Context, r io.Reader, opts ParseOpts) (*ParseResult, error)
}
```

### 5.2 结构感知策略（保真映射）

| 源格式元素 | 目标 Block | 保真级别 |
|---|---|---|
| DOCX `pStyle=Heading1-6` | `BlockHeading`(level) | 高 |
| DOCX 段落 | `BlockParagraph` | 高 |
| DOCX `numPr` | `BlockList`(ordered/bullet) | 高 |
| DOCX `w:tbl` | **见表格挂起项 §8** | 待定 |
| DOCX 代码样式 | `BlockCode` | 中（靠样式名启发） |
| XLSX sheet | 文档分节 + 行→Block | 中 |
| PDF 文本字号 | heading 层级启发 → Markdown `#` → `MarkdownToBlocks` | 低（可接受降级） |
| HTML h1-h6/p/ul/ol/pre | 对应 Block | 高 |
| HTML `<table>` | **见表格挂起项 §8** | 待定 |
| MD | `MarkdownToBlocks` 现成 | 原生 |

### 5.3 与 RAG pipeline 的衔接

解析后：① 块走 `MarkdownToBlocks`/直建 Block 落 `documents.content`；② `pipeline.go:161` 的 `snap.ContentText` 由解析产出的 `Markdown`/`ExtractText(blocks)` 填充，闭合 `extract.go` 的上游依赖注释；③ 现有 Valkey Streams → chunk → Qdrant + `visible_to` RBAC payload 链路不变。

### 5.4 OCR/VLM 可选增强（P1，不进 P0 主路径）

PDF 扫描件/图片 → 图像 → **Ollama 多模态模型**（如 llava）做描述/OCR → 文本 → Markdown → Block。复用现有 `OllamaURL`（`internal/platform/config/config.go`），是「不出网」的本地化正道。`gosseract`(MIT) 需系统级 tesseract 二进制，与「Docker Compose 一键拉起」有张力，**不进 P0**。

---

## 6. License 合规声明

| 组件 | License | 合规影响 |
|---|---|---|
| `archive/zip` + `encoding/xml`（DOCX） | Go 标准库 BSD-3 | 无传染 |
| `xuri/excelize/v2` | BSD-3-Clause | 无传染 |
| `ledongthuc/pdf` | BSD-3-Clause | 无传染 |
| `golang.org/x/net/html` | BSD-3-Clause | 无传染 |
| `~unidoc/unioffice~`（**否决**） | 专有 UniDoc EULA | 需付费 license code，**不符合闭源私有化商用基线，不引入** |

**合规结论**：选用项全为 BSD-3/标准库，继承 01 §5 基线，支持闭源私有化商用交付。

---

## 7. 风险与缓解

| 风险 | 等级 | 缓解措施 |
|---|---|---|
| DOCX 自研覆盖不全（复杂样式/嵌套表格/OLE 对象） | 中 | P0 仅保主流子集；异常样式降级为 paragraph，不阻塞导入；测试用例覆盖 5 类样例文档 |
| PDF 文本层抽取丢失版式（表格/分栏） | 中 | P0 接受降级；PDF 表格见 §8 挂起项；扫描件走 P1 OCR |
| `ledongthuc/pdf` 社区规模较小（614 star） | 低 | PDF 文本层抽取是成熟稳定场景；必要时可 fork 自维护，BSD-3 允许 |
| DOCX 代码块识别靠样式名启发，不精确 | 低 | P0 接受中保真；提供样式名映射配置项 |
| 嵌入图片/附件抽取 | 中 | P0 先记录 `Attachments` 元数据，图片落 MinIO（复用现有附件存储）；图片内容理解挂 P1 VLM |

---

## 8. 挂起待补：table / Block 模型扩展

> **本节挂起，待 PM 定 P0 表格保真口径后补入，不自行拍板。** 触发点：PM §1.1 要求「表格转 Block JSON 不丢信息」，架构师核实 `internal/domain/block.go:9-17` 的 BlockType **无 `table` 类型**，保真需扩域模型。

### 8.1 待 PM 决策的二选一

- **(a) 保留表格保真** → 接受域模型扩展入 P0 scope（P0 工作量上修、周期后移）：
  - 新增 `BlockTable` + `tableRow`/`tableCell` 子节点类型（`domain/block.go`）；
  - `content/converter.go` 补 table ↔ GFM Markdown table 双向；
  - `rag/pipeline/extract.go` 补 table 分支（现 `writeBlock` 把 table 落 `default` 分支，`collectText` 拍平为纯文本，**行列结构丢失**）；
  - DOCX `w:tbl` / HTML `<table>` / XLSX 行 的 Parser 映射目标由「待定」改为 `BlockTable`。
- **(b) P0 显式降级** → 放弃表格保真，表格降级为 paragraph + `|` 分隔文本，`table` 域模型扩展列 **P0.5 fast-follow**。

### 8.2 架构师权衡提示（供 PM 决策）

降级不仅影响编辑保真，也影响 RAG 召回质量：`extract.go` 现 `default` 分支的 `collectText` 把表格所有 text leaf 拍平为单段纯文本，**行列关系与单元格边界丢失**——表格密集文档（财务/规格/对比表）的检索召回质量会下降。若 P0 范围内表格密集文档占比高，(a) 的 RAG 召回收益可能抵过周期成本；若 P0 主要是文本型文档，(b) + P0.5 更经济。

### 8.3 补入流程

PM 在本 issue 回复选定 (a)/(b) 后，架构师据此：
- 选 (a)：本 §8.1 升级为定稿 §8「table 域模型扩展决策」，补迁移要点与索引策略，P0 scope 上修；
- 选 (b)：本 §8.1 落为定稿 §8「表格降级决策」，`table` 扩展单列 P0.5 issue，本决策书 §1–§7 即为完整定稿。

无论 (a)/(b)，§1–§7 选型结论不受影响（DOCX/HTML/XLSX Parser 选型与 table 无关），**勿因 §8 挂起阻塞其余章节落地**。

---

> 本决策书 §1–§7 现在可定稿，交付研发依选型表实现 Parser 模块与 import/export 路由。§8 待 PM 口径回复后补入。table 域模型扩展的迁移脚本要点与索引策略将在 §8 定稿时补齐。
