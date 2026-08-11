// Phase 1 Asset Registry / Source API client (§4.4 contract-driven).
//
// All list endpoints use cursor pagination (§4.4). Write/trigger actions
// accept an Idempotency-Key so retries are safe. Asset/Source reads follow
// §11.4 error semantics: 404 means either "no permission" or "does not
// exist" — the two are deliberately indistinguishable so the frontend must
// render a 404 as an empty state, never as a leaky error.
//
// Architecture red line (§4.4 + §9): the frontend consumes these REST
// endpoints as the ONLY data source. It must never read projection tables
// directly. Version history / review inbox rendering is bound to what these
// endpoints return — nothing else.

import { http, ApiError } from "./client"
import type {
  AssetDetail,
  AssetListParams,
  CursorPage,
  KnowledgeAsset,
  KnowledgeAssetVersion,
  KnowledgeRelation,
  KnowledgeSource,
  ReviewDecision,
  ReviewRequest,
  SourceSyncRun,
} from "@/types/assets"

export { ApiError }

// ---- Cursor pagination helper ---------------------------------------------

/**
 * Build a query string from a params object, omitting null/undefined and
 * empty-string values. Values are URL-encoded. Caller passes the result
 * without a leading '?'.
 */
function toQuery(params: Record<string, string | number | null | undefined>): string {
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === null || v === undefined || v === "") continue
    sp.set(k, String(v))
  }
  const s = sp.toString()
  return s ? `?${s}` : ""
}

// ---- Assets (§4.4) --------------------------------------------------------

export async function apiGetAssets(
  workspaceId: string,
  params: AssetListParams = {},
): Promise<CursorPage<KnowledgeAsset>> {
  const q = toQuery({
    cursor: params.cursor ?? null,
    page_size: params.page_size ?? 20,
    asset_type: params.asset_type ?? null,
    status: params.status ?? null,
  })
  return http.get<CursorPage<KnowledgeAsset>>(
    `/workspaces/${workspaceId}/knowledge/assets${q}`,
  )
}

export async function apiGetAsset(assetId: string): Promise<AssetDetail> {
  return http.get<AssetDetail>(`/knowledge/assets/${assetId}`)
}

export async function apiGetAssetVersions(
  assetId: string,
  cursor?: string | null,
  pageSize = 50,
): Promise<CursorPage<KnowledgeAssetVersion>> {
  const q = toQuery({ cursor: cursor ?? null, page_size: pageSize })
  return http.get<CursorPage<KnowledgeAssetVersion>>(
    `/knowledge/assets/${assetId}/versions${q}`,
  )
}

export async function apiGetAssetRelations(
  assetId: string,
  relationType?: string | null,
  cursor?: string | null,
  pageSize = 50,
): Promise<CursorPage<KnowledgeRelation>> {
  const q = toQuery({
    relation_type: relationType ?? null,
    cursor: cursor ?? null,
    page_size: pageSize,
  })
  return http.get<CursorPage<KnowledgeRelation>>(
    `/knowledge/assets/${assetId}/relations${q}`,
  )
}

// ---- Sources (§4.4) --------------------------------------------------------

export async function apiGetSources(
  workspaceId: string,
  cursor?: string | null,
  pageSize = 20,
  sourceType?: string | null,
  enabled?: boolean | null,
): Promise<CursorPage<KnowledgeSource>> {
  const q = toQuery({
    cursor: cursor ?? null,
    page_size: pageSize,
    source_type: sourceType ?? null,
    enabled:
      enabled === null || enabled === undefined ? null : enabled ? "true" : "false",
  })
  return http.get<CursorPage<KnowledgeSource>>(
    `/workspaces/${workspaceId}/knowledge/sources${q}`,
  )
}

export async function apiGetSource(id: string): Promise<KnowledgeSource> {
  return http.get<KnowledgeSource>(`/knowledge/sources/${id}`)
}

export async function apiGetSyncRuns(
  sourceId: string,
  cursor?: string | null,
  pageSize = 20,
): Promise<CursorPage<SourceSyncRun>> {
  const q = toQuery({ cursor: cursor ?? null, page_size: pageSize })
  return http.get<CursorPage<SourceSyncRun>>(
    `/knowledge/sources/${sourceId}/sync-runs${q}`,
  )
}

/**
 * Trigger a new sync Run (§4.4 POST .../sync-runs). Returns the created run
 * on success. `idempotencyKey` is required by §4.4 for write actions.
 */
export async function apiTriggerSync(
  sourceId: string,
  idempotencyKey: string,
  requestedRevision?: string | null,
): Promise<SourceSyncRun> {
  return http.post<SourceSyncRun>(
    `/knowledge/sources/${sourceId}/sync-runs`,
    { requested_revision: requestedRevision ?? null },
    { "Idempotency-Key": idempotencyKey },
  )
}

// ---- Review inbox (§4.4) ---------------------------------------------------

export async function apiGetReviews(
  workspaceId: string,
  cursor?: string | null,
  pageSize = 20,
): Promise<CursorPage<ReviewRequest>> {
  const q = toQuery({
    status: "pending",
    cursor: cursor ?? null,
    page_size: pageSize,
  })
  return http.get<CursorPage<ReviewRequest>>(
    `/workspaces/${workspaceId}/knowledge/reviews${q}`,
  )
}

/**
 * Submit a review decision (§4.4 POST /knowledge/reviews/{id}/decisions).
 * `idempotencyKey` is required for the write action. Rollback via
 * `promote`/`deprecate` must carry an `expected_current` per §7 — pass it in
 * the optional `expectedCurrent` field; the backend validates.
 */
export async function apiSubmitReviewDecision(
  reviewId: string,
  idempotencyKey: string,
  decision: ReviewDecision,
  rationale?: string | null,
  expectedCurrent?: string | null,
): Promise<void> {
  const body: Record<string, unknown> = { decision }
  if (rationale) body.rationale = rationale
  if (expectedCurrent) body.expected_current = expectedCurrent
  await http.post(
    `/knowledge/reviews/${reviewId}/decisions`,
    body,
    { "Idempotency-Key": idempotencyKey },
  )
}

// ---- Status predicate (shared by all asset UI) ----------------------------

/**
 * True when an ApiError represents the §11.4 "no permission or not found"
 * case — indistinguishable by design. Callers that hit this render an empty
 * state, never an error, to honor "存在性不泄露".
 */
export function isNotFoundOrForbidden(e: unknown): boolean {
  return e instanceof ApiError && (e.status === 404 || e.status === 403)
}

/** True when the backend refused a write/governance action (403 + audit). */
export function isForbidden(e: unknown): boolean {
  return e instanceof ApiError && e.status === 403
}
