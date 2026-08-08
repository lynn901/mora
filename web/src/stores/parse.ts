// Parse UI store — orchestrates the parse-related dialogs/drawers and the
// monitoring list. Kept separate from mora.ts so the document-tree store stays
// focused on editing; parse interactions call into mora.ts only to refresh the
// tree after an upload lands.
import { create } from "zustand"
import type {
  ParseProgress,
  ParseConfig,
  ParseOptions,
  ParseOptionsFormState,
} from "@/types/parse"
import {
  apiGetParseProgress,
  apiListParseConfigs,
} from "@/api/parse"
import { ApiError } from "@/api/client"

interface ParseState {
  // --- upload dialog (screen 1) ---
  uploadOpen: boolean
  setUploadOpen: (open: boolean) => void

  // --- progress drawer (screen 2) ---
  progressOpen: boolean
  progressDocumentId: string | null
  progress: ParseProgress | null
  progressLoading: boolean
  progressError: string | null
  openProgress: (documentId: string) => void
  closeProgress: () => void
  refreshProgress: () => Promise<void>

  // --- batch reparse dialog (screen 3) ---
  reparseOpen: boolean
  reparseDocumentIds: string[]
  openReparse: (documentIds: string[]) => void
  closeReparse: () => void

  // --- monitoring page (screen 4) ---
  monitoringOpen: boolean
  setMonitoringOpen: (open: boolean) => void

  // --- parse config templates ---
  configs: ParseConfig[]
  configsLoading: boolean
  loadConfigs: (workspaceId: string) => Promise<void>

  /** Build the per-upload ParseOptions payload from dialog form state. */
  buildParseOptions: (form: ParseOptionsFormState) => ParseOptions
}

export const useParseStore = create<ParseState>((set, get) => ({
  uploadOpen: false,
  setUploadOpen: (uploadOpen) => set({ uploadOpen }),

  progressOpen: false,
  progressDocumentId: null,
  progress: null,
  progressLoading: false,
  progressError: null,
  openProgress: (documentId) => {
    set({
      progressOpen: true,
      progressDocumentId: documentId,
      progress: null,
      progressError: null,
    })
    void get().refreshProgress()
  },
  closeProgress: () =>
    set({ progressOpen: false, progressDocumentId: null, progress: null }),
  refreshProgress: async () => {
    const id = get().progressDocumentId
    if (!id) return
    set({ progressLoading: true, progressError: null })
    try {
      const progress = await apiGetParseProgress(id)
      set({ progress, progressLoading: false })
    } catch (e) {
      // 404 means the doc is gone or the caller lacks read permission — both
      // surface as "not found" so existence is never leaked (architecture §6.3).
      const msg = e instanceof ApiError && e.status === 404
        ? "文档不存在或无访问权限"
        : (e as Error).message
      set({ progressLoading: false, progressError: msg })
    }
  },

  reparseOpen: false,
  reparseDocumentIds: [],
  openReparse: (documentIds) => set({ reparseOpen: true, reparseDocumentIds: documentIds }),
  closeReparse: () => set({ reparseOpen: false, reparseDocumentIds: [] }),

  monitoringOpen: false,
  setMonitoringOpen: (monitoringOpen) => set({ monitoringOpen }),

  configs: [],
  configsLoading: false,
  loadConfigs: async (workspaceId) => {
    set({ configsLoading: true })
    try {
      const configs = await apiListParseConfigs(workspaceId)
      set({ configs, configsLoading: false })
    } catch {
      set({ configsLoading: false })
    }
  },

  // Maps the dialog form state to the backend ParseOptions payload. P2 fields
  // are omitted entirely (the backend defaults them to off) — we never send
  // a disabled switch's value.
  buildParseOptions: (f) => {
    const opts: ParseOptions = {
      chunking_strategy: f.chunkingStrategy as ParseOptions["chunking_strategy"],
      chunk_size: f.chunkSize,
      chunk_overlap: f.chunkOverlap,
      respect_heading: f.respectHeading,
      parser: f.parser,
      import_form: f.importForm,
      conflict_strategy: f.conflictStrategy,
    }
    return opts
  },
}))
