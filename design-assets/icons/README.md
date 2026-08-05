# Mora Platform Icons

本目录存放从 `../brand/mora-mark.svg` 派生的发布平台图标。品牌路径、节点位置与颜色保持不变；尺寸、安全区和背景仅针对平台要求适配。

## SVG 源文件

| 文件 | 用途 |
|---|---|
| `favicon.svg` | 浏览器标签、书签及现代浏览器 favicon |
| `app-icon.svg` | Apple App Store、桌面应用与通用 App Icon 母版 |
| `app-icon-maskable.svg` | PWA / Android maskable icon 母版 |
| `app-icon-monochrome.svg` | Android themed icon、系统遮罩及单色平台场景 |
| `favicon-dark.svg` | 深色界面的浏览器 favicon |
| `app-icon-dark.svg` | 深色 App Icon 母版 |
| `app-icon-maskable-dark.svg` | 深色 PWA / Android maskable 母版 |

## 导出文件

| 文件 | 尺寸 | 平台用途 |
|---|---:|---|
| `favicon-16.png` | `16 × 16` | 浏览器小尺寸标签 |
| `favicon-32.png` | `32 × 32` | 标准浏览器标签 |
| `favicon-48.png` | `48 × 48` | Windows 与快捷方式 |
| `favicon.ico` | `16 / 32 / 48` | 传统浏览器兼容 |
| `apple-touch-icon-180.png` | `180 × 180` | iOS / iPadOS 主屏幕 |
| `pwa-icon-192.png` | `192 × 192` | Web App Manifest |
| `pwa-icon-512.png` | `512 × 512` | Web App Manifest |
| `pwa-maskable-512.png` | `512 × 512` | Manifest `purpose: maskable` |
| `app-icon-1024.png` | `1024 × 1024` | App Store 与平台下游导出母版 |
| `favicon-dark-16.png` | `16 × 16` | 深色浏览器小尺寸标签 |
| `favicon-dark-32.png` | `32 × 32` | 深色标准浏览器标签 |
| `favicon-dark-48.png` | `48 × 48` | 深色 Windows 与快捷方式 |
| `favicon-dark.ico` | `16 / 32 / 48` | 深色传统浏览器兼容 |
| `apple-touch-icon-dark-180.png` | `180 × 180` | iOS / iPadOS 深色图标 |
| `pwa-icon-dark-192.png` | `192 × 192` | 深色 Web App Manifest |
| `pwa-icon-dark-512.png` | `512 × 512` | 深色 Web App Manifest |
| `pwa-maskable-dark-512.png` | `512 × 512` | 深色 Manifest maskable |
| `app-icon-dark-1024.png` | `1024 × 1024` | 深色 App Store 母版 |

## 使用规则

- Light App Icon 使用不透明 `#F3F6F4`，Dark App Icon 使用不透明 `#121614`。
- 不在源文件中预切圆角；iOS、macOS、Android 和 PWA 蒙版由平台生成。
- maskable 标记保持在中心安全区内，不单独放大或移动绿色节点。
- `app-icon-monochrome.svg` 是技术性单色遮罩，与品牌目录中的灰阶展示版用途不同。
- favicon 不等同于 Logo；它只是 `mora-mark.svg` 的小尺寸平台派生物。
- 如品牌母版路径发生变化，必须重新生成本目录全部导出文件。
