// Presentational components for the Phase 5-4 配装管理 UI — this file exports
// only components, so react-refresh/only-export-components stays happy.
//
// Status colors follow the Mora Design Language V3.1 §3.5 rule: status is
// never conveyed by color alone — every colored badge also carries an icon
// and a text label, so a viewer who can't distinguish the hue still reads
// the state.
import { AlertTriangle, Ban, FileQuestion } from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import type {
  AgentStatus,
  BindingDeliveryMode,
  BindingEffect,
  BindingScopeKind,
  BindingVersionPolicy,
  DeliveryVerdict,
  SkillValidationStatus,
  ValidationSeverity,
} from "@/types/binding"
import {
  AGENT_STATUS,
  BINDING_EFFECT,
  BINDING_SCOPE,
  DELIVERY_MODE,
  DELIVERY_VERDICT,
  VALIDATION_SEVERITY,
  VALIDATION_STATUS,
  VERSION_POLICY,
  type StatusMeta,
} from "./binding-utils"

function StatusBadge<T extends string>({
  status,
  map,
}: {
  status: T
  map: Record<T, StatusMeta>
}) {
  const m = map[status]
  if (!m) return null
  const Icon = m.icon
  return (
    <Badge variant={m.variant}>
      <Icon className={m.variant === "info" ? "size-3 animate-spin" : "size-3"} />
      {m.label}
    </Badge>
  )
}

export function EffectBadge({ status }: { status: BindingEffect }) {
  return <StatusBadge status={status} map={BINDING_EFFECT} />
}

export function ScopeBadge({ status }: { status: BindingScopeKind }) {
  return <StatusBadge status={status} map={BINDING_SCOPE} />
}

export function VersionPolicyBadge({ status }: { status: BindingVersionPolicy }) {
  return <StatusBadge status={status} map={VERSION_POLICY} />
}

export function DeliveryModeBadge({ status }: { status: BindingDeliveryMode }) {
  return <StatusBadge status={status} map={DELIVERY_MODE} />
}

export function ValidationStatusBadge({ status }: { status: SkillValidationStatus }) {
  return <StatusBadge status={status} map={VALIDATION_STATUS} />
}

export function SeverityBadge({ status }: { status: ValidationSeverity }) {
  return <StatusBadge status={status} map={VALIDATION_SEVERITY} />
}

export function DeliveryVerdictBadge({ status }: { status: DeliveryVerdict }) {
  return <StatusBadge status={status} map={DELIVERY_VERDICT} />
}

export function AgentStatusBadge({ status }: { status: AgentStatus }) {
  return <StatusBadge status={status} map={AGENT_STATUS} />
}

/**
 * §5.1 阻断态: a pinned binding whose pinned version is revoked/missing
 * MUST surface a block+alert, not silently fall back to the latest version.
 * This badge renders that阻断 state — it is destructive (red) with a Ban
 * icon + text, never conveyed by color alone.
 */
export function PinnedBlockedBadge() {
  return (
    <Badge variant="destructive">
      <Ban className="size-3" />
      版本已阻断
    </Badge>
  )
}

/** §11.4: a 404 on a binding read means "no permission or does not exist" —
 * the two are indistinguishable by design. Renders a calm empty state. */
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
        <Button variant="outline" size="sm" className="mt-1" onClick={onRetry}>
          重试
        </Button>
      )}
    </div>
  )
}

/**
 * §5.1 block+alert banner for pinned bindings whose version is not usable.
 * Placed at the top of the binding row / detail so the operator sees the
 * block before acting. Explains the阻断 explicitly: use is blocked until
 * the version is restored or the binding is repinned — there is no fallback.
 */
export function PinnedBlockedBanner({ reason }: { reason: string }) {
  return (
    <div
      className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive"
      role="alert"
    >
      <Ban className="mt-0.5 size-4 shrink-0" />
      <div className="space-y-0.5">
        <p className="font-medium">固定版本已阻断（不回退）</p>
        <p className="text-destructive/80">{reason}</p>
        <p className="text-destructive/70">
          使用将被阻断直至该版本恢复或重新固定——不自动回退到最新版本（§5.1）。
        </p>
      </div>
    </div>
  )
}
