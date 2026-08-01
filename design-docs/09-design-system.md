# Mora 设计系统规范 v1.0

> 状态：**基准文档** · 子任务 YS-54 的设计交付物
> 适用范围：mora 前端 `web/` 全量 UI
> 诊断基线：2026-08-01 代码快照

本规范解决一个问题：**把 mora 前端从「shadcn 默认灰阶模板」升级为「有品牌设计语言的产品」**。涵盖品牌色体系、字体系统、硬编码色清理、品牌标识、主题切换、基础组件补全与排版规范。

---

## 1. 现状诊断

### 1.1 品牌色缺失

`web/src/index.css` 所有 CSS 变量均为 `oklch(X 0 0)` —— **纯灰阶，chroma=0**。`primary`、`accent`、`ring` 无任何色相，视觉上与中性灰无法区分。用户看到的是一套无品牌辨识度的灰阶 UI。

| 令牌 | 当前值 | 问题 |
|---|---|---|
| `--primary` | `oklch(0.205 0 0)` | 纯黑灰，无品牌色 |
| `--accent` | `oklch(0.97 0 0)` | 与 `--secondary`、`--muted` 色相相同 |
| `--ring` | `oklch(0.708 0 0)` | 焦点环无品牌色 |
| `--sidebar-primary` | `oklch(0.205 0 0)` | 同 `--primary`，无差异化 |

### 1.2 字体系统缺失

- 未安装 `@tailwindcss/typography`，编辑器 `prose` 类无效。
- 无 `@font-face` 声明，无自定义字体栈。
- 全栈依赖系统默认 sans-serif，中文回退不可控。
- 代码块、行内代码无等宽字体指定。

### 1.3 硬编码裸色

两类裸色绕过设计令牌体系：

**A. hex 色值（6 处）**

| # | 位置 | 裸色 | 用途 |
|---|---|---|---|
| 1 | `stores/collab.ts:7-8` | `#3b82f6` 等 8 色 | 协同用户光标色板 |
| 2 | `stores/collab.ts:82` | `#999` | 光标色 fallback |
| 3 | `api/mock.ts:127` | `#3b82f6` | mock 用户光标色 |
| 4 | `api/mock.ts:128` | `#10b981` | mock 用户光标色 |
| 5 | `editor/BlockEditor.tsx:76` | `#999` | 光标色 fallback |

**B. Tailwind 硬编码色类（10 处）**

| # | 位置 | 裸色类 | 应替换为 |
|---|---|---|---|
| 1 | `tree/DirectoryTree.tsx:54` | `text-blue-500` | `text-primary`（文件夹图标用品牌色） |
| 2 | `editor/BlockEditor.tsx:261` | `text-amber-600 dark:text-amber-400` | `text-warning` |
| 3 | `history/VersionHistory.tsx:145` | `bg-green-100 dark:bg-green-900/30 text-green-800 dark:text-green-200` | `bg-success/15 text-success` |
| 4 | `history/VersionHistory.tsx:146` | `bg-red-100 dark:bg-red-900/30 text-red-800 dark:text-red-200` | `bg-destructive/15 text-destructive` |
| 5 | `collab/CollabSidebar.tsx:19` | `text-green-600 dark:text-green-400` | `text-success` |
| 6 | `collab/CollabSidebar.tsx:30` | `text-amber-600 dark:text-amber-400` | `text-warning` |
| 7 | `collab/CollabSidebar.tsx:40` | `text-amber-600 dark:text-amber-400` | `text-warning` |
| 8 | `collab/CollabSidebar.tsx:53` | `text-blue-600 dark:text-blue-400` | `text-info` |
| 9 | `collab/CollabSidebar.tsx:88` | `bg-green-500` | `bg-success` |
| 10 | `collab/CollabSidebar.tsx:118` | `text-blue-500` | `text-info` |

### 1.4 品牌标识缺失

- `index.html:5` favicon 仍为 `/vite.svg`（Vite 默认）。
- `public/` 目录仅有 `vite.svg`，无 mora 品牌图标。
- 登录页使用 `BookOpen`（lucide 通用图标）作为 logo 占位。
- 侧边栏无品牌标识。

### 1.5 主题切换 UI 缺失

- `theme-provider.tsx` 已实现完整的 ThemeProvider（支持 dark/light/system），但：
  - **未在 `main.tsx` 或 `App.tsx` 中挂载 ThemeProvider**。
  - **无 ThemeToggle 组件**，用户无法切换主题。
  - 仅存一个键盘快捷键 `D` 可切换（但 provider 未挂载，实际无效）。

### 1.6 基础组件缺失

以下通用反馈/状态组件无标准化实现，散落在各页面内联：

| 组件 | 当前状态 |
|---|---|
| EmptyState | `MoraLayout.tsx:175-181` 内联空态（BookOpen 图标 + 文案） |
| LoadingState | `MoraLayout.tsx:154-159` 内联加载态（spinner + "Loading..."） |
| StatusBadge | 无，权限/状态展示无统一 badge |
| Toast / 反馈 | 无全局 toast 系统，操作反馈缺失 |

---

## 2. 设计原则

### 品牌理念

> **让知识流动，让协作发生。**
> 
> Make Knowledge Flow.

### 产品视觉原则

1. **Content First** — 内容第一，不是工具第一
2. **Knowledge First** — 知识第一，不是文档第一
3. **Collaboration First** — 协作第一，不是权限第一
4. **AI Native** — AI 天然融合，而不是外挂
5. **Calm Design** — 让用户专注工作，而不是关注 UI

### 设计特征

- **Less UI** — 减少 UI，内容永远第一
- **White Space** — 大量留白，减少视觉压力
- **Card First** — 信息卡片化，方便浏览
- **Rounded** — 统一圆角（8px、12px、16px）
- **Soft Shadow** — 轻阴影，避免厚重感

### 品牌个性

- **Friendly** — 亲和，让知识没有门槛
- **Calm** — 沉稳，帮助用户安心工作
- **Smart** — 智能，AI 辅助用户而非抢夺
- **Professional** — 专业，适合企业长期使用
- **Minimal** — 极简，减少视觉噪音，聚焦内容

---

## 3. 品牌色体系

### 3.1 主色：Mora Blue

品牌主色为 **Mora Blue**（#4F6BFF），传达科技、智能与信任。

| 令牌 | HEX | RGB | 用途 |
|---|---|---|---|
| `--primary` | `#4F6BFF` | `79, 107, 255` | 品牌主色，按钮、链接、焦点环 |
| `--primary-hover` | `#3D5BFF` | `61, 91, 255` | 悬停态 |
| `--primary-active` | `#2B4BFF` | `43, 75, 255` | 激活态 |

### 3.2 品牌渐变

知识流动的视觉隐喻：

```css
--gradient-brand: linear-gradient(135deg, #7B61FF 0%, #4F6BFF 50%, #3CC7FF 100%);
```

用途：Logo、Hero 区域、特殊强调元素。保持克制使用。

### 3.3 AI Purple

AI 功能专属色：

| 令牌 | HEX | 用途 |
|---|---|---|
| `--ai-purple` | `#7A5AF8` | AI Assistant、AI Summary、AI Writing、AI Search |

AI 元素视觉：Sparkle、星点、光晕、渐变。保持克制，避免赛博朋克风格。

### 3.4 语义状态色

| 语义 | HEX | 用途 |
|---|---|---|
| `--success` | `#34C759` | 成功、在线、已发布 |
| `--warning` | `#FFB020` | 警告、待审核 |
| `--error` | `#FF5A5F` | 错误、删除、危险操作 |
| `--info` | `#5AC8FA` | 信息提示 |

### 3.5 中性色

```css
--neutral-900: #0F172A;  /* 主文本 */
--neutral-700: #334155;  /* 次要文本 */
--neutral-500: #64748B;  /* 辅助文本 */
--neutral-300: #CBD5E1;  /* 边框 */
--neutral-50:  #F8FAFC;  /* 背景 */
```

高留白、高可读。

---

## 4. 协同用户色板

协同编辑的光标/选区色板是功能性色彩，不走品牌色，但需在双主题下均可辨识。定义为 CSS 变量：

```
--collab-cursor-1: oklch(0.65 0.20 260);  /* 蓝 */
--collab-cursor-2: oklch(0.60 0.22 25);   /* 红 */
--collab-cursor-3: oklch(0.70 0.17 160);  /* 绿 */
--collab-cursor-4: oklch(0.75 0.16 80);   /* 橙 */
--collab-cursor-5: oklch(0.60 0.20 290);  /* 紫 */
--collab-cursor-6: oklch(0.65 0.20 350);  /* 粉 */
--collab-cursor-7: oklch(0.70 0.15 200);  /* 青 */
--collab-cursor-8: oklch(0.75 0.15 120);  /* 黄绿 */
```

暗色主题下亮度 +0.05、色度 -0.03 以适配深色背景。

**实现要求**：
- `stores/collab.ts` 的 `USER_COLORS` 数组改为读取 CSS 变量（通过 `getComputedStyle`），不再硬编码 hex。
- `api/mock.ts` 的 mock 用户色同步引用令牌。
- `#999` fallback 改为 `var(--muted-foreground)`。

---

## 5. 字体系统

### 5.1 字体栈

| 角色 | 字体栈 | 说明 |
|---|---|---|
| **中文** | `"HarmonyOS Sans", "Source Han Sans SC", "MiSans", system-ui, sans-serif` | 鸿蒙字体优先，思源黑体备选 |
| **英文** | `"Inter", "SF Pro Display", system-ui, sans-serif` | Inter 为主，SF Pro 为 Apple 设备优化 |
| **等宽（Mono）** | `"JetBrains Mono", "Fira Code", ui-monospace, monospace` | 代码块、行内代码、技术标识符 |
| **排版（Prose）** | 同中英文栈，由 `@tailwindcss/typography` 的 `prose` 类控制行高与间距 | 文档正文渲染 |

### 5.2 字号规范

| 类型 | 大小 | 用途 |
|---|---|---|
| H1 | 40px | 页面主标题 |
| H2 | 32px | 章节标题 |
| H3 | 28px | 子章节标题 |
| H4 | 24px | 小标题 |
| H5 | 20px | 段落标题 |
| Body | 16px | 正文 |
| Caption | 14px | 辅助文本 |
| Small | 12px | 标签、时间戳 |

特点：圆角、高可读、留白充足。

### 5.3 Tailwind Typography 插件

安装 `@tailwindcss/tailwindcss-typography`（Tailwind v4 兼容版），在 `index.css` 中 `@import "tailwindcss/typography"`。

编辑器文档渲染区域使用 `prose prose-neutral dark:prose-invert` 类。

### 5.4 字号与行高体系

基于 `rem`，以 `16px` 为基准：

| 令牌 | 大小 | 行高 | 用途 |
|---|---|---|---|
| `--text-xs` | `0.75rem` (12px) | 1.5 | 辅助文本、标签、时间戳 |
| `--text-sm` | `0.875rem` (14px) | 1.5 | 正文（侧边栏、表格） |
| `--text-base` | `1rem` (16px) | 1.625 | 正文（默认） |
| `--text-lg` | `1.125rem` (18px) | 1.6 | 小标题、卡片标题 |
| `--text-xl` | `1.25rem` (20px) | 1.5 | 页面标题 |
| `--text-2xl` | `1.5rem` (24px) | 1.4 | 大标题 |
| `--text-3xl` | `1.875rem` (30px) | 1.3 | 登录页标题 |

### 5.5 字重

| 令牌 | 值 | 用途 |
|---|---|---|
| `font-normal` | 400 | 正文 |
| `font-medium` | 500 | 标签、导航项、表头 |
| `font-semibold` | 600 | 标题、强调 |
| `font-bold` | 700 | 大标题、品牌名 |

---

## 6. 品牌标识

### 6.1 Logo 设计理念

Logo 由三个元素融合而成：

1. **字母 M**（Mora）— 品牌名称的主体识别
2. **打开的书**（Knowledge）— 知识库、文档、学习、知识共享
3. **文档**（Document）— 在线编辑、富文本、Markdown

整体表达：**一本正在展开的知识之书**。

### 6.2 Logo 文件

已产出品牌 Logo SVG 文件，存放于 `web/public/`：

- **Logo**：`web/public/logo.svg`（应用内品牌标识，登录页、侧边栏）
- **Favicon**：`web/public/favicon.svg`（浏览器标签页图标）

**设计特征**：
- 使用品牌渐变（#7B61FF → #4F6BFF → #3CC7FF）
- M 由两本打开的书组成，中间留白形成文档视觉
- 配合 "mora" 文字标识（Inter 字体）

**引用方式**：
```html
<link rel="icon" type="image/svg+xml" href="/favicon.svg" />
```

**组件封装**：`web/src/components/ui/mora-logo.tsx`，props：
- `size`：`number`，默认 `32`
- `className`：`string`，可选

---

## 7. 主题切换

### 7.1 ThemeProvider 挂载

`main.tsx` 中用 `ThemeProvider` 包裹 `App`：

```tsx
<ThemeProvider defaultTheme="system">
  <App />
</ThemeProvider>
```

### 7.2 ThemeToggle 组件

**位置**：`web/src/components/ui/theme-toggle.tsx`

**设计**：
- 图标按钮（`Button variant="ghost" size="icon"`）
- 三个状态循环：light → dark → system → light
- 图标：`Sun`（light）、`Moon`（dark）、`Monitor`（system）
- `aria-label` 随状态变化：`"Switch to dark theme"` / `"Switch to system theme"` / `"Switch to light theme"`

**放置位置**：
- 侧边栏底部（用户头像/登出区域旁）
- 移动端：汉堡菜单行内

---

## 8. 基础组件规范

### 8.1 EmptyState

**位置**：`web/src/components/ui/empty-state.tsx`

**API**：
| Prop | 类型 | 必需 | 说明 |
|---|---|---|---|
| `icon` | `LucideIcon` | 否 | 顶部图标，默认 `Inbox` |
| `title` | `string` | 是 | 主标题 |
| `description` | `string` | 否 | 副文案说明 |
| `action` | `ReactNode` | 否 | 操作按钮区 |

**视觉规范**：
- 居中布局，最大宽度 `20rem`
- 图标：`size-12`，`text-muted-foreground/40`
- 标题：`text-lg font-medium text-foreground`，上边距 `1rem`
- 描述：`text-sm text-muted-foreground`，上边距 `0.25rem`
- 操作区：上边距 `1.5rem`

**替换**：`MoraLayout.tsx:175-181` 的内联空态。

### 8.2 LoadingState

**位置**：`web/src/components/ui/loading-state.tsx`

**API**：
| Prop | 类型 | 必需 | 说明 |
|---|---|---|---|
| `label` | `string` | 否 | 加载文案，默认 `"Loading..."` |
| `fullPage` | `boolean` | 否 | 是否全屏居中，默认 `true` |

**视觉规范**：
- Spinner：`size-8 border-2 border-primary border-t-transparent rounded-full animate-spin`
- 文案：`text-sm text-muted-foreground mt-3`
- 全屏时 `flex items-center justify-center flex-1`

**替换**：`MoraLayout.tsx:154-159` 的内联加载态。

### 8.3 StatusBadge

**位置**：`web/src/components/ui/status-badge.tsx`

基于现有 `badge.tsx` 扩展，新增语义 variant：

| Variant | 底色 | 文本色 | 用途 |
|---|---|---|---|
| `success` | `bg-success/10` | `text-success` | 已发布、在线、已完成 |
| `warning` | `bg-warning/10` | `text-warning` | 待审核、草稿 |
| `info` | `bg-info/10` | `text-info` | 进行中、已同步 |
| `error` | `bg-error/10` | `text-error` | 已拒绝、失败 |

**实现**：在 `badgeVariants` 中新增 variant，颜色引用 §3.4 语义色令牌。

### 8.4 Toast

**位置**：`web/src/components/ui/toast.tsx` + `web/src/components/ui/toaster.tsx` + `web/src/hooks/use-toast.ts`

**方案**：使用 shadcn/ui 的 toast 组件（基于 Radix UI Toast），通过 `npx shadcn@latest add toast` 生成。

**语义映射**：
| Toast 类型 | 图标 | 颜色 |
|---|---|---|
| `default` | `Info` | `--info` |
| `success` | `CheckCircle` | `--success` |
| `warning` | `AlertTriangle` | `--warning` |
| `destructive` | `XCircle` | `--error` |

**放置**：`App.tsx` 中挂载 `<Toaster />`，全局可用。

---

## 9. 验收标准

### 9.1 品牌色

- [ ] `index.css` 中 `--primary`、`--accent`、`--ring` 等令牌引用品牌靛蓝色阶。
- [ ] 亮/暗主题下品牌色独立调校，不存在简单反相。
- [ ] 语义状态色（success/warning/info/error）在双主题下可辨识。
- [ ] 所有按钮、链接、焦点环呈现品牌色。

### 9.2 字体

- [ ] `@tailwindcss/tailwindcss-typography` 已安装并导入。
- [ ] Inter + Noto Sans SC 字体在页面加载后可用。
- [ ] JetBrains Mono 用于代码块与行内代码（`<code>`、`pre`）。
- [ ] 编辑器 `prose` 类正确渲染文档内容。

### 9.3 硬编码色清理

**A. hex 色值**

- [ ] `stores/collab.ts` 的 `USER_COLORS` 改为 CSS 变量引用。
- [ ] `api/mock.ts` 的 mock 色值改为 CSS 变量引用。
- [ ] `editor/BlockEditor.tsx` 的 `#999` fallback 改为 `var(--muted-foreground)`。
- [ ] `stores/collab.ts` 的 `#999` fallback 改为 `var(--muted-foreground)`。
- [ ] 全仓 grep `#[0-9a-fA-F]{3,8}` 仅剩 `USER_COLORS` CSS 变量定义处的合法值。

**B. Tailwind 硬编码色类**

- [ ] `DirectoryTree.tsx:54` `text-blue-500` → `text-primary`
- [ ] `BlockEditor.tsx:261` `text-amber-600 dark:text-amber-400` → `text-warning`
- [ ] `VersionHistory.tsx:145` `bg-green-100 ... text-green-800` → `bg-success/15 text-success`
- [ ] `VersionHistory.tsx:146` `bg-red-100 ... text-red-800` → `bg-destructive/15 text-destructive`
- [ ] `CollabSidebar.tsx:19` `text-green-600 dark:text-green-400` → `text-success`
- [ ] `CollabSidebar.tsx:30,40` `text-amber-600 dark:text-amber-400` → `text-warning`
- [ ] `CollabSidebar.tsx:53` `text-blue-600 dark:text-blue-400` → `text-info`
- [ ] `CollabSidebar.tsx:88` `bg-green-500` → `bg-success`
- [ ] `CollabSidebar.tsx:118` `text-blue-500` → `text-info`
- [ ] 全仓 grep `text-blue-\|text-amber-\|bg-green-\|bg-red-\|text-green-\|text-red-` 无业务裸色残留。

### 9.4 品牌标识

- [ ] `public/favicon.svg` 已替换，`index.html` 引用更新。
- [ ] `<MoraLogo />` 组件在登录页与侧边栏正确渲染。
- [ ] 浏览器标签页显示 mora favicon，非 vite.svg。

### 9.5 主题切换

- [ ] `ThemeProvider` 已在 `main.tsx` 挂载。
- [ ] `ThemeToggle` 组件在侧边栏可见且可操作。
- [ ] light → dark → system 三态循环正常。
- [ ] 切换后所有 UI 元素在双主题下正确渲染。

### 9.6 基础组件

- [ ] `EmptyState` 组件已创建，替换 `MoraLayout` 内联空态。
- [ ] `LoadingState` 组件已创建，替换 `MoraLayout` 内联加载态。
- [ ] `StatusBadge` variant 扩展完成。
- [ ] Toast 系统已集成，`<Toaster />` 全局挂载。

### 9.7 构建与质量

- [ ] `npm run typecheck` 通过。
- [ ] `npm run lint` 通过。
- [ ] `npm run build` 通过。
- [ ] 无新增 TypeScript 错误。

---

## 10. 实施分解（3 个子任务）

### 子任务 1：设计令牌与字体基础设施

**范围**：§3（品牌色）+ §4（协同色板）+ §5（字体）+ §3.5（令牌映射）

**交付**：
- `index.css`：品牌色阶变量、语义色变量、字体栈声明、Typography 插件导入、shadcn 令牌映射更新。
- `package.json`：新增 `@tailwindcss/tailwindcss-typography` 依赖。
- `index.html`：Google Fonts `<link>` 标签。
- `stores/collab.ts`：`USER_COLORS` 改为 CSS 变量引用。

**验收**：§9.1 + §9.2 + §9.3（部分）+ §9.7

### 子任务 2：硬编码色清理 + 品牌标识 + 主题切换

**范围**：§6（品牌标识）+ §7（主题切换）+ §9.3（剩余裸色清理）

**交付**：
- `public/favicon.svg`：品牌 favicon。
- `components/ui/mora-logo.tsx`：`<MoraLogo />` 组件。
- `components/ui/theme-toggle.tsx`：`<ThemeToggle />` 组件。
- `main.tsx`：挂载 `ThemeProvider`。
- `App.tsx`：集成 `ThemeToggle` 到布局。
- `LoginPage.tsx`：logo 替换为 `<MoraLogo />`。
- `MoraLayout.tsx`：侧边栏品牌标识 + ThemeToggle 放置。
- `editor/BlockEditor.tsx`、`api/mock.ts`：裸色清理。

**验收**：§9.3 + §9.4 + §9.5 + §9.7

**前置**：子任务 1 完成（令牌体系就绪后清理裸色才有意义）。

### 子任务 3：基础组件补全与排版打磨

**范围**：§8（EmptyState / LoadingState / StatusBadge / Toast）+ 排版微调

**交付**：
- `components/ui/empty-state.tsx`
- `components/ui/loading-state.tsx`
- `components/ui/status-badge.tsx`（扩展 badge）
- `components/ui/toast.tsx` + `toaster.tsx` + `hooks/use-toast.ts`
- `MoraLayout.tsx`：替换内联空态/加载态为标准组件。
- `App.tsx`：挂载 `<Toaster />`。

**验收**：§9.6 + §9.7

**前置**：子任务 1 完成（语义色令牌就绪后 StatusBadge 才能引用）。

---

## 附录 A：新增/修改文件清单

| 文件 | 操作 | 子任务 |
|---|---|---|
| `web/src/index.css` | 修改 | 1 |
| `web/package.json` | 修改（+1 dep） | 1 |
| `web/index.html` | 修改 | 1, 2 |
| `web/src/stores/collab.ts` | 修改 | 1 |
| `web/public/favicon.svg` | 新增 | 2 |
| `web/src/components/ui/mora-logo.tsx` | 新增 | 2 |
| `web/src/components/ui/theme-toggle.tsx` | 新增 | 2 |
| `web/src/main.tsx` | 修改 | 2 |
| `web/src/App.tsx` | 修改 | 2, 3 |
| `web/src/components/mora/MoraLayout.tsx` | 修改 | 2, 3 |
| `web/src/components/auth/LoginPage.tsx` | 修改 | 2 |
| `web/src/components/editor/BlockEditor.tsx` | 修改 | 2 |
| `web/src/api/mock.ts` | 修改 | 2 |
| `web/src/components/ui/empty-state.tsx` | 新增 | 3 |
| `web/src/components/ui/loading-state.tsx` | 新增 | 3 |
| `web/src/components/ui/status-badge.tsx` | 新增 | 3 |
| `web/src/components/ui/badge.tsx` | 修改 | 3 |
| `web/src/components/ui/toast.tsx` | 新增 | 3 |
| `web/src/components/ui/toaster.tsx` | 新增 | 3 |
| `web/src/hooks/use-toast.ts` | 新增 | 3 |

## 附录 B：新增依赖

| 包名 | 版本 | 用途 |
|---|---|---|
| `@tailwindcss/tailwindcss-typography` | `^0.6` | Tailwind v4 排版插件（prose 类） |
| `@radix-ui/react-toast` | `^1.2.x` | Toast 组件（shadcn toast 依赖） |

## 附录 C：设计决策记录

| 决策 | 选择 | 备选 | 理由 |
|---|---|---|---|
| 品牌色相 | 靛蓝（265°） | 蓝（220°）、青（190°） | 靛蓝兼具专业感与辨识度，不像纯蓝那样「通用」 |
| 色彩空间 | oklch | HSL | oklch 感知均匀，色阶过渡自然，Tailwind v4 原生支持 |
| 正文字体 | Inter + Noto Sans SC | system-ui only | system-ui 中文回退不可控，Noto Sans SC 保证跨平台一致性 |
| 等宽字体 | JetBrains Mono | Fira Code | JetBrains Mono 字形高度更统一，适合 UI 内代码块 |
| 字体引入方式 | Google Fonts CDN | 自托管 | MVP 阶段简单优先，后续可切换自托管 |
| Toast 方案 | shadcn/ui toast | react-hot-toast | 与现有 shadcn 体系一致，无额外设计语言冲突 |
| Favicon 格式 | SVG | ICO/PNG | SVG 矢量、体积小、现代浏览器全支持 |
