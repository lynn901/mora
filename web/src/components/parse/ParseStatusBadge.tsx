// ParseStatusBadge — extends the existing index_status badge system (UI spec §5).
// Color never carries status alone: every variant pairs text + icon + color.
import {
  Clock,
  Loader,
  Hourglass,
  Check,
  AlertTriangle,
  MinusCircle,
} from "lucide-react"
import { Badge } from "@/components/ui/badge"
import { cn } from "@/lib/utils"
import { PARSE_STATUS_META } from "@/lib/parse-constants"
import type { ParseStatus } from "@/types/parse"

const ICONS = { Clock, Loader, Hourglass, Check, AlertTriangle, MinusCircle }

export function ParseStatusBadge({
  status,
  className,
  animated = false,
}: {
  status: ParseStatus
  className?: string
  /** Spin the icon for in-flight states (parsing). */
  animated?: boolean
}) {
  const meta = PARSE_STATUS_META[status]
  const Icon = ICONS[meta.icon as keyof typeof ICONS] || Clock
  return (
    <Badge variant={meta.badge} className={cn("gap-1", className)}>
      <Icon
        className={cn("size-3", status === "parsing" && animated && "animate-spin")}
        aria-hidden
      />
      {meta.label}
    </Badge>
  )
}

/** Partial-skip badge for multimodal degradation (UI spec §5). */
export function ParseSkippedBadge({ className }: { className?: string }) {
  return (
    <Badge variant="warning" className={cn("gap-1", className)}>
      <MinusCircle className="size-3" aria-hidden />
      部分跳过
    </Badge>
  )
}
