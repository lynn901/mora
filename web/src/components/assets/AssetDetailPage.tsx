// Phase 1-6 — asset detail + version history (§4.4 GET /knowledge/assets/{id}
// and GET /knowledge/assets/{id}/versions).
//
// Version history renders only what the version API returns — no direct
// projection-table reads (architecture red line). Per §11.4, a stale/building
// version surfaces as a non-error notice with the last available version;
// a 404 on the asset read renders as an existence-empty state.
import { useEffect, useState } from "react"
import { ArrowLeft, AlertTriangle, History, GitBranch } from "lucide-react"
import { Button } from "@/components/ui/button"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import {
  apiGetAsset,
  isNotFoundOrForbidden,
} from "@/api/assets"
import { ApiError } from "@/api/client"
import { useAssetVersions } from "./asset-hooks"
import { ASSET_TYPE_LABEL, fmtTime } from "./asset-utils"
import {
  AssetStatusBadge,
  BuildStatusBadge,
  ErrorState,
  ForbiddenEmpty,
  GovernanceStatusBadge,
} from "./asset-primitives"
import type {
  AssetDetail,
  KnowledgeAssetVersion,
  VersionFreshness,
} from "@/types/assets"

const FRESHNESS_LABEL: Record<VersionFreshness, string> = {
  fresh: "最新",
  stale: "版本已过期，显示最后可用版本",
  building: "版本构建中，显示最后可用版本",
}

export function AssetDetailPage({
  assetId,
  onBack,
}: {
  assetId: string
  onBack: () => void
}) {
  const [detail, setDetail] = useState<AssetDetail | null>(null)
  const [isLoading, setIsLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [notFound, setNotFound] = useState(false)
  const [reloadNonce, setReloadNonce] = useState(0)

  // Synchronize with external API state — load the asset when its id (or a
  // manual retry) changes. Fetching is the canonical use case for an effect;
  // the synchronous setState calls below mark the load as in-progress before
  // the first await, which is exactly the pattern react-hooks/set-state-in-effect
  // flags but is the intended behavior for a data-fetch effect.
  useEffect(() => {
    let cancelled = false
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setError(null)
    setNotFound(false)
    setIsLoading(true)
    void (async () => {
      try {
        const d = await apiGetAsset(assetId)
        if (!cancelled) setDetail(d)
      } catch (e) {
        if (cancelled) return
        if (isNotFoundOrForbidden(e)) {
          setNotFound(true)
          setDetail(null)
        } else if (e instanceof ApiError) {
          setError(e.message)
        } else {
          setError("加载资产失败")
        }
      } finally {
        if (!cancelled) setIsLoading(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [assetId, reloadNonce])

  const versions = useAssetVersions(assetId)

  if (isLoading) {
    return (
      <div className="flex h-full items-center justify-center">
        <div className="mx-auto size-8 animate-spin rounded-full border-2 border-primary border-t-transparent" />
      </div>
    )
  }

  if (notFound) {
    return (
      <div className="flex h-full flex-col">
        <BackBar onBack={onBack} />
        <ForbiddenEmpty
          message="无权查看此资产或资产不存在"
          hint="只读资源无权访问与不存在返回相同结果（不泄露存在性）。"
        />
      </div>
    )
  }

  if (error || !detail) {
    return (
      <div className="flex h-full flex-col">
        <BackBar onBack={onBack} />
        <ErrorState message={error ?? "加载资产失败"} onRetry={() => setReloadNonce((n) => n + 1)} />
      </div>
    )
  }

  const { asset, current_version: currentVersion, freshness } = detail
  const stale = freshness !== "fresh"

  return (
    <div className="flex h-full flex-col">
      <BackBar onBack={onBack} />
      <ScrollArea className="flex-1">
        <div className="mx-auto max-w-3xl px-6 py-6">
          {/* Header */}
          <div className="flex items-start gap-3">
            <div className="min-w-0 flex-1">
              <h1 className="truncate text-xl font-semibold">{asset.name}</h1>
              <p className="mt-1 text-xs text-muted-foreground">
                {ASSET_TYPE_LABEL[asset.asset_type]} · v{asset.latest_requested_version_no}
                {asset.native_document_id ? " · 原生文档" : ""}
              </p>
            </div>
            <AssetStatusBadge status={asset.status} />
          </div>

          {asset.description && (
            <p className="mt-3 whitespace-pre-wrap text-sm text-muted-foreground">
              {asset.description}
            </p>
          )}

          {/* Metadata grid */}
          <dl className="mt-5 grid grid-cols-2 gap-x-6 gap-y-2 text-xs sm:grid-cols-3">
            <Field label="所有者" value={`${asset.owner_type}:${asset.owner_id.slice(0, 8)}`} />
            <Field label="可见性" value={asset.visibility} />
            <Field label="置信度" value={asset.confidence != null ? String(asset.confidence) : "—"} />
            <Field label="生效时间" value={fmtTime(asset.valid_from)} />
            <Field label="过期时间" value={fmtTime(asset.expires_at)} />
            <Field label="创建于" value={fmtTime(asset.created_at)} />
          </dl>

          <Separator className="my-6" />

          {/* Current version */}
          <section>
            <h2 className="flex items-center gap-2 text-sm font-semibold">
              当前版本
              {stale && (
                <span className="inline-flex items-center gap-1 rounded-full bg-warning/10 px-2 py-0.5 text-xs font-medium text-warning">
                  <AlertTriangle className="size-3" />
                  {FRESHNESS_LABEL[freshness]}
                </span>
              )}
            </h2>
            {currentVersion ? (
              <VersionCard v={currentVersion} highlight={stale} />
            ) : (
              <p className="mt-2 text-sm text-muted-foreground">
                尚无可用版本。
              </p>
            )}
          </section>

          <Separator className="my-6" />

          {/* Version history */}
          <section>
            <h2 className="flex items-center gap-2 text-sm font-semibold">
              <History className="size-4" />
              版本历史
            </h2>
            <div className="mt-3">
              {versions.isLoading ? (
                <div className="flex items-center justify-center py-6">
                  <div className="size-6 animate-spin rounded-full border-2 border-primary border-t-transparent" />
                </div>
              ) : versions.notFound ? (
                <ForbiddenEmpty message="无权查看版本历史" />
              ) : versions.error ? (
                <ErrorState message={versions.error} onRetry={() => void versions.reload()} />
              ) : versions.items.length === 0 ? (
                <p className="py-4 text-sm text-muted-foreground">暂无版本记录。</p>
              ) : (
                <>
                  <ul className="divide-y rounded-md border">
                    {versions.items.map((v) => (
                      <li
                        key={v.id}
                        className="flex flex-wrap items-center gap-3 px-3 py-2.5"
                      >
                        <span className="font-mono text-xs text-muted-foreground">
                          v{v.version_no}
                        </span>
                        <BuildStatusBadge status={v.build_status} />
                        <GovernanceStatusBadge status={v.governance_status} />
                        <span className="ml-auto text-xs text-muted-foreground">
                          {fmtTime(v.created_at)}
                        </span>
                      </li>
                    ))}
                  </ul>
                  {versions.hasMore && (
                    <div className="mt-3 flex justify-center">
                      <Button
                        size="sm"
                        variant="outline"
                        className="h-7 text-xs"
                        disabled={versions.isLoadingMore}
                        onClick={() => void versions.loadMore()}
                      >
                        {versions.isLoadingMore ? "加载中…" : "加载更多"}
                      </Button>
                    </div>
                  )}
                </>
              )}
            </div>
          </section>
        </div>
      </ScrollArea>
    </div>
  )
}

function BackBar({ onBack }: { onBack: () => void }) {
  return (
    <div className="flex items-center gap-2 border-b px-4 py-2">
      <Button variant="ghost" size="sm" className="h-8" onClick={onBack}>
        <ArrowLeft className="size-4" /> 返回
      </Button>
    </div>
  )
}

function Field({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="truncate font-mono">{value}</dd>
    </div>
  )
}

function VersionCard({
  v,
  highlight,
}: {
  v: KnowledgeAssetVersion
  highlight: boolean
}) {
  return (
    <div
      className={
        "mt-2 rounded-md border p-3 " +
        (highlight ? "border-warning/40 bg-warning/5" : "")
      }
    >
      <div className="flex flex-wrap items-center gap-3">
        <span className="font-mono text-sm">v{v.version_no}</span>
        <BuildStatusBadge status={v.build_status} />
        <GovernanceStatusBadge status={v.governance_status} />
        <span className="ml-auto text-xs text-muted-foreground">
          {fmtTime(v.created_at)}
        </span>
      </div>
      {v.source_revision && (
        <p className="mt-2 flex items-center gap-1 text-xs text-muted-foreground">
          <GitBranch className="size-3" />
          {v.source_revision}
        </p>
      )}
    </div>
  )
}
