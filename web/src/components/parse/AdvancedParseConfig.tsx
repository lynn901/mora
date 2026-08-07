// AdvancedParseConfig — the shared "advanced parsing configuration" block
// (UI spec §4 control map). Used verbatim by the upload dialog (screen 1) and
// the batch reparse dialog (screen 3) so the two surfaces never drift.
//
// P0 fields are interactive; P2 fields (VLM / OCR / graph / QA) render
// disabled with a "二期" badge so the design is honest about scope. The form
// does not own submit logic — it lifts state up via `value`/`onChange`.
import { useState } from "react"
import { ChevronDown } from "lucide-react"
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from "@/components/ui/collapsible"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import { Badge } from "@/components/ui/badge"
import type { ParseOptionsFormState } from "@/types/parse"
import { cn } from "@/lib/utils"


interface Props {
  value: ParseOptionsFormState
  onChange: (next: ParseOptionsFormState) => void
  /** Hide the import-form control (reparse never changes the form, UI spec §3.3). */
  hideImportForm?: boolean
  /** Disable the conflict strategy control (reparse keeps the original form). */
  hideConflictStrategy?: boolean
  className?: string
}

function Field({
  label,
  htmlFor,
  hint,
  children,
}: {
  label: string
  htmlFor?: string
  hint?: string
  children: React.ReactNode
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
      {hint && <p className="text-xs text-muted-foreground">{hint}</p>}
    </div>
  )
}

export function AdvancedParseConfig({
  value,
  onChange,
  hideImportForm,
  hideConflictStrategy,
  className,
}: Props) {
  const [open, setOpen] = useState(true)
  const set = (patch: Partial<ParseOptionsFormState>) => onChange({ ...value, ...patch })

  return (
    <Collapsible open={open} onOpenChange={setOpen} className={cn("rounded-md border", className)}>
      <CollapsibleTrigger className="flex w-full items-center justify-between px-3 py-2 text-sm font-medium hover:bg-accent/50">
        <span>高级解析配置</span>
        <ChevronDown
          className={cn("size-4 text-muted-foreground transition-transform", open && "rotate-180")}
        />
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div className="space-y-4 px-3 pb-3 pt-1">
          <div className="grid grid-cols-2 gap-3">
            <Field label="分块策略" htmlFor="chunk-strategy">
              <Select
                value={value.chunkingStrategy}
                onValueChange={(v) => set({ chunkingStrategy: v })}
              >
                <SelectTrigger id="chunk-strategy" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="fixed">flat（固定长度）</SelectItem>
                  <SelectItem value="adaptive_3tier" disabled>
                    <span className="flex items-center gap-2">
                      adaptive_3tier <Badge variant="secondary">二期</Badge>
                    </span>
                  </SelectItem>
                  <SelectItem value="parent_child" disabled>
                    <span className="flex items-center gap-2">
                      parent_child <Badge variant="secondary">二期</Badge>
                    </span>
                  </SelectItem>
                </SelectContent>
              </Select>
            </Field>

            <Field label="解析器" htmlFor="parser">
              <Select value={value.parser} onValueChange={(v) => set({ parser: v as ParseOptionsFormState["parser"] })}>
                <SelectTrigger id="parser" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="auto">auto</SelectItem>
                  <SelectItem value="text">text</SelectItem>
                  <SelectItem value="ocr" disabled>
                    <span className="flex items-center gap-2">
                      ocr <Badge variant="secondary">二期</Badge>
                    </span>
                  </SelectItem>
                </SelectContent>
              </Select>
            </Field>
          </div>

          <div className="grid grid-cols-2 gap-3">
            <Field label="分块大小" htmlFor="chunk-size" hint="128–1024 token">
              <Input
                id="chunk-size"
                type="number"
                min={128}
                max={1024}
                value={value.chunkSize}
                onChange={(e) => set({ chunkSize: Number(e.target.value) || 0 })}
              />
            </Field>
            <Field label="重叠" htmlFor="chunk-overlap" hint="0–128 token">
              <Input
                id="chunk-overlap"
                type="number"
                min={0}
                max={128}
                value={value.chunkOverlap}
                onChange={(e) => set({ chunkOverlap: Number(e.target.value) || 0 })}
              />
            </Field>
          </div>

          <div className="flex items-center justify-between rounded-md border px-3 py-2">
            <div className="space-y-0.5">
              <Label htmlFor="respect-heading">按标题边界分块</Label>
              <p className="text-xs text-muted-foreground">在标题处切分，保持语义完整</p>
            </div>
            <Switch
              id="respect-heading"
              checked={value.respectHeading}
              onCheckedChange={(v) => set({ respectHeading: v })}
            />
          </div>

          {!hideImportForm && (
            <Field label="导入形态" htmlFor="import-form">
              <Select
                value={value.importForm}
                onValueChange={(v) => set({ importForm: v as ParseOptionsFormState["importForm"] })}
              >
                <SelectTrigger id="import-form" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="block">Block 文档（可编辑）</SelectItem>
                  <SelectItem value="attachment">可检索附件</SelectItem>
                </SelectContent>
              </Select>
            </Field>
          )}

          {!hideConflictStrategy && (
            <Field label="冲突策略" htmlFor="conflict-strategy">
              <Select
                value={value.conflictStrategy}
                onValueChange={(v) => set({ conflictStrategy: v as ParseOptionsFormState["conflictStrategy"] })}
              >
                <SelectTrigger id="conflict-strategy" className="w-full">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="append">追加</SelectItem>
                  <SelectItem value="overwrite">覆盖</SelectItem>
                  <SelectItem value="skip">跳过</SelectItem>
                </SelectContent>
              </Select>
            </Field>
          )}

          {/* P2 multimodal — disabled, surfaced honestly (UI spec §4). */}
          <div className="space-y-2 border-t pt-3">
            <div className="flex items-center justify-between">
              <Label>多模态与增强项</Label>
              <Badge variant="secondary">二期</Badge>
            </div>
            <p className="text-xs text-muted-foreground">
              多模态与增强项默认关闭，启用须经管理员授权并审计。
            </p>
            <div className="grid grid-cols-2 gap-2">
              {[
                { key: "VLM 图像描述", on: false },
                { key: "OCR 引擎", on: false },
                { key: "图抽取", on: false },
                { key: "问答生成", on: false },
              ].map((item) => (
                <div
                  key={item.key}
                  className="flex items-center justify-between rounded-md border border-dashed bg-muted/30 px-3 py-2 opacity-60"
                >
                  <span className="text-xs">{item.key}</span>
                  <Switch checked={false} disabled aria-label={item.key} />
                </div>
              ))}
            </div>
          </div>
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}
