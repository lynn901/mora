// Phase 1-6 — unified asset list (§4.4 GET /knowledge/assets).
//
// Data source is the REST API only (architecture red line: frontend never
// reads projection tables directly). Cursor-paginated; filters by asset_type
// and status. State-complete: loading / empty / error / forbidden-empty.
import { useState } from "react"
import { Boxes, FileText, GitBranch, Brain, Sparkles } from "lucide-react"
import { Button } from "@/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { ScrollArea } from "@/components/ui/scroll-area"
import { useMoraStore } from "@/stores/mora"
import { useAssetList } from "./asset-hooks"
import { ASSET_TYPE_LABEL } from "./asset-utils"
import {
  AssetStatusBadge,
  ErrorState,
  ForbiddenEmpty,
} from "./asset-primitives"
import type { AssetStatus, AssetType } from "@/types/assets"

const TYPE_ICONS: Record<AssetType, React.ReactNode> = {
  document: <FileText className="size-4 text-info" />,
  codebase: <GitBranch className="size-4 text-info" />,
  memory: <Brain className="size-4 text-info" />,
  skill: <Sparkles className="size-4 text-info" />,
}

const ALL = "all"

export function AssetListPage({
  onOpenAsset,
}: {
  onOpenAsset: (assetId: string) => void
}) {
  const { currentWorkspace } = useMoraStore()
  const [assetType, setAssetType] = useState<AssetType | "all">(ALL)
  const [status, setStatus] = useState<AssetStatus | "all">(ALL)

  const list = useAssetList(currentWorkspace?.id ?? null, {
    asset_type: assetType === ALL ? null : assetType,
    status: status === ALL ? null : status,
  })

  if (!currentWorkspace) {
    return (
      <div className="flex h-full items-center justify-center p-8 text-sm text-muted-foreground">
        请先选择工作空间
      </div>
    )
  }

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2 border-b px-4 py-3">
        <Boxes className="size-4" />
        <h2 className="text-sm font-semibold">资产</h2>
        <span className="text-xs text-muted-foreground">
          {ASSET_TYPE_LABEL[assetType === ALL ? "document" : assetType] !==
            undefined && currentWorkspace.name}
        </span>
        <div className="ml-auto flex items-center gap-2">
          <Select value={assetType} onValueChange={(v) => setAssetType(v as AssetType | "all")}>
            <SelectTrigger className="h-8 w-[120px] text-xs">
              <SelectValue placeholder="类型" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>全部类型</SelectItem>
              <SelectItem value="document">文档</SelectItem>
              <SelectItem value="codebase">代码库</SelectItem>
              <SelectItem value="memory">记忆</SelectItem>
              <SelectItem value="skill">技能</SelectItem>
            </SelectContent>
          </Select>
          <Select value={status} onValueChange={(v) => setStatus(v as AssetStatus | "all")}>
            <SelectTrigger className="h-8 w-[120px] text-xs">
              <SelectValue placeholder="状态" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ALL}>全部状态</SelectItem>
              <SelectItem value="draft">草稿</SelectItem>
              <SelectItem value="published">已发布</SelectItem>
              <SelectItem value="archived">已归档</SelectItem>
              <SelectItem value="rejected">已拒绝</SelectItem>
            </SelectContent>
          </Select>
        </div>
      </div>

      <div className="flex-1 overflow-hidden">
        {list.isLoading ? (
          <div className="flex h-full items-center justify-center">
            <div className="mx-auto size-8 animate-spin rounded-full border-2 border-primary border-t-transparent" />
          </div>
        ) : list.notFound ? (
          <ForbiddenEmpty
            message="无权查看本工作空间资产或暂无资产"
            hint="数据源受 RBAC 约束，如需访问请联系管理员。"
          />
        ) : list.error ? (
          <ErrorState message={list.error} onRetry={() => void list.reload()} />
        ) : list.items.length === 0 ? (
          <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
            <Boxes className="size-10 text-muted-foreground/50" />
            <p className="text-sm font-medium text-muted-foreground">暂无资产</p>
            <p className="text-xs text-muted-foreground/70">
              文档写入或来源同步后，资产将在此列出。
            </p>
          </div>
        ) : (
          <ScrollArea className="h-full">
            <ul className="divide-y">
              {list.items.map((a) => (
                <li key={a.id}>
                  <button
                    onClick={() => onOpenAsset(a.id)}
                    className="flex w-full items-center gap-3 px-4 py-2.5 text-left transition-colors hover:bg-accent"
                  >
                    <span className="shrink-0">
                      {TYPE_ICONS[a.asset_type] ?? <FileText className="size-4 text-info" />}
                    </span>
                    <div className="min-w-0 flex-1">
                      <p className="truncate text-sm font-medium">{a.name}</p>
                      <p className="truncate text-xs text-muted-foreground">
                        {ASSET_TYPE_LABEL[a.asset_type]} · v{a.latest_requested_version_no}
                      </p>
                    </div>
                    <AssetStatusBadge status={a.status} />
                  </button>
                </li>
              ))}
            </ul>
            {list.hasMore && (
              <div className="flex justify-center p-3">
                <Button
                  size="sm"
                  variant="outline"
                  className="h-7 text-xs"
                  disabled={list.isLoadingMore}
                  onClick={() => void list.loadMore()}
                >
                  {list.isLoadingMore ? "加载中…" : "加载更多"}
                </Button>
              </div>
            )}
          </ScrollArea>
        )}
      </div>
    </div>
  )
}
