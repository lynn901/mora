// Phase 5-4 — batch 配装 dialog (§5.2). Lets the operator build multiple
// binding inputs and submit them in one transaction with an Idempotency-Key.
// Each row is one binding: scope / target / effect / version_policy /
// pinned version (when asset+pinned) / delivery_mode / priority. §5.1 is
// honored inside the FixedVersionSelector. §1.2: no secret values exist on
// this surface — bindings reference versions, they never carry credentials.
import { useEffect, useState } from "react"
import { Plus, Trash2, Loader2 } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Separator } from "@/components/ui/separator"
import { DeliveryModeSelector } from "./DeliveryModeSelector"
import { FixedVersionSelector } from "./FixedVersionSelector"
import {
  BINDING_EFFECT,
  BINDING_SCOPE,
  newIdempotencyKey,
} from "./binding-utils"
import type {
  BindingDeliveryMode,
  BindingEffect,
  BindingInput,
  BindingScopeKind,
  BindingVersionPolicy,
} from "@/types/binding"

// A draft row is a BindingInput plus a transient asset-id field used to feed
// the FixedVersionSelector (the selector needs the asset id to load versions).
interface DraftRow extends Omit<BindingInput, "asset_id" | "asset_type"> {
  asset_id: string
  asset_type: string | null
}

function emptyRow(): DraftRow {
  return {
    id: null,
    etag: null,
    scope_kind: "asset",
    asset_id: "",
    asset_type: null,
    effect: "allow",
    version_policy: "follow_published",
    pinned_version_id: null,
    delivery_mode: "tool",
    priority: 100,
  }
}

export function BatchBindingDialog({
  open,
  onOpenChange,
  onSubmit,
  submitting,
  error,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSubmit: (inputs: BindingInput[], idempotencyKey: string) => Promise<void>
  submitting: boolean
  error: string | null
}) {
  const [rows, setRows] = useState<DraftRow[]>([emptyRow()])
  // Regenerate the Idempotency-Key each time the dialog opens so a fresh
  // batch gets a fresh key (§5.2). A retry of the same logical batch would
  // reuse a key the caller cached; here each open is a new logical batch.
  const [idempotencyKey, setIdempotencyKey] = useState("")
  useEffect(() => {
    if (open) {
      // eslint-disable-next-line react-hooks/set-state-in-effect
      setRows([emptyRow()])
      setIdempotencyKey(newIdempotencyKey())
    }
  }, [open])

  const update = (idx: number, patch: Partial<DraftRow>) => {
    setRows((rs) =>
      rs.map((r, i) => {
        if (i !== idx) return r
        const next = { ...r, ...patch }
        // §5.1 invariant: pinned requires scope_kind=asset AND a version id.
        // Switching to non-asset scope or follow_published clears the pin.
        if (
          next.scope_kind !== "asset" ||
          next.version_policy !== "pinned"
        ) {
          next.pinned_version_id = null
        }
        // asset_type scope carries asset_type, not asset_id.
        if (next.scope_kind === "asset_type") {
          next.asset_id = ""
        }
        return next
      }),
    )
  }

  const addRow = () => setRows((rs) => [...rs, emptyRow()])
  const removeRow = (idx: number) =>
    setRows((rs) => rs.filter((_, i) => i !== idx))

  // Validate before submit. Each asset-scoped row needs an asset_id; each
  // pinned row needs a pinned_version_id; asset_type rows need asset_type.
  const validated = rows.map((r) => {
    const out: BindingInput = {
      id: r.id,
      etag: r.etag,
      scope_kind: r.scope_kind,
      asset_id: r.scope_kind === "asset" ? r.asset_id || null : null,
      asset_type: r.scope_kind === "asset_type" ? r.asset_type || null : null,
      effect: r.effect,
      version_policy: r.version_policy,
      pinned_version_id:
        r.scope_kind === "asset" && r.version_policy === "pinned"
          ? r.pinned_version_id
          : null,
      delivery_mode: r.delivery_mode,
      priority: r.priority,
    }
    return out
  })
  const rowValid = (r: DraftRow) => {
    if (r.scope_kind === "asset" && !r.asset_id.trim()) return false
    if (r.scope_kind === "asset_type" && !r.asset_type?.trim()) return false
    if (
      r.scope_kind === "asset" &&
      r.version_policy === "pinned" &&
      !r.pinned_version_id
    )
      return false
    return true
  }
  const allValid = rows.length > 0 && rows.every(rowValid)

  const handleSubmit = async () => {
    if (!allValid || submitting) return
    await onSubmit(validated, idempotencyKey)
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>批量配装</DialogTitle>
          <DialogDescription>
            一次性为该 Agent 创建多条 Binding，事务内原子写入 + 幂等键
            （Idempotency-Key）。显式 deny 优先于所有 allow scope。
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3">
          {rows.map((r, idx) => {
            const EffectIcon = BINDING_EFFECT[r.effect].icon
            const ScopeIcon = BINDING_SCOPE[r.scope_kind].icon
            return (
              <div
                key={idx}
                className="space-y-2 rounded-md border bg-muted/20 p-3"
              >
                <div className="flex items-center justify-between">
                  <span className="text-xs font-medium text-muted-foreground">
                    Binding #{idx + 1}
                  </span>
                  {rows.length > 1 && (
                    <Button
                      variant="ghost"
                      size="sm"
                      className="h-6 px-1.5 text-xs text-destructive"
                      onClick={() => removeRow(idx)}
                    >
                      <Trash2 className="size-3" /> 删除
                    </Button>
                  )}
                </div>

                {/* scope + target */}
                <div className="grid grid-cols-2 gap-2">
                  <div className="space-y-1">
                    <Label className="text-xs">范围</Label>
                    <Select
                      value={r.scope_kind}
                      onValueChange={(v) =>
                        update(idx, {
                          scope_kind: v as BindingScopeKind,
                        })
                      }
                    >
                      <SelectTrigger className="h-8 text-sm">
                        <ScopeIcon className="size-3" />
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="asset">资产</SelectItem>
                        <SelectItem value="workspace">工作空间</SelectItem>
                        <SelectItem value="asset_type">资产类型</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>

                  <div className="space-y-1">
                    <Label className="text-xs">目标</Label>
                    {r.scope_kind === "asset" && (
                      <Input
                        className="h-8 text-sm"
                        placeholder="资产 ID"
                        value={r.asset_id}
                        onChange={(e) =>
                          update(idx, { asset_id: e.target.value })
                        }
                      />
                    )}
                    {r.scope_kind === "asset_type" && (
                      <Select
                        value={r.asset_type ?? undefined}
                        onValueChange={(v) =>
                          update(idx, { asset_type: v })
                        }
                      >
                        <SelectTrigger className="h-8 text-sm">
                          <SelectValue placeholder="选择类型" />
                        </SelectTrigger>
                        <SelectContent>
                          <SelectItem value="document">文档</SelectItem>
                          <SelectItem value="codebase">代码库</SelectItem>
                          <SelectItem value="memory">记忆</SelectItem>
                          <SelectItem value="skill">技能</SelectItem>
                        </SelectContent>
                      </Select>
                    )}
                    {r.scope_kind === "workspace" && (
                      <Input
                        disabled
                        className="h-8 text-sm text-muted-foreground"
                        placeholder="（当前工作空间，无需目标）"
                      />
                    )}
                  </div>
                </div>

                {/* effect */}
                <div className="flex items-center gap-2">
                  <Label className="text-xs">效果</Label>
                  <Select
                    value={r.effect}
                    onValueChange={(v) =>
                      update(idx, { effect: v as BindingEffect })
                    }
                  >
                    <SelectTrigger className="h-8 flex-1 text-sm">
                      <EffectIcon className="size-3" />
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="allow">允许</SelectItem>
                      <SelectItem value="deny">拒绝</SelectItem>
                    </SelectContent>
                  </Select>
                </div>

                {/* version policy + fixed version selector */}
                <div className="grid grid-cols-2 gap-2">
                  <div className="space-y-1">
                    <Label className="text-xs">版本策略</Label>
                    <Select
                      value={r.version_policy}
                      onValueChange={(v) =>
                        update(idx, {
                          version_policy: v as BindingVersionPolicy,
                        })
                      }
                      disabled={r.scope_kind !== "asset"}
                    >
                      <SelectTrigger className="h-8 text-sm">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="follow_published">
                          跟随已发布
                        </SelectItem>
                        <SelectItem value="pinned">固定版本</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  <div className="space-y-1">
                    <Label className="text-xs">优先级</Label>
                    <Input
                      type="number"
                      className="h-8 text-sm"
                      value={r.priority}
                      onChange={(e) =>
                        update(idx, {
                          priority: Number(e.target.value) || 0,
                        })
                      }
                    />
                  </div>
                </div>

                {r.scope_kind === "asset" &&
                  r.version_policy === "pinned" && (
                    <FixedVersionSelector
                      assetId={r.asset_id || null}
                      pinnedVersionId={r.pinned_version_id}
                      onValueChange={(v) =>
                        update(idx, { pinned_version_id: v })
                      }
                    />
                  )}

                {/* delivery mode */}
                <div className="space-y-1">
                  <Label className="text-xs">交付模式</Label>
                  <DeliveryModeSelector
                    value={r.delivery_mode}
                    onValueChange={(v) =>
                      update(idx, { delivery_mode: v as BindingDeliveryMode })
                    }
                  />
                </div>

                {!rowValid(r) && (
                  <p className="text-[11px] text-destructive">
                    该行未满足：资产范围需资产 ID；固定版本需选择版本；资产类型范围需选择类型。
                  </p>
                )}
              </div>
            )
          })}

          <Button
            variant="outline"
            size="sm"
            className="w-full"
            onClick={addRow}
          >
            <Plus className="size-3" /> 添加一条 Binding
          </Button>
        </div>

        <Separator />

        <div className="space-y-1 text-[11px] text-muted-foreground">
          <p>
            §1.2：本界面不展示任何 Secret 值，只展示需求声明与版本引用。
          </p>
          <p>
            Idempotency-Key：
            <code className="font-mono">{idempotencyKey}</code>
          </p>
        </div>

        {error && (
          <p className="flex items-center gap-1 text-xs text-destructive">
            <span>{error}</span>
          </p>
        )}

        <DialogFooter>
          <Button
            variant="outline"
            size="sm"
            onClick={() => onOpenChange(false)}
            disabled={submitting}
          >
            取消
          </Button>
          <Button size="sm" onClick={handleSubmit} disabled={!allValid || submitting}>
            {submitting ? (
              <>
                <Loader2 className="size-3 animate-spin" /> 提交中…
              </>
            ) : (
              <>提交配装</>
            )}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
