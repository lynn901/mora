import { useEffect, useState } from "react"
import {
  BookOpen,
  Search,
  Shield,
  Clock,
  PanelLeftClose,
  Menu,
  LogOut,
  Upload,
  Gauge,
  X,
  Boxes,
  Inbox,
  Link2,
} from "lucide-react"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
} from "@/components/ui/select"
import { DirectoryTree } from "@/components/tree/DirectoryTree"
import { BlockEditor } from "@/components/editor/BlockEditor"
import { SearchPanel } from "@/components/search/SearchPanel"
import { RBACPanel } from "@/components/rbac/RBACPanel"
import { AssetsPanel } from "@/components/assets/AssetsPanel"
import { ReviewInboxPage } from "@/components/assets/ReviewInboxPage"
import { BindingPanel } from "@/components/binding/BindingPanel"
import { CollabSidebar } from "@/components/collab/CollabSidebar"
import { VersionHistory } from "@/components/history/VersionHistory"
import { ThemeToggle } from "@/components/mora/ThemeToggle"
import { ErrorBoundary } from "@/components/ui/error-boundary"
import { ParseHost } from "@/components/parse/ParseHost"
import { ParseMonitoringPanel } from "@/components/parse/ParseMonitoringPanel"
import { useMoraStore } from "@/stores/mora"
import { useParseStore } from "@/stores/parse"
import { useCollabStore } from "@/stores/collab"
import { useAuthStore } from "@/stores/auth"

type SidePanel =
  | "tree"
  | "search"
  | "rbac"
  | "history"
  | "assets"
  | "review"
  | "bindings"

export function MoraLayout({ children }: { children?: React.ReactNode }) {
  const {
    currentWorkspace,
    workspaces,
    currentDocument,
    isLoading,
    error,
    loadWorkspaces,
    setWorkspace,
    selectNode,
  } = useMoraStore()
  const { setUploadOpen, setMonitoringOpen, monitoringOpen } = useParseStore()
  const [activePanel, setActivePanel] = useState<SidePanel>("tree")
  const [sidebarOpen, setSidebarOpen] = useState(true)
  const [isMobile, setIsMobile] = useState(false)
  const { initCollab, destroyCollab } = useCollabStore()
  const { user, logout } = useAuthStore()
  const currentDocumentId = currentDocument?.id
  const currentDocumentVersion = currentDocument?.versionNo
  const userName = user?.name || "User"

  useEffect(() => {
    loadWorkspaces()
    const check = () => setIsMobile(window.innerWidth < 768)
    check()
    window.addEventListener("resize", check)
    return () => window.removeEventListener("resize", check)
  }, [loadWorkspaces])

  useEffect(() => {
    if (currentDocumentId) {
      initCollab(currentDocumentId, "u1", userName)
    } else {
      destroyCollab()
    }
    return () => {
      destroyCollab()
    }
  }, [
    currentDocumentId,
    currentDocumentVersion,
    destroyCollab,
    initCollab,
    userName,
  ])

  const panelContent: Record<SidePanel, React.ReactNode> = {
    tree: <DirectoryTree />,
    search: (
      <SearchPanel
        onNavigate={(nodeId) => {
          selectNode(nodeId)
          if (isMobile) setSidebarOpen(false)
        }}
      />
    ),
    rbac: <RBACPanel />,
    history: <VersionHistory />,
    assets: <AssetsPanel />,
    review: <ReviewInboxPage />,
    bindings: <BindingPanel />,
  }

  const panelLabels: Record<SidePanel, string> = {
    tree: "目录",
    search: "搜索",
    rbac: "权限",
    history: "历史",
    assets: "资产",
    review: "审核",
    bindings: "配装",
  }

  const panelIcons: Record<SidePanel, React.ReactNode> = {
    tree: <BookOpen className="size-4" />,
    search: <Search className="size-4" />,
    rbac: <Shield className="size-4" />,
    history: <Clock className="size-4" />,
    assets: <Boxes className="size-4" />,
    review: <Inbox className="size-4" />,
    bindings: <Link2 className="size-4" />,
  }

  if (error) {
    return (
      <div className="flex h-screen items-center justify-center" role="alert">
        <div className="text-center">
          <p className="font-medium text-destructive">工作空间加载失败</p>
          <p className="mt-1 text-sm text-muted-foreground">{error}</p>
          <Button variant="outline" className="mt-4" onClick={loadWorkspaces}>
            重试
          </Button>
        </div>
      </div>
    )
  }

  return (
    <div className="flex h-screen overflow-hidden">
      {isMobile && sidebarOpen && (
        <div
          className="fixed inset-0 z-40 bg-black/50 md:hidden"
          onClick={() => setSidebarOpen(false)}
        />
      )}

      <aside
        className={cn(
          "z-50 flex flex-col border-r bg-sidebar transition-all duration-200",
          isMobile
            ? sidebarOpen
              ? "fixed inset-y-0 left-0 w-72"
              : "hidden"
            : "w-72"
        )}
      >
        <div className="flex items-center gap-2 border-b p-3">
          {isMobile && (
            <Button
              variant="ghost"
              size="icon"
              className="size-8 shrink-0"
              onClick={() => setSidebarOpen(false)}
            >
              <PanelLeftClose className="size-4" />
            </Button>
          )}
          <Select
            value={currentWorkspace?.id || ""}
            onValueChange={(v) => {
              const ws = workspaces.find((w) => w.id === v)
              if (ws) setWorkspace(ws)
            }}
          >
            <SelectTrigger className="h-8 border-0 px-2 text-sm font-medium shadow-none focus:ring-0">
              <span className="flex items-center gap-2 truncate">
                <span>{currentWorkspace?.icon || "📚"}</span>
                <span className="truncate">
                  {currentWorkspace?.name || "选择工作空间"}
                </span>
              </span>
            </SelectTrigger>
            <SelectContent>
              {workspaces.map((ws) => (
                <SelectItem key={ws.id} value={ws.id}>
                  <span className="flex items-center gap-2">
                    {ws.icon} {ws.name}
                  </span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <div className="ml-auto flex items-center gap-0.5">
            <ThemeToggle />
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7 shrink-0"
                  onClick={logout}
                  aria-label="退出登录"
                >
                  <LogOut className="size-3.5" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>退出登录</TooltipContent>
            </Tooltip>
          </div>
        </div>

        <div className="flex border-b">
          {(["tree", "search", "rbac", "history", "bindings"] as SidePanel[]).map(
            (panel) => (
              <Tooltip key={panel}>
                <TooltipTrigger asChild>
                  <button
                    className={cn(
                      "flex flex-1 cursor-pointer items-center justify-center border-b-2 py-2.5 text-xs font-medium transition-colors",
                      activePanel === panel
                        ? "border-primary text-primary"
                        : "border-transparent text-muted-foreground hover:text-foreground"
                    )}
                    onClick={() => setActivePanel(panel)}
                    aria-label={panelLabels[panel]}
                  >
                    {panelIcons[panel]}
                  </button>
                </TooltipTrigger>
                <TooltipContent side="bottom">
                  {panelLabels[panel]}
                </TooltipContent>
              </Tooltip>
            )
          )}
        </div>

        {/* Parse entry points — upload + monitoring, aligned with the tree panel (UI spec §2.1). */}
        {activePanel === "tree" && (
          <div className="flex items-center gap-1 border-b px-2 py-1.5">
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 flex-1 text-xs"
                  onClick={() => setUploadOpen(true)}
                >
                  <Upload className="size-3.5" /> 上传并解析
                </Button>
              </TooltipTrigger>
              <TooltipContent>上传文档并解析</TooltipContent>
            </Tooltip>
            <Tooltip>
              <TooltipTrigger asChild>
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-7"
                  onClick={() => setMonitoringOpen(true)}
                  aria-label="解析监控"
                >
                  <Gauge className="size-3.5" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>解析监控</TooltipContent>
            </Tooltip>
          </div>
        )}

        <div className="flex-1 overflow-hidden">
          {panelContent[activePanel]}
        </div>
      </aside>

      <main className="flex flex-1 overflow-hidden">
        <div className="flex flex-1 flex-col overflow-hidden">
          {isMobile && !sidebarOpen && (
            <div className="flex items-center gap-2 border-b p-2">
              <Button
                variant="ghost"
                size="icon"
                className="size-8"
                onClick={() => setSidebarOpen(true)}
              >
                <Menu className="size-4" />
              </Button>
              <span className="truncate text-sm font-medium">
                {currentDocument?.title || "Mora"}
              </span>
            </div>
          )}

          {children ? (
            <div className="flex-1 overflow-hidden">{children}</div>
          ) : activePanel === "assets" ||
            activePanel === "review" ||
            activePanel === "bindings" ? (
            // Full-area asset / review / bindings views — these are primary
            // destinations, not sidebar sub-panels, so they take the main stage.
            <div className="flex-1 overflow-hidden">{panelContent[activePanel]}</div>
          ) : monitoringOpen ? (
            <div className="relative flex-1 overflow-hidden">
              <Button
                variant="ghost"
                size="icon"
                className="absolute top-3 right-3 z-10 size-7"
                onClick={() => setMonitoringOpen(false)}
                aria-label="关闭监控"
              >
                <X className="size-4" />
              </Button>
              <ParseMonitoringPanel />
            </div>
          ) : isLoading && !currentDocument ? (
            <div className="flex flex-1 items-center justify-center">
              <div className="text-center">
                <div className="mx-auto size-8 animate-spin rounded-full border-2 border-primary border-t-transparent" />
                <p className="mt-3 text-sm text-muted-foreground">
                  正在准备工作空间…
                </p>
              </div>
            </div>
          ) : currentDocument ? (
            <>
              <div className="flex items-center gap-3 border-b px-6 py-3">
                <h1 className="flex-1 truncate text-lg font-semibold">
                  {currentDocument.title}
                </h1>
                <div className="flex items-center gap-2">
                  <CollabSidebar />
                </div>
              </div>
              <ErrorBoundary>
                <div className="flex-1 overflow-auto">
                  <BlockEditor />
                </div>
              </ErrorBoundary>
            </>
          ) : (
            <div className="flex flex-1 items-center justify-center">
              <div className="max-w-sm text-center">
                <BookOpen className="mx-auto size-12 text-muted-foreground/50" />
                <h2 className="mt-4 text-lg font-medium">欢迎使用 Mora</h2>
                <p className="mt-1 text-sm text-muted-foreground">
                  从左侧目录选择一个页面开始编辑，或新建一个页面。
                </p>
              </div>
            </div>
          )}
        </div>
      </main>

      {/* Parse overlays — upload dialog, progress drawer, batch reparse dialog. */}
      <ParseHost />
    </div>
  )
}
