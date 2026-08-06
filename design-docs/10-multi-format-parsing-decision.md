# 多格式解析技术选型决策书

> 文档版本：v1.0 ｜ 产出人：Mora项目架构师 ｜ 对应任务：YS-70《分析 WeKnora 功能并制定 Mora 整体规划》
> 父决策：01-tech-selection-decision.md（自研路线 + 全组件宽松 License 基线）
> 产品基线：YS-70 PM 初版规划 §1.1 / 阶段一 P0「多格式导入」
> 评审状态：**已定稿**（v1.1）——§1–§7 选型论证 + Parser 接口 + §8 table 章节（PM 定调 (a) 最小可用 BlockTable）三项全量收敛

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

## 8. table / Block 模型扩展决策（已定稿）

> PM 已定调 **(a) 保留表格保真，范围收成「最小可用 BlockTable」**：P0 做 `BlockTable` + `tableRow`/`tableCell`，cell **纯文本**、**无合并单元格**、**无 cell 内富文本**；converter 补 GFM pipe 表格双向；`extract.go` 补 table 分支按行结构化输出。合并单元格 / cell 内富文本 / 嵌套表 / 样式列 **P1**。本节据此出方案，不扩到 P1 项。

### 8.1 P0 边界（最小可用 BlockTable）

| 维度 | P0（本决策书范围） | P1（不在 P0 内） |
|---|---|---|
| cell 内容 | 纯文本（单 string，无 marks） | cell 内富文本（bold/italic/link/多 run） |
| 合并单元格 | 不支持（`rowspan`/`colspan` 丢弃或拆分为重复 cell） | 支持 `rowspan`/`colspan` |
| 嵌套 | 表格不可嵌套于表格 | 嵌套表 |
| 样式 | 无 cell 背景色/对齐样式 | cell 样式 |
| 行列结构 | 保留行列（GFM 管道表格可逆） | — |

### 8.2 数据模型（`internal/domain/block.go`）

新增三个 BlockType 常量；节点形状遵循现有 TipTap/ProseMirror `{type, attrs, content[]}`：

```go
const (
    BlockTable    BlockType = "table"
    BlockTableRow BlockType = "tableRow"
    BlockTableCell BlockType = "tableCell"
)
```

- `BlockTable`：`Content []Block`（一组 `tableRow`），`Attrs` 可带 `columnCount`（可选，便于渲染）。
- `BlockTableRow`：`Content []Block`（一组 `tableCell`）。
- `BlockTableCell`：**P0 用 `Text string` 承载纯文本**（cell 纯文本边界）；P1 升级为 `Content []Block` 承载富文本时，新增 `inlineContent` 或复用 `Content` 字段（TipTap 约定 cell 为 block 容器，富文本 cell 走 `Content []Block{paragraph}`）。

> 选 `Text string` 而非 `Content []Block{paragraph}` 是为锁定 P0 「cell 纯文本」边界，避免 Parser 顺手把 run marks 写进 `Marks` 字段越界。现有 `Block.Text`/`Marks` 字段已支持 leaf text node，P0 cell 用 `Text` 即可。

**无 DB 迁移**：`documents.content` 是 JSONB，新增 block type 不改表结构（`migrations/003_documents.up.sql` 的 `content JSONB` 天然承接）。前端 TipTap 需新增 table 节点渲染，属前端工作项，非本决策书范围。

### 8.3 Converter 双向（`internal/module/mora/content/converter.go`）

现状：converter 支持 heading/paragraph/codeBlock/blockquote/list/divider，**无 table**（`writeBlockMarkdown` 无 table 分支，`MarkdownToBlocks` 不识别 GFM 管道表格；注释自述「unsupported constructs degrade gracefully」）。

P0 补：

- **BlocksToMarkdown**：`writeBlockMarkdown` 新增 `case BlockTable` 分支，输出 GFM 管道表格：
  ```
  | H1 | H2 |
  | --- | --- |
  | a | b |
  ```
  首行取 `tableRow[0]` 的 cell `Text` 为表头；分隔行固定 `---`；后续行按 cell `Text` 输出。
- **MarkdownToBlocks**：识别 GFM 管道表格行（`| ... |` 起始 + 第二行 `| --- | --- |` 分隔符），构造 `BlockTable` → `tableRow` → `tableCell(Text)`。
- **行列保持**：GFM 表格天然保留行列，往返不丢行列（满足 PM §1.1 验收「Markdown 往返不丢行列」）。
- **降级约束**：cell 含 `|` 字符需转义为 `\|`；非 GFM 表格（HTML `<table>` 非管道形式、合并单元格）在 P0 不保证双向，Parser 侧先转 GFM 管道表格再走 converter。

### 8.4 RAG `extract.go` table 分支（`internal/module/rag/pipeline/extract.go`）

现状：`writeBlock` 无 table 分支，table 落 `default`，`collectText` 把所有 text leaf 拍平为单段纯文本，**行列丢失**（`extract.go:48-83`）。

P0 补 `case btype == "table"` 分支，**按行结构化输出**：

- 遍历 `tableRow` → 每个 `tableCell` 取 `Text` → 行内 cell 用 ` | ` 连接，行末换行；
- `StructuredText` 输出形如 `H1 | H2 \n a | b`（保留行列，chunker 可按行切分）；
- `PlainText` 同样按行输出（BM25 索引时 cell 文本带行列上下文）。

效果：表格密集文档的 chunk 不再把整表压成一段无结构文本，RAG 召回时行列上下文保留——满足 PM 验收「RAG 按行结构化」。

### 8.5 Parser 映射（DOCX/HTML/XLSX → BlockTable）

| 源 | 映射路径 | P0 保真 |
|---|---|---|
| DOCX `w:tbl` | `w:tr`→`tableRow`，`w:tc`→`tableCell`，cell 取 `w:p/w:r/w:t` 文本拼接为 `Text` | 行列保留；cell run marks **丢弃**（见 §8.6） |
| HTML `<table>` | `<tr>`→`tableRow`，`<td>/<th>`→`tableCell`，cell `textContent` 为 `Text` | 行列保留；cell 内 HTML 标签降级为纯文本 |
| XLSX | `excelize` 读 sheet，行→`tableRow`，单元格→`tableCell(Text)` | 行列保留；公式取结果值 |

### 8.6 回答 PM 留的问题：DOCX `w:tbl` → 最小 BlockTable 是否丢弃 cell 内 run marks？

**会丢弃。但符合 PM 既定 P0 边界，边界无需微调。** 具体如下：

DOCX cell 内容结构：`w:tc` → `w:p`（段落）→ `w:r`（run）→ `w:rPr`（run properties，含 `w:b` 加粗 / `w:i` 斜体 / `w:color` 等）+ `w:t`（文本）；`w:hyperlink` 含链接 run。P0 「cell 纯文本」边界要求 cell 落为 `BlockTableCell{Text: string}`，**cell 内容是单 string，不是 `[]Block` with marks**。因此 Parser 在抽取 cell 时把所有 `w:r` 的 `w:t` 文本拼接，**`w:rPr` 的 bold/italic 与 `w:hyperlink` 的 link 均在 Parser 阶段丢弃**——这不是 converter 或 extract 的丢失，而是 P0 边界本身的规定（cell 纯文本、无 cell 内富文本）。

依据：`domain.Block` 已有 `Marks []Mark` 字段（`block.go:24`），converter 的 `writeInline`/`parseInline` 已支持 `bold`/`italic`/`code` 三种 mark（`converter.go:209-259`），**模型与 converter 对 marks 的基础设施已就绪**——P0 cell 纯文本是**主动收敛**，非模型限制。故：

- **P0 边界无需微调**：丢 marks 是 PM 明确收敛的项，不是越界丢失；
- **P1 升级路径清晰**：cell 升级为 `Content []Block{paragraph}` 后，Parser 把 `w:r` 的 `w:rPr`→`Mark{Type:"bold"}`、`w:hyperlink`→`Mark{Type:"link",Attrs:{href}}` 直接写入，converter 与 extract 已有 marks 处理，无需再改域模型——P1 是 Parser + cell 节点形态的升级，不是架构重做；
- **一个 RAG 侧 nuance 提示**：丢 marks 不影响 RAG 召回（召回基于文本，marks 不是检索维度），但若未来 P1 保留链接 `href` 作为实体线索用于知识图谱导航（PM §1.4 远期项），则 link mark 需在 P1 补回。此为远期提示，不进 P0。

### 8.7 估期（架构层粗估，供 PM 排期，研发定稿为准）

| 项 | 估期 | 说明 |
|---|---|---|
| 域模型 `BlockTable`/`tableRow`/`tableCell` 常量 | 0.5d | 仅 `block.go` 加常量，无 DB 迁移 |
| converter GFM 双向 + 测试 | 2–3d | `writeBlockMarkdown` table 分支 + `MarkdownToBlocks` GFM 识别 + 往返测试 |
| `extract.go` table 分支 + 测试 | 1d | 按行结构化输出 + chunk 验证 |
| DOCX `w:tbl` Parser 映射 | 1.5d | 含 cell 文本拼接、`w:rPr` 丢弃策略、样例测试 |
| HTML `<table>` / XLSX 映射 | 1d | 复用 table 节点构造 |
| 合计（table 章节） | **~6d** | 与 §1–§7 选型实现并行，不阻塞其余格式 |

> 此估期仅含 table 章节相关改动，不含 §1–§7 的 Parser 模块整体实现与 import/export 路由（后者由研发依选型表另行排期）。

---

> 本决策书 §1–§8 现已全量定稿（选型论证 + Parser 接口 + table 章节三项收敛），交付研发依选型表与 §8 方案实现 Parser 模块、converter/extract table 扩展与 import/export 路由。合并单元格/cell 富文本/嵌套/样式列 P1，cell 升级为 `Content []Block` 后 marks 基础设施已就绪，无需再改域模型。
