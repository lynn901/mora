// Pure helpers for the Phase 1 asset UI — no JSX, so this file exports
// constants and functions without tripping react-refresh/only-export-components.
//
// Status colors follow the Mora Design Language V3.1 §3.5 rule: status is
// never conveyed by color alone — every badge pairs a color with an icon +
// text label, so a viewer who can't distinguish the hue still reads the
// state. The icon is stored as a component reference and rendered by the
// badge components in asset-primitives.tsx.
import {
  CheckCircle2,
  XCircle,
  Clock,
  Loader2,
  Archive,
  AlertTriangle,
  type LucideIcon,
} from "lucide-react"
import type {
  AssetStatus,
  AssetType,
  BuildStatus,
  GovernanceStatus,
} from "@/types/assets"

export type BadgeVariant =
  | "default"
  | "secondary"
  | "destructive"
  | "outline"
  | "success"
  | "warning"
  | "info"

export interface StatusMeta {
  label: string
  variant: BadgeVariant
  icon: LucideIcon
}

export const ASSET_STATUS: Record<AssetStatus, StatusMeta> = {
  draft: { label: "草稿", variant: "secondary", icon: Clock },
  published: { label: "已发布", variant: "success", icon: CheckCircle2 },
  archived: { label: "已归档", variant: "outline", icon: Archive },
  rejected: { label: "已拒绝", variant: "destructive", icon: XCircle },
}

export const BUILD_STATUS: Record<BuildStatus, StatusMeta> = {
  pending: { label: "待构建", variant: "secondary", icon: Clock },
  building: { label: "构建中", variant: "info", icon: Loader2 },
  ready: { label: "就绪", variant: "success", icon: CheckCircle2 },
  failed: { label: "失败", variant: "destructive", icon: AlertTriangle },
  stale: { label: "过期", variant: "warning", icon: Clock },
}

export const GOVERNANCE_STATUS: Record<GovernanceStatus, StatusMeta> = {
  candidate: { label: "待审核", variant: "warning", icon: Clock },
  published: { label: "已发布", variant: "success", icon: CheckCircle2 },
  rejected: { label: "已拒绝", variant: "destructive", icon: XCircle },
  deprecated: { label: "已废弃", variant: "outline", icon: Archive },
}

export const ASSET_TYPE_LABEL: Record<AssetType, string> = {
  document: "文档",
  codebase: "代码库",
  memory: "记忆",
  skill: "技能",
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
