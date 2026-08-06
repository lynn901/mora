import { useEffect, useState } from "react"
import { BookOpen, Search, Shield, Clock, PanelLeftClose, PanelLeft, Menu, LogOut } from "lucide-react"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Tooltip, TooltipTrigger, TooltipContent } from "@/components/ui/tooltip"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { DirectoryTree } from "@/components/tree/DirectoryTree"
import { BlockEditor } from "@/components/editor/BlockEditor"
import { SearchPanel } from "@/components/search/SearchPanel"
import { RBACPanel } from "@/components/rbac/RBACPanel"
import { CollabSidebar } from "@/components/collab/CollabSidebar"
import { VersionHistory } from "@/components/history/VersionHistory"
import { ThemeToggle } from "@/components/mora/ThemeToggle"
import { ErrorBoundary } from "@/components/ui/error-boundary"
import { useMoraStore } from "@/stores/mora"
import { useCollabStore } from "@/stores/collab"
import { useAuthStore } from "@/stores/auth"

type SidePanel = "tree" | "search" | "rbac" | "history"

export function MoraLayout() {
  const {
    currentWorkspace, workspaces, currentDocument, isLoading, error,
    loadWorkspaces, setWorkspace, selectNode,
  } = useMoraStore()
  const [activePanel, setActivePanel] = useState<SidePanel>("tree")
  const [sidebarOpen, setSidebarOpen] = useState(true)
  const [isMobile, setIsMobile] = useState(false)
  const { initCollab, destroyCollab } = useCollabStore()
  const { user, logout } = useAuthStore()

  useEffect(() => {
    loadWorkspaces()
    const check = () => setIsMobile(window.innerWidth < 768)
    check()
    window.addEventListener("resize", check)
    return () => window.removeEventListener("resize", check)
  }, [])

  useEffect(() => {
    if (currentDocument) {
      initCollab(currentDocument.id, "u1", user?.name || "User")
    } else {
      destroyCollab()
    }
    return () => {
      destroyCollab()
    }
  }, [currentDocument?.id, currentDocument?.versionNo])

  const panelContent: Record<SidePanel, React.ReactNode> = {
    tree: <DirectoryTree />,
    search: <SearchPanel onNavigate={(nodeId) => { selectNode(nodeId); if (isMobile) setSidebarOpen(false) }} />,
    rbac: <RBACPanel />,
    history: <VersionHistory />,
  }

  const panelLabels: Record<SidePanel, string> = {
    tree: "目录",
    search: "搜索",
    rbac: "权限",
    history: "历史",
  }

  const panelIcons: Record<SidePanel, React.ReactNode> = {
    tree: <BookOpen className="size-4" />,
    search: <Search className="size-4" />,
    rbac: <Shield className="size-4" />,
    history: <Clock className="size-4" />,
  }

  if (error) {
    return (
      <div className="flex items-center justify-center h-screen" role="alert">
        <div className="text-center">
          <p className="text-destructive font-medium">工作空间加载失败</p>
          <p className="text-sm text-muted-foreground mt-1">{error}</p>
          <Button variant="outline" className="mt-4" onClick={loadWorkspaces}>重试</Button>
        </div>
      </div>
    )
  }

  return (
    <div className="flex h-screen overflow-hidden">
      {isMobile && sidebarOpen && (
        <div className="fixed inset-0 z-40 bg-black/50 md:hidden" onClick={() => setSidebarOpen(false)} />
      )}

      <aside className={cn(
        "flex flex-col border-r bg-sidebar transition-all duration-200 z-50",
        isMobile ? (sidebarOpen ? "fixed inset-y-0 left-0 w-72" : "hidden") : "w-72",
      )}>
        <div className="flex items-center gap-2 p-3 border-b">
          {isMobile && (
            <Button variant="ghost" size="icon" className="size-8 shrink-0" onClick={() => setSidebarOpen(false)}>
              <PanelLeftClose className="size-4" />
            </Button>
          )}
          <Select value={currentWorkspace?.id || ""} onValueChange={(v) => { const ws = workspaces.find((w) => w.id === v); if (ws) setWorkspace(ws) }}>
            <SelectTrigger className="h-8 text-sm font-medium border-0 shadow-none focus:ring-0 px-2">
              <span className="flex items-center gap-2 truncate">
                <span>{currentWorkspace?.icon || "📚"}</span>
                <span className="truncate">{currentWorkspace?.name || "选择工作空间"}</span>
              </span>
            </SelectTrigger>
            <SelectContent>
              {workspaces.map((ws) => (
                <SelectItem key={ws.id} value={ws.id}>
                  <span className="flex items-center gap-2">{ws.icon} {ws.name}</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <div className="flex items-center gap-0.5 ml-auto">
            <ThemeToggle />
            <Tooltip>
              <TooltipTrigger asChild>
                <Button variant="ghost" size="icon" className="size-7 shrink-0" onClick={logout} aria-label="退出登录">
                  <LogOut className="size-3.5" />
                </Button>
              </TooltipTrigger>
              <TooltipContent>退出登录</TooltipContent>
            </Tooltip>
          </div>
        </div>

        <div className="flex border-b">
          {(["tree", "search", "rbac", "history"] as SidePanel[]).map((panel) => (
            <Tooltip key={panel}>
              <TooltipTrigger asChild>
                <button
                  className={cn(
                    "flex-1 flex items-center justify-center py-2.5 text-xs font-medium transition-colors border-b-2 cursor-pointer",
                    activePanel === panel ? "border-primary text-primary" : "border-transparent text-muted-foreground hover:text-foreground"
                  )}
                  onClick={() => setActivePanel(panel)}
                  aria-label={panelLabels[panel]}
                >
                  {panelIcons[panel]}
                </button>
              </TooltipTrigger>
              <TooltipContent side="bottom">{panelLabels[panel]}</TooltipContent>
            </Tooltip>
          ))}
        </div>

        <div className="flex-1 overflow-hidden">
          {panelContent[activePanel]}
        </div>
      </aside>

      <main className="flex flex-1 overflow-hidden">
        <div className="flex-1 flex flex-col overflow-hidden">
          {isMobile && !sidebarOpen && (
            <div className="flex items-center gap-2 p-2 border-b">
              <Button variant="ghost" size="icon" className="size-8" onClick={() => setSidebarOpen(true)}>
                <Menu className="size-4" />
              </Button>
              <span className="text-sm font-medium truncate">{currentDocument?.title || "Mora"}</span>
            </div>
          )}

          {isLoading && !currentDocument ? (
            <div className="flex items-center justify-center flex-1">
              <div className="text-center">
                <div className="size-8 border-2 border-primary border-t-transparent rounded-full animate-spin mx-auto" />
                <p className="text-sm text-muted-foreground mt-3">正在准备工作空间…</p>
              </div>
            </div>
          ) : currentDocument ? (
            <>
              <div className="flex items-center gap-3 px-6 py-3 border-b">
                <h1 className="text-lg font-semibold truncate flex-1">{currentDocument.title}</h1>
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
            <div className="flex items-center justify-center flex-1">
              <div className="text-center max-w-sm">
                <BookOpen className="size-12 text-muted-foreground/50 mx-auto" />
                <h2 className="text-lg font-medium mt-4">欢迎使用 Mora</h2>
                <p className="text-sm text-muted-foreground mt-1">从左侧目录选择一个页面开始编辑，或新建一个页面。</p>
              </div>
            </div>
          )}
        </div>
      </main>
    </div>
  )
}
