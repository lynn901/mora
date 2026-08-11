// Presentational components for the Phase 1 asset UI — this file exports
// only components, so react-refresh/only-export-components stays happy.
//
// Status colors follow the Mora Design Language V3.1 §3.5 rule: status is
// never conveyed by color alone — every colored badge also carries an icon
// and a text label, so a viewer who can't distinguish the hue still reads
// the state.
import { FileQuestion, AlertTriangle } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import {
  ASSET_STATUS,
  BUILD_STATUS,
  GOVERNANCE_STATUS,
} from "./asset-utils"
import type {
  AssetStatus,
  BuildStatus,
  GovernanceStatus,
} from "@/types/assets"

export function AssetStatusBadge({ status }: { status: AssetStatus }) {
  const m = ASSET_STATUS[status] ?? ASSET_STATUS.draft
  const Icon = m.icon
  return (
    <Badge variant={m.variant}>
      <Icon className="size-3" />
      {m.label}
    </Badge>
  )
}

export function BuildStatusBadge({ status }: { status: BuildStatus }) {
  const m = BUILD_STATUS[status] ?? BUILD_STATUS.pending
  const Icon = m.icon
  return (
    <Badge variant={m.variant}>
      <Icon className={m.variant === "info" ? "size-3 animate-spin" : "size-3"} />
      {m.label}
    </Badge>
  )
}

export function GovernanceStatusBadge({ status }: { status: GovernanceStatus }) {
  const m = GOVERNANCE_STATUS[status] ?? GOVERNANCE_STATUS.candidate
  const Icon = m.icon
  return (
    <Badge variant={m.variant}>
      <Icon className="size-3" />
      {m.label}
    </Badge>
  )
}

/**
 * §11.4: a 404 on a read path means "no permission or does not exist" — the
 * two are indistinguishable by design. This renders a calm empty state, never
 * a leaky error.
 */
export function ForbiddenEmpty({
  message = "无权访问或资源不存在",
  hint = "如需访问，请联系工作空间管理员。",
}: {
  message?: string
  hint?: string
}) {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
      <FileQuestion className="size-10 text-muted-foreground/50" />
      <p className="text-sm font-medium text-muted-foreground">{message}</p>
      <p className="text-xs text-muted-foreground/70">{hint}</p>
    </div>
  )
}

export function ErrorState({
  message,
  onRetry,
}: {
  message: string
  onRetry?: () => void
}) {
  return (
    <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
      <AlertTriangle className="size-10 text-destructive/60" />
      <p className="text-sm font-medium text-destructive">{message}</p>
      {onRetry && (
        <button
          onClick={onRetry}
          className="mt-1 rounded-md border px-3 py-1 text-xs text-foreground hover:bg-accent"
        >
          重试
        </button>
      )}
    </div>
  )
}
