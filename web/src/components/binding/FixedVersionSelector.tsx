// Phase 5-4 — fixed version selector (§5.1). Pins a binding to a specific
// asset version. Per §5.1, a pinned version MUST be ready (build_status=ready)
// AND published (governance_status=published); any other state is not
// selectable. If a previously-pinned version is no longer usable (revoked /
// superseded / building), the UI shows the阻断态 — block + alert — rather
// than silently falling back to the latest published version.
//
// The selector consumes §4.4 GET /knowledge/assets/{id}/versions (the only
// permitted data source — architecture red line §4.4 + §9).
import { useEffect, useState } from "react"
import { Loader2, Pin, AlertTriangle } from "lucide-react"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { apiGetAssetVersions } from "@/api/assets"
import { PinnedBlockedBanner } from "./binding-primitives"
import type { KnowledgeAssetVersion } from "@/types/assets"

/** A version is pinable iff ready AND published (§5.1). */
function isPinable(v: KnowledgeAssetVersion): boolean {
  return v.build_status === "ready" && v.governance_status === "published"
}

export function FixedVersionSelector({
  assetId,
  pinnedVersionId,
  onValueChange,
  disabled,
}: {
  assetId: string | null
  pinnedVersionId: string | null
  onValueChange: (versionId: string | null) => void
  disabled?: boolean
}) {
  const [versions, setVersions] = useState<KnowledgeAssetVersion[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  // Load versions when the asset changes. Only ready+published versions are
  // selectable; others are filtered out per §5.1. The synchronous setState
  // calls below mark the load as in-progress before the first await, which is
  // exactly the pattern react-hooks/set-state-in-effect flags but is the
  // intended behavior for a data-fetch effect (same precedent as
  // AssetDetailPage).
  useEffect(() => {
    let cancelled = false
    if (!assetId) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setVersions([])
      return
    }
    setIsLoading(true)
    setError(null)
    void (async () => {
      try {
        const page = await apiGetAssetVersions(assetId, null, 50)
        if (!cancelled) setVersions(page.items ?? [])
      } catch {
        if (!cancelled) setError("加载版本失败")
      } finally {
        if (!cancelled) setIsLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [assetId])

  const pinable = versions.filter(isPinable)
  const pinned = pinnedVersionId
    ? versions.find((v) => v.id === pinnedVersionId) ?? null
    : null

  // §5.1: a pinned version that is no longer ready+published is BLOCKED —
  // the UI surfaces the block + alert, never a silent fallback.
  const blocked =
    pinnedVersionId !== null && pinned !== null && !isPinable(pinned)
  // The pinned id refers to a version that no longer exists at all — also a
  // block (the version was deleted/revoked entirely).
  const ghostBlocked =
    pinnedVersionId !== null && pinned === null && !isLoading && !error

  if (isLoading) {
    return (
      <div className="flex items-center gap-2 text-xs text-muted-foreground">
        <Loader2 className="size-3 animate-spin" />
        加载版本…
      </div>
    )
  }

  if (error) {
    return (
      <p className="flex items-center gap-1 text-xs text-destructive">
        <AlertTriangle className="size-3" />
        {error}
      </p>
    )
  }

  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        <Pin className="size-3.5 text-muted-foreground" />
        <Select
          value={pinnedVersionId ?? undefined}
          onValueChange={(v) => onValueChange(v)}
          disabled={disabled || pinable.length === 0}
        >
          <SelectTrigger className="h-8 text-sm">
            <SelectValue
              placeholder={
                pinable.length === 0
                  ? "无可固定版本"
                  : "选择固定版本"
              }
            />
          </SelectTrigger>
          <SelectContent>
            {pinable.map((v) => (
              <SelectItem key={v.id} value={v.id}>
                v{v.version_no} · {v.governance_status}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      {pinable.length === 0 && !blocked && !ghostBlocked && (
        <p className="text-[11px] text-muted-foreground">
          仅可选择 build_status=ready 且 governance_status=published 的版本（§5.1）。
          当前资产无符合条件的版本。
        </p>
      )}

      {blocked && pinned && (
        <PinnedBlockedBanner
          reason={`固定版本 v${pinned.version_no} 当前状态：build=${pinned.build_status}，governance=${pinned.governance_status}，不再是 ready+published。`}
        />
      )}

      {ghostBlocked && (
        <PinnedBlockedBanner reason="固定版本已不存在（已被删除或撤权）。" />
      )}
    </div>
  )
}
