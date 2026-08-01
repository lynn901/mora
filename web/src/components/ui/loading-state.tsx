import * as React from "react"
import { cn } from "@/lib/utils"

interface LoadingStateProps extends React.ComponentProps<"div"> {
  label?: string
}

/**
 * Unified spinner loading state. Use for full-panel or centered "fetching"
 * moments. Prefer the `Skeleton` variant for content-shaped placeholders.
 */
export function LoadingState({ label = "Loading...", className, ...props }: LoadingStateProps) {
  return (
    <div
      className={cn("flex flex-col items-center justify-center text-center", className)}
      role="status"
      aria-live="polite"
      {...props}
    >
      <div className="size-8 border-2 border-primary border-t-transparent rounded-full animate-spin" />
      <p className="text-sm text-muted-foreground mt-3">{label}</p>
    </div>
  )
}

/**
 * Content-shaped loading placeholder. Use `rows` for list/paragraph shapes
 * rather than a bare spinner, so the layout doesn't jump when data lands.
 */
export function Skeleton({ className, ...props }: React.ComponentProps<"div">) {
  return <div className={cn("animate-pulse rounded-md bg-muted", className)} {...props} />
}

/** A stack of skeleton lines standing in for a list of items. */
export function SkeletonList({ rows = 3, className }: { rows?: number; className?: string }) {
  return (
    <div className={cn("space-y-2", className)} role="status" aria-label="Loading">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="flex items-center gap-2 px-2 py-1.5">
          <Skeleton className="size-4 rounded" />
          <Skeleton className="h-3.5 flex-1" style={{ maxWidth: `${100 - i * 12}%` }} />
        </div>
      ))}
    </div>
  )
}
