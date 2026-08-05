# Mora Brand Assets

本目录是 Mora 品牌视觉资产的唯一可信来源。产品、动效、社媒和平台图标应从这里的正式源文件派生，不在下游目录重新绘制品牌结构。

## 正式文件

| 文件 | 定义 | 主要用途 |
|---|---|---|
| `mora-primary-lockup.svg` | 知识节点网络与 Mora 字标的完整组合 | 品牌展示、规范文件、大尺寸发布物料 |
| `mora-wordmark.svg` | 横向 Mora 字标 | 产品导航、登录页和横向紧凑区域 |
| `mora-mark.svg` | 偏心 `o` 与绿色知识节点 | 头像、独立品牌标记及平台资产母版 |
| `mora-mark-monochrome.svg` | 单色独立标记 | 单色印刷、遮罩、雕刻和受限色环境 |

## 品牌常量

| 名称 | 色值 | 用途 |
|---|---|---|
| Ink | `#202523` | 字标、主体与主要结构 |
| Structure | `#252A28` | 节点连接线 |
| Green | `#12A77C` | 唯一活跃知识节点 |
| Soft White | `#F3F6F4` | 深色环境中的反白主体 |

## 管理规则

- `mora-primary-lockup.svg` 是完整品牌结构的母版。
- `mora-wordmark.svg`、`mora-mark.svg` 与单色版必须保持母版中的路径比例和节点位置。
- `web/public/` 中的 Logo 与 favicon 是产品发布副本，不是品牌源文件。
- favicon、App Icon 等平台适配资产放入 `design-assets/icons/`，不得覆盖本目录源文件。
- IP、动效和社媒资产分别放入 `../ip/`、`../motion/` 与 `../social/`。
- 探索稿、未定稿方案和历史归档不得混入本目录。
