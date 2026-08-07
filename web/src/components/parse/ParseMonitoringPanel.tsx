// ParseMonitoringPanel — screen 4 (UI spec §3.4). A workspace-level parse
// monitoring view: KPI cards (pending / parsing / indexed / failed), filters,
// and a document table with retry / reparse / view-detail actions. Rendered
// as a full-content panel (admin-visible) toggled from the layout header.
import { useMemo, useState } from "react"
import {
  Clock,
  Loader,
  Check,
  AlertTriangle,
  Inbox,
  RefreshCw,
  Eye,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { ScrollArea } from "@/components/ui/scroll-area"
import { useMoraStore } from "@/stores/mora"
import { useParseStore } from "@/stores/parse"
import { apiGetParseProgress } from "@/api/parse"
import { ParseStatusBadge } from "./ParseStatusBadge"
import { buildTreeFlatten, FORMAT_LABELS } from "@/lib/parse-constants"
import { cn } from "@/lib/utils"
import type { ParseStatus, ParseProgress } from "@/types/parse"
import type { TreeNode } from "@/types"

// KPI card: a discrete object card, repeated — no nesting (UI spec §1.3).
function KpiCard({
  icon,
  count,
  label,
  tone,
}: {
  icon: React.ReactNode
  count: number
  label: string
  tone: "neutral" | "destructive"
}) {
  return (
    <div className="flex items-center gap-3 rounded-lg border bg-surface px-4 py-3">
      <span
        className={cn(
          "flex size-9 items-center justify-center rounded-md",
          tone === "destructive" ? "bg-destructive/10 text-destructive" : "bg-muted text-muted-foreground",
        )}
      >
        {icon}
      </span>
      <div>
        <div className={cn("text-2xl font-semibold leading-none", tone === "destructive" && "text-destructive")}>
          {count}
        </div>
        <div className="text-xs text-muted-foreground mt-1">{label}</div>
      </div>
    </div>
  )
}

export function ParseMonitoringPanel() {
  const { tree, currentWorkspace } = useMoraStore()
  const { openProgress, openReparse } = useParseStore()
  const [statusFilter, setStatusFilter] = useState<string>("all")
  const [query, setQuery] = useState("")
  const [selected, setSelected] = useState<Set<string>>(new Set())
  // Per-doc progress cache (fetched on demand; in a real deployment the
  // workspace-wide progress endpoint feeds this — see architecture §6.3).
  const [progressCache, setProgressCache] = useState<Record<string, ParseProgress | null>>({})
  const [loadingIds, setLoadingIds] = useState<Set<string>>(new Set())

  // Flatten the tree to docs for the monitoring table. The monitoring surface
  // mirrors the tree until a dedicated parse-progress listing lands.
  const docs = useMemo(() => buildTreeFlatten(tree as TreeNode[]), [tree])

  const docsInProgress = docs.filter((d) => {
    const p = progressCache[d.id]
    return p && (p.parse_status === "pending" || p.parse_status === "parsing")
  })
  const docsIndexed = docs.filter((d) => progressCache[d.id]?.parse_status === "indexed")
  const docsFailed = docs.filter((d) => progressCache[d.id]?.parse_status === "failed")
  const docsPending = docs.filter((d) => progressCache[d.id]?.parse_status === "pending")

  const filtered = docs.filter((d) => {
    if (query && !d.name.toLowerCase().includes(query.toLowerCase())) return false
    if (statusFilter !== "all") {
      const p = progressCache[d.id]
      if (statusFilter === "none" && p) return false
      if (statusFilter !== "none" && (!p || p.parse_status !== statusFilter)) return false
    }
    return true
  })
  // Failed-first sort (UI spec §3.4: default sort = failed-first).
  filtered.sort((a, b) => {
    const af = progressCache[a.id]?.parse_status === "failed" ? 0 : 1
    const bf = progressCache[b.id]?.parse_status === "failed" ? 0 : 1
    return af - bf
  })

  const refreshDoc = async (id: string) => {
    setLoadingIds((s) => new Set(s).add(id))
    try {
      const p = await apiGetParseProgress(id)
      setProgressCache((c) => ({ ...c, [id]: p }))
    } catch {
      // 404 = no parse record yet or no read permission — treat as "none".
      setProgressCache((c) => ({ ...c, [id]: null }))
    } finally {
      setLoadingIds((s) => { const n = new Set(s); n.delete(id); return n })
    }
  }

  // Eagerly load progress for the first page so the KPI cards aren't empty on
  // first paint. This is bounded to the visible page, not the whole workspace.
  const visible = filtered.slice(0, 50)

  const toggleSelect = (id: string) =>
    setSelected((s) => {
      const n = new Set(s)
      if (n.has(id)) n.delete(id)
      else n.add(id)
      return n
    })

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center justify-between border-b px-6 py-4">
        <div>
          <h1 className="text-xl font-semibold">解析监控</h1>
          <p className="text-sm text-muted-foreground">{currentWorkspace?.name || ""}</p>
        </div>
        <Button variant="outline" size="sm" onClick={() => visible.forEach((d) => refreshDoc(d.id))}>
          <RefreshCw className="size-3.5" /> 刷新
        </Button>
      </div>

      {/* KPI row */}
      <div className="grid grid-cols-2 gap-3 px-6 py-4 sm:grid-cols-4">
        <KpiCard icon={<Clock className="size-4" />} count={docsPending.length} label="待解析" tone="neutral" />
        <KpiCard icon={<Loader className="size-4" />} count={docsInProgress.length} label="解析中" tone="neutral" />
        <KpiCard icon={<Check className="size-4" />} count={docsIndexed.length} label="已索引" tone="neutral" />
        <KpiCard icon={<AlertTriangle className="size-4" />} count={docsFailed.length} label="失败" tone="destructive" />
      </div>

      {/* Filter strip */}
      <div className="flex flex-wrap items-center gap-2 border-b px-6 py-3">
        <Input
          placeholder="按文件名筛选…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          className="h-8 w-56 text-sm"
          aria-label="按文件名筛选"
        />
        <Select value={statusFilter} onValueChange={setStatusFilter}>
          <SelectTrigger className="h-8 w-40 text-sm">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">全部状态</SelectItem>
            <SelectItem value="failed">失败优先</SelectItem>
            <SelectItem value="parsing">解析中</SelectItem>
            <SelectItem value="indexed">已索引</SelectItem>
            <SelectItem value="none">未解析</SelectItem>
          </SelectContent>
        </Select>
        {selected.size > 0 && (
          <Button
            variant="outline"
            size="sm"
            onClick={() => { openReparse(Array.from(selected)); setSelected(new Set()) }}
          >
            <RefreshCw className="size-3.5" /> 重解析 {selected.size} 个
          </Button>
        )}
      </div>

      {/* Document table */}
      <ScrollArea className="flex-1">
        {visible.length === 0 ? (
          <div className="flex flex-col items-center justify-center py-16 text-center">
            <Inbox className="size-12 text-muted-foreground/40" />
            <h3 className="mt-3 text-sm font-medium">该工作区暂无解析记录</h3>
            <p className="mt-1 text-xs text-muted-foreground">上传文档后，解析记录将在此展示</p>
          </div>
        ) : (
          <div className="px-6">
            {/* header row */}
            <div className="grid grid-cols-[1fr_80px_80px_120px_80px] gap-2 border-b py-2 text-xs font-medium text-muted-foreground">
              <span>文件名</span>
              <span>格式</span>
              <span>状态</span>
              <span>最近解析</span>
              <span className="text-right">操作</span>
            </div>
            {visible.map((doc) => {
              const p = progressCache[doc.id]
              const isLoading = loadingIds.has(doc.id)
              return (
                <div
                  key={doc.id}
                  className="grid grid-cols-[1fr_80px_80px_120px_80px] items-center gap-2 border-b py-2 text-sm hover:bg-accent/30"
                >
                  <label className="flex items-center gap-2 truncate">
                    <input
                      type="checkbox"
                      className="size-3.5 accent-primary"
                      checked={selected.has(doc.id)}
                      onChange={() => toggleSelect(doc.id)}
                      aria-label={`选择 ${doc.name}`}
                    />
                    <span className="truncate">{doc.name}</span>
                  </label>
                  <span>{doc.format ? (FORMAT_LABELS[doc.format as keyof typeof FORMAT_LABELS] || doc.format) : "—"}</span>
                  <span>
                    {p ? (
                      <ParseStatusBadge status={p.parse_status as ParseStatus} animated />
                    ) : (
                      <Badge variant="secondary" className="text-[10px]">未解析</Badge>
                    )}
                  </span>
                  <span className="text-xs text-muted-foreground">
                    {p?.updated_at ? new Date(p.updated_at).toLocaleString("zh-CN") : "—"}
                  </span>
                  <span className="flex items-center justify-end gap-1">
                    <Button
                      variant="ghost"
                      size="icon"
                      className="size-6"
                      onClick={() => openProgress(doc.id)}
                      aria-label="查看详情"
                    >
                      <Eye className="size-3.5" />
                    </Button>
                    <Button
                      variant="ghost"
                      size="icon"
                      className="size-6"
                      onClick={() => refreshDoc(doc.id)}
                      aria-label="刷新进度"
                      disabled={isLoading}
                    >
                      <RefreshCw className={cn("size-3.5", isLoading && "animate-spin")} />
                    </Button>
                  </span>
                </div>
              )
            })}
            {docsFailed.length > 0 && (
              <div className="flex items-center justify-between rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 mt-3 text-sm">
                <span className="text-destructive">{docsFailed.length} 个文档解析失败</span>
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => { void docsFailed.forEach((d) => void refreshDoc(d.id)); openReparse(docsFailed.map((d) => d.id)) }}
                >
                  全部重试
                </Button>
              </div>
            )}
          </div>
        )}
      </ScrollArea>
    </div>
  )
}
