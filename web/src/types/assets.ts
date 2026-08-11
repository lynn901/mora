// Phase 1 Asset Registry / Source types — driven by §4.4 API contract +
// 013/014 table schemas (design-docs/14-phase1-asset-registry-source.md).
//
// Field names mirror the backend snake_case contract; the API layer maps
// them to these camelCase types so UI components stay backend-agnostic.
// Status enums are closed unions matching the DB CHECK constraints so the
// compiler catches mismatched literals at build time.

/** Cursor-paginated list response (§4.4 — all batch lists use cursor paging). */
export interface CursorPage<T> {
  items: T[]
  /** Opaque cursor for the next page; null/empty when there is no next page. */
  next_cursor: string | null
  /** Total count when the backend can compute it cheaply; otherwise null. */
  total: number | null
}

// ---- Enums (closed unions per DB CHECK constraints) ------------------------

export type AssetType = "document" | "codebase" | "memory" | "skill"

/** knowledge_assets.status */
export type AssetStatus = "draft" | "published" | "archived" | "rejected"

/** knowledge_asset_versions.build_status */
export type BuildStatus = "pending" | "building" | "ready" | "failed" | "stale"

/**
 * knowledge_asset_versions.governance_status.
 * `candidate` versions are never returned by read paths that resolve the
 * current version — callers see the last `published` version instead (§11.4).
 */
export type GovernanceStatus =
  | "candidate"
  | "published"
  | "rejected"
  | "deprecated"

export type SourceType = "file" | "url_api" | "git"

export type TrustLevel = "untrusted" | "trusted" | "internal"

/** source_sync_runs.status */
export type SyncRunStatus =
  | "queued"
  | "fetching"
  | "processing"
  | "ready"
  | "failed"
  | "cancelled"

/** review_requests.status */
export type ReviewStatus = "pending" | "approved" | "rejected" | "superseded"

/** review_decisions.decision (§4.4 — approve/reject/merge/promote/deprecate) */
export type ReviewDecision =
  | "approve"
  | "reject"
  | "merge"
  | "promote"
  | "deprecate"

/** asset_projections.projection_kind */
export type ProjectionKind =
  | "fts"
  | "vector"
  | "summary"
  | "codegraph"
  | "relation"

/** asset_projections.status */
export type ProjectionStatus =
  | "pending"
  | "building"
  | "ready"
  | "failed"
  | "stale"

/** knowledge_relations.relation_type */
export type RelationType =
  | "derived_from"
  | "explains"
  | "implements"
  | "supersedes"
  | "contradicts"
  | "uses"
  | "related_to"

// ---- Domain models ---------------------------------------------------------

/**
 * Version-staleness marker per §11.4: a read path returns 200 with a
 * `stale`/`building` flag plus the last available version when the requested
 * version is still being built or has been superseded. UI surfaces this as a
 * non-error notice rather than a failure.
 */
export type VersionFreshness = "fresh" | "stale" | "building"

export interface KnowledgeAsset {
  id: string
  workspace_id: string
  asset_type: AssetType
  name: string
  description: string | null
  owner_type: string
  owner_id: string
  status: AssetStatus
  visibility: string
  governance_profile_id: string | null
  /** Only set for asset_type='document' (013 partial unique index). */
  native_document_id: string | null
  current_version_id: string | null
  latest_requested_version_no: number
  confidence: number | null
  valid_from: string | null
  expires_at: string | null
  created_at: string
  updated_at: string
}

export interface KnowledgeAssetVersion {
  id: string
  asset_id: string
  version_no: number
  source_id: string | null
  source_revision: string | null
  native_document_version_id: string | null
  content_origin: string
  content_hash: string | null
  dedupe_key: string
  build_status: BuildStatus
  governance_status: GovernanceStatus
  approved_by_type: string | null
  approved_by_id: string | null
  approved_at: string | null
  created_by_type: string
  created_by_id: string
  created_at: string
}

/**
 * Asset detail payload (§4.4 GET /knowledge/assets/{id}). Carries the current
 * version plus a freshness marker so the UI can render a stale/building
 * notice without treating it as an error.
 */
export interface AssetDetail {
  asset: KnowledgeAsset
  current_version: KnowledgeAssetVersion | null
  /** Versions overview — newest first (cursor-paged separately via /versions). */
  versions: KnowledgeAssetVersion[]
  freshness: VersionFreshness
}

export interface AssetProjection {
  id: string
  asset_version_id: string
  projection_kind: ProjectionKind
  provider: string
  provider_version: string | null
  build_revision: string
  status: ProjectionStatus
  locator: Record<string, unknown> | null
  built_at: string | null
  last_error: string | null
  created_at: string
  updated_at: string
}

export interface KnowledgeSource {
  id: string
  workspace_id: string
  source_type: SourceType
  name: string
  uri_normalized: string
  credential_ref: string | null
  sync_policy: Record<string, unknown>
  trust_level: TrustLevel
  license: { spdx?: string; status?: string; notice_required?: boolean } | null
  current_revision: string | null
  enabled: boolean
  last_synced_at: string | null
  /** Already redacted by the backend (§6.5) — safe to display. */
  last_error: string | null
  created_by_type: string
  created_by_id: string
  created_at: string
  updated_at: string
}

export interface SourceSyncRun {
  id: string
  source_id: string
  requested_by_type: string
  requested_by_id: string
  requested_revision: string | null
  resolved_revision: string | null
  credential_version: string | null
  governance_profile_id: string | null
  requested_asset_type: AssetType
  status: SyncRunStatus
  attempt: number
  idempotency_key: string
  started_at: string | null
  finished_at: string | null
  error_code: string | null
  /** Already redacted (§6.5). */
  error_detail_redacted: string | null
  created_at: string
}

export interface ReviewRequest {
  id: string
  workspace_id: string
  asset_id: string
  asset_version_id: string
  governance_profile_id: string
  requested_by_type: string
  requested_by_id: string
  status: ReviewStatus
  /** Redacted rationale from the requester. */
  rationale: string | null
  created_at: string
  resolved_at: string | null
  resolved_by_type: string | null
  resolved_by_id: string | null
  /** Joined for display — the asset/version under review. */
  asset_name?: string
  asset_type?: AssetType
  version_no?: number
}

export interface KnowledgeRelation {
  id: string
  workspace_id: string
  from_asset_id: string
  from_version_id: string | null
  relation_type: RelationType
  to_asset_id: string
  to_version_id: string | null
  origin: "human" | "generated" | "system"
  confidence: number | null
  created_by_type: string
  created_by_id: string
  created_at: string
}

/** Query params for asset list filtering (§4.4 GET /knowledge/assets). */
export interface AssetListParams {
  cursor?: string | null
  page_size?: number
  asset_type?: AssetType
  status?: AssetStatus
}
