// BatchReparseDialog — screen 3 (UI spec §3.3). Takes a selection of document
// ids, shows a permission-filtered summary, lets the caller override parse
// config (not import form — reparse never changes the form), and enqueues.
import { useState } from "react"
import { RefreshCw, AlertTriangle, FileText } from "lucide-react"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import { AdvancedParseConfig } from "./AdvancedParseConfig"
import { useParseStore } from "@/stores/parse"
import { useMoraStore } from "@/stores/mora"
import { apiReparseDocuments } from "@/api/parse"
import { buildTreeFlatten } from "@/lib/parse-constants"
import { FORMAT_LABELS } from "@/lib/parse-constants"
import type { ParseOptionsFormState } from "@/types/parse"
import { DEFAULT_PARSE_FORM } from "@/types/parse"

export function BatchReparseDialog() {
  const { reparseOpen, closeReparse, reparseDocumentIds, buildParseOptions } = useParseStore()
  const { currentWorkspace, tree } = useMoraStore()
  const [form, setForm] = useState<ParseOptionsFormState>(DEFAULT_PARSE_FORM)
  const [submitting, setSubmitting] = useState(false)
  const [result, setResult] = useState<{ enqueued: number; total: number } | null>(null)
  const [error, setError] = useState<string | null>(null)

  // Resolve selected docs into tree-flattened metadata (title, format) so the
  // summary table reads naturally. Docs not in the tree are still enqueued
  // (RBAC lives on the backend — missing/forbidden are silently dropped).
  const flat = buildTreeFlatten(tree)
  const selected = reparseDocumentIds
    .map((id) => flat.find((n) => n.id === id))
    .filter(Boolean) as { id: string; name: string; format?: string }[]

  const handleSubmit = async () => {
    if (!currentWorkspace || reparseDocumentIds.length === 0) return
    setSubmitting(true)
    setError(null)
    try {
      const parseOptions = buildParseOptions(form)
      const res = await apiReparseDocuments(currentWorkspace.id, reparseDocumentIds, parseOptions)
      setResult({ enqueued: res.enqueued, total: reparseDocumentIds.length })
      setTimeout(() => {
        setSubmitting(false)
        setResult(null)
        setForm(DEFAULT_PARSE_FORM)
        closeReparse()
      }, 1500)
    } catch (e) {
      setError((e as Error).message)
      setSubmitting(false)
    }
  }

  const visible = selected.slice(0, 5)
  const hiddenCount = selected.length - visible.length

  return (
    <Dialog open={reparseOpen} onOpenChange={() => { if (!submitting) closeReparse() }}>
      <DialogContent className="sm:max-w-[720px] max-h-[90vh] overflow-hidden flex flex-col p-0 gap-0">
        <DialogHeader className="px-6 pt-6 pb-3">
          <DialogTitle className="flex items-center gap-2">
            <RefreshCw className="size-5 text-primary" />
            批量重解析
          </DialogTitle>
          <DialogDescription>
            以新配置对选中的 {reparseDocumentIds.length} 个文档重新解析，不改变导入形态
          </DialogDescription>
        </DialogHeader>

        <ScrollArea className="flex-1 max-h-[55vh]">
          <div className="px-6 pb-4 space-y-4">
            {/* KPI summary */}
            <div className="flex items-center gap-4 rounded-md border bg-muted/30 px-3 py-2 text-sm">
              <span><strong>{reparseDocumentIds.length}</strong> 个已选</span>
              <Separator orientation="vertical" className="h-4" />
              <span className="text-muted-foreground">实际入队由权限校验决定</span>
            </div>

            {/* Selected doc list */}
            {selected.length > 0 ? (
              <div className="space-y-1">
                {visible.map((doc) => (
                  <div key={doc.id} className="flex items-center gap-2 rounded-md border px-2 py-1.5 text-sm">
                    <FileText className="size-4 shrink-0 text-muted-foreground" />
                    <span className="truncate flex-1">{doc.name}</span>
                    {doc.format && (
                      <Badge variant="secondary" className="text-[10px]">
                        {FORMAT_LABELS[doc.format as keyof typeof FORMAT_LABELS] || doc.format}
                      </Badge>
                    )}
                  </div>
                ))}
                {hiddenCount > 0 && (
                  <p className="text-xs text-muted-foreground px-2">+{hiddenCount} 更多</p>
                )}
              </div>
            ) : (
              <div className="rounded-md border border-dashed py-6 text-center text-sm text-muted-foreground">
                未选择文档，从文档列表多选后唤起重解析
              </div>
            )}

            {/* Permission strip — informational; the backend is the authority. */}
            <div className="flex items-start gap-2 rounded-md border border-warning/30 bg-warning/5 px-3 py-2 text-xs text-warning">
              <AlertTriangle className="size-4 shrink-0 mt-0.5" />
              <span>
                无编辑权限的文档将被自动剔除且不返回。旧索引在解析过渡期短暂不可见。
              </span>
            </div>

            {/* New config — import form hidden (reparse invariant). */}
            <div className="space-y-1.5">
              <span className="text-sm font-medium">新解析配置</span>
              <AdvancedParseConfig
                value={form}
                onChange={setForm}
                hideImportForm
                hideConflictStrategy
              />
            </div>

            {error && (
              <div role="alert" className="flex items-center gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
                <AlertTriangle className="size-4 shrink-0" />
                {error}
              </div>
            )}
            {result && (
              <div role="status" className="flex items-center gap-2 rounded-md border border-success/30 bg-success/5 px-3 py-2 text-sm text-success">
                <FileText className="size-4 shrink-0" />
                已入队 {result.enqueued} 个重解析任务
              </div>
            )}
          </div>
        </ScrollArea>

        <Separator />
        <DialogFooter className="flex items-center justify-between px-6 py-3 sm:justify-between">
          <p className="text-xs text-muted-foreground">
            {result ? `${result.enqueued} 个文档将入队` : "重解析不改变导入形态，仅改解析/分块/多模态参数"}
          </p>
          <div className="flex gap-2">
            <Button variant="outline" onClick={closeReparse} disabled={submitting}>
              取消
            </Button>
            <Button onClick={handleSubmit} disabled={submitting || reparseDocumentIds.length === 0}>
              {submitting ? "正在入队…" : "开始重解析"}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
