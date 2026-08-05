# Mora UI Icons

本目录存放 Mora 特有业务语义的 UI 图标设计母版。搜索、删除、设置、撤销等通用操作继续使用 `lucide-react`，不在这里重复设计。

`ui-icons-preview.svg` 提供整组图标的名称与视觉总览。

## 图标清单

| 文件 | 业务语义 | 建议场景 |
|---|---|---|
| `knowledge-graph.svg` | 知识图谱 | 关系视图、知识网络入口 |
| `linked-pages.svg` | 页面关联 | 双向链接、相关页面 |
| `semantic-recall.svg` | 语义召回 | 向量检索结果、相似内容 |
| `hybrid-retrieval.svg` | 混合检索 | BM25 + Vector / RRF 搜索 |
| `knowledge-chunk.svg` | 知识分块 | Chunk 预览、索引范围 |
| `index-pipeline.svg` | 索引流水线 | RAG 索引、重建状态 |
| `source-provenance.svg` | 来源追溯 | 引用、AI 来源、审计线索 |
| `context-window.svg` | 上下文窗口 | Agent 上下文、选中知识范围 |
| `mcp-bridge.svg` | MCP 桥接 | MCP Server、工具连接状态 |
| `permission-inheritance.svg` | 权限继承 | RBAC 范围与继承关系 |
| `collaboration-presence.svg` | 协作在线 | 多人编辑、光标与在线状态 |
| `version-lineage.svg` | 版本血缘 | 版本链、分支与回滚来源 |

## 绘制规范

- 画布统一为 `24 × 24`，默认描边 `1.75px`。
- 使用 `currentColor`，不内置 Light/Dark 色值；由组件文本色控制主题。
- 端点和转角统一使用 `round`，仅知识节点使用小面积实心方块。
- 紧凑控件显示为 `16px`，标准控件为 `18–20px`，空状态最大 `24px`。
- 保持 `viewBox="0 0 24 24"`，不得通过非等比缩放改变轮廓。
- 纯图标按钮必须提供 `aria-label`；不明显的业务图标必须配 Tooltip。

## 工程使用

定稿 SVG 应转换为 React 组件后放入 `web/src/components/icons/`。转换时保留 `currentColor`、`viewBox` 和描边属性，并允许透传标准 `SVGProps<SVGSVGElement>`。

SVG 作为 `<img>` 使用时不会继承页面文字颜色；需要主题适配的产品界面必须以内联 SVG 或 React 组件形式使用。
