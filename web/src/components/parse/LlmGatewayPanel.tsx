// LlmGatewayPanel — screen 5 (UI spec §3.5). Admin config for external
// OpenAI-compatible endpoints. Surfaces the privacy / external endpoint controls
// (F0.2): content leaves the workspace, calls are audited and rate-limited,
// keys come from env/secret (never stored in plaintext).
//
// P2 note (architecture §0): the sidecar gateway is not yet wired; this page
// renders the management surface with empty-state + form so the design is
// ready when the backend lands. The current page only manages configuration;
// the backend will issue calls once the gateway is connected.
import { useState } from "react"
import { Plus, Pencil, Trash2, Plug, ShieldAlert, Info } from "lucide-react"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
  SheetDescription,
  SheetFooter,
  SheetClose,
} from "@/components/ui/sheet"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Separator } from "@/components/ui/separator"

interface GatewayEndpoint {
  id: string
  name: string
  baseUrl: string
  modelName: string
  enabled: boolean
  desensitized: boolean
  lastCall: string | null
}

const EMPTY: GatewayEndpoint[] = []

export function LlmGatewayPanel() {
  const [endpoints, setEndpoints] = useState<GatewayEndpoint[]>(EMPTY)
  const [editing, setEditing] = useState<GatewayEndpoint | null>(null)
  const [draft, setDraft] = useState<Omit<GatewayEndpoint, "id">>({
    name: "",
    baseUrl: "",
    modelName: "",
    enabled: false,
    desensitized: false,
    lastCall: null,
  })
  const [testResult, setTestResult] = useState<{ ok: boolean; msg: string } | null>(null)
  // Sheet visibility is explicit state — never derived from draft emptiness,
  // otherwise the drawer traps the user whenever the name field is blank
  // (YS-85: auto-open + un-closeable on empty draft).
  const [sheetOpen, setSheetOpen] = useState(false)

  const openNew = () => {
    setEditing(null)
    setDraft({ name: "", baseUrl: "", modelName: "", enabled: false, desensitized: false, lastCall: null })
    setTestResult(null)
    setSheetOpen(true)
  }
  const openEdit = (ep: GatewayEndpoint) => {
    setEditing(ep)
    setDraft({ ...ep })
    setTestResult(null)
    setSheetOpen(true)
  }

  // Close from any source (cancel button, X, ESC): drop editing + hide sheet.
  const closeSheet = () => {
    setEditing(null)
    setSheetOpen(false)
  }

  const save = () => {
    if (!draft.name || !draft.baseUrl) return
    if (editing) {
      setEndpoints((eps) => eps.map((e) => (e.id === editing.id ? { ...editing, ...draft } : e)))
    } else {
      setEndpoints((eps) => [...eps, { ...draft, id: `ep-${Date.now()}` }])
    }
    closeSheet()
  }

  const remove = (id: string) => setEndpoints((eps) => eps.filter((e) => e.id !== id))

  const testConnection = () => {
    // The sidecar is not wired (P2); surface this honestly instead of faking it.
    setTestResult({ ok: false, msg: "网关后端尚未启用（二期），暂不可测试连接" })
  }

  return (
    <div className="flex h-full flex-col">
      <div className="border-b px-6 py-4">
        <h1 className="text-xl font-semibold">外部 LLM 网关</h1>
        <p className="text-sm text-muted-foreground">
          配置 OpenAI 兼容端点，供 VLM 图像描述、问答生成等生成式 LLM 特性复用
        </p>
      </div>

      <ScrollArea className="flex-1">
        <div className="mx-auto max-w-3xl px-6 py-4 space-y-4">
          {/* Alert strip — privacy / external endpoint controls (F0.2). */}
          <div className="flex items-start gap-2 rounded-md border border-info/30 bg-info/5 px-3 py-2 text-sm text-info">
            <Info className="size-4 shrink-0 mt-0.5" />
            <span>
              工作区内容会发往外部端点。启用前须完成内容脱敏评估；所有外部调用记审计、受速率限制。
            </span>
          </div>

          {/* Endpoint table */}
          <div className="rounded-lg border">
            <div className="grid grid-cols-[1.5fr_2fr_1.2fr_80px_60px_80px] gap-2 border-b px-3 py-2 text-xs font-medium text-muted-foreground">
              <span>名称</span>
              <span>base_url</span>
              <span>模型</span>
              <span>状态</span>
              <span>脱敏</span>
              <span className="text-right">操作</span>
            </div>
            {endpoints.length === 0 ? (
              <div className="flex flex-col items-center justify-center py-12 text-center">
                <Plug className="size-10 text-muted-foreground/40" />
                <h3 className="mt-3 text-sm font-medium">尚未配置外部端点</h3>
                <p className="mt-1 text-xs text-muted-foreground">VLM / 问答生成相关开关在解析配置中将维持灰显</p>
                <Button className="mt-4" size="sm" onClick={openNew}>
                  <Plus className="size-3.5" /> 新增端点
                </Button>
              </div>
            ) : (
              endpoints.map((ep) => (
                <div key={ep.id} className="grid grid-cols-[1.5fr_2fr_1.2fr_80px_60px_80px] items-center gap-2 border-b px-3 py-2 text-sm last:border-0">
                  <span className="truncate">{ep.name}</span>
                  <span className="truncate font-mono text-xs">{ep.baseUrl}</span>
                  <span className="truncate">{ep.modelName}</span>
                  <span>
                    {ep.enabled ? (
                      <Badge variant="success" className="text-[10px]">启用</Badge>
                    ) : (
                      <Badge variant="secondary" className="text-[10px]">禁用</Badge>
                    )}
                  </span>
                  <span>
                    {ep.desensitized ? (
                      <Badge variant="success" className="text-[10px]">已通过</Badge>
                    ) : (
                      <Badge variant="warning" className="text-[10px]">待评估</Badge>
                    )}
                  </span>
                  <span className="flex items-center justify-end gap-1">
                    <Button variant="ghost" size="icon" className="size-6" onClick={() => openEdit(ep)} aria-label="编辑">
                      <Pencil className="size-3.5" />
                    </Button>
                    <Button variant="ghost" size="icon" className="size-6" onClick={() => remove(ep.id)} aria-label="删除">
                      <Trash2 className="size-3.5" />
                    </Button>
                  </span>
                </div>
              ))
            )}
          </div>

          {/* Desensitization hard constraint — endpoints without assessment stay disabled. */}
          <div className="flex items-start gap-2 rounded-md border border-warning/30 bg-warning/5 px-3 py-2 text-xs text-warning">
            <ShieldAlert className="size-4 shrink-0 mt-0.5" />
            <span>未通过脱敏评估的端点不可启用；密钥通过环境变量 / K8s Secret 注入，不入库明文。</span>
          </div>

          {/* Boundary note (F0.4). */}
          <p className="text-xs text-muted-foreground">
            网关为一期平台基础能力；Embedding / Reranker 路径不受影响，向量入库仍走 TEI / Ollama。
          </p>
        </div>
      </ScrollArea>

      {/* Create / edit drawer */}
      <Sheet open={sheetOpen} onOpenChange={(o) => { if (!o) closeSheet() }}>
        <SheetContent side="right" className="w-full sm:max-w-[420px] p-0 gap-0">
          <SheetHeader className="px-6 pt-6 pb-3 border-b">
            <SheetTitle>{editing ? "编辑端点" : "新增端点"}</SheetTitle>
            <SheetDescription>配置 OpenAI 兼容端点</SheetDescription>
          </SheetHeader>
          <ScrollArea className="flex-1">
            <div className="px-6 py-4 space-y-4">
              <div className="space-y-1.5">
                <Label htmlFor="ep-name">名称</Label>
                <Input id="ep-name" value={draft.name} onChange={(e) => setDraft({ ...draft, name: e.target.value })} placeholder="生产环境 LLM" />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="ep-url">base_url</Label>
                <Input id="ep-url" value={draft.baseUrl} onChange={(e) => setDraft({ ...draft, baseUrl: e.target.value })} placeholder="https://llm.internal/v1" />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="ep-model">model_name</Label>
                <Input id="ep-model" value={draft.modelName} onChange={(e) => setDraft({ ...draft, modelName: e.target.value })} placeholder="qwen-vl-max" />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="ep-key">api_key 引用</Label>
                <Input id="ep-key" disabled placeholder="从环境变量 / Secret 选择" />
                <p className="text-xs text-muted-foreground">密钥不入库明文，通过环境变量 / K8s Secret 注入。</p>
              </div>
              <div className="flex items-center justify-between rounded-md border px-3 py-2">
                <Label htmlFor="ep-egress">允许出网</Label>
                <Switch id="ep-egress" checked={draft.enabled} onCheckedChange={(v) => setDraft({ ...draft, enabled: v })} />
              </div>
              <div className="flex items-center justify-between rounded-md border px-3 py-2">
                <Label htmlFor="ep-desens">已通过脱敏评估</Label>
                <Switch id="ep-desens" checked={draft.desensitized} onCheckedChange={(v) => setDraft({ ...draft, desensitized: v })} />
              </div>

              <Separator />
              <div className="space-y-1.5">
                <Button variant="ghost" size="sm" onClick={testConnection}>
                  <Plug className="size-3.5" /> 测试连接
                </Button>
                {testResult && (
                  <p className={testResult.ok ? "text-xs text-success" : "text-xs text-destructive"}>
                    {testResult.ok ? "连接成功" : "连接失败"}：{testResult.msg}
                  </p>
                )}
              </div>
            </div>
          </ScrollArea>
          <Separator />
          <SheetFooter className="px-6 py-3">
            <SheetClose asChild>
              <Button variant="outline">取消</Button>
            </SheetClose>
            <Button onClick={save} disabled={!draft.name || !draft.baseUrl}>
              保存
            </Button>
          </SheetFooter>
        </SheetContent>
      </Sheet>
    </div>
  )
}
