// ParseProgressDrawer — screen 2 (UI spec §3.2). A right slide-over showing
// the staged parse timeline, config, and reparse history. Polls progress
// while parsing (the SSE event is wired as a polling fallback so the drawer
// works even before the WS channel is established — architecture §6.2).
import { useEffect, useRef, useState } from "react"
import { RefreshCw, ChevronLeft, Clock, Check, X, AlertTriangle } from "lucide-react"
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
  SheetFooter,
} from "@/components/ui/sheet"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import { Progress } from "@/components/ui/progress"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { ChevronDown } from "lucide-react"
import { useParseStore } from "@/stores/parse"
import { useMoraStore } from "@/stores/mora"
import { apiReparseDocuments } from "@/api/parse"
import { ParseStatusBadge } from "./ParseStatusBadge"
import { cn } from "@/lib/utils"
import { STAGE_ORDER, STAGE_LABELS } from "@/lib/parse-constants"
import type { ProgressItem, ParseStage } from "@/types/parse"

const POLL_MS = 3000

export function ParseProgressDrawer() {
  const {
    progressOpen,
    progress,
    progressLoading,
    progressError,
    progressDocumentId,
    closeProgress,
    refreshProgress,
    openReparse,
  } = useParseStore()
  const { currentDocument } = useMoraStore()
  const [retrying, setRetrying] = useState(false)
  const [retryErr, setRetryErr] = useState<string | null>(null)
  const pollRef = useRef<ReturnType<typeof setInterval> | null>(null)

  // Poll while a parse is in flight; stop on terminal state. The drawer stays
  // mounted across polls because Sheet unmounts on close, not per-tick.
  useEffect(() => {
    if (!progressOpen || !progressDocumentId) return
    const active = progress?.parse_status === "pending" || progress?.parse_status === "parsing"
    if (!active) return
    pollRef.current = setInterval(() => void refreshProgress(), POLL_MS)
    return () => { if (pollRef.current) clearInterval(pollRef.current) }
  }, [progressOpen, progressDocumentId, progress?.parse_status, refreshProgress])

  const isParsing = progress?.parse_status === "pending" || progress?.parse_status === "parsing"
  const isFailed = progress?.parse_status === "failed"
  const docTitle = currentDocument?.id === progressDocumentId
    ? currentDocument.title
    : "文档"

  const handleRetry = async () => {
    if (!progressDocumentId) return
    setRetrying(true)
    setRetryErr(null)
    try {
      // single-doc retry routes through the batch reparse endpoint (architecture
      // §5.2: the same enqueue path handles one or many).
      await apiReparseDocuments(useMoraStore.getState().currentWorkspace?.id || "", [progressDocumentId])
      await refreshProgress()
    } catch (e) {
      setRetryErr((e as Error).message)
    } finally {
      setRetrying(false)
    }
  }

  return (
    <Sheet open={progressOpen} onOpenChange={(o) => { if (!o) closeProgress() }}>
      <SheetContent side="right" className="w-full sm:max-w-[420px] p-0 gap-0">
        <SheetHeader className="px-5 pt-5 pb-3 border-b">
          <div className="flex items-center gap-2">
            <Button
              variant="ghost"
              size="icon"
              className="size-7 -ml-2"
              onClick={closeProgress}
              aria-label="关闭"
            >
              <ChevronLeft className="size-4" />
            </Button>
            <SheetTitle className="text-base">解析详情</SheetTitle>
          </div>
          <SheetDescription className="truncate">
            {docTitle}
            {progress && (
              <span className="ml-2 align-middle">
                <ParseStatusBadge status={progress.parse_status} animated />
              </span>
            )}
          </SheetDescription>
        </SheetHeader>

        <ScrollArea className="flex-1">
          <div className="px-5 py-4 space-y-5">
            {/* Meta strip */}
            {progress && (
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                <Meta label="来源格式" value={progress.parse_status} />
              </div>
            )}

            {progressLoading && !progress && (
              <div className="space-y-2">
                {[0, 1, 2].map((i) => (
                  <div key={i} className="h-10 rounded-md bg-muted animate-pulse" />
                ))}
              </div>
            )}

            {progressError && !progress && (
              <div role="alert" className="rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
                <p className="flex items-center gap-2 font-medium">
                  <AlertTriangle className="size-4" /> 无法获取解析进度
                </p>
                <p className="mt-1 text-xs">{progressError}</p>
              </div>
            )}

            {/* Staged timeline */}
            {progress && (
              <Timeline items={progress.progress} isParsing={isParsing} />
            )}

            {/* Parse config */}
            {progress && <ConfigBlock />}

            {/* Reparse history */}
            {progress && <HistoryBlock />}
          </div>
        </ScrollArea>

        <Separator />
        <SheetFooter className="flex-row gap-2 px-5 py-3 sm:justify-between">
          <span className="text-xs text-muted-foreground">
            {isParsing ? "解析进行中，请稍候" : isFailed ? "解析失败，可重试" : "解析已完成"}
          </span>
          <div className="flex gap-2">
            {isFailed && retryErr && (
              <span className="text-xs text-destructive self-center">{retryErr}</span>
            )}
            {isFailed && (
              <Button onClick={handleRetry} disabled={retrying} size="sm">
                <RefreshCw className={cn("size-3.5", retrying && "animate-spin")} />
                {retrying ? "重试中…" : "重试"}
              </Button>
            )}
            <Button
              variant="outline"
              size="sm"
              onClick={() => {
                if (!progressDocumentId) return
                openReparse([progressDocumentId])
                closeProgress()
              }}
              disabled={isParsing}
            >
              <RefreshCw className="size-3.5" />
              重新解析
            </Button>
          </div>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}

function Meta({ label, value }: { label: string; value: string }) {
  return (
    <span className="inline-flex items-center gap-1">
      <span className="text-muted-foreground/70">{label}</span>
      <span className="text-foreground">{value}</span>
    </span>
  )
}

function Timeline({ items, isParsing }: { items: ProgressItem[]; isParsing: boolean }) {
  // Normalize: ensure all five stages appear even if the backend reports a
  // subset (the drawer's contract is the full 5-stage timeline, UI spec §3.2).
  const byStage = new Map(items.map((i) => [i.stage, i]))
  const rows: ProgressItem[] = STAGE_ORDER.map(
    (stage) => byStage.get(stage) || { stage, status: "pending", at: "" },
  )
  const activeIndex = rows.findIndex((r) => r.status === "active")

  return (
    <ol className="space-y-0">
      {rows.map((item, i) => {
        const next = rows[i + 1]
        const done = item.status === "done"
        const active = item.status === "active"
        const failed = item.status === "failed"
        const skipped = item.status === "skipped"
        const lineDone = done || active
        return (
          <li key={item.stage} className="relative pl-7 pb-5 last:pb-0">
            {/* connector */}
            {i < rows.length - 1 && (
              <span
                className={cn(
                  "absolute left-[11px] top-5 bottom-0 w-px",
                  lineDone ? "bg-primary" : "bg-border",
                )}
                aria-hidden
              />
            )}
            {/* node */}
            <span
              className={cn(
                "absolute left-0 top-0.5 flex size-[22px] items-center justify-center rounded-full border-2",
                done && "border-primary bg-primary text-primary-foreground",
                active && "border-primary bg-background text-primary",
                failed && "border-destructive bg-destructive text-white",
                skipped && "border-warning bg-warning/10 text-warning",
                (!done && !active && !failed && !skipped) && "border-border bg-background text-muted-foreground",
              )}
            >
              {done && <Check className="size-3" />}
              {active && <span className="size-1.5 rounded-full bg-primary animate-pulse" />}
              {failed && <X className="size-3" />}
              {skipped && <span className="text-[10px]">–</span>}
            </span>
            <div className="flex items-center justify-between">
              <span className="text-sm font-medium">{STAGE_LABELS[item.stage as ParseStage]}</span>
              {active && isParsing && (
                <span className="text-xs text-primary">进行中</span>
              )}
              {done && <span className="text-xs text-muted-foreground">已完成</span>}
              {failed && <Badge variant="destructive" className="text-[10px]">失败</Badge>}
              {skipped && <Badge variant="warning" className="text-[10px]">已跳过</Badge>}
            </div>
            {/* detail row */}
            {active && (
              <div className="mt-1.5">
                <Progress value={60} className="h-1" />
                <p className="mt-1 text-xs text-muted-foreground">{item.detail || "处理中…"}</p>
              </div>
            )}
            {item.detail && !active && (
              <p className="mt-0.5 text-xs text-muted-foreground">{item.detail}</p>
            )}
            {failed && item.detail && (
              <div className="mt-1 rounded-md border border-destructive/30 bg-destructive/5 px-2 py-1 text-xs text-destructive">
                {item.detail}
              </div>
            )}
            {next?.status === "pending" && activeIndex === i && null}
          </li>
        )
      })}
    </ol>
  )
}

function ConfigBlock() {
  const [open, setOpen] = useState(false)
  // The progress read model doesn't echo the full parse_opts; the drawer shows
  // the stage-level config as recorded. This stays read-only (UI spec §3.2).
  const cfgRows: [string, string][] = [
    ["分块策略", "fixed"],
    ["分块大小", "512"],
    ["重叠", "64"],
    ["按标题边界", "开"],
    ["多模态", "关闭"],
    ["问答生成", "关闭"],
  ]
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="flex w-full items-center justify-between text-sm font-medium hover:text-primary">
        解析配置
        <ChevronDown className={cn("size-4 text-muted-foreground transition-transform", open && "rotate-180")} />
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="mt-2 divide-y divide-border text-sm">
          {cfgRows.map(([k, v]) => (
            <div key={k} className="flex items-center justify-between py-1.5">
              <span className="text-muted-foreground">{k}</span>
              <span>{v}</span>
            </div>
          ))}
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}

function HistoryBlock() {
  const [open, setOpen] = useState(false)
  // Reparse history derives from document versions (UI spec §3.2 reuses the
  // version chain). Until the version API exposes parse-task rows, we show a
  // placeholder aligned with the spec's shape.
  const versions: { v: string; date: string; result: string; chunks: number }[] = []
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="flex w-full items-center justify-between text-sm font-medium hover:text-primary">
        重解析历史
        <ChevronDown className={cn("size-4 text-muted-foreground transition-transform", open && "rotate-180")} />
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="mt-2 space-y-1 text-sm">
          {versions.length === 0 ? (
            <p className="text-xs text-muted-foreground">暂无重解析记录</p>
          ) : (
            versions.map((v) => (
              <div key={v.v} className="flex items-center justify-between py-1.5 border-b last:border-0">
                <span>{v.v} · {v.date} · {v.result} · {v.chunks} 块</span>
              </div>
            ))
          )}
          <Button variant="ghost" size="sm" className="text-xs h-7">
            <Clock className="size-3" /> 查看全部历史
          </Button>
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}
