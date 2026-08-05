# Mora 社媒模板组

## 文件

| 默认文件 | 尺寸 | 默认主题 | 对应主题文件 | 用途 |
|---|---:|---|---|---|
| `story-01-announcement.svg` | `1080 × 1920` | Light | `story-01-announcement-dark.svg` | 产品更新、活动公告 |
| `story-02-quote.svg` | `1080 × 1920` | Dark | `story-02-quote-light.svg` | 观点摘录、金句 |
| `story-03-digest.svg` | `1080 × 1920` | Light | `story-03-digest-dark.svg` | 周报、三条内容摘要 |
| `cover-01-editorial.svg` | `1080 × 1080` | Light | `cover-01-editorial-dark.svg` | 文章、观点封面 |
| `cover-02-report.svg` | `1080 × 1080` | Light | `cover-02-report-dark.svg` | 报告、研究封面 |
| `cover-03-ip.svg` | `1080 × 1080` | Dark | `cover-03-ip-light.svg` | 品牌叙事、轻量内容封面 |

## 编辑规则

- 标有 `editable-*` 的图层为主要替换区域。
- 字体使用 `Inter / Microsoft YaHei / sans-serif` 本地字体栈，不请求外部字体。
- Story 的主要内容保持在左右 `72px`、顶部 `160px`、底部 `220px` 安全区内。
- 标题建议不超过 3 行；摘要不超过 2 行；不得缩小到难以在手机端阅读。
- 绿色只用于栏目、编号和活跃节点，不改成大面积绿色背景。
- 不添加渐变、阴影、发光、额外品牌色或装饰性节点网络。
- 深色模板中的 Logo/IP 使用柔白 `#F3F6F4` 与深色背景 `#121614`。
- 深色模板的次要文字使用 `#AAB3AF`，弱分隔线使用 `#343C38`；可读性强调文字可使用 `#45D1A8`，品牌节点仍保持 `#12A77C`。
- Light/Dark 对应文件必须保留相同画布、布局、editable 图层 ID 与安全区。

SVG 可直接导入 Figma、Illustrator 或 Sketch；发布前按平台要求导出 PNG/JPEG。
