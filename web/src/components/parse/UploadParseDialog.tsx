// UploadParseDialog — screen 1 (UI spec §3.1). Upload confirmation with file
// list, target-directory picker (read-only tree, write-permissioned only),
// conflict strategy, and the shared advanced parse config. Handles every
// state in the spec's completeness table: unsupported format, attachment-locked
// form, complex-PDF warning, oversize, no-permission directory, loading, success.
import { useRef, useState, useCallback } from "react"
import { Upload, FileText, X, AlertTriangle, Lock, FileWarning } from "lucide-react"
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
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"
import { AdvancedParseConfig } from "./AdvancedParseConfig"
import { useParseStore } from "@/stores/parse"
import { useMoraStore } from "@/stores/mora"
import { apiUploadDocument } from "@/api/parse"
import { ApiError } from "@/api/client"
import { cn } from "@/lib/utils"
import {
  ATTACHMENT_ONLY_FORMATS,
  FORMAT_LABELS,
  formatFromFilename,
  MAX_FILE_MB,
  MAX_BATCH_FILES,
  MAX_BATCH_MB,
} from "@/lib/parse-constants"
import type { UploadFileEntry, ImportForm, SourceFormat, ParseOptionsFormState } from "@/types/parse"
import { DEFAULT_PARSE_FORM } from "@/types/parse"
import type { TreeNode } from "@/types"

interface Props {
  open: boolean
  onOpenChange: (open: boolean) => void
}

let idSeq = 0
const nextId = () => `f-${++idSeq}`

export function UploadParseDialog({ open, onOpenChange }: Props) {
  const { currentWorkspace, tree } = useMoraStore()
  const { setUploadOpen, buildParseOptions } = useParseStore()
  const [files, setFiles] = useState<UploadFileEntry[]>([])
  const [targetDir, setTargetDir] = useState<string | null>(null)
  const [dirSearch, setDirSearch] = useState("")
  const [form, setForm] = useState<ParseOptionsFormState>(DEFAULT_PARSE_FORM)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [toast, setToast] = useState<string | null>(null)
  const fileInputRef = useRef<HTMLInputElement>(null)

  const addFiles = useCallback((fileList: FileList | File[]) => {
    setError(null)
    const incoming = Array.from(fileList).map((file) => {
      const format = formatFromFilename(file.name)
      const unsupported = !format
      const oversized = file.size > MAX_FILE_MB * 1024 * 1024
      const importForm: ImportForm = format && ATTACHMENT_ONLY_FORMATS.includes(format)
        ? "attachment"
        : "block"
      return {
        id: nextId(),
        file,
        format,
        importForm,
        parser: "auto" as const,
        size: file.size,
        unsupported,
        oversized,
      }
    })
    setFiles((prev) => {
      const total = [...prev, ...incoming]
      if (total.length > MAX_BATCH_FILES) {
        setError(`超出数量上限，单次最多 ${MAX_BATCH_FILES} 个文件，请分批上传`)
      }
      const totalMB = total.reduce((s, f) => s + f.size, 0) / (1024 * 1024)
      if (totalMB > MAX_BATCH_MB) {
        setError(`超出批量大小上限（${MAX_BATCH_MB}MB），请分批上传`)
      }
      return total
    })
  }, [])

  const removeFile = (id: string) => setFiles((prev) => prev.filter((f) => f.id !== id))

  const setFileForm = (id: string, importForm: ImportForm) =>
    setFiles((prev) => prev.map((f) => (f.id === id ? { ...f, importForm } : f)))

  const onDrop = useCallback(
    (e: React.DragEvent) => {
      e.preventDefault()
      if (e.dataTransfer.files.length) addFiles(e.dataTransfer.files)
    },
    [addFiles],
  )

  const submittable = files.filter((f) => !f.unsupported && !f.oversized)
  const totalBytes = submittable.reduce((s, f) => s + f.size, 0)
  const totalMB = (totalBytes / (1024 * 1024)).toFixed(1)
  const canSubmit = submittable.length > 0 && targetDir && !submitting

  const handleSubmit = async () => {
    if (!currentWorkspace || !targetDir || !canSubmit) return
    setSubmitting(true)
    setError(null)
    let enqueued = 0
    let lastErr: string | null = null
    // Per-file upload: the backend's multipart endpoint takes one file at a
    // time. The dialog enqueues each and surfaces an aggregate result.
    for (const entry of submittable) {
      const perFileForm: ParseOptionsFormState = {
        ...form,
        importForm: entry.importForm,
      }
      const parseOptions = buildParseOptions(perFileForm)
      try {
        await apiUploadDocument(currentWorkspace.id, entry.file, {
          directoryId: targetDir,
          title: entry.file.name.replace(/\.[^.]+$/, ""),
          parseOptions,
        })
        enqueued++
      } catch (e) {
        lastErr = e instanceof ApiError ? e.message : (e as Error).message
      }
    }
    setSubmitting(false)
    if (enqueued > 0) {
      setToast(`已开始解析，${enqueued} 个文件已入队`)
      // refresh tree so the new draft docs appear (parse_status=pending badge)
      void useMoraStore.getState().setWorkspace(currentWorkspace)
      setTimeout(() => {
        setFiles([])
        setTargetDir(null)
        setForm(DEFAULT_PARSE_FORM)
        setToast(null)
        setUploadOpen(false)
      }, 1200)
    } else if (lastErr) {
      setError(lastErr)
    }
  }

  const filteredTree = dirSearch
    ? tree.filter((n) => n.name.toLowerCase().includes(dirSearch.toLowerCase()))
    : tree

  return (
    <Dialog open={open} onOpenChange={(o) => { if (!submitting) { onOpenChange(o); setUploadOpen(o) } }}>
      <DialogContent className="sm:max-w-[640px] max-h-[90vh] overflow-hidden flex flex-col p-0 gap-0">
        <DialogHeader className="px-6 pt-6 pb-3">
          <DialogTitle className="flex items-center gap-2 text-xl">
            <Upload className="size-5 text-primary" />
            上传并解析文档
          </DialogTitle>
          <DialogDescription>
            将存量文档解析为可编辑页面或可检索附件
          </DialogDescription>
        </DialogHeader>

        <ScrollArea className="flex-1 max-h-[55vh]">
          <div className="px-6 pb-2 space-y-4">
            {/* File list */}
            <div className="space-y-1.5">
              <div className="flex items-center justify-between">
                <span className="text-sm font-medium">已选文件（{files.length}）</span>
                {files.length > 0 && (
                  <Button variant="ghost" size="sm" onClick={() => setFiles([])}>
                    清空
                  </Button>
                )}
              </div>
              {files.length === 0 ? (
                <div
                  className="rounded-md border border-dashed py-6 text-center text-sm text-muted-foreground"
                  onDrop={onDrop}
                  onDragOver={(e) => e.preventDefault()}
                >
                  拖拽文件到此处，或点击下方按钮选择
                </div>
              ) : (
                <div className="space-y-1">
                  {files.map((entry) => (
                    <FileRow
                      key={entry.id}
                      entry={entry}
                      onRemove={() => removeFile(entry.id)}
                      onFormChange={(f) => setFileForm(entry.id, f)}
                    />
                  ))}
                  <button
                    type="button"
                    onClick={() => fileInputRef.current?.click()}
                    className="w-full rounded-md border border-dashed py-2 text-xs text-muted-foreground hover:bg-accent/50 hover:text-foreground transition-colors"
                    onDrop={onDrop}
                    onDragOver={(e) => e.preventDefault()}
                  >
                    + 添加更多文件
                  </button>
                </div>
              )}
              <input
                ref={fileInputRef}
                type="file"
                multiple
                className="hidden"
                accept=".txt,.md,.markdown,.html,.htm,.json,.csv,.pdf,.docx,.xlsx,.pptx,.epub,.mhtml,.mht"
                onChange={(e) => { if (e.target.files) addFiles(e.target.files); e.target.value = "" }}
              />
            </div>

            {/* Two-column config: directory + settings */}
            <div className="grid grid-cols-2 gap-3">
              <div className="space-y-1.5">
                <span className="text-sm font-medium">目标目录</span>
                <Input
                  placeholder="搜索目录…"
                  value={dirSearch}
                  onChange={(e) => setDirSearch(e.target.value)}
                  className="h-7 text-xs"
                  aria-label="搜索目录"
                />
                <div className="rounded-md border max-h-48 overflow-auto" role="tree" aria-label="目标目录选择">
                  {filteredTree.length === 0 ? (
                    <p className="px-2 py-3 text-xs text-muted-foreground">无可选目录</p>
                  ) : (
                    filteredTree.map((node) => (
                      <DirRow
                        key={node.id}
                        node={node}
                        depth={0}
                        selectedId={targetDir}
                        onSelect={setTargetDir}
                      />
                    ))
                  )}
                </div>
                <p className="text-xs text-muted-foreground">仅显示你有编辑权限的目录</p>
              </div>

              <div className="space-y-2.5">
                <span className="text-sm font-medium">上传设置</span>
                <AdvancedParseConfig
                  value={form}
                  onChange={setForm}
                  hideImportForm
                  className="border-0 p-0 [&_[data-slot=collapsible-trigger]]:px-0"
                />
              </div>
            </div>

            {error && (
              <div role="alert" className="flex items-center gap-2 rounded-md border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
                <AlertTriangle className="size-4 shrink-0" />
                {error}
              </div>
            )}
            {toast && (
              <div role="status" className="flex items-center gap-2 rounded-md border border-success/30 bg-success/5 px-3 py-2 text-sm text-success">
                <FileText className="size-4 shrink-0" />
                {toast}
              </div>
            )}
          </div>
        </ScrollArea>

        <Separator />
        <DialogFooter className="flex items-center justify-between px-6 py-3 sm:justify-between">
          <p className="text-xs text-muted-foreground">
            {submittable.length} 个文件 · 约 {totalMB} MB · 预计解析后即可检索
          </p>
          <div className="flex gap-2">
            <Button variant="outline" onClick={() => onOpenChange(false)} disabled={submitting}>
              取消
            </Button>
            <Button onClick={handleSubmit} disabled={!canSubmit}>
              {submitting ? "正在入队…" : "开始解析"}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function FileRow({
  entry,
  onRemove,
  onFormChange,
}: {
  entry: UploadFileEntry
  onRemove: () => void
  onFormChange: (form: ImportForm) => void
}) {
  const sizeKB = (entry.size / 1024).toFixed(0)
  const sizeLabel = entry.size > 1024 * 1024
    ? `${(entry.size / (1024 * 1024)).toFixed(1)} MB`
    : `${sizeKB} KB`
  const isComplexPdf = entry.format === "pdf"
  const attachmentLocked =
    entry.format != null && ATTACHMENT_ONLY_FORMATS.includes(entry.format as SourceFormat)

  return (
    <div
      className={cn(
        "group flex items-center gap-2 rounded-md border px-2 py-1.5 text-sm",
        entry.unsupported && "border-destructive/40 bg-destructive/5",
      )}
    >
      <FileText className={cn("size-4 shrink-0", entry.unsupported && "text-destructive")} />
      <div className="flex-1 min-w-0">
        <p className="truncate">{entry.file.name}</p>
        <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <span>{sizeLabel}</span>
          {entry.format && <Badge variant="secondary" className="text-[10px]">{FORMAT_LABELS[entry.format]}</Badge>}
          {entry.unsupported && (
            <span className="text-destructive flex items-center gap-0.5">
              <FileWarning className="size-3" /> 一期暂不支持，请转为 md/txt/html/pdf/docx/csv/json
            </span>
          )}
          {entry.oversized && !entry.unsupported && (
            <span className="text-destructive">超出 {MAX_FILE_MB}MB 上限</span>
          )}
        </div>
      </div>

      {!entry.unsupported && (
        <div className="flex items-center gap-1.5">
          <Select
            value={entry.importForm}
            onValueChange={(v) => onFormChange(v as ImportForm)}
            disabled={attachmentLocked}
          >
            <SelectTrigger className="h-7 w-[130px] text-xs">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="block">Block 文档</SelectItem>
              <SelectItem value="attachment">可检索附件</SelectItem>
            </SelectContent>
          </Select>
          {attachmentLocked && <Lock className="size-3 text-muted-foreground" aria-label="数据型格式仅可附件索引" />}
        </div>
      )}

      <Button variant="ghost" size="icon" className="size-6 shrink-0" onClick={onRemove} aria-label="移除文件">
        <X className="size-3" />
      </Button>
      {isComplexPdf && entry.importForm === "block" && (
        <p className="col-span-full text-xs text-warning flex items-center gap-1 px-2 pb-1">
          <AlertTriangle className="size-3" /> 复杂版式 PDF 转 Block 可能丢失格式，建议改为附件索引
        </p>
      )}
    </div>
  )
}

// Read-only directory picker — a flattened, permission-filtered view of the
// existing tree (UI spec §2.2: reuse DirectoryTree, write+ nodes only).
function DirRow({
  node,
  depth,
  selectedId,
  onSelect,
}: {
  node: TreeNode
  depth: number
  selectedId: string | null
  onSelect: (id: string) => void
}) {
  const isSelected = selectedId === node.id
  return (
    <>
      <button
        type="button"
        className={cn(
          "flex w-full items-center gap-1.5 px-2 py-1 text-left text-xs hover:bg-accent/50",
          isSelected && "bg-accent text-accent-foreground font-medium",
        )}
        style={{ paddingLeft: `${depth * 12 + 8}px` }}
        role="treeitem"
        aria-selected={isSelected}
        onClick={() => onSelect(node.id)}
      >
        <span className="truncate">{node.name}</span>
      </button>
      {node.children?.map((child) => (
        <DirRow key={child.id} node={child} depth={depth + 1} selectedId={selectedId} onSelect={onSelect} />
      ))}
    </>
  )
}
