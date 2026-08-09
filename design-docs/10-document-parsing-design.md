# 多格式文档解析技术架构设计与可行性调研

> 文档版本：v1.0 ｜ 产出人：Mora 项目架构师 ｜ 对应任务：YS-75（父 issue YS-73）
> 依据：父 issue YS-73（参考 WeKnora 多格式解析）、design-docs/02-system-architecture.md、03-data-model.md、05-rag-pipeline-design.md、04-api-contract.md
> 技术栈约束：Go 1.25 / Gin / PostgreSQL 16 + zhparser FTS / Valkey Streams / Qdrant / TEI / Ollama / y-websocket；支持 100% 私有化部署，网络连接可配置

---

## 0. 摘要（结论先行）

本设计在 Mora 现有 RAG 流水线（Valkey Streams 事件驱动 → rag-worker 消费 → 抽取/分块/向量化/写 Qdrant）之上，扩展一个**文档解析层**，把"上传的任意格式文件"转化为现有流水线已经消费得动的 `content`（Block JSONB）/`content_text`（纯文本）输入，从而**复用而非重写**既有分块、向量化、混合检索、RBAC 能力。

核心决策：

1. **解析引擎分层**：纯文本/结构化格式（Txt/MD/HTML/JSON/CSV）走**纯 Go 内联解析器**（无新依赖、无 CGO）；富文档/二进制格式（PDF/Word/Excel/PPT/EPUB/MHTML）走**可插拔 Parser 接口**，MVP 用成熟纯 Go 库，复杂版式 PDF 与 OCR/VLM/ASR 走**可选 Python sidecar**（mora-parser），以 HTTP/gRPC 暴露，Docker 镜像私有化部署。
2. **不引入 OpenDataLoader / PaddleOCR-VL 作为硬依赖**：前者为 WeKnora 自有 Python 框架、绑定其技术栈；后者体量大、GPU 依赖强。本设计以"等价能力、更低耦合"为目标，用 Go 库 + 轻量 sidecar 替代，能力对齐但解耦。
3. **分块策略**：在现有固定长度+重叠+标题边界分块基础上，增加**自适应三层分块（adaptive 3-tier）**与**parent-child 分块**两种可选模式，作为 `chunking_strategy` 配置项，与 Valkey Streams / Qdrant 衔接点不变。
4. **解析全流程受 RBAC 硬约束**：解析任务携带 `visible_to` 快照，无权文档不可解析、不可见（存在性不泄露），沿用 05 §4.3 的 payload 过滤与 SQL 双重保障。
5. **实现优先级**：P0 = Txt/MD/HTML/JSON + PDF/Word 文本层解析 + 复用现有分块；P1 = Excel/PPT/EPUB/MHTML/CSV + 自适应三层分块 + parent-child + 解析进度追踪 + 批量重解析；P2 = OCR/VLM/ASR 多模态 + 图抽取 + 问答生成。

---

## 1. 解析引擎选型

### 1.1 设计原则与约束

- **私有化部署**：所有解析依赖必须可 Docker 化并支持本地运行；需要调用外部 SaaS API 时通过配置启用并纳入授权与审计。模型权重可预置、本地挂载或从配置的模型源获取。
- **CGO 谨慎**：Mora 现为纯 Go 构建（见 Dockerfile），引入 CGO 会拉高交叉编译与镜像构建成本。优先纯 Go；仅在无可用纯 Go 实现时引入 sidecar。
- **解耦优先**：解析层与 RAG 流水线之间只通过 `DocumentSnapshot{Content []byte, ContentText string}` 与 `ParsedDocument` 结构通信，不直接写 Qdrant。
- **可插拔**：定义 `Parser` 接口，按 MIME/扩展名路由；新增格式只加一个 Parser 实现，不改管线。

### 1.2 Parser 接口设计

```go
// internal/module/rag/parser/parser.go

// ParsedDocument is the output of a Parser: structured content the existing
// pipeline already consumes (blocks JSONB) + plain text for FTS + extracted
// assets (images/tables) referenced by block id.
type ParsedDocument struct {
    Blocks      []byte          // Block JSONB array, same schema as documents.content
    ContentText string          // plain text, written to documents.content_text for FTS
    Title       string          // extracted document title (heading or metadata)
    Assets      []ParsedAsset   // images/figures/tables extracted for multimodal (P2)
    Meta       ParsedMeta       // source format, parser name, page count, warnings
}

type ParsedAsset struct {
    BlockID   string            // links the asset to its placeholder block
    Kind      string             // image / table / chart
    MIMEType  string
    StorageKey string            // object-storage key (parsed assets persist like attachments)
}

type ParsedMeta struct {
    Format      string           // pdf / docx / ...
    ParserName  string           // "unipdf" / "docx-go" / "mora-parser"
    PageCount   int
    Warnings    []string
    DurationMS  int64
}

// Parser converts a raw uploaded file (read from object storage) into a
// ParsedDocument. Implementations are registered per MIME/extension.
type Parser interface {
    // Parse reads the object at storageKey and returns structured content.
    Parse(ctx context.Context, storageKey string, opts ParseOptions) (*ParsedDocument, error)
    // Supports reports whether this parser handles the given MIME / filename.
    Supports(mime, filename string) bool
    // Name identifies the parser in metadata and metrics.
    Name() string
}

// ParseOptions carries per-upload config overrides (§7).
type ParseOptions struct {
    ChunkingStrategy string             // fixed / adaptive_3tier / parent_child
    ChunkSize        int
    ChunkOverlap     int
    EnableOCR        bool               // scanned PDF / image OCR (P2)
    EnableVLM        bool               // image description (P2)
    EnableASR        bool               // audio/video transcription (P2)
    EnableGraph      bool               // graph extraction (P2)
    EnableQAGen      bool               // question generation (P2)
    OcrLang          string             // chi_sim+eng / eng
    VLMModel         string             // ollama model id, e.g. minicpm-v
}
```

`ParsedDocument.Blocks` 复用 `documents.content` 的 Block schema（heading/paragraph/codeBlock/chart/canvas），因此 03 §2.3 的 documents 表、05 §3.2 的 `ExtractText` 均无需改动——解析层只是"另一种产生 Block 的来源"。

### 1.3 各格式解析方案与依赖

> 下表选型基于 2026-08-06 对各库 license/语言/维护状态的核实（见 §1.6 来源）。MVP（P0）列标注 **[P0]**，其余 **[P1]** / **[P2]**。"纯 Go" = 无 CGO、无外部运行时。

| 格式 | 推荐方案 | 类型 | License | CGO | 私有化 | 优先级 | 备注 |
|---|---|---|---|---|---|---|---|
| **Txt** | `strings`/`bufio` + 编码探测(`golang.org/x/text/encoding`) | 纯 Go | BSD-3 | 否 | ✅ | **P0** | UTF-8/GBK/Big5 自动探测转码 |
| **Markdown** | `github.com/yuin/goldmark`（AST，含 heading id）→ Block；或直接复用现有 `moracontent.MarkdownToBlocks` | 纯 Go | MIT | 否 | ✅ | **P0** | goldmark 4.9k★ 活跃；MVP 可先复用现有 converter，goldmark 用于更精细的标题边界 |
| **HTML** | `github.com/PuerkitoBio/goquery`（基于 `golang.org/x/net/html`）→ 去标签+保留标题层级 | 纯 Go | BSD-3 | 否 | ✅ | **P0** | goquery 15k★ 活跃；h1-h6 映射 heading block |
| **JSON** | `encoding/json` → 序列化为 code block 或按需结构化 | 纯 Go | BSD-3 | 否 | ✅ | **P0** | 大 JSON 流式 tokenize |
| **CSV** | `encoding/csv` → 每 N 行合成一个 paragraph block | 纯 Go | BSD-3 | 否 | ✅ | **P1** | 保留表头作为 section_path |
| **PDF（文本层）** | `github.com/pdfcpu/pdfcpu`（文本抽取）或 `github.com/ledongthuc/pdf` | 纯 Go | Apache-2.0 / BSD-3 | 否 | ✅ | **P0** | 仅文本层；版式/表格弱；富版式走 sidecar |
| **PDF（版式/扫描件）** | mora-parser sidecar（markitdown 或 OpenDataLoader-PDF 做布局，PaddleOCR 做 OCR） | Python/Java sidecar | Apache-2.0 / MIT | — | ✅(镜像) | **P1/P2** | 布局分析+OCR，可选 GPU |
| **Word (.docx)** | 轻量 OOXML reader（`archive/zip`+`encoding/xml`，~300 行，自研无 License 风险）；富功能备选 `github.com/unidoc/unioffice`（AGPL，见 §1.5） | 纯 Go | 自研无约束 / AGPL⚠️ | 否 | ✅ | **P0** | `dslipak/docx`(MIT) 已停更(2020)不选；docx = zip+xml 易自研 |
| **Excel (.xlsx)** | `github.com/qax-os/excelize/v2` | 纯 Go | BSD-3 | 否 | ✅ | **P1** | excelize 20.8k★ 活跃，Go 生态标杆；每表→section |
| **PowerPoint (.pptx)** | 轻量 OOXML reader（自研）或 mora-parser sidecar（markitdown） | 纯 Go 自研 / Python sidecar | 自研无约束 / MIT | 否 | ✅ | **P1** | 纯 Go 唯一全功能库 unioffice 为 AGPL，不进主进程（§1.5） |
| **EPUB** | `github.com/go-shiori/go-epub`（MIT）或 `archive/zip`+goquery 自研 reader（~150 行） | 纯 Go | MIT | 否 | ✅ | **P1** | EPUB=zip of XHTML；spine 顺序解析 |
| **MHTML** | 标准库 `net/mime/multipart`+`encoding/quotedprintable` 解包 → 复用 goquery | 纯 Go（标准库） | BSD-3 | 否 | ✅ | **P1** | 无专用库；~100 行自研，非问题 |
| **Images（解码）** | 标准库 `image/*`（PNG/JPEG/GIF/BMP） | 纯 Go | BSD-3 | 否 | ✅ | P2 | 解码 trivial；PDF 页面渲染为图需 pdfium（CGO 或 Python `pypdfium2` sidecar） |
| **Images（OCR/VLM）** | mora-parser sidecar（PaddleOCR OCR + Ollama VLM） | Python sidecar | Apache-2.0 / MIT | — | ✅ | **P2** | 见 §3 多模态 |
| **Audio/Video（ASR）** | whisper.cpp HTTP server（`whisper-server`，OpenAI 兼容 API） | C++ sidecar | MIT | — | ✅ | **P2** | Ollama **不支持** ASR；whisper.cpp 自带 HTTP 服务，CPU+CUDA |

> **Go 生态强弱结论**（驱动 sidecar 边界）：强（无需 sidecar）= Markdown/HTML/CSV/JSON/xlsx/MHTML/图片解码/docx(自研)；弱（需 sidecar）= 版式 PDF/扫描件 OCR/PPTX(避开 AGPL)/ASR。

### 1.4 mora-parser sidecar 架构（多模态与复杂解析）

纯 Go 生态在"版式 PDF 表格抽取""OCR""VLM""ASR"上能力薄弱（核实结论见 §1.3 末），强行自研成本高、质量低。采用**单一可插拔 Python sidecar**（命名 `mora-parser`），统一承载这些能力，架构与 WeKnora 的 `docreader`（Python gRPC 服务）对齐但解耦：

```
┌───────────────┐   HTTP/gRPC    ┌──────────────────────────────────┐
│  rag-worker   │──────────────▶│  mora-parser (Python, FastAPI)   │
│ (Go, 解析编排)  │◀─────JSON─────│                                   │
└───────────────┘                │  routes:                         │
                                 │   POST /parse   {storage_key,    │
                                 │                   opts}          │
                                 │   POST /ocr     {image_key,lang} │
                                 │   POST /describe {image_key}      │
                                 │                                   │
                                 │  engines (按需启用):              │
                                 │   - markitdown / OpenDataLoader  │
                                 │     (版式 PDF + PPT)             │
                                 │   - PaddleOCR (CPU 默认, 布局+OCR) │
                                 │   - Ollama VLM (复用, 见 §3)      │
                                 │   - whisper.cpp server (ASR)     │
                                 └──────────────────────────────────┘
```

**为什么是单一 sidecar 而非多个**：减少部署面（一个镜像、一条健康检查、一份 GPU 调度）、统一权重缓存卷、统一可观测。**为什么 Python 而非 Go 重写**：上述生态原生 Python（OpenDataLoader 虽为 Java 但经 Python 包装消费），Go 重写质量与维护成本不划算；sidecar 隔离也避免了把重型依赖塞进 Go 主进程（镜像膨胀、CGO 传染）。

**OCR 引擎选型**（纯 Go OCR 不存在，必须 sidecar）：

| 引擎 | 语言/运行时 | License | GPU | 版式检测 | 适用 |
|---|---|---|---|---|---|
| **PaddleOCR**（PP-OCRv6） | Python(C++ 后端) | Apache-2.0 | CPU+GPU | ✅ PP-DocLayoutV3（文本/图/表区域+阅读序） | **默认**，自托管 CPU，100+ 语言 |
| surya | Python | Apache-2.0(代码)+modified-RAIL(权重) | GPU 强烈推荐 | ✅ 全版式 | 有 GPU 且需最高质量阅读序时 |
| ocrs | Rust | Apache-2.0/MIT | CPU 友好 | ⚠️ 仅词/行框，非全页版式 | 单图轻量 OCR |
| Tesseract | C++ | Apache-2.0 | CPU | ⚠️ v3 PSM，无表/图区域 | 传统默认，已被 PaddleOCR 取代 |

**决策**：默认 PaddleOCR（CPU，Apache-2.0）；有 GPU 用 surya；ocrs 仅极简单图 OCR 备选。

**部署形态**：
- CPU 模式：mora-parser 镜像含 CPU 版 PaddleOCR + whisper.cpp 客户端，与 rag-worker 同网部署。
- GPU 模式：`docker compose --profile gpu-parser up`，mora-parser 挂 GPU，rag-worker 通过 `MORA_PARSER_URL` 路由。
- mora-parser 启动时可从挂载的 `models/` 卷加载预置权重，也可按配置连接模型服务。

**MVP 边界**：P0/P1 不依赖 mora-parser（纯 Go 库即可覆盖文本 PDF/Word/Excel/PPT/EPUB/MHTML/CSV）；mora-parser 在 P2 才成为硬依赖，此时可独立迭代而不阻塞主线。

### 1.5 License 与合规

- **AGPLv3 风险**：`github.com/unidoc/unioffice`、`unipdf` 为 AGPLv3，对 Mora（私有化交付给客户自托管）**有传染性义务**——若纳入主进程需开源衍生作品。**决策：不直接依赖 AGPL 库进 Go 主进程**；PPT/复杂 PDF 解析改走 mora-parser sidecar，sidecar 内部若用 AGPL 库则仅该 sidecar 受约束（独立进程，不传染 Go 主程序），并在决策书中注明。这与 CLAUDE.md "fork 或引入开源项目时严格遵守 License 传染性义务"一致。
- **MIT/BSD/Apache**：纯 Go 库（pdfcpu/excelize/goldmark/goquery/go-epub 等）多为 MIT/BSD/Apache，无传染性，可直接进主进程。
- **TEI/Ollama 已有能力复用**：Embedding 已走 TEI/Ollama；VLM 复用 Ollama（LLaVA/MiniCPM-V/qwen-vl），不新增推理栈。**ASR 须独立**：Ollama 不支持音频模型（Whisper），P2 用 whisper.cpp HTTP server（MIT，自带 `whisper-server`，CPU+CUDA）。

### 1.6 与 WeKnora 的对齐与差异

> 经核实 WeKnora 源码（`Tencent/WeKnora/docreader/`）：其解析层为 **Python gRPC 服务**（非 Go），依赖 `opendataloader-pdf`（底层为 **Java** PDF 解析器，Apache-2.0，经 PyPI 包装、`convert()` 时 spawn JVM）、`markitdown[docx,pdf,xls,xlsx]`（微软，MIT）、`python-docx`/`openpyxl`/`xlrd`/`ebooklib`(EPUB)/`trafilatura`(HTML)/`pypdfium2`(PDF 渲染)/`textract`(PPT)/`playwright`(JS 渲染)。扫描页由 `pdf_parser.py` 用 `pypdfium2` 渲染为 JPEG（全局锁串行，pdfium 非线程安全）后交给 **Go 侧 OCR/VLM 服务**；`image_parser.py` 返回 base64 图引用由 Go 侧调用 OCR/VLM 后端。`PaddleOCR-VL` 并非独立 repo（`PaddlePaddle/PaddleOCR-VL` 404），是 PaddleOCR 项目下的 VLM 权重，经社区 CPU sidecar 暴露。可选 `MINERU_ENDPOINT` sidecar 做进阶文档结构。

| WeKnora 能力 | WeKnora 实现 | Mora 对应 | 差异说明 |
|---|---|---|---|
| 多格式解析 | docreader（Python gRPC：OpenDataLoader+markitdown+多库） | Go 纯库 + mora-parser sidecar | Mora Go 核心保持纯 Go，仅复杂格式走 sidecar；不引入其整套 Python 框架 |
| 版式 PDF | OpenDataLoader-PDF（Java，PyPI 包装） | mora-parser（markitdown/OpenDataLoader 可选） | 等价能力，sidecar 隔离 JVM |
| 扫描件/图像 OCR | PaddleOCR-VL（PaddleOCR 权重） | mora-parser（PaddleOCR CPU 默认/surya GPU） | 默认 CPU PaddleOCR；GPU 选 surya |
| VLM 图像描述 | Go 侧调 OCR/VLM 后端 | Ollama VLM（LLaVA/MiniCPM-V） | 复用 Mora 已有 Ollama，不新增 OCR/VLM 服务 |
| ASR | （WeKnora 未内置 ASR） | whisper.cpp HTTP server（P2） | Mora 扩展能力，whisper.cpp 自带 HTTP |
| 自适应三层分块 | adaptive 3-tier | `adaptive_3tier` 策略（§2.2） | 落地到现有 chunker |
| parent-child 分块 | parent-child | `parent_child` 策略（§2.3） | 落地到现有 chunker + chunks 表 |
| 图抽取/问答生成 | per-batch | P2，per-upload opts 开关 | 见 §7 |

**结论**：Mora 不照搬 WeKnora 的 Python 全栈解析层，而是用"Go 纯库为主 + 单 Python sidecar 补复杂格式与多模态"的等价能力、更低耦合方案，且复用已有 Ollama 做 VLM、whisper.cpp 做 ASR。这与 CLAUDE.md "是否 fork 或引入开源基座需决策书说明理由"一致——不 fork WeKnora，独立实现并注明 License 合规。

---

## 2. 分块策略

### 2.1 现状与衔接点

现有分块（05 §3.3 / `internal/module/rag/pipeline/chunk.go`）：
- 固定长度（默认 512 token）+ 重叠（64）+ 标题边界（`#`/`##`/`###`）+ 句子对齐。
- 输出 `ChunkRef{Text, ChunkIndex, SectionPath, TokenCount}`，由 `Pipeline.handleIndex` 写入 Qdrant point（`uuid5(doc+version+chunk_index)`）+ PG `chunks` 表。

**衔接原则**：新增策略产出同样的 `ChunkRef` 流（或扩展结构），写入路径（Qdrant upsert + chunks 表 + `visible_to` payload）完全复用，**不破坏既有 RAG 能力**。仅 `chunkSection`/`splitSections` 算法可替换，`Chunk()` 入口按 `cfg.Strategy` 分发。

### 2.2 自适应三层分块（adaptive 3-tier）

**目标**：根据文档结构自适应——小节整体成块、大节拆分、超大段硬切，避免一刀切。

```
Tier 1 (section):  以标题为边界形成 section（h1-h3）
Tier 2 (merge):    连续小 section 合并至 ~chunk_size 下限（避免碎片 chunk）
Tier 3 (split):    超大 section 按句/词硬切（保留 overlap）
```

与现有固定策略的差异仅在前处理：Tier 2 的"合并"是新增步骤。落地：

```go
// internal/module/rag/pipeline/chunk.go
type Strategy string
const (
    StrategyFixed        Strategy = "fixed"          // 现状默认
    StrategyAdaptive3Tier Strategy = "adaptive_3tier" // 新增
    StrategyParentChild  Strategy = "parent_child"    // 新增
)

func Chunk(text string, cfg Config, tok Tokenizer) []ChunkRef {
    switch cfg.Strategy {
    case StrategyAdaptive3Tier:
        return chunkAdaptive3Tier(text, cfg, tok)
    case StrategyParentChild:
        return chunkParentChild(text, cfg, tok)
    default:
        return chunkFixed(text, cfg, tok) // 现有逻辑改名
    }
}
```

`Config` 增加 `Strategy Strategy` 字段（默认 `fixed`，保持现状）。`adaptive_3tier` 与 `fixed` 共享 `chunkSection`，只在 sections 归并阶段加一层 merge：相邻 section token 之和 ≤ chunk_size 且各自 < chunk_size/2 时合并为一个 chunk，`section_path` 取并集（"3.1 > 3.2"）。

**预览**（PRD/WeKnora "实时预览"）：新增 `POST /rag/chunk-preview`（见 §6 API），输入文本+策略，返回 `[]ChunkRef`，不落库、不向量化。供上传确认弹窗使用。

### 2.3 parent-child 分块

**目标**：父子块关联——父块（大粒度，如一整节）用于上下文召回，子块（小粒度）用于精准匹配；检索命中子块时可回溯父块补全上下文。

数据模型扩展（不改现有 chunks 表结构，仅用 `metadata` JSONB 增字段，向后兼容）：

```jsonc
// chunks.metadata (Qdrant payload 同步)
{
  "chunk_role": "child",            // parent / child / standalone
  "parent_chunk_id": "uuid",        // child 指向 parent；parent 为 null
  "parent_chunk_index": 3,
  "window": "..."                   // parent 自身文本（冗余存于 parent chunk，便于回溯）
}
```

- **parent 块**：整节文本（可能 >> chunk_size），向量化后作为"上下文召回"候选。
- **child 块**：父块内按 chunk_size 切分的细块，作为"精准匹配"候选。
- **检索**：Dense/BM25 命中 child → 按 `parent_chunk_id` 聚合 → 返回 child 片段 + parent 上下文（与 05 §6 检索结果 `section_path` 互补）。
- **point_id**：parent 与 child 各自独立 point（`uuid5(doc+version+chunk_index)`），child 的 `chunk_index` 在父之后续编，不冲突。

**兼容性**：`chunk_role` 缺失视为 `standalone`（现状）。parent-child 为可选策略，默认不开，对存量数据零影响。

### 2.4 与 Valkey Streams / Qdrant / BM25 的衔接

| 衔接点 | 现状 | 本设计 | 改动面 |
|---|---|---|---|
| 事件投递 | `document.create/update` 携带 version_no | 不变；解析产物写入 `documents.content/content_text` 后照常投递 | 无 |
| rag-worker 消费 | `ExtractText(snap.Content)` → `Chunk()` | 解析在 worker 内作为 `handleIndex` 前置步骤（见 §4）；`Chunk()` 按 strategy 分发 | `pipeline.go` 增加 parser 调用 + strategy 分发 |
| Qdrant payload | `visible_to`/`section_path` | 新增 `chunk_role`/`parent_chunk_id`（可选） | payload 扩展，向后兼容 |
| BM25 (PG FTS) | `documents.content_text` | 解析产出的 `content_text` 直接写入，GIN 索引不变 | 无 |
| 模型切换/重建 | `EventModelRebuild` | 不变 | 无 |

---

## 3. 多模态处理（VLM / ASR / OCR）

### 3.1 能力矩阵

| 能力 | 触发 | 实现 | 复用 | 依赖 | 优先级 |
|---|---|---|---|---|---|
| **OCR**（扫描 PDF/图片→文本） | `enable_ocr=true` 且文档为扫描件/图片 | mora-parser `/ocr`（PaddleOCR CPU 默认 / surya GPU / ocrs 单图） | — | mora-parser 镜像 | P2 |
| **VLM 图像描述** | `enable_vlm=true` 且 block 为 image | Ollama `/api/generate`（LLaVA/MiniCPM-V/qwen-vl） | **复用 Ollama** | Ollama + VLM 权重 | P2 |
| **ASR 语音转写** | `enable_asr=true` 且附件为音视频 | whisper.cpp `/asr`（自带 `whisper-server`，OpenAI 兼容 HTTP） | — | whisper.cpp 镜像 + Whisper 权重 | P2 |
| **图抽取** | `enable_graph=true` | 后置：用 LLM 从 chunks 抽实体关系，存 PG（图域表，P2 详设） | 复用 Ollama/TEI | — | P2 |
| **问答生成** | `enable_qagen=true` | 后置：用 LLM 从 chunks 生成 Q-A 对，作为额外 chunk 入库供 FAQ 检索 | 复用 Ollama | — | P2 |

### 3.2 集成流程

```
解析产出 ParsedDocument（含 ParsedAsset[]）
   │
   ├─ image asset ──▶ (enable_vlm) ──▶ Ollama VLM ──▶ 描述文本 ──▶ 作为该 image block 的 alt/caption（参与分块向量化）
   ├─ image asset ──▶ (enable_ocr) ──▶ mora-parser /ocr ──▶ 文本 ──▶ 作为 paragraph block
   └─ media asset ──▶ (enable_asr) ──▶ whisper.cpp /asr ──▶ 转写文本 ──▶ 作为 paragraph block
```

**降级**（沿用 05 §5.4 原则）：
- VLM 不可用 → image block 的描述留空，图不参与向量化（不阻塞流水线，事件不丢）。
- ASR/OCR 不可用 → 该 asset 跳过，文档其余部分正常索引；`ParsedMeta.Warnings` 记录。
- 多模态失败不计入主任务 `max_attempt` 失败计数（与 Embedding 失败语义一致：可降级则降级，不 dead-letter）。

### 3.3 存储与可见性

- 解析出的 asset（图片/音视频）与原附件同等对待：存对象存储，`ParsedAsset.StorageKey` 引用，RBAC 继承所属文档。
- VLM 描述/OCR 文本/ASR 转写进入 Block JSONB → 与文档正文同生命周期、同权限边界，**无独立可见性问题**。

---

## 4. 解析管线与存储

### 4.1 端到端流程

```
用户上传文件 (POST /workspaces/{ws}/documents/upload 或 /import)
   │  multipart
   ▼
mora-api: 鉴权(RBAC: workspace write) → 落 MinIO(对象存储) → 创建 documents 行
   │   (content=占位 blocks, content_text='', status=draft, index_status=pending,
   │    parse_status=pending, storage_key=...)
   ▼
mora-api: 投递 doc_events {event_type: document.parse, document_id, storage_key, parse_opts}
   ▼
Valkey Streams (doc_events)
   ▼
rag-worker 消费:
   1. 幂等去重 + 创建/读取 ParseTask(状态机, 见 §4.3)
   2. 解析: Parser.Parse(ctx, storage_key, opts) → ParsedDocument
      - 纯文本格式: 内联 Go parser
      - 富文档/多模态: mora-parser sidecar (按 opts)
   3. 回写 documents.content (Block JSONB) + content_text (FTS)
   4. 状态机 parse_status → parsed
   5. 投递 doc_events {event_type: document.create, document_id, version_no=1}
      ▶ 进入既有 RAG 流水线(05 §3): 抽取→分块→向量化→写 Qdrant→index_status=indexed
   6. ACK parse 事件
```

**关键衔接**：解析是 `document.parse` 事件，向量化是后续 `document.create` 事件——**两段解耦**。解析失败不影响既有文档向量化；向量化失败不影响解析产物落库。这与 05 "事件驱动、读写隔离"一致。

### 4.2 解析中间产物与最终分块的存储模型

#### 4.2.1 对象存储（MinIO，复用现有 `attachments` 的 storage_type='minio' 约定）

| 对象 | Key 规则 | 生命周期 |
|---|---|---|
| 上传原文件 | `mora/{workspace_id}/{document_id}/source/{filename}` | 与文档同（文档删则级联删） |
| 解析中间产物（OCR/VLM/ASR 临时大文本） | `mora/{workspace_id}/{document_id}/parsed/` | 解析完成即清理（仅失败重试保留） |
| 提取的 asset（图片/音视频） | `mora/{workspace_id}/{document_id}/assets/{block_id}` | 与文档同（block 引用） |

> **注**：现 `docker-compose.yml` 未包含 MinIO 服务（config 已有 `MinioEndpoint/AccessKey/SecretKey` 字段）。**部署影响（§8）**：需在 compose/Helm 补 MinIO 服务，或复用客户既有对象存储。MVP 可先用本地卷挂载模拟，但生产必须对象存储。

#### 4.2.2 PostgreSQL 表结构（新增迁移 011_parse）

```sql
-- migrations/011_parse.up.sql
-- 文档解析域：解析任务状态机、解析配置、解析进度时间线

-- documents 表新增列（ALTER，向后兼容）
ALTER TABLE documents
    ADD COLUMN IF NOT EXISTS storage_key   TEXT,              -- 上传原文件对象存储 key
    ADD COLUMN IF NOT EXISTS source_format VARCHAR(20),      -- 上传文件格式: pdf/docx/...
    ADD COLUMN IF NOT EXISTS parse_status  VARCHAR(20) NOT NULL DEFAULT 'parsed', -- pending/parsing/parsed/failed/skipped
    ADD COLUMN IF NOT EXISTS parse_task_id UUID;             -- 关联当前 parse_tasks.id

-- 解析任务状态机（类比 indexing_tasks）
CREATE TABLE IF NOT EXISTS parse_tasks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    document_id   UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    event_id      UUID NOT NULL,                            -- 幂等键
    status        VARCHAR(20) NOT NULL DEFAULT 'pending',   -- pending/parsing/parsed/failed
    attempt       INTEGER NOT NULL DEFAULT 0,
    max_attempt   INTEGER NOT NULL DEFAULT 3,
    parse_opts    JSONB NOT NULL DEFAULT '{}',              -- 本次解析配置覆盖（§7）
    parser_name   VARCHAR(100),                             -- 实际使用的 parser
    progress      JSONB NOT NULL DEFAULT '[]',              -- 分阶段进度时间线（§6）
    error_message TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(document_id, event_id)
);
CREATE INDEX IF NOT EXISTS idx_parse_tasks_status ON parse_tasks(status, updated_at);
CREATE INDEX IF NOT EXISTS idx_parse_tasks_document ON parse_tasks(document_id);
CREATE INDEX IF NOT EXISTS idx_parse_tasks_pending ON parse_tasks(status, created_at) WHERE status IN ('pending','failed');

-- 解析配置模板（工作区级默认 + 全局默认，供上传时回填）
CREATE TABLE IF NOT EXISTS parse_configs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  UUID REFERENCES workspaces(id) ON DELETE CASCADE,  -- NULL=全局默认
    name          VARCHAR(100) NOT NULL,
    config        JSONB NOT NULL DEFAULT '{}',              -- {chunking_strategy, chunk_size, ...}
    is_default    BOOLEAN NOT NULL DEFAULT false,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(workspace_id, name)
);

-- parent-child 分块的父子关系（独立表，避免 chunks 表膨胀；P1）
CREATE TABLE IF NOT EXISTS chunk_relations (
    child_chunk_id   UUID NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
    parent_chunk_id  UUID NOT NULL REFERENCES chunks(id) ON DELETE CASCADE,
    document_id      UUID NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    PRIMARY KEY (child_chunk_id, parent_chunk_id)
);
CREATE INDEX IF NOT EXISTS idx_chunk_relations_parent ON chunk_relations(parent_chunk_id);
CREATE INDEX IF NOT EXISTS idx_chunk_relations_doc ON chunk_relations(document_id);
```

- **向后兼容**：`parse_status` 默认 `'parsed'`，存量文档（已是 Block JSONB）视为已解析，不触发解析。
- `parse_opts` JSONB 记录本次覆盖配置，`progress` JSONB 记录分阶段时间线（见 §6.3）。

### 4.3 解析状态机

```
pending → parsing → parsed
              └→ failed ──(retry ×3, 指数退避)──→ parsing
                            └(max_attempt)→ dead(告警, parse_status=failed)
```

与 `indexing_tasks` 状态机对称（05 §3.1），同样支持：幂等（`event_id`）、重试退避、死信（`doc_events:parse_dead`）、补偿扫描器扫 `parse_tasks(status=pending, updated_at<now-5min)` 重投。

**两段状态机衔接**：`parse_status=parsed` 后投递 `document.create` → `index_status` 走 `pending→processing→indexed/failed`。文档徽标同时展示两个状态（解析中/已解析 + 索引中/已索引）。

---

## 5. 批量重解析机制

### 5.1 需求

以**新配置**（如换分块策略、启用 VLM）对**多文档**重新入队解析。

### 5.2 队列与状态机设计

- 新增事件 `document.reparse`，payload：`{document_ids: [uuid...], new_opts: {...}, scope: workspace|directory|selection}`。
- mora-api 收到批量重解析请求 → 校验调用者对**每个目标文档**有 `write/admin` 权限（RBAC 硬约束，存在性不泄露：无权文档不在入队列表、不返回计数）→ 为每个文档生成 `document.reparse` 事件入 `doc_events`（按文档拆分，独立幂等、独立重试、独立进度）。
- rag-worker 消费 `document.reparse`：
  1. 旧解析产物清理：删 `documents.content`/`content_text` 中该版本解析块、删 `chunks`/Qdrant（级联，复用 `handleDelete` 的 `DeleteByDocumentVersion`）。
  2. 新建 `parse_task`（`parse_opts=new_opts`）→ 走 §4 解析流程。
  3. 解析完成投递 `document.create` → 重新向量化。
- **批量编排**：单事件/单文档粒度，消费组天然并行负载均衡，无需额外批调度器。管理后台可按 `document_id` 批量查进度（§6.3）。

### 5.3 一致性

- 重解析期间文档 `parse_status=parsing`、`index_status=processing`，检索不可见新版本（旧 chunk 已删、新 chunk 未写时该文档在检索中暂缺——与 05 §3.5 update "先删旧版本再写新版本"语义一致，过渡期短暂不可见，非数据损坏）。
- 失败回滚：解析失败则 `parse_status=failed`，文档 `index_status=failed`，徽标显示失败，管理后台可重试单文档。

---

## 6. 解析进度追踪

### 6.1 分阶段进度

解析进度以**阶段**粒度上报，而非逐 chunk（chunk 粒度过细、噪声大）。阶段：

```
queued → fetching → parsing → (ocr|vlm|asr, 可选) → chunking → persisting → done
```

每阶段记录 `{stage, status: started|done|failed, at, duration_ms, detail}`。

### 6.2 上报机制

- rag-worker 在 `ParseTask.progress` JSONB 数组追加阶段记录（同事务更新 `parse_tasks`）。
- 进度变更通过 SSE 推前端（复用 05 §3.6 索引就绪回执的 SSE 通道，新增 `parse_progress` 事件类型）。

### 6.3 查询接口

```
GET /documents/{id}/parse-progress
  → 200 { data: { parse_status, index_status, progress: [...], updated_at } }
```

- 权限：调用者须对文档有 `read+`（与查看文档同级），无权返回 404（存在性不泄露）。
- 批量：`GET /workspaces/{ws}/documents/parse-progress?directory_id=...&status=parsing` 分页返回在解析中的文档进度。

---

## 7. 配置覆盖

### 7.1 数据模型

配置分层（优先级高→低）：**per-upload opts** > **workspace `parse_configs`** > **全局 `parse_configs`(workspace_id IS NULL)** > **代码默认**。

`ParseOptions`（§1.2）序列化为 JSONB，存 `parse_tasks.parse_opts`。`parse_configs` 表存可复用模板，上传确认弹窗/ API 可引用模板 id 或内联覆盖。

### 7.2 API 草案（新增，RESTful，遵循 04-api-contract.md 风格）

```yaml
# 解析配置模板
  /workspaces/{workspace_id}/parse-configs:
    get:
      tags: [Parse]
      summary: 列出工作区解析配置模板
      security: [BearerAuth: []]
      responses:
        '200': { description: 模板列表 }
    post:
      tags: [Parse]
      summary: 创建解析配置模板
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                name: { type: string }
                config: { $ref: '#/components/schemas/ParseOptions' }
                is_default: { type: boolean, default: false }
      responses:
        '201': { description: 创建 }

  /workspaces/{workspace_id}/parse-configs/{config_id}:
    put:
      tags: [Parse]
      summary: 更新模板
      security: [BearerAuth: []]
      responses:
        '200': { description: 更新 }
    delete:
      tags: [Parse]
      summary: 删除模板
      security: [BearerAuth: []]
      responses:
        '204': { description: 删除 }

# 文档上传（含解析配置覆盖）—— 扩展 04 §12 导入域
  /workspaces/{workspace_id}/documents/upload:
    post:
      tags: [Parse]
      summary: 上传文档并解析（multipart）
      security: [BearerAuth: []]
      parameters:
        - { name: workspace_id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        content:
          multipart/form-data:
            schema:
              type: object
              properties:
                file: { type: string, format: binary }
                directory_id: { type: string, format: uuid }
                title: { type: string }
                parse_config_id: { type: string, format: uuid, description: 引用模板 }
                parse_options: { type: string, description: 内联 JSON 覆盖（优先于模板） }
                conflict_strategy: { type: string, enum: [overwrite, skip, append], default: append }
      responses:
        '202':
          description: 已入队解析
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      document_id: { type: string, format: uuid }
                      parse_task_id: { type: string, format: uuid }
                      parse_status: { type: string, example: "pending" }

# 分块预览
  /rag/chunk-preview:
    post:
      tags: [Parse]
      summary: 预览分块（不落库）
      security: [BearerAuth: []]
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                text: { type: string }
                parse_options: { $ref: '#/components/schemas/ParseOptions' }
      responses:
        '200':
          description: 预览结果
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      chunks: { type: array, items: { $ref: '#/components/schemas/ChunkRef' } }
                      strategy: { type: string }
                      total: { type: integer }

# 批量重解析
  /workspaces/{workspace_id}/documents/reparse:
    post:
      tags: [Parse]
      summary: 批量重解析
      security: [BearerAuth: []]
      parameters:
        - { name: workspace_id, in: path, required: true, schema: { type: string, format: uuid } }
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                document_ids: { type: array, items: { type: string, format: uuid } }
                scope: { type: string, enum: [selection, directory, workspace] }
                directory_id: { type: string, format: uuid }
                parse_options: { $ref: '#/components/schemas/ParseOptions' }
      responses:
        '202':
          description: 已入队
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      enqueued: { type: integer, description: 实际入队数（无权文档不计入） }
                      task_ids: { type: array, items: { type: string, format: uuid } }

# 解析进度
  /documents/{id}/parse-progress:
    get:
      tags: [Parse]
      summary: 查询解析进度
      security: [BearerAuth: []]
      parameters:
        - { name: id, in: path, required: true, schema: { type: string, format: uuid } }
      responses:
        '200':
          description: 进度
          content:
            application/json:
              schema:
                type: object
                properties:
                  data:
                    type: object
                    properties:
                      parse_status: { type: string }
                      index_status: { type: string }
                      progress: { type: array }
                      updated_at: { type: string, format: date-time }
```

### 7.3 限流（对齐 04-api-contract.md §附件上传 60 req/min）

- 上传：沿用 60 req/min。
- 分块预览：120 req/min（轻量、不落库）。
- 批量重解析：10 req/min，单次最多 500 文档（防过载）。

---

## 8. 权限约束（RBAC 硬约束）

贯穿全流程，沿用 05 §4.3 + CLAUDE.md "显式优先、存在性不泄露"：

| 阶段 | 权限校验 | 失败行为 |
|---|---|---|
| 上传 | workspace `write+` | 403 |
| 解析任务入队 | 对目标文档 `write+`（批量重解析逐文档校验） | 无权文档不入队、不返回（存在性不泄露） |
| 解析执行 | rag-worker 以系统身份运行，不受用户权限二次约束（任务已鉴权） | — |
| `visible_to` 计算 | 解析产出的 chunk 沿用文档当前权限快照，写入 Qdrant payload | 无权主体不在 `visible_to` |
| 进度查询 | 文档 `read+` | 无权返回 404 |
| 重解析结果 | 受检索时 `visible_to` 过滤（与现有检索一致） | 无权 chunk 不返回、不计 total |

**无权文档不可解析**：上传/重解析 API 在 mora-api 层先做 RBAC，rag-worker 只消费已鉴权任务。解析本身不产生新的可见性面——解析产物即文档内容，继承文档全部权限边界。

---

## 9. 部署影响

### 9.1 docker-compose / Helm 变更

| 组件 | 变更 | 优先级 |
|---|---|---|
| **MinIO** | compose 与 Helm 补 MinIO 服务（现 config 已有字段但无服务）；或文档化"接入客户既有 S3 兼容存储" | **P0 阻塞**（上传依赖对象存储） |
| **mora-parser** | 新增 Python sidecar 服务，`--profile parser`；CPU 镜像含 PaddleOCR + markitdown；GPU profile 含 surya | P2（多模态/版式启用时） |
| **whisper.cpp** | 新增 ASR sidecar（自带 `whisper-server` HTTP），`--profile asr`；CPU/CUDA | P2（ASR 启用时） |
| **Ollama** | 复用现有 profile；P2 需预拉 VLM 权重（`ollama pull minicpm-v`） | P2 |
| **postgres** | 新增迁移 011_parse（documents 加列 + parse_tasks/parse_configs/chunk_relations 表） | P0/P1 |
| **rag-worker** | 增加 `MORA_PARSER_URL`、`PARSE_*` 配置；消费 `document.parse`/`document.reparse` 事件 | P0 |
| **mora-api** | 新增 upload/parse-progress/reparse/chunk-preview 路由 + MinIO client 接入 | P0 |
| **资源** | mora-parser CPU 镜像 ~1-2GB；GPU 镜像 + 权重 ~5-10GB（按需） | P2 |

### 9.2 配置新增（env / config.go）

```
MORA_PARSER_URL=http://mora-parser:8000   # mora-parser sidecar（版式/OCR/VLM）；空=禁用复杂解析(纯Go)
WHISPER_URL=http://whisper:8080            # whisper.cpp server；空=禁用 ASR
PARSE_MAX_FILE_MB=100                      # 单文件上限
PARSE_OCR_DEFAULT_LANG=chi_sim+eng
PARSE_VLM_MODEL=minicpm-v                  # Ollama VLM 模型 id
PARSE_TASK_MAX_ATTEMPT=3
PARSE_DEAD_LETTER_STREAM=doc_events:parse_dead
```

### 9.3 可观测（沿用 07-security-observability.md）

- Prometheus 指标新增：`rag_parse_duration_seconds{format,parser}`、`rag_parse_tasks{status}`、`rag_parse_dead_letter_total`、`mora_parser_call_duration_seconds{route}`。
- 审计：`document.upload`、`document.reparse` 记入 `audit_logs`（actor/action/target）。

---

## 10. 实现优先级（与 PRD P0/P1 对齐）

| 阶段 | 范围 | 交付物 | 阻塞 |
|---|---|---|---|
| **P0** | Txt/MD/HTML/JSON 解析（纯 Go）+ PDF/Word 文本层解析 + 复用现有 fixed 分块 + MinIO 接入 + upload/parse-progress API + parse_tasks 状态机 + 迁移 011（documents 加列 + parse_tasks） | 端到端：上传→解析→向量化→可检索 | MinIO 服务 |
| **P1** | Excel/PPT/EPUB/MHTML/CSV 解析 + adaptive_3tier + parent_child 策略 + chunk-preview API + 批量重解析 + parse_configs 配置覆盖 + chunk_relations 表 + 迁移 011（完整） | 多策略分块 + 批量重解析 | P0 |
| **P2** | mora-parser sidecar（OCR/VLM/ASR）+ 图抽取 + 问答生成 + GPU profile | 多模态能力 | P1 |

**P0 与现有 RAG 衔接点**（不破坏既有能力）：解析层产出 `documents.content`（Block JSONB）后投递 `document.create`，05 §3 流水线原样运行。分块入口 `Chunk()` 按 strategy 分发，默认 `fixed` 保持现状。存量文档 `parse_status=parsed`，不触发解析。

---

## 11. 风险与缓解

| 风险 | 影响 | 缓解 |
|---|---|---|
| MinIO 在 compose 缺失 | 上传无对象存储，P0 阻塞 | §9.1 补 MinIO 服务或对接客户既有 S3 |
| 纯 Go PDF 库对版式/表格弱 | 富版式 PDF 解析质量低 | P0 先满足文本层；版式走 mora-parser(P1/P2) |
| PPT 纯 Go 库多为 AGPL | License 传染 | 不进主进程；走 sidecar，隔离 AGPL 义务 |
| 解析与向量化两段状态机复杂度 | 调试链路长 | 双状态机对称设计、统一 SSE 进度、统一死信；补偿扫描器复用 |
| mora-parser 依赖 Python 生态 | 运维面增加 | 单一镜像、单一健康检查、默认按需启用(P2 才需要)、权重可预置或按配置获取 |
| 重解析过渡期文档暂不可见 | 检索体验波动 | 沿用 update 先删后写语义；过渡期 < 解析时长，徽标明示 |
| 大文件/超大文档 | worker 内存/超时 | 分页流式解析、`PARSE_MAX_FILE_MB`、大文档复用 05 §3.3.4 分批 |

---

## 12. 交付说明

本文档覆盖任务要求的 8 项：§1 解析引擎选型（含每格式方案与依赖、WeKnora 对齐、License 合规）、§2 分块策略（adaptive 3-tier / parent-child 落地与衔接）、§3 多模态（VLM/ASR/OCR，TEI/Ollama 复用评估）、§4 解析管线与存储（端到端流程、PG 表结构、对象存储）、§5 批量重解析（队列与状态机）、§6 解析进度追踪（阶段化上报与查询接口）、§7 配置覆盖（数据模型与 API 草案）、§8 权限约束（RBAC 硬约束、存在性不泄露）。

与现有 RAG 管线衔接点已在 §2.4、§4.1 明确：解析层作为 `document.parse` 事件的前置，产出 `documents.content/content_text` 后照常进入 `document.create` → 现有 05 §3 流水线，既有能力（分块/向量化/混合检索/RBAC/级联删除）零改动复用。

实现优先级（§10）与 PRD P0/P1 对齐：P0 以纯 Go 库 + MinIO 打通主线，P1 补全格式与分块策略，P2 引入多模态 sidecar。

本文档可作为 design-docs/ 第 10 篇（`10-document-parsing-design.md`）沉淀，供后端研发直接拆解为 P0/P1/P2 任务。如产品范围或配置项需与产品经理进一步对齐，建议聚焦：① per-upload 配置项的 MVP 子集（P0 是否暴露 `parse_options` 还是固定默认）；② 批量重解析的 UI 入口范围（selection vs workspace 全量）；③ 多模态 P2 的默认启用项。
