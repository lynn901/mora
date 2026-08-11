// Phase 1-6 — review inbox (§4.4 GET /knowledge/reviews + POST .../decisions).
//
// Lists pending candidate reviews for the current workspace and supports
// approve/reject (plus promote/deprecate for governance rollback). Each
// decision POST carries an Idempotency-Key so retries are safe. A 403 on a
// decision means the caller is not in review_roles (§10.4 case 29) — we
// surface that as a disabled-state notice rather than a crash, and the
// backend writes the audit row.
import { useState } from "react"
import { Inbox, CheckCircle2, XCircle, ArrowUpCircle, Ban } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useMoraStore } from "@/stores/mora"
import {
  apiSubmitReviewDecision,
  isForbidden,
} from "@/api/assets"
import { useReviewInbox } from "./asset-hooks"
import { ASSET_TYPE_LABEL, fmtTime } from "./asset-utils"
import {
  BuildStatusBadge,
  ErrorState,
  ForbiddenEmpty,
} from "./asset-primitives"
import type { ReviewDecision, ReviewRequest } from "@/types/assets"

const DECISIONS: { value: ReviewDecision; label: string; icon: React.ReactNode; tone: "default" | "outline" | "destructive" }[] = [
  { value: "approve", label: "通过", icon: <CheckCircle2 className="size-3.5" />, tone: "default" },
  { value: "reject", label: "拒绝", icon: <XCircle className="size-3.5" />, tone: "destructive" },
  { value: "promote", label: "提升", icon: <ArrowUpCircle className="size-3.5" />, tone: "outline" },
  { value: "deprecate", label: "废弃", icon: <Ban className="size-3.5" />, tone: "outline" },
]

export function ReviewInboxPage() {
  const { currentWorkspace } = useMoraStore()
  const inbox = useReviewInbox(currentWorkspace?.id ?? null)

  if (!currentWorkspace) {
    return (
      <div className="flex h-full items-center justify-center p-8 text-sm text-muted-foreground">
        请先选择工作空间
      </div>
    )
  }

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2 border-b px-4 py-3">
        <Inbox className="size-4" />
        <h2 className="text-sm font-semibold">审核收件箱</h2>
        <span className="ml-auto text-xs text-muted-foreground">
          {inbox.notFound ? "—" : `${inbox.items.length} 项待审核`}
        </span>
      </div>

      <div className="flex-1 overflow-hidden">
        {inbox.isLoading ? (
          <div className="flex h-full items-center justify-center">
            <div className="mx-auto size-8 animate-spin rounded-full border-2 border-primary border-t-transparent" />
          </div>
        ) : inbox.notFound ? (
          <ForbiddenEmpty
            message="无权查看审核列表或暂无待审核项"
            hint="审核可见性受 RBAC 约束。"
          />
        ) : inbox.error ? (
          <ErrorState message={inbox.error} onRetry={() => void inbox.reload()} />
        ) : inbox.items.length === 0 ? (
          <div className="flex h-full flex-col items-center justify-center gap-2 p-8 text-center">
            <Inbox className="size-10 text-muted-foreground/50" />
            <p className="text-sm font-medium text-muted-foreground">收件箱已清空</p>
            <p className="text-xs text-muted-foreground/70">
              来源同步产生的 candidate 资产将在此等待审核。
            </p>
          </div>
        ) : (
          <ScrollArea className="h-full">
            <ul className="divide-y">
              {inbox.items.map((r) => (
                <li key={r.id}>
                  <ReviewRow review={r} onResolved={() => void inbox.reload()} />
                </li>
              ))}
            </ul>
            {inbox.hasMore && (
              <div className="flex justify-center p-3">
                <Button
                  size="sm"
                  variant="outline"
                  className="h-7 text-xs"
                  disabled={inbox.isLoadingMore}
                  onClick={() => void inbox.loadMore()}
                >
                  {inbox.isLoadingMore ? "加载中…" : "加载更多"}
                </Button>
              </div>
            )}
          </ScrollArea>
        )}
      </div>
    </div>
  )
}

function ReviewRow({
  review,
  onResolved,
}: {
  review: ReviewRequest
  onResolved: () => void
}) {
  const [decision, setDecision] = useState<ReviewDecision>("approve")
  const [rationale, setRationale] = useState("")
  const [expectedCurrent, setExpectedCurrent] = useState("")
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  // promote/deprecate are governance rollback actions and require
  // expected_current (§7 — CAS guard against concurrent version changes).
  const needsExpectedCurrent = decision === "promote" || decision === "deprecate"

  const submit = async () => {
    if (needsExpectedCurrent && !expectedCurrent.trim()) {
      setErr("回滚动作需指定 expected_current（当前版本 ID）")
      return
    }
    setSubmitting(true)
    setErr(null)
    try {
      // crypto.randomUUID is deterministic-per-call and available in all
      // evergreen browsers; Idempotency-Key makes retry safe.
      await apiSubmitReviewDecision(
        review.id,
        crypto.randomUUID(),
        decision,
        rationale.trim() || null,
        needsExpectedCurrent ? expectedCurrent.trim() : null,
      )
      setRationale("")
      setExpectedCurrent("")
      onResolved()
    } catch (e) {
      if (isForbidden(e)) {
        setErr("无审核权限（不在 review_roles 中），操作已被审计记录")
      } else if (e instanceof Error) {
        setErr(e.message)
      } else {
        setErr("提交失败")
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div className="px-4 py-3">
      <div className="flex items-start gap-3">
        <div className="min-w-0 flex-1">
          <p className="truncate text-sm font-medium">
            {review.asset_name ?? "未命名资产"}
            {review.asset_type && (
              <span className="ml-2 text-xs text-muted-foreground">
                {ASSET_TYPE_LABEL[review.asset_type]}
              </span>
            )}
          </p>
          <p className="mt-0.5 text-xs text-muted-foreground">
            {review.version_no != null ? `v${review.version_no}` : "版本未知"} ·{" "}
            {fmtTime(review.created_at)}
          </p>
          {review.rationale && (
            <p className="mt-1 line-clamp-2 text-xs text-muted-foreground">
              理由：{review.rationale}
            </p>
          )}
        </div>
        {review.version_no != null && (
          <BuildStatusBadge status="building" />
        )}
      </div>

      <div className="mt-2 flex flex-wrap items-center gap-2">
        <Select value={decision} onValueChange={(v) => setDecision(v as ReviewDecision)}>
          <SelectTrigger className="h-8 w-[110px] text-xs">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {DECISIONS.map((d) => (
              <SelectItem key={d.value} value={d.value}>
                <span className="flex items-center gap-1.5">
                  {d.icon}
                  {d.label}
                </span>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Input
          className="h-8 flex-1 min-w-[160px] text-xs"
          placeholder={needsExpectedCurrent ? "expected_current（当前版本 ID）" : "审核理由（可选）"}
          value={needsExpectedCurrent ? expectedCurrent : rationale}
          onChange={(e) =>
            needsExpectedCurrent
              ? setExpectedCurrent(e.target.value)
              : setRationale(e.target.value)
          }
        />
        <Button
          size="sm"
          className="h-8 text-xs"
          disabled={submitting}
          onClick={() => void submit()}
        >
          {submitting ? "提交中…" : "提交"}
        </Button>
      </div>

      {err && <p className="mt-1.5 text-xs text-destructive">{err}</p>}
    </div>
  )
}
