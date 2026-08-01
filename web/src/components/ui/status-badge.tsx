import * as React from "react"
import { cn } from "@/lib/utils"
import type { IndexStatusValue } from "@/types"

type BadgeTone = "success" | "info" | "destructive" | "muted"

interface StatusBadgeProps extends React.ComponentProps<"span"> {
  status: IndexStatusValue
  /** Show a leading status dot (default true). */
  withDot?: boolean
}

const STATUS_CONFIG: Record<IndexStatusValue, { tone: BadgeTone; label: string; pulse?: boolean }> = {
  indexed: { tone: "success", label: "Indexed" },
  processing: { tone: "info", label: "Indexing", pulse: true },
  pending: { tone: "muted", label: "Pending" },
  failed: { tone: "destructive", label: "Index failed" },
}

const TONE_STYLES: Record<BadgeTone, { wrapper: string; dot: string }> = {
  success: {
    wrapper: "bg-success/15 text-success border-success/30",
    dot: "bg-success",
  },
  info: {
    wrapper: "bg-info/15 text-info border-info/30",
    dot: "bg-info",
  },
  destructive: {
    wrapper: "bg-destructive/15 text-destructive border-destructive/30",
    dot: "bg-destructive",
  },
  muted: {
    wrapper: "bg-muted text-muted-foreground border-border",
    dot: "bg-muted-foreground/60",
  },
}

/**
 * Index-status badge for a document. Maps the backend `index_status` enum
 * (indexed / processing / pending / failed) to a tonal badge with a status dot.
 * The processing dot pulses to signal active work.
 */
export function StatusBadge({ status, withDot = true, className, ...props }: StatusBadgeProps) {
  const config = STATUS_CONFIG[status] ?? STATUS_CONFIG.pending
  const tone = TONE_STYLES[config.tone]

  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full border px-1.5 py-0 text-[10px] font-medium leading-tight",
        tone.wrapper,
        className,
      )}
      title={config.label}
      {...props}
    >
      {withDot && (
        <span
          className={cn("size-1.5 rounded-full", tone.dot, config.pulse && "animate-pulse")}
          aria-hidden
        />
      )}
      {config.label}
    </span>
  )
}
