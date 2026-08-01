# Mora 字体系统

Mora 默认**不出网**（私有化部署优先），字体通过本地文件提供；可选切换为 Google Fonts CDN。

## 字体族

| 变量 | 字体 | 用途 |
|------|------|------|
| `--font-sans` | Inter + Noto Sans SC | 正文 / UI |
| `--font-mono` | JetBrains Mono | 代码 / 等宽 |

令牌定义在 `web/src/index.css` 的 `:root`，并在 `@theme inline` 中映射为 `--font-sans` / `--font-mono`，因此可直接使用 Tailwind 的 `font-sans` / `font-mono`，或通过 `var(--font-…)` 引用。

## 两种加载方式（环境变量 `VITE_FONT_SOURCE`）

### `local`（默认，私有化）

- `web/index.html` 中带 `data-font-local` 的 `<link>` 生效，加载 `/fonts/fonts.css`。
- 字体文件位于本目录（`web/public/fonts/*.woff2`），由构建时一同输出。
- **首次部署需运行一次** `./fetch-fonts.sh` 下载 woff2（Inter、JetBrains Mono 的 Latin 子集）。
- Noto Sans SC 完整 CJK 字体约 10MB / 100+ 切片，**不在本地打包**；本地模式下中日韩字符回退到系统字体（PingFang SC / Microsoft YaHei 等，已在 `--font-sans` 栈中声明），保持离线包精简。

### `cdn`

```bash
VITE_FONT_SOURCE=cdn npm run build
```

- `web/index.html` 中带 `data-font-cdn` 的 `<link>` 生效，从 Google Fonts 拉取 Inter / JetBrains Mono / Noto Sans SC 全套。
- 不需要本地 woff2 文件，运行时需能访问 `fonts.googleapis.com` / `fonts.gstatic.com`。

切换逻辑在 `web/vite.config.ts` 的 `fontSourcePlugin()`：构建时按环境变量保留对应 `<link>`、剔除另一组。

## 文件清单

```
web/public/fonts/
├── fonts.css            # @font-face 声明（引用下方 woff2）
├── fetch-fonts.sh       # 一次性下载脚本（local 模式前运行）
├── inter-latin-*.woff2  # Inter 400/500/600/700
└── jetbrains-mono-*.woff2  # JetBrains Mono 400/500/600
```

> 缺失的 woff2 文件会通过 `@font-face` 的 `local()` / `--font-sans` 栈优雅回退到系统字体，不影响可用性。
