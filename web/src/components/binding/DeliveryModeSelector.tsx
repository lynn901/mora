// Phase 5-4 — delivery_mode selector (§5.3). tool / summary / inline with
// semantic hints so the operator understands what each mode delivers before
// committing. Hints come from the shared DELIVERY_MODE map so the label and
// the hint never drift.
import { Info } from "lucide-react"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { DELIVERY_MODE } from "./binding-utils"
import type { BindingDeliveryMode } from "@/types/binding"

const ORDER: BindingDeliveryMode[] = ["tool", "summary", "inline"]

export function DeliveryModeSelector({
  value,
  onValueChange,
  disabled,
  id,
}: {
  value: BindingDeliveryMode
  onValueChange: (v: BindingDeliveryMode) => void
  disabled?: boolean
  id?: string
}) {
  const hint = DELIVERY_MODE[value]?.hint
  return (
    <div className="space-y-1">
      <Select
        value={value}
        onValueChange={(v) => onValueChange(v as BindingDeliveryMode)}
        disabled={disabled}
      >
        <SelectTrigger id={id} className="h-8 text-sm">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {ORDER.map((m) => (
            <SelectItem key={m} value={m}>
              {DELIVERY_MODE[m].label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {hint && (
        <p className="flex items-start gap-1 text-[11px] text-muted-foreground">
          <Info className="mt-0.5 size-3 shrink-0" />
          {hint}
        </p>
      )}
    </div>
  )
}
