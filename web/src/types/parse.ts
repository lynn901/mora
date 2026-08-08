// Multi-format document parsing types (design-docs/10 §7, parse-ui-design-spec.md).
// Field names mirror the backend JSON keys (snake_case) so the API layer can
// pass responses through without renaming.

/** Source format bucket derived from the uploaded file extension. */
export type SourceFormat =
  | "txt"
  | "md"
  | "html"
  | "json"
  | "csv"
  | "pdf"
  | "docx"
  | "xlsx"
  | "pptx"
  | "epub"
  | "mhtml"

/** Import shape — Block document (editable) or searchable attachment (index-only). */
export type ImportForm = "block" | "attachment"

/** Conflict strategy when a document with the same title exists in the target directory. */
export type ConflictStrategy = "overwrite" | "skip" | "append"

/** Chunking strategy (architecture §7.1; adaptive/parent-child are P2). */
export type ChunkingStrategy = "fixed" | "adaptive_3tier" | "parent_child"

/** Parse task state machine (design-docs/10 §4.3). */
export type ParseStatus =
  | "pending"
  | "parsing"
  | "parsed"
  | "indexed"
  | "failed"

/** Indexing status from the existing RAG pipeline (reused, not redefined). */
export type IndexStatus = "pending" | "indexing" | "indexed" | "failed"

/** Staged timeline stage names (design-docs/10 §6.1). */
export type ParseStage =
  | "queued"
  | "extracting"
  | "chunking"
  | "embedding"
  | "indexed"

/** Per-upload parse options — serialized as JSONB parse_opts (architecture §7.1). */
export interface ParseOptions {
  chunking_strategy?: ChunkingStrategy
  chunk_size?: number
  chunk_overlap?: number
  respect_heading?: boolean
  parser?: "auto" | "text" | "ocr"
  import_form?: ImportForm
  conflict_strategy?: ConflictStrategy
  // P2 multimodal — always sent as false; UI surfaces them disabled.
  vlm_image_describe?: boolean
  ocr_engine?: "off" | "paddleocr_vl" | "tesseract"
  graph_extraction?: boolean
  question_generation?: boolean
}

/** A staged progress entry on the parse timeline (service.ProgressItem). */
export interface ProgressItem {
  stage: ParseStage
  status: "pending" | "active" | "done" | "skipped" | "failed"
  at: string
  detail?: string
}

/** Parse-progress read model (service.ParseProgressResult). */
export interface ParseProgress {
  parse_status: ParseStatus
  index_status: IndexStatus
  progress: ProgressItem[]
  updated_at?: string
}

/** Upload response (service.UploadResult). */
export interface UploadResult {
  document_id: string
  parse_status: ParseStatus
  parse_options?: ParseOptions
}

/** Batch reparse response (service.ReparseResult). */
export interface ReparseResult {
  enqueued: number
  task_ids?: string[]
}

/** A reusable parse config template (service.ParseConfig). */
export interface ParseConfig {
  id: string
  workspace_id?: string
  name: string
  config: ParseOptions
  is_default: boolean
}

/** Chunk preview item (service.ChunkPreviewItem). */
export interface ChunkPreviewItem {
  text: string
  chunk_index: number
  section_path?: string
  token_count: number
  role?: string
}

/** Chunk preview response (service.ChunkPreviewResult). */
export interface ChunkPreviewResult {
  chunks: ChunkPreviewItem[]
  strategy: string
  total: number
}

/** A file queued in the upload confirmation dialog (client-side state). */
export interface UploadFileEntry {
  id: string
  file: File
  format: SourceFormat | null
  importForm: ImportForm
  parser: "auto" | "text" | "ocr"
  size: number
  unsupported: boolean
  oversized: boolean
}

/** Client-side form state for the advanced parse config (camelCase dialog fields,
 * mapped to ParseOptions at submit). Shared by the upload + reparse dialogs. */
export interface ParseOptionsFormState {
  chunkingStrategy: string
  chunkSize: number
  chunkOverlap: number
  respectHeading: boolean
  parser: "auto" | "text" | "ocr"
  importForm: ImportForm
  conflictStrategy: ConflictStrategy
}

/** Default form values (UI spec §4: chunk 512, overlap 64, heading on, append). */
export const DEFAULT_PARSE_FORM: ParseOptionsFormState = {
  chunkingStrategy: "fixed",
  chunkSize: 512,
  chunkOverlap: 64,
  respectHeading: true,
  parser: "auto",
  importForm: "block",
  conflictStrategy: "append",
}
