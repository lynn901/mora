// Phase 5-4 配装管理 UI types — driven by the §6.1 REST contract +
// domain types in internal/domain/skill.go / knowledge_asset.go
// (design-docs/19 §3.2 / §4.3 / §5 / §6.1).
//
// Field names mirror the backend snake_case contract; the API layer maps
// them to these TS types so UI components stay backend-agnostic. Closed
// unions mirror the DB CHECK constraints and Go domain const sets so the
// compiler catches mismatched literals at build time.

// ---- Enums (closed unions, mirror Go domain const sets) --------------------

/** agent_bindings.scope_kind (§3.2). */
export type BindingScopeKind = "asset" | "workspace" | "asset_type"

/** agent_bindings.effect (§3.2). allow grants, deny excludes (deny wins). */
export type BindingEffect = "allow" | "deny"

/**
 * agent_bindings.version_policy (§5.1).
 * - follow_published: resolves to the asset's current published version.
 * - pinned: frozen to pinned_version_id; if that version is revoked/missing
 *   the binding BLOCKS use (no silent fallback to latest) — §5.1 / §11.4.
 */
export type BindingVersionPolicy = "follow_published" | "pinned"

/**
 * agent_bindings.delivery_mode (§3.2 / §5.3).
 * - tool: deliver SKILL.md head + resource manifest.
 * - summary: deliver description + resource manifest only.
 * - inline: progressive resource reads on demand.
 */
export type BindingDeliveryMode = "tool" | "summary" | "inline"

/** skill_packages.validation_status (§4.3). passed = saveable, NOT executable. */
export type SkillValidationStatus =
  | "pending"
  | "passed"
  | "failed"
  | "opaque"

/** validation_report.findings[].severity (§4.3). A single `block` → failed. */
export type ValidationSeverity = "block" | "warn" | "info"

/** compatibility_report.delivery (§4.3). */
export type DeliveryVerdict =
  | "lossless"
  | "runtime_adaptation_needed"
  | "incompatible"

// ---- Domain models --------------------------------------------------------

/**
 * AgentBinding as returned by GET /agents/{id}/bindings (§6.1). The shape
 * mirrors domain.AgentBinding. pinned_version_blocked is a derived flag the
 * service sets when a pinned version is detected as no-longer-usable so the
 * UI can render the §5.1 阻断+告警 (block + alert) state instead of silently
 * falling back to the latest version.
 */
export interface AgentBinding {
  id: string
  agent_id: string
  workspace_id: string
  scope_kind: BindingScopeKind
  asset_id: string | null
  asset_type: string | null
  effect: BindingEffect
  version_policy: BindingVersionPolicy
  pinned_version_id: string | null
  delivery_mode: BindingDeliveryMode
  priority: number
  created_by: string | null
  created_at: string
  revoked_at: string | null
  /** §5.1: pinned version not usable → use will阻断, no fallback. */
  pinned_version_blocked: boolean
}

/** One file in the normalized skill manifest (§2.1). */
export interface SkillFileEntry {
  path: string
  size: number
  hash: string
  exec_bit: boolean
  kind: "skill_md" | "script" | "asset" | "manifest" | "other"
}

/** Normalized skill manifest (§3.1 / §2.1). */
export interface SkillManifest {
  files: SkillFileEntry[]
  capability_summary: Record<string, unknown> | null
  content_hash: string
  entry_count: number
  total_size: number
}

/** One static-check finding (§4.3). */
export interface ValidationFinding {
  check: string
  severity: ValidationSeverity
  code: string
  message: string
  path: string | null
}

/** skill_packages.validation_report (§4.3). No secret values — only signature
 * presence/shape, never verified against a key store. */
export interface ValidationReport {
  findings: ValidationFinding[]
  hashes: Record<string, string>
  signature: Record<string, unknown> | null
}

/** skill_packages.compatibility_report (§4.3). */
export interface CompatibilityReport {
  delivery: DeliveryVerdict
  runtime_needs: string[]
  opaque_fields: string[]
}

/**
 * SkillPackage projection returned alongside a skill asset version (§6.1
 * GET /knowledge/assets/{id}/versions/{vid}). Carries the manifest + reports
 * the UI visualizes. Never carries secret values (§1.2).
 */
export interface SkillPackage {
  asset_version_id: string
  storage_key: string
  format_id: string
  schema_version: string
  manifest: SkillManifest
  original_frontmatter: Record<string, unknown> | null
  content_hash: string
  signature: Record<string, unknown> | null
  provenance_ref: Record<string, unknown> | null
  validation_status: SkillValidationStatus
  validation_report: ValidationReport
  compatibility_report: CompatibilityReport
  scanner_version: string
  created_at: string
  updated_at: string
}

// ---- Batch upsert (§5.2) --------------------------------------------------

/**
 * One item in a batch upsert (§6.1 POST /agents/{id}/bindings:batch).
 * id empty → create; id set → update (gated by etag via If-Match).
 * version_policy=pinned requires scope_kind=asset AND pinned_version_id.
 */
export interface BindingInput {
  id: string | null
  etag: number | null
  scope_kind: BindingScopeKind
  asset_id: string | null
  asset_type: string | null
  effect: BindingEffect
  version_policy: BindingVersionPolicy
  pinned_version_id: string | null
  delivery_mode: BindingDeliveryMode
  priority: number
}

/** Per-item batch outcome (§5.2). */
export interface BindingResult {
  binding: AgentBinding
  pinned_version_blocked: boolean
}

/** BatchResult (§5.2). results is 1:1 with inputs, order preserved. */
export interface BatchResult {
  results: BindingResult[]
  /** Indexes (into results) whose pinned version is blocked (§5.1 alert). */
  alerted: number[]
  new_revision: number
  idempotent_hit: boolean
}

// ---- Agent picker (§6.1 control plane) ------------------------------------

/**
 * Agent principal in a workspace (§4.3). Surfaced by the agent list route so
 * the 配装 management UI can pick which agent to bind. Mirrors domain.Agent.
 * status mirrors AgentStatus — a suspended/revoked agent cannot be bound.
 */
export type AgentStatus = "active" | "suspended" | "revoked"

export interface Agent {
  id: string
  workspace_id: string
  name: string
  description: string | null
  owner_id: string
  status: AgentStatus
  runtime_type: string
  service_account_id: string | null
  created_at: string
  updated_at: string
}
