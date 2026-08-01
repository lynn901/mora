import * as React from "react"
import { cn } from "@/lib/utils"

interface EmptyStateAction {
  label: string
  onClick: () => void
  icon?: React.ReactNode
}

interface EmptyStateProps extends React.ComponentProps<"div"> {
  icon?: React.ReactNode
  title: string
  description?: string
  action?: EmptyStateAction
  /** Tighter vertical rhythm for use inside narrow side panels (default false). */
  compact?: boolean
}

/**
 * Unified empty state. An empty screen is an invitation to act, so the copy
 * names the next step, not the absence of data. Use across panels so every
 * "nothing here yet" moment reads the same.
 */
export function EmptyState({
  icon,
  title,
  description,
  action,
  compact = false,
  className,
  ...props
}: EmptyStateProps) {
  return (
    <div
      className={cn(
        "flex flex-col items-center justify-center text-center",
        compact ? "py-8" : "py-16",
        className,
      )}
      {...props}
    >
      {icon && (
        <div className={cn("flex items-center justify-center text-muted-foreground/50", compact ? "mb-2" : "mb-4")}>
          {icon}
        </div>
      )}
      <p className={cn("font-medium text-foreground", compact ? "text-sm" : "text-base")}>{title}</p>
      {description && (
        <p className={cn("text-muted-foreground mt-1 max-w-xs", compact ? "text-xs" : "text-sm")}>{description}</p>
      )}
      {action && (
        <button
          type="button"
          onClick={action.onClick}
          className="mt-4 inline-flex h-8 items-center gap-1.5 rounded-md bg-primary px-3 text-xs font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
        >
          {action.icon}
          {action.label}
        </button>
      )}
    </div>
  )
}
