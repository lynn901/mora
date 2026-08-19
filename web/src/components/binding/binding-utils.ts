// Pure helpers for the Phase 5-4 配装管理 UI — no JSX, so this file exports
// constants and functions without tripping react-refresh/only-export-components.
//
// Status colors follow the Mora Design Language V3.1 §3.5 rule: status is
// never conveyed by color alone — every badge pairs a color with an icon +
// text label, so a viewer who can't distinguish the hue still reads the
// state.
import {
  CheckCircle2,
  XCircle,
  Clock,
  AlertTriangle,
  Ban,
  ShieldCheck,
  ShieldOff,
  Pin,
  GitBranch,
  type LucideIcon,
} from "lucide-react"
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
import type { BadgeVariant } from "@/components/assets/asset-utils"

export type { BadgeVariant }

export interface StatusMeta {
  label: string
  variant: BadgeVariant
  icon: LucideIcon
  /** Short semantic hint shown under the selector (§8 delivery semantics). */
  hint?: string
}

// ---- effect (allow / deny) ------------------------------------------------

export const BINDING_EFFECT: Record<BindingEffect, StatusMeta> = {
  allow: {
    label: "允许",
    variant: "success",
    icon: ShieldCheck,
    hint: "授予 Agent 访问该范围的能力（只能缩小，不扩大权限）",
  },
  deny: {
    label: "拒绝",
    variant: "destructive",
    icon: ShieldOff,
    hint: "显式排除该范围——deny 优先于所有 allow",
  },
}

// ---- scope_kind -----------------------------------------------------------

export const BINDING_SCOPE: Record<BindingScopeKind, StatusMeta> = {
  asset: {
    label: "资产",
    variant: "secondary",
    icon: Pin,
    hint: "绑定到单个资产（可固定版本）",
  },
  workspace: {
    label: "工作空间",
    variant: "secondary",
    icon: GitBranch,
    hint: "绑定到工作空间内全部资产",
  },
  asset_type: {
    label: "资产类型",
    variant: "secondary",
    icon: GitBranch,
    hint: "绑定到某类资产（如全部 skill）",
  },
}

// ---- version_policy -------------------------------------------------------

export const VERSION_POLICY: Record<BindingVersionPolicy, StatusMeta> = {
  follow_published: {
    label: "跟随已发布",
    variant: "secondary",
    icon: GitBranch,
    hint: "解析为资产当前已发布版本，版本升级自动跟随",
  },
  pinned: {
    label: "固定版本",
    variant: "info",
    icon: Pin,
    hint: "冻结到指定版本；该版本撤权时阻断使用，不回退到最新版",
  },
}

// ---- delivery_mode (§5.3) ------------------------------------------------

export const DELIVERY_MODE: Record<BindingDeliveryMode, StatusMeta> = {
  tool: {
    label: "Tool",
    variant: "default",
    icon: CheckCircle2,
    hint: "交付 SKILL.md 头部 + 资源清单（完整工具语义）",
  },
  summary: {
    label: "Summary",
    variant: "secondary",
    icon: CheckCircle2,
    hint: "只交付描述 + 资源清单（摘要语义）",
  },
  inline: {
    label: "Inline",
    variant: "info",
    icon: CheckCircle2,
    hint: "渐进式资源按需读取（内联语义）",
  },
}

// ---- skill validation_status (§4.3) --------------------------------------

export const VALIDATION_STATUS: Record<SkillValidationStatus, StatusMeta> = {
  pending: { label: "待校验", variant: "secondary", icon: Clock },
  passed: {
    label: "校验通过",
    variant: "success",
    icon: CheckCircle2,
  },
  failed: { label: "校验失败", variant: "destructive", icon: XCircle },
  opaque: { label: "不透明", variant: "warning", icon: AlertTriangle },
}

export const VALIDATION_SEVERITY: Record<ValidationSeverity, StatusMeta> = {
  block: { label: "阻断", variant: "destructive", icon: Ban },
  warn: { label: "警告", variant: "warning", icon: AlertTriangle },
  info: { label: "提示", variant: "info", icon: CheckCircle2 },
}

// ---- compatibility_report.delivery (§4.3) --------------------------------

export const DELIVERY_VERDICT: Record<DeliveryVerdict, StatusMeta> = {
  lossless: {
    label: "无损",
    variant: "success",
    icon: CheckCircle2,
    hint: "agentskills.io 全量理解，可无损交付",
  },
  runtime_adaptation_needed: {
    label: "需运行时适配",
    variant: "warning",
    icon: AlertTriangle,
    hint: "hermes 扩展字段已保留，运行时需自行适配",
  },
  incompatible: {
    label: "不兼容",
    variant: "destructive",
    icon: XCircle,
    hint: "无法交付",
  },
}

// ---- agent status ---------------------------------------------------------

export const AGENT_STATUS: Record<AgentStatus, StatusMeta> = {
  active: { label: "活跃", variant: "success", icon: CheckCircle2 },
  suspended: { label: "已暂停", variant: "warning", icon: Clock },
  revoked: { label: "已吊销", variant: "destructive", icon: XCircle },
}

/** Format an ISO timestamp as a short locale string; "—" if null/invalid. */
export function fmtTime(iso: string | null | undefined): string {
  if (!iso) return "—"
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return "—"
  return d.toLocaleString(undefined, {
    year: "numeric",
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  })
}

export function errMsg(e: unknown): string {
  if (e instanceof Error) return e.message
  return "请求失败"
}

/**
 * Generate a per-batch Idempotency-Key (§5.2). Uses the Web Crypto API when
 * available; falls back to a timestamp+random token. The key must be unique
 * per logical batch so a retry returns the original result instead of creating
 * duplicates.
 */
export function newIdempotencyKey(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID()
  }
  return `bid_${Date.now()}_${Math.random().toString(36).slice(2, 10)}`
}
