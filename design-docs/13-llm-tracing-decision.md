# LLM 追踪技术选型决策书（OTel LLM span vs Langfuse）

> 文档版本：v1.0 ｜ 产出人：Mora项目架构师 ｜ 对应任务：YS-70《分析 WeKnora 功能并制定 Mora 整体规划》§1.6 / 阶段二 P1
> 父决策：01-tech-selection-decision.md（可观测栈：Prometheus + Grafana + Loki + OpenTelemetry）、07-security-observability.md §7、12-llm-provider-react-decision.md（#3，`LLMProvider.Chat`/`TokenUsage` 已预留接入点）
> 产品基线：YS-70 PM 初版规划 §1.6「可观测性追踪」（LLM/Agent 推理维度与现有 OTel 正交叠加，不替代 Prometheus/Grafana）
> 评审状态：**草案 v1.0**——推荐 + 依赖成本对比 + OTel SDK 前置核实

---

## 0. 结论先行

**推荐：纯 OTel LLM span（GenAI 语义约定）优先，Langfuse 列可选增强，二者不互斥、决策可逆。**

| 维度 | 纯 OTel LLM span | 引入 Langfuse |
|---|---|---|
| 依赖成本 | OTel SDK 本就要接入 Go 服务（见 §3 前置），加 `gen_ai.*` 语义属性即可 | +1 compose 服务 +1 DB（PG/ClickHouse）+ SDK + traceparent 桥接 |
| 栈一致性 | 复用现有 OTel Collector → Jaeger/Tempo（`02:547`、`07 §7.3`） | 第二套追踪后端，与现有 Prometheus/Grafana/Loki 栈并存 |
| 数据可得性 | token/步/工具链均可作 span，Tempo/Grafana 可按 trace 检索 | 同数据 + 开箱即用的 prompt/eval UI |
| 不出网 | 本地 Collector，天然不出网 | 自托管 Langfuse 亦可，但多一个运维面 |
| 可逆性 | 后续可把 Langfuse 作为**额外 OTLP exporter** 按需叠加（Langfuse 接受 OTLP） | 反向去掉则需改导出配置 |

**关键事实**：`internal/platform/observ` 目前**只实现 Prometheus 指标**（`metrics.go` 全 `promauto`），`go.mod` **无 OTel/trace/langfuse 依赖**，`config.go` **无 OTel/trace 配置键**，全树**无 `trace_id`/span/otel 任何用法**。即 **OTel 在 Mora 是「设计已定、代码未接」**（`07 §7.2-7.3` 设计了 trace_id 贯穿 + OTel Collector → Jaeger/Tempo，但 Go 服务未接 SDK）。故 #4 的「纯 OTel LLM span」路径**需先补 OTel SDK 接入作为前置**（见 §3），这是 #4 的真正成本所在。

---

## 1. 决策背景与范围

### 1.1 背景

PM §1.6：WeKnora 用 Langfuse 作**唯一**追踪后端（v0.6.2 移除 Jaeger），覆盖 ReAct 循环/token 计量/工具调用/pipeline 追踪；Mora 现有可观测是「基础设施维度」（HTTP/RAG/MCP/存储，Prometheus + 结构化日志 + OTel 设计），**缺口在 LLM/Agent 推理维度**。两者不冲突而互补，PM 请架构师评估「引入 Langfuse vs 纯 OTel LLM span」。

### 1.2 P1 范围

- LLM 调用 span（token/延迟/model）纳入 OTel；
- Agent 单轮起草的 trace 链路（search→get_document→create_draft 各步 span）；
- RAG pipeline stage-by-stage 进度时间线（借鉴 WeKnora）作为 Grafana 增强。

### 1.3 决策原则（继承 01 §1.2）

私有化优先、栈一致性（不另起追踪后端）、复用既有基建、可逆决策（Langfuse 留作未来 OTLP exporter 叠加）。

---

## 2. 候选方案评估

### 2.1 纯 OTel LLM span（GenAI 语义约定）

- **机制**：OTel Go SDK 在 `go.opentelemetry.io/otel`，**有专用 `genaiconv` 包**（`go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv`，提供 `gen_ai.*` 命名空间属性）。在 `OllamaLLMProvider.Chat`（#3 §3）外层包一个 span，属性写 `gen_ai.system="ollama"`、`gen_ai.request.model`、`gen_ai.usage.input_tokens`/`output_tokens`（取 #3 `ChatResult.Usage`）。
- **License**：OTel Go SDK Apache-2.0，无传染。
- **接入点**：`provider.Chat` 调用点（类比现有 `prov.Embed` 在 `pipeline.go:252`/`search.go:115`），span 在 Provider 实现内或调用方包一层。

### 2.2 引入 Langfuse

- **机制**：Langfuse 自托管（docker compose +1 服务 +1 DB），Go SDK（`langfuse-go`，MIT）手动埋点，或走 OTLP exporter（Langfuse 接受 OTLP）。
- **License**：Langfuse 核心 MIT（自托管）。
- **独有价值**：prompt 管理/版本、评测（eval）UI、开箱即用的 LLM 调用看板。

### 2.3 对比

| 维度 | 纯 OTel LLM span | Langfuse |
|---|---|---|
| 依赖 | OTel SDK（前置必接，非额外） + `genaiconv`（SDK 内置） | +Langfuse 服务 + DB + SDK/OTLP |
| 栈一致 | ✅ 复用现有设计栈 | ⚠️ 第二套追踪后端 |
| token/步/工具链 | ✅ span 属性 | ✅ + prompt/eval UI |
| 不出网 | ✅ 本地 Collector | ✅ 自托管（多运维面） |
| License | Apache-2.0 | MIT |
| 工作量 | OTel SDK 接入 + gen_ai span 埋点 | +1 服务部署 + SDK 埋点 + traceparent 桥接 |

**结论**：先做纯 OTel LLM span。OTel SDK 接入是**反正要做的前置**（设计已定未接），纯 OTel 相对成本反而更低；Langfuse 的独有价值在 prompt 管理/评测，可作未来**额外 OTLP exporter** 按需叠加（Langfuse 接受 OTLP，无需改埋点），决策可逆、不互斥。

---

## 3. OTel SDK 接入前置（响应 PM「核实 `internal/platform/observ` 现状」）

### 3.1 现状核实（代码依据）

- `internal/platform/observ/` **仅有 `metrics.go`**，全 `prometheus`/`promauto`，无 trace/span。
- `go.mod` **无** `go.opentelemetry.io/otel` 或任何 trace 依赖。
- `config.go` **无** `OTEL_*`/trace 配置键。
- 全树 `internal/`/`cmd/` **无** `trace_id`/`span`/`otel`/`Tracer` 任何用法。
- `design-docs/07 §7.2` 设计了日志含 `trace_id`/`span_id`，`§7.3` 设计了 OTel SDK → Collector → Jaeger/Tempo，`02:547` 有 `OTEL_EXPORTER_OTLP_ENDPOINT`，但 `deployments/` **无 otel-collector 服务定义**。

**结论**：OTel 在 Mora 是「设计已定、代码与部署均未接」。**#4 的真正前置是先把 OTel SDK 接入 Go 服务**（建立 Tracer Provider、OTLP exporter、trace 传播、日志注入 trace_id），LLM span 只是在此之上加 `gen_ai.*` 属性。这与 PM「纯 OTel 优先」判断一致，但前置成本需显式标注。

### 3.2 前置工作（OTel SDK 接入）

| 项 | 内容 | 说明 |
|---|---|---|
| Tracer Provider | `go.opentelemetry.io/otel/sdk/trace` 初始化，OTLP gRPC exporter | 各入口（`cmd/mora-api`、`cmd/rag-worker`、`cmd/mcp-server`）注入 |
| 传播 | `go.opentelemetry.io/otel/propagation` W3C tracecontext | API→MQ→Worker 贯穿（Valkey Stream 消息带 traceparent） |
| 日志注入 | zerolog hook 注入 `trace_id`/`span_id` | 落地 `07 §7.2` 设计 |
| 配置 | `OTEL_EXPORTER_OTLP_ENDPOINT`/采样率 | `config.go` 新增 OTel 配置键 |
| 部署 | `deployments/` 加 otel-collector 服务（→ Jaeger/Tempo） | 落地 `02:547`、`07 §7.3` |

> **此前置不属 #4 独有**：它是 `07 §7` 可观测设计的既定项，只是尚未实现。#4 把 LLM span 挂在其上。建议前置与 #4 同卷推进，或前置单独立项由 mora 基础设施研发承接（见 §6 立项建议）。

### 3.3 LLM span 埋点（#4 本体，前置就绪后）

在 `OllamaLLMProvider.Chat`（#3 §3.1）实现内或调用方：

```go
ctx, span := tracer.Start(ctx, "ollama.chat",
    trace.WithAttributes(
        semconv.GenAISystemOllama,                  // gen_ai.system="ollama"
        semconv.GenAIRequestModel(o.ModelName),       // gen_ai.request.model
    ))
defer span.End()
// ... call /api/chat ...
span.SetAttributes(
    semconv.GenAIUsageInputTokens(resp.PromptTokens),     // from ChatResult.Usage
    semconv.GenAIUsageOutputTokens(resp.CompletionTokens),
)
```

Agent 单轮起草的 trace 链：`search_knowledge_base` span → `get_document` span → `create_draft` span，各 MCP 工具 `Execute` 外层包 span，复用 #3 `TokenUsage`。

---

## 4. 决策结论

| 项 | 选型 | 说明 |
|---|---|---|
| LLM 推理追踪 | **纯 OTel LLM span（GenAI 语义约定）** | `genaiconv` 包，`gen_ai.*` 属性 |
| Langfuse | **列可选增强（不进 P1 主路径）** | 未来作额外 OTLP exporter 按需叠加 |
| 前置 | OTel SDK 接入 Go 服务 | 既定设计项，`07 §7` 已定，代码未接 |
| RAG 进度时间线 | stage-by-stage span 增强 Grafana | 借鉴 WeKnora，复用现有 `rag_indexing_*` 指标 |

---

## 5. License 合规声明

| 组件 | License | 合规影响 |
|---|---|---|
| `go.opentelemetry.io/otel` SDK + `genaiconv` | Apache-2.0 | 无传染 |
| OTel Collector | Apache-2.0 | 无传染 |
| Jaeger/Tempo | Apache-2.0 | 无传染 |
| Langfuse（可选，未来） | MIT | 自托管无传染 |

**合规结论**：纯 OTel 路径全 Apache-2.0，无传染；Langfuse 未来叠加亦 MIT 合规。

---

## 6. 门控与被低估改动

| 项 | 状态 | 说明 |
|---|---|---|
| OTel SDK 接入前置 | ⚠️ **前置，未解** | `07 §7` 设计已定，代码与部署均未接；#4 LLM span 挂其上 |
| `genaiconv` 包 | ✅ 已有 | OTel Go SDK v1.41.0 内置 `gen_ai.*` 语义包，无需自研 |
| #3 `TokenUsage` 接入点 | ✅ 已预留 | `ChatResult.Usage` 直接映射 `gen_ai.usage.*` |
| Langfuse | 不进 P1 主路径 | 可选增强，未来 OTLP exporter 叠加 |
| 三方 SDK 依赖 | 零额外（仅 OTel SDK） | Langfuse SDK 不引入 |

**被低估改动**：#4 的真正成本不是 LLM span 本身（`genaiconv` 让埋点极轻），而是 **OTel SDK 接入 Go 服务这个前置**——它跨越 Tracer Provider、传播、日志注入、配置、部署（otel-collector），属 mora 基础设施层，非 LLM 域。这一点 PM 上轮评估「OTel SDK 未接入」的判断经核实**成立且更严重**：不仅 SDK 未接，配置键、部署服务、trace 传播全缺。

### 6.1 立项建议：OTel SDK 接入前置**单独立项**，不并入本决策书

理由同 #3 的 draft/review：① 域归属不同——OTel SDK 接入属 mora 基础设施可观测层，非 LLM 域，本决策书是 LLM 追踪选型；② 可解耦——`genaiconv` span 埋点依赖前置就绪，前置独立排期后 #4 埋点即接；③ 既有设计已定——`07 §7` 是既定设计项，前置是把设计落地，不是新架构。建议由 PM 起一个 P1 issue「OTel SDK 接入 Go 服务」（Tracer Provider + OTLP exporter + W3C 传播 + 日志注入 trace_id + `deployments/` 加 otel-collector + `config.go` 加 `OTEL_*` 键），本决策书 §3.3 LLM span 埋点在该 issue 闭环后接入。

---

## 7. 风险与缓解

| 风险 | 等级 | 缓解措施 |
|---|---|---|
| OTel SDK 接入前置工作量被低估 | 中 | §3.2 显式列出 5 项工作，建议单独立项排期 |
| `genaiconv` 版本漂移（semconv 仍在演进） | 低 | 选稳定版（如 v1.36.0/genaiconv），属性集不追最新；OTel 向后兼容 |
| 采样丢 trace | 低 | 错误请求 100% 采样（`07 §7.3`），LLM 调用建议头部采样或全采 |
| Langfuse 未来叠加的 traceparent 桥接 | 低 | Langfuse 接受 OTLP，复用同一 exporter，无需改埋点 |

### 7.1 估期（架构层粗估，供 PM 排期，研发定稿为准）

| 项 | 估期 | 说明 |
|---|---|---|
| OTel SDK 接入前置（Tracer/传播/日志/配置/部署） | 4–5d | 跨三入口 + deployments + config，建议单独立项 |
| LLM span 埋点（`genaiconv` + #3 `TokenUsage`） | 1–2d | 前置就绪后，埋点极轻 |
| Agent 工具调用 span（search/get/create_draft） | 1d | 各 MCP 工具 Execute 外层包 span |
| RAG stage-by-stage 进度时间线（Grafana） | 1d | 复用 `rag_indexing_*` 指标增强 |
| 合计 | **~7d** | 含前置；若前置单独立项，#4 本体埋点 ~4d |

---

> 本决策书为 #4 LLM 追踪草案。推荐纯 OTel LLM span（GenAI `genaiconv`）优先、Langfuse 列可选增强（可逆、不互斥）。核实结论：OTel 在 Mora 设计已定代码未接，**OTel SDK 接入为 #4 前置**，建议单独立项（§6.1）。#3 `LLMProvider.TokenUsage` 已为 #4 预留接入点。研发可依 §3.3 在前置就绪后接入 LLM span 埋点。
