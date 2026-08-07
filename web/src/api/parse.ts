// Multi-format document parsing API client (design-docs/10 §7.2).
// The upload endpoint is multipart/form-data and bypasses the JSON request()
// helper in client.ts; the other endpoints are plain JSON and reuse http.*.
import { http, getToken } from "@/api/client"
import type {
  ParseOptions,
  ParseProgress,
  UploadResult,
  ReparseResult,
  ParseConfig,
  ChunkPreviewResult,
} from "@/types/parse"

const BASE_URL = "/api/v1"

/** POST /workspaces/:ws/documents/upload — multipart upload + parse enqueue. */
export async function apiUploadDocument(
  workspaceId: string,
  file: File,
  opts: {
    directoryId?: string
    title?: string
    parseConfigId?: string
    parseOptions?: ParseOptions
  },
): Promise<UploadResult> {
  const form = new FormData()
  form.append("file", file)
  if (opts.directoryId) form.append("directory_id", opts.directoryId)
  if (opts.title) form.append("title", opts.title)
  if (opts.parseConfigId) form.append("parse_config_id", opts.parseConfigId)
  if (opts.parseOptions) {
    form.append("parse_options", JSON.stringify(opts.parseOptions))
  }

  const token = getToken()
  const res = await fetch(`${BASE_URL}/workspaces/${workspaceId}/documents/upload`, {
    method: "POST",
    headers: token ? { Authorization: `Bearer ${token}` } : undefined,
    body: form,
  })

  const json = await res.json()
  if (!res.ok || json.code !== 0) {
    throw new Error(json.message || `上传失败 (${res.status})`)
  }
  return json.data as UploadResult
}

/** POST /workspaces/:ws/documents/reparse — batch re-parse (≤500). */
export async function apiReparseDocuments(
  workspaceId: string,
  documentIds: string[],
  parseOptions?: ParseOptions,
): Promise<ReparseResult> {
  return http.post<ReparseResult>(`/workspaces/${workspaceId}/documents/reparse`, {
    document_ids: documentIds,
    parse_options: parseOptions,
  })
}

/** GET /documents/:id/parse-progress — staged timeline + badges. */
export async function apiGetParseProgress(documentId: string): Promise<ParseProgress> {
  return http.get<ParseProgress>(`/documents/${documentId}/parse-progress`)
}

/** POST /rag/chunk-preview — preview chunker without persisting. */
export async function apiChunkPreview(
  text: string,
  parseOptions?: ParseOptions,
): Promise<ChunkPreviewResult> {
  return http.post<ChunkPreviewResult>("/rag/chunk-preview", {
    text,
    parse_options: parseOptions,
  })
}

// --- parse config templates (10 §7.1) ---

export async function apiListParseConfigs(workspaceId: string): Promise<ParseConfig[]> {
  const data = await http.get<{ configs: ParseConfig[] }>(
    `/workspaces/${workspaceId}/parse-configs`,
  )
  return data.configs || []
}

export async function apiCreateParseConfig(
  workspaceId: string,
  config: { name: string; config: ParseOptions; is_default?: boolean },
): Promise<ParseConfig> {
  return http.post<ParseConfig>(`/workspaces/${workspaceId}/parse-configs`, config)
}

export async function apiUpdateParseConfig(
  workspaceId: string,
  configId: string,
  config: { name: string; config: ParseOptions; is_default?: boolean },
): Promise<ParseConfig> {
  return http.put<ParseConfig>(`/workspaces/${workspaceId}/parse-configs/${configId}`, config)
}

export async function apiDeleteParseConfig(
  workspaceId: string,
  configId: string,
): Promise<void> {
  await http.delete(`/workspaces/${workspaceId}/parse-configs/${configId}`)
}
