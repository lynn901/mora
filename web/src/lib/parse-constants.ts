// Shared constants for the parse UI (parse-ui-design-spec.md §4, §5).
// Kept here so the upload dialog, drawer, and monitoring table stay in sync.

import type { SourceFormat, ParseStage, ParseStatus } from "@/types/parse"
import type { TreeNode } from "@/types"

/** One-time formats supported in P0/P1 (architecture §0 P0+P1 scope). */
export const SUPPORTED_EXTENSIONS: Record<string, SourceFormat> = {
  txt: "txt",
  md: "md",
  markdown: "md",
  html: "html",
  htm: "html",
  json: "json",
  csv: "csv",
  pdf: "pdf",
  docx: "docx",
  xlsx: "xlsx",
  pptx: "pptx",
  epub: "epub",
  mhtml: "mhtml",
  mht: "mhtml",
}

/** Data-type formats that cannot become Block documents — index-only. */
export const ATTACHMENT_ONLY_FORMATS: SourceFormat[] = ["csv", "json"]

export function formatFromFilename(name: string): SourceFormat | null {
  const ext = name.split(".").pop()?.toLowerCase() || ""
  return SUPPORTED_EXTENSIONS[ext] || null
}

/** Format → display label. */
export const FORMAT_LABELS: Record<SourceFormat, string> = {
  txt: "Txt",
  md: "Markdown",
  html: "HTML",
  json: "JSON",
  csv: "CSV",
  pdf: "PDF",
  docx: "Word",
  xlsx: "Excel",
  pptx: "PPT",
  epub: "EPUB",
  mhtml: "MHTML",
}

/** Ordered staged timeline (design-docs/10 §6.1, UI spec §3.2). */
export const STAGE_ORDER: ParseStage[] = [
  "queued",
  "extracting",
  "chunking",
  "embedding",
  "indexed",
]

export const STAGE_LABELS: Record<ParseStage, string> = {
  queued: "排队",
  extracting: "文本抽取",
  chunking: "分块",
  embedding: "向量化",
  indexed: "写入索引",
}

/** Parse status → badge (UI spec §5). Color is never the sole carrier. */
export const PARSE_STATUS_META: Record<
  ParseStatus,
  { label: string; badge: "info" | "warning" | "success" | "destructive"; icon: string }
> = {
  pending: { label: "排队中", badge: "info", icon: "Clock" },
  parsing: { label: "解析中", badge: "info", icon: "Loader" },
  parsed: { label: "待索引", badge: "warning", icon: "Hourglass" },
  indexed: { label: "已索引", badge: "success", icon: "Check" },
  failed: { label: "解析失败", badge: "destructive", icon: "AlertTriangle" },
}

/** P2 fields surfaced as disabled + "二期" badge in the config UI (UI spec §4). */
export const P2_CONFIG_FIELDS = [
  "vlm_image_describe",
  "ocr_engine",
  "graph_extraction",
  "question_generation",
] as const

/** Upload limits (architecture §9.2 / UI spec §3.1 state table). */
export const MAX_FILE_MB = 50
export const MAX_BATCH_FILES = 20
export const MAX_BATCH_MB = 200

/** Flatten a directory tree into id → { name, format? } lookups for the batch
 * reparse summary. Format is derived from the filename when present. */
export function buildTreeFlatten(nodes: TreeNode[]): { id: string; name: string; format?: string }[] {
  const out: { id: string; name: string; format?: string }[] = []
  const walk = (ns: TreeNode[]) => {
    for (const n of ns) {
      out.push({ id: n.id, name: n.name })
      if (n.children?.length) walk(n.children)
    }
  }
  walk(nodes)
  return out
}
