// Phase 5-4 配装管理 API client — §6.1 REST control plane contract
// (design-docs/19 §6.1). All list endpoints use cursor pagination; write
// actions accept an Idempotency-Key so retries are safe; updates are gated by
// If-Match (ETag).
//
// §11.4 leak-safe error semantics: 404 means "no permission" OR "does not
// exist" — the two are deliberately indistinguishable, so the frontend renders
// a 404 as an empty state, never a leaky error (mirrors api/assets.ts).
//
// Architecture red line (§4.4 + §9): the frontend consumes these REST
// endpoints as the ONLY data source — it never reads agent_bindings or
// skill_packages directly.

import { http, ApiError } from "./client"
import type { CursorPage } from "@/types/assets"
import type {
  Agent,
  AgentBinding,
  BatchResult,
  BindingDeliveryMode,
  BindingInput,
} from "@/types/binding"

export { ApiError }
export type { CursorPage }

// §6.1 — a read returns the active bindings for one agent, cursor-paginated.
// The backend maps the list cursor to an opaque string; we pass it through.
export interface BindingListParams {
  cursor?: string | null
  page_size?: number
}

// Cursor pagination helper — mirrors the one in api/assets.ts so the contract
// is consistent across modules.
function toQuery(params: Record<string, string | number | null | undefined>): string {
  const sp = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v === null || v === undefined || v === "") continue
    sp.set(k, String(v))
  }
  const s = sp.toString()
  return s ? `?${s}` : ""
}

/**
 * §6.1 GET /agents/{id}/bindings — list active bindings for an agent.
 * Cursor-paginated; a 404 surfaces as not-found (existence not leaked).
 */
export async function apiGetBindings(
  agentId: string,
  params: BindingListParams = {},
): Promise<CursorPage<AgentBinding>> {
  const q = toQuery({
    cursor: params.cursor ?? null,
    page_size: params.page_size ?? 50,
  })
  return http.get<CursorPage<AgentBinding>>(`/agents/${agentId}/bindings${q}`)
}

/**
 * §6.1 POST /agents/{id}/bindings:batch — batch upsert in one transaction
 * (§5.2). Idempotency-Key is required: a duplicate key for a DIFFERENT
 * payload is a 409; the same payload returns the original batch. The caller
 * generates the key (uuid or content-hash) per logical batch.
 */
export async function apiBatchUpsertBindings(
  agentId: string,
  workspaceId: string,
  inputs: BindingInput[],
  idempotencyKey: string,
): Promise<BatchResult> {
  return http.post<BatchResult>(
    `/agents/${agentId}/bindings:batch`,
    {
      workspace_id: workspaceId,
      inputs,
    },
    { "Idempotency-Key": idempotencyKey },
  )
}

/**
 * §6.1 PATCH /agents/{id}/bindings/{binding_id} — update delivery_mode /
 * priority. Gated by If-Match (ETag) to prevent lost updates (§11.1).
 */
export async function apiUpdateBinding(
  agentId: string,
  bindingId: string,
  etag: number,
  patch: { delivery_mode?: BindingDeliveryMode; priority?: number },
): Promise<AgentBinding> {
  return http.patch<AgentBinding>(
    `/agents/${agentId}/bindings/${bindingId}`,
    patch,
    { "If-Match": String(etag) },
  )
}

/**
 * §6.1 POST /agents/{id}/bindings/{binding_id}:revoke — revoke a binding.
 * Sets revoked_at=now() and bumps the workspace authz revision in the same tx
 * (§5.4: revoke → revision+1 → cache invalidates → next request denies).
 */
export async function apiRevokeBinding(
  agentId: string,
  bindingId: string,
): Promise<void> {
  await http.post(`/agents/${agentId}/bindings/${bindingId}:revoke`)
}

/**
 * List agents in a workspace (§6.1 control plane — GET
 * /workspaces/{ws}/agents). Used to populate the agent picker for 配装
 * management. Returns an empty list on 404/403 (existence not leaked, §11.4)
 * so a not-yet-wired route degrades to manual agent-id entry rather than
 * blocking the panel.
 */
export async function apiGetAgents(
  workspaceId: string,
): Promise<Agent[]> {
  try {
    const data = await http.get<CursorPage<Agent>>(
      `/workspaces/${workspaceId}/agents`,
    )
    return data.items ?? []
  } catch (e) {
    if (isNotFoundOrForbidden(e)) return []
    throw e
  }
}

// §11.4 — a 404 (or 403) on a binding read means "no permission or not
// found"; the two are indistinguishable by design. Callers render an empty
// state, never a leaky error.
export function isNotFoundOrForbidden(e: unknown): boolean {
  if (e instanceof ApiError) {
    return e.status === 404 || e.status === 403
  }
  return false
}
