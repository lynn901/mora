# Mora Design Language

> Version 3.0 · Updated 2026-08-05 · Status: canonical product UI reference
>
> Brand promise: **Make knowledge connected, useful, and alive.**

This document defines Mora's visual language, interaction behavior, and engineering contract across `web/`. It is derived from the approved Mora mark in `design-assets/brand/mora-master.svg`. The Chinese counterpart is [`design-zh.md`](./design-zh.md). Archived v2 documents are historical references only.

---

## 1. Brand foundation

Mora is a private collaborative knowledge workspace. Its identity is built from three connected nodes: independent knowledge units become more valuable when relationships are visible, while one active green node represents knowledge currently being created, discussed, or applied.

### Logo meaning

| Element | Meaning | Product implication |
|---|---|---|
| Three nodes | documents, people, and agents as distinct knowledge actors | preserve identity and provenance |
| Connecting triangle | relationships turn isolated content into a knowledge system | make context and links visible |
| Ink nodes | durable, trusted knowledge | neutral surfaces carry most information |
| Green node | active contribution and useful change | green marks intent, progress, and focus |
| Rounded wordmark | technology with a human cadence | precise without feeling mechanical |

The logo is a brand asset, not a component library. Do not rotate cards into diamonds, repeat node triangles as page decoration, or turn every connection into a visible line.

### Design character

- **Connected**: relationships and context are visible without making the interface diagrammatic.
- **Editorial**: reading and writing quality take priority over dashboard decoration.
- **Calm**: stable geometry, quiet surfaces, and restrained motion support concentration.
- **Precise**: state, ownership, permission, and consequence are explicit.
- **Alive**: active work receives timely green signals; inactive chrome recedes.

### Product principles

1. **Content leads**: the document is the primary surface; tools support it.
2. **Context travels with content**: location, owner, permission, save state, and relationships remain discoverable.
3. **Green means active intent**: reserve brand green for actions, focus, progress, and meaningful selection.
4. **Structure before decoration**: use alignment, spacing, and borders before cards, color, and shadow.
5. **Collaboration is attributable**: people and AI output always retain identity and provenance.
6. **Calm is not empty**: layouts are compact and ordered, not padded to feel premium.

### What Mora is not

- Not a blue-purple technology theme.
- Not an all-green canvas; green must remain scarce enough to communicate.
- Not a card-first admin dashboard.
- Not a network diagram applied to every screen.
- Not an AI chat shell or an illustration-led marketing surface.

---

## 2. Brand assets

### Master artwork

- Source: `design-assets/brand/mora-master.svg`
- Production wordmark must use outlined paths, not a runtime font.
- Product icons and favicons are derived from the node mark, never manually redrawn.

### Clear space and scale

- Clear space: at least one node's half-width on every side.
- Minimum full-lockup width: `120px`.
- Minimum standalone mark: `20px`; use an optically simplified favicon below `20px`.
- Preserve the original aspect ratio and path geometry.

### Color variants

| Variant | Mark | Wordmark | Surface |
|---|---|---|---|
| Primary | Ink + Mora Green | Ink | light neutral |
| Reversed | Soft white + Active Green | Soft white | dark neutral |
| Monochrome dark | Ink | Ink | light or transparent |
| Monochrome light | Soft white | Soft white | dark or photographic |

Do not add gradients, shadows, bevels, outlines, or extra node colors. Do not place the primary mark on green because the active node loses meaning.

---

## 3. Color system

### 3.1 Logo colors

These values are immutable identity anchors:

| Token | Value | Role |
|---|---|---|
| `--brand-ink` | `#202523` | wordmark, durable knowledge nodes |
| `--brand-line` | `#252A28` | relationships and structural connection |
| `--brand-green` | `#12A77C` | active knowledge node |

`--brand-green` is optimized for recognition in the logo. It is not dark enough for all small text or white-on-green controls. Product components use the accessible interaction tokens below.

### 3.2 Green scale

| Token | Value | Use |
|---|---|---|
| `--green-50` | `#ECF9F5` | subtle page highlight |
| `--green-100` | `#D7F3EA` | selected background |
| `--green-200` | `#A9E5D4` | progress track, decorative data |
| `--green-300` | `#6DD0B4` | dark-theme secondary accent |
| `--green-400` | `#35BB96` | dark-theme hover |
| `--green-500` | `#12A77C` | logo and large graphical accent |
| `--green-600` | `#0E8967` | medium emphasis |
| `--green-700` | `#0B7659` | accessible light-theme primary |
| `--green-800` | `#095F49` | hover and strong text |
| `--green-900` | `#074C3B` | pressed state |

### 3.3 Light theme

| Semantic token | Value | Role |
|---|---|---|
| `--background` | `#F7F9F8` | application canvas |
| `--foreground` | `#202523` | primary text |
| `--surface` / `--card` | `#FFFFFF` | editor, popover, dialog |
| `--surface-subtle` | `#EFF3F1` | sidebar and grouped controls |
| `--muted-foreground` | `#626B67` | secondary text |
| `--border` | `#DCE2DF` | structural division |
| `--primary` | `#0B7659` | primary action and accessible link |
| `--primary-hover` | `#095F49` | hover |
| `--primary-active` | `#074C3B` | pressed |
| `--primary-foreground` | `#FFFFFF` | text on primary |
| `--accent` | `#D7F3EA` | selection and hover surface |
| `--accent-foreground` | `#07533F` | text on accent |
| `--ring` | `#0B7659` | keyboard focus |

### 3.4 Dark theme

| Semantic token | Value | Role |
|---|---|---|
| `--background` | `#121614` | application canvas |
| `--foreground` | `#F3F6F4` | primary text |
| `--surface` / `--card` | `#191E1C` | editor, popover, dialog |
| `--surface-subtle` | `#202624` | sidebar and grouped controls |
| `--muted-foreground` | `#AAB3AF` | secondary text |
| `--border` | `#343C38` | structural division |
| `--primary` | `#45D1A8` | primary action and link |
| `--primary-hover` | `#66DCB9` | hover |
| `--primary-active` | `#2BBE94` | pressed |
| `--primary-foreground` | `#10221C` | text on primary |
| `--accent` | `#173D32` | selection and hover surface |
| `--accent-foreground` | `#80E3C7` | text on accent |
| `--ring` | `#45D1A8` | keyboard focus |

Dark mode is designed independently, not produced by inversion. Large dark surfaces use warm ink neutrals rather than blue slate.

### 3.5 Semantic and functional color

| Meaning | Light | Dark | Use |
|---|---|---|---|
| Success | `#0E7A52` | `#56D39A` | saved, online, completed |
| Warning | `#9A6209` | `#F0BC62` | read-only, degraded, attention |
| Destructive | `#B83B4A` | `#FF7D8A` | error, delete, denied |
| Info | `#3D6872` | `#78C0CE` | sync, index, neutral information |

Color never carries state alone. Pair status with text, icon, shape, or position. Collaboration cursor colors may use a broader functional palette, but every cursor also displays the participant name.

### 3.6 Color discipline

- Normal product surfaces are neutral; no large green backgrounds.
- Primary green appears once per action region.
- Green does not mean generic success when it would conflict with an active selection; use label and icon to disambiguate.
- AI uses Ink + Green with a Sparkles icon and provenance label. It does not receive a separate purple brand.
- Gradients, glowing green effects, and decorative network lines are prohibited in the workspace.

---

## 4. Typography

Use local and system fonts so private deployments never depend on external font requests.

```css
--font-sans: Inter, "HarmonyOS Sans", "Source Han Sans SC", "Microsoft YaHei", system-ui, sans-serif;
--font-mono: "JetBrains Mono", "SFMono-Regular", Consolas, ui-monospace, monospace;
```

| Role | Size / line height | Weight | Use |
|---|---|---|---|
| Document title | `32 / 40px` | 600 | editable document title only |
| Page title | `20 / 28px` | 600 | search, settings, history |
| Section title | `16 / 24px` | 600 | panel sections |
| Body | `15 / 24px` | 400 | product copy |
| Editor body | `16 / 28px` | 400 | long-form content |
| UI label | `14 / 20px` | 500 | controls and navigation |
| Metadata | `12 / 18px` | 400–500 | time, status, helper text |
| Code | `13 / 21px` | 400 | code and technical identifiers |

- Use `0` letter spacing in product UI; custom spacing is reserved for outlined brand artwork.
- Do not scale font size with viewport width.
- Avoid all caps except unavoidable acronyms such as MCP and RBAC.
- Editor paragraphs target `60–75` Latin characters per line.

---

## 5. Spacing, shape, and elevation

### Spacing

Use a `4px` base unit: `4, 8, 12, 16, 24, 32, 48`.

- Compact control: `32px`; default control: `36px`; prominent action: `40px`.
- Desktop icon target: at least `36 × 36px`; touch target: `44 × 44px`.
- Sidebar row: `32px`; panel header: `44px`; document toolbar: `44–48px`.
- Prose measure: `680–760px`; tables and media may use available content width.
- Main content padding: `24px` desktop, `16px` tablet, `12px` mobile.

### Shape

| Token | Value | Use |
|---|---|---|
| `--radius-sm` | `4px` | tree rows, tags, compact controls |
| `--radius-md` | `6px` | buttons, inputs, menus |
| `--radius-lg` | `8px` | dialogs, popovers, repeated item cards |

- Product UI radius never exceeds `8px`.
- The logo's rotated nodes are not a reason to rotate UI containers.
- Pills are reserved for people, tags, and status.

### Border and elevation

- Use `1px` borders to establish structure before adding shadow.
- Sidebars, toolbars, editor regions, and page sections are not cards.
- Popovers use a small shadow; dialogs use a stronger shadow plus overlay.
- Normal inline content and navigation receive no shadow.
- Never nest cards.

---

## 6. Application composition

### Workspace shell

```text
┌──────────────────┬──────────────────────────────────────────┐
│ Workspace        │ Document: title · state · people · tools │
│ Sidebar 240–288  ├──────────────────────────────────────────┤
│                  │ Contextual toolbar                      │
│ Tree / Search /  ├──────────────────────────────────────────┤
│ Access / History │ Document canvas                         │
└──────────────────┴──────────────────────────────────────────┘
```

- Sidebar default width: `272px`; label length never resizes it.
- Center prose in the document canvas; do not frame the editor as a decorative card.
- Search, access, and history begin as contextual panels.
- Persistent header contains only page identity, save state, collaborators, and frequent actions.

### Responsive behavior

| Width | Behavior |
|---|---|
| `≥ 1200px` | full sidebar and document; optional contextual right panel |
| `768–1199px` | collapsible sidebar; one contextual panel at a time |
| `< 768px` | sidebar becomes a modal drawer; document header becomes one compact row |

At every width, preserve the document title, save state, and route back to navigation. Move secondary commands into a menu before text shrinks or overlaps.

### Hierarchy rules

- Full-width regions and separators define major structure.
- Cards are limited to repeated discrete objects such as results, templates, and integrations.
- Tabs switch peer views; segmented controls switch modes; toggles control binary settings; menus contain option sets.
- Each region has one primary action. Secondary commands use outline or ghost treatment.

---

## 7. Core interaction patterns

### Navigation

- Hover is subtle; selection is persistent and uses `accent` plus a text or primary indicator.
- Tree indentation is `16px` per level.
- Long user titles truncate on one line and expose the full value on demand.
- Keyboard focus remains visible and visually distinct from selection.

### Editing and saving

- Editing is direct; do not wrap document content in another surface.
- Show `正在保存…`, `已保存`, or `保存失败` near the title.
- Autosave success is inline, not a repeated toast.
- Read-only state appears before typing and explains why.
- Consequential actions identify the object and recovery consequence.

### Search and relationships

- Focus search on entry and support keyboard navigation through results.
- Highlight terms with semantic tokens, never hard-coded yellow.
- Results show title, two-line excerpt, and compact metadata.
- Active filters remain visible and removable.
- Backlinks and related pages use quiet lists or graph summaries; never place a decorative graph behind content.

### Collaboration

- Show up to three participant avatars, then `+N`.
- Participant identity color stays stable within a session.
- `在线`, `正在编辑`, `只读`, and degraded connection states use text plus icon/color.
- Cursor updates never animate layout.

### AI

- AI entry points use Sparkles + Ink/Green, with outcome labels such as `总结此页`.
- Generated content shows source, pending review state, and accept/discard actions.
- AI never overwrites user content automatically.
- Streaming feedback stays inside the affected region.
- No purple glow, floating orb, particle field, or separate AI visual universe.

### Loading, empty, error, and success

| State | Pattern |
|---|---|
| Loading | geometry-matched skeleton; spinner only for short indeterminate actions |
| Empty | one plain icon, title, useful sentence, optional primary action |
| Error | what failed, what remains safe, and a recovery action |
| Success | inline confirmation for persistent state; toast for completed commands |
| Disabled | reduced emphasis plus an accessible reason when non-obvious |

The application shell remains visible while feature data loads. Empty states are not tutorials.

---

## 8. Components and icons

### Buttons

- `primary`: one main command per region.
- `outline` / `secondary`: alternative or reversible commands.
- `ghost`: toolbar and low-emphasis commands.
- `destructive`: destructive confirmation, not every delete icon.
- Icon buttons have stable square dimensions and accessible names.

### Inputs and overlays

- Labels remain visible; placeholders provide examples, not labels.
- Validation links through `aria-describedby`.
- Focus uses `ring`; error uses destructive border plus text.
- Menus contain brief actions, popovers contain lightweight context, dialogs contain focused or consequential tasks.
- Dialogs implement initial focus, trapping, Escape, and focus return.

### Icons

Use `lucide-react` at `1.75–2px` stroke: `16px` compact, `18–20px` standard, `24px` empty state. Familiar commands use familiar symbols. Unfamiliar icon-only actions require a tooltip.

### Tables and diffs

- Row height: `36–44px`; headers stay visible for long tables.
- Numbers align right, text left.
- Added and removed lines use `+` / `−` plus color.
- Prefer horizontal scrolling over transforming technical comparisons into cards.

---

## 9. Motion and accessibility

### Motion

| Interaction | Duration | Curve |
|---|---|---|
| Hover / focus | `120ms` | `ease-out` |
| Menu / popover | `160ms` | `ease-out` |
| Sidebar / panel | `200ms` | `ease-out` |
| Dialog | `220ms` | `ease-out` |

Motion explains cause and change. No bounce, parallax, or perpetual ambient animation. Honor `prefers-reduced-motion: reduce`.

### Accessibility and localization

- Meet WCAG 2.2 AA: `4.5:1` normal text and `3:1` large text or meaningful graphics.
- All workflows work by keyboard with logical, visible focus.
- Do not encode state, authorship, or permission through color alone.
- Use semantic HTML before ARIA and announce asynchronous status appropriately.
- Containers tolerate at least `1.5×` current English label length.
- Dates, times, numbers, and pluralization use locale-aware formatters.

---

## 10. Voice

Mora sounds concise, warm, and operational.

| Situation | Use | Avoid |
|---|---|---|
| Saved | 已保存 | 操作成功 |
| Loading workspace | 正在准备工作空间… | 正在初始化系统资源… |
| Create | 新建页面 / 新建知识库 | 创建新的知识空间 |
| Permission | 你没有编辑此页面的权限 | 403 Forbidden |
| Search empty | 未找到相关内容 | 无数据 |
| Delete | 删除页面？此页面可从回收站恢复。 | 确定执行操作？ |

Buttons begin with a clear verb. Routine feedback avoids exclamation marks. Errors explain what happened, whether data is safe, and what the user can do next.

---

## 11. Engineering contract

### Token rules

- Components consume semantic classes such as `bg-background`, `text-foreground`, `bg-primary`, and `text-warning`.
- Raw HEX, RGB, and Tailwind palette classes are prohibited in components except approved participant colors and third-party embedded content.
- Every new token includes light and dark values.
- Logo constants live in brand assets; accessible interaction tokens live in the product theme.

### Component ownership

- Generic primitives: `web/src/components/ui/`.
- Domain patterns: `editor/`, `search/`, `collab/`, `history/`, and `rbac/`.
- Extend existing primitives before creating visually similar duplicates.
- States remain demonstrable without production data.

### Definition of done

1. Uses semantic tokens in light and dark themes.
2. Covers relevant default, hover, focus, active, disabled, loading, empty, and error states.
3. Supports keyboard navigation and accessible names.
4. Does not overflow at `320px`, `768px`, `1280px`, or `200%` zoom.
5. Remains stable with long titles, empty data, and slow responses.
6. Passes `npm run typecheck`, `npm run lint`, and `npm run build` in `web/`.
7. Is visually checked in both themes at desktop and mobile widths.

### Migration order

1. Replace gray and blue-purple variables in `web/src/index.css` with §3 semantic tokens.
2. Derive `web/public/logo.svg` and favicon assets from `design-assets/brand/mora-master.svg`.
3. Mount Theme Provider and expose light / dark / system controls.
4. Remove hard-coded colors from tree, editor, collaboration, search, and history.
5. Standardize save, loading, empty, error, toast, and status badge patterns.
6. Refine the workspace shell and editor typography before adding visual polish.

---

## 12. Review checklist

- Is the document or current task still the strongest element on screen?
- Can the user identify workspace, page, permission, save state, and collaborators?
- Is green communicating active intent instead of filling space?
- Does the UI use alignment and borders before cards and shadow?
- Are the logo nodes confined to meaningful brand or relationship contexts?
- Are AI output and collaborator identity attributable?
- Are destructive actions explicit and recoverable where possible?
- Do both themes, keyboard use, and mobile width support the workflow?
- Does real long-form content remain readable?
- Is every visual value a semantic token or documented exception?

