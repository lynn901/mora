// Phase 5-4 — 配装管理 panel (§8). The primary management surface for agent
// bindings: agent picker, cursor-paginated binding list, batch配装 dialog,
// revoke, and the §5.1 阻断态 rendering for pinned bindings whose version is
// no longer usable. The §8 §1.2 rule is honored everywhere: no Secret values
// are ever shown — only requirement declarations and version references.
//
// Data source is the §6.1 REST control plane only (architecture red line
// §4.4 + §9): the panel never reads agent_bindings or skill_packages
// directly. §11.4 leak-safe: a 404 on the agent or binding list renders as a
// calm empty state, never a leaky error.
import { useEffect, useState } from "react"
import {
  Boxes,
  Plus,
  Loader2,
  Ban,
} from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Separator } from "@/components/ui/separator"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip"
import { useMoraStore } from "@/stores/mora"
import { apiGetAgents, apiBatchUpsertBindings, apiRevokeBinding } from "@/api/binding"
import { ApiError } from "@/api/client"
import { useBindings } from "./binding-hooks"
import { BatchBindingDialog } from "./BatchBindingDialog"
import {
  DeliveryModeBadge,
  EffectBadge,
  ErrorState,
  ForbiddenEmpty,
  PinnedBlockedBadge,
  PinnedBlockedBanner,
  ScopeBadge,
  VersionPolicyBadge,
} from "./binding-primitives"
import { errMsg, fmtTime } from "./binding-utils"
import type { Agent, AgentBinding, BindingInput } from "@/types/binding"

export function BindingPanel() {
  const { currentWorkspace } = useMoraStore()
  const [agents, setAgents] = useState<Agent[]>([])
  const [agentId, setAgentId] = useState<string | null>(null)
  const [agentLoadError, setAgentLoadError] = useState<string | null>(null)
  const [agentsNotFound, setAgentsNotFound] = useState(false)

  // Manual agent id entry fallback when the agent list route is not yet
  // wired on the backend (degrades gracefully per §11.4 + the agent list API
  // returning [] on 404). Lets the operator still manage bindings by pasting
  // an agent id.
  const [manualMode, setManualMode] = useState(false)
  const [manualAgentId, setManualAgentId] = useState("")

  const [showBatch, setShowBatch] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [submitError, setSubmitError] = useState<string | null>(null)
  const [revoking, setRevoking] = useState<string | null>(null)
  const [batchAlerts, setBatchAlerts] = useState<number[] | null>(null)

  const list = useBindings(agentId)

  // Load agents when the workspace changes. Empty list on 404/403 so the
  // panel degrades to manual entry (§11.4). The synchronous setState calls
  // below mark the load as in-progress before the first await, which is
  // exactly the pattern react-hooks/set-state-in-effect flags but is the
  // intended behavior for a data-fetch effect (same precedent as
  // AssetDetailPage).
  useEffect(() => {
    let cancelled = false
    if (!currentWorkspace) return
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setAgentLoadError(null)
    setAgentsNotFound(false)
    void (async () => {
      try {
        const list = await apiGetAgents(currentWorkspace.id)
        if (cancelled) return
        setAgents(list)
        setAgentsNotFound(list.length === 0)
      } catch (e) {
        if (cancelled) return
        if (e instanceof ApiError && (e.status === 404 || e.status === 403)) {
          setAgents([])
          setAgentsNotFound(true)
        } else {
          setAgentLoadError(errMsg(e))
        }
      }
    })()
    return () => {
      cancelled = true
    }
  }, [currentWorkspace])

  // Auto-select the first active agent when the list lands. This is a
  // derived-state synchronization (no external system) flagged by
  // react-hooks/set-state-in-effect; suppressing is the intended behavior —
  // the selection only writes when the picked agent differs from the
  // current one.
  useEffect(() => {
    if (agentId) return
    const firstActive = agents.find((a) => a.status === "active")
    if (firstActive) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setAgentId(firstActive.id)
    }
  }, [agents, agentId])

  const activeAgent = agents.find((a) => a.id === agentId) ?? null

  const handleBatchSubmit = async (
    inputs: BindingInput[],
    idempotencyKey: string,
  ) => {
    if (!agentId || !currentWorkspace) return
    setSubmitting(true)
    setSubmitError(null)
    setBatchAlerts(null)
    try {
      const result = await apiBatchUpsertBindings(
        agentId,
        currentWorkspace.id,
        inputs,
        idempotencyKey,
      )
      setBatchAlerts(result.alerted ?? [])
      setShowBatch(false)
      await list.reload()
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        setSubmitError("幂等键冲突或并发冲突（409）。请勿重复提交相同键的不同请求。")
      } else {
        setSubmitError(errMsg(e))
      }
    } finally {
      setSubmitting(false)
    }
  }

  const handleRevoke = async (bindingId: string) => {
    if (!agentId) return
    setRevoking(bindingId)
    try {
      await apiRevokeBinding(agentId, bindingId)
      await list.reload()
    } catch (e) {
      setSubmitError(errMsg(e))
    } finally {
      setRevoking(null)
    }
  }

  if (!currentWorkspace) {
    return (
      <div className="flex h-full items-center justify-center p-8 text-sm text-muted-foreground">
        请先选择工作空间
      </div>
    )
  }

  return (
    <div className="flex h-full flex-col">
      {/* Header — agent picker + batch配装 entry */}
      <div className="flex items-center gap-2 border-b px-4 py-3">
        <Boxes className="size-4" />
        <h2 className="text-sm font-semibold">配装管理</h2>
        <div className="ml-auto flex items-center gap-2">
          <Button
            size="sm"
            className="h-7 text-xs"
            disabled={!agentId}
            onClick={() => setShowBatch(true)}
          >
            <Plus className="size-3" /> 批量配装
          </Button>
        </div>
      </div>

      {/* Agent picker — list mode or manual-entry fallback */}
      <div className="border-b px-4 py-3">
        {!manualMode && (agents.length > 0 || agentLoadError) && (
          <div className="space-y-1">
            <Label className="text-xs text-muted-foreground">Agent</Label>
            <Select
              value={agentId ?? undefined}
              onValueChange={(v) => setAgentId(v)}
              disabled={!!agentLoadError}
            >
              <SelectTrigger className="h-8 text-sm">
                <SelectValue
                  placeholder={
                    agentLoadError ? "加载失败" : "选择 Agent"
                  }
                />
              </SelectTrigger>
              <SelectContent>
                {agents.map((a) => (
                  <SelectItem
                    key={a.id}
                    value={a.id}
                    disabled={a.status !== "active"}
                  >
                    {a.name}
                    {a.status !== "active" ? `（${a.status}）` : ""}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        )}

        {agentsNotFound && !manualMode && (
          <div className="space-y-2">
            <p className="text-xs text-muted-foreground">
              未发现可用 Agent 列表路由或无权访问。可手动输入 Agent ID。
            </p>
            <Button
              variant="outline"
              size="sm"
              className="h-7 text-xs"
              onClick={() => setManualMode(true)}
            >
              手动输入 Agent ID
            </Button>
          </div>
        )}

        {manualMode && (
          <div className="space-y-1">
            <Label className="text-xs text-muted-foreground">Agent ID</Label>
            <div className="flex items-center gap-2">
              <Input
                className="h-8 text-sm"
                placeholder="粘贴 Agent ID"
                value={manualAgentId}
                onChange={(e) => setManualAgentId(e.target.value)}
              />
              <Button
                size="sm"
                className="h-8 text-xs"
                disabled={!manualAgentId.trim()}
                onClick={() => setAgentId(manualAgentId.trim())}
              >
                确定
              </Button>
              <Button
                variant="outline"
                size="sm"
                className="h-8 text-xs"
                onClick={() => {
                  setManualMode(false)
                  setManualAgentId("")
                }}
              >
                返回列表
              </Button>
            </div>
          </div>
        )}

        {activeAgent && (
          <p className="mt-1 text-[11px] text-muted-foreground">
            {activeAgent.description || activeAgent.name}
          </p>
        )}
      </div>

      {/* Batch §5.1 alert summary — surfaced when the last batch had blocked pinned versions */}
      {batchAlerts && batchAlerts.length > 0 && (
        <PinnedBlockedBanner
          reason={`上一批次有 ${batchAlerts.length} 条固定版本已阻断——使用将被阻断直至版本恢复或重新固定（§5.1 阻断+告警，不回退）。`}
        />
      )}

      {/* Binding list */}
      <ScrollArea className="flex-1">
        <div className="p-2">
          {!agentId ? (
            <div className="py-8 text-center text-sm text-muted-foreground">
              选择一个 Agent 以查看其配装。
            </div>
          ) : list.isLoading ? (
            <div className="flex items-center justify-center py-8">
              <Loader2 className="size-5 animate-spin text-muted-foreground" />
            </div>
          ) : list.notFound ? (
            <ForbiddenEmpty
              message="无权查看该 Agent 配装或 Agent 不存在"
              hint="只读资源无权访问与不存在返回相同结果（不泄露存在性）。"
            />
          ) : list.error ? (
            <ErrorState
              message={list.error}
              onRetry={() => void list.reload()}
            />
          ) : list.items.length === 0 ? (
            <div className="py-8 text-center text-sm text-muted-foreground">
              该 Agent 暂无配装。点击「批量配装」创建。
            </div>
          ) : (
            <div className="space-y-2">
              {list.items.map((b) => (
                <BindingRow
                  key={b.id}
                  binding={b}
                  onRevoke={() => handleRevoke(b.id)}
                  revoking={revoking === b.id}
                />
              ))}
              {list.hasMore && (
                <Button
                  variant="ghost"
                  size="sm"
                  className="w-full text-xs"
                  disabled={list.isLoadingMore}
                  onClick={() => void list.loadMore()}
                >
                  {list.isLoadingMore ? (
                    <>
                      <Loader2 className="size-3 animate-spin" /> 加载中…
                    </>
                  ) : (
                    "加载更多"
                  )}
                </Button>
              )}
            </div>
          )}
        </div>
      </ScrollArea>

      <BatchBindingDialog
        open={showBatch}
        onOpenChange={setShowBatch}
        onSubmit={handleBatchSubmit}
        submitting={submitting}
        error={submitError}
      />

      {submitError && !showBatch && (
        <div className="border-t px-4 py-2 text-xs text-destructive">
          {submitError}
        </div>
      )}
    </div>
  )
}

/** One binding row — shows scope/effect/version_policy/delivery_mode + revoke,
 * and renders the §5.1 阻断态 banner when the pinned version is blocked. */
function BindingRow({
  binding,
  onRevoke,
  revoking,
}: {
  binding: AgentBinding
  onRevoke: () => void
  revoking: boolean
}) {
  return (
    <div className="rounded-md border bg-card p-3">
      <div className="flex flex-wrap items-center gap-1.5">
        <ScopeBadge status={binding.scope_kind} />
        <EffectBadge status={binding.effect} />
        <VersionPolicyBadge status={binding.version_policy} />
        <DeliveryModeBadge status={binding.delivery_mode} />
        <Badge variant="outline" className="font-mono">
          P{binding.priority}
        </Badge>
        {binding.pinned_version_blocked && <PinnedBlockedBadge />}
        <div className="ml-auto flex items-center gap-1">
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                className="size-7"
                onClick={onRevoke}
                disabled={revoking || !!binding.revoked_at}
                aria-label="撤销"
              >
                {revoking ? (
                  <Loader2 className="size-3.5 animate-spin" />
                ) : (
                  <Ban className="size-3.5" />
                )}
              </Button>
            </TooltipTrigger>
            <TooltipContent>撤销</TooltipContent>
          </Tooltip>
        </div>
      </div>

      {/* Target + version reference */}
      <div className="mt-2 space-y-0.5 text-xs text-muted-foreground">
        {binding.asset_id && (
          <p>
            资产: <code className="font-mono">{binding.asset_id}</code>
          </p>
        )}
        {binding.asset_type && <p>资产类型: {binding.asset_type}</p>}
        {binding.pinned_version_id && (
          <p>
            固定版本:{" "}
            <code className="font-mono">{binding.pinned_version_id}</code>
          </p>
        )}
        {binding.revoked_at && (
          <p className="text-destructive">
            已撤销: {fmtTime(binding.revoked_at)}
          </p>
        )}
        <p>创建: {fmtTime(binding.created_at)}</p>
      </div>

      {/* §5.1 阻断态 — pinned version no longer usable. Surfaced as block +
       * alert, NOT a silent fallback to the latest published version. */}
      {binding.pinned_version_blocked && (
        <>
          <Separator className="my-2" />
          <PinnedBlockedBanner reason="该固定版本已撤权或不可用。" />
        </>
      )}
    </div>
  )
}
