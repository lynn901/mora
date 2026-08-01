import { useEffect, useState } from "react"
import { Clock, RotateCcw, GitCompare, AlertTriangle } from "lucide-react"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogFooter, DialogDescription } from "@/components/ui/dialog"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import type { DocumentVersion, User } from "@/types"
import { apiGetVersions, apiRollbackVersion, apiGetUsers } from "@/api"
import { useMoraStore } from "@/stores/mora"

function computeDiff(oldText: string, newText: string): { type: "added" | "removed" | "unchanged"; text: string }[] {
  const oldLines = oldText.split("\n")
  const newLines = newText.split("\n")
  const result: { type: "added" | "removed" | "unchanged"; text: string }[] = []
  const maxLen = Math.max(oldLines.length, newLines.length)
  for (let i = 0; i < maxLen; i++) {
    const oldLine = oldLines[i]
    const newLine = newLines[i]
    if (oldLine === newLine) {
      result.push({ type: "unchanged", text: oldLine || "" })
    } else {
      if (oldLine !== undefined) result.push({ type: "removed", text: oldLine })
      if (newLine !== undefined) result.push({ type: "added", text: newLine })
    }
  }
  return result
}

export function VersionHistory() {
  const { currentDocument, selectNode } = useMoraStore()
  const [versions, setVersions] = useState<DocumentVersion[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [selectedVersions, setSelectedVersions] = useState<string[]>([])
  const [showDiff, setShowDiff] = useState(false)
  const [showRollback, setShowRollback] = useState<string | null>(null)

  useEffect(() => {
    if (currentDocument) {
      apiGetVersions(currentDocument.id).then(setVersions)
    }
    apiGetUsers().then(setUsers)
  }, [currentDocument?.id])

  const toggleVersion = (id: string) => {
    setSelectedVersions((prev) => {
      if (prev.includes(id)) return prev.filter((v) => v !== id)
      if (prev.length >= 2) return [prev[1], id]
      return [...prev, id]
    })
  }

  const handleRollback = async (versionId: string) => {
    if (!currentDocument) return
    await apiRollbackVersion(currentDocument.id, versionId)
    setShowRollback(null)
    await selectNode(currentDocument.nodeId)
    const newVersions = await apiGetVersions(currentDocument.id)
    setVersions(newVersions)
  }

  const diffData = selectedVersions.length === 2
    ? computeDiff(
        versions.find((v) => v.id === selectedVersions[0])?.content || "",
        versions.find((v) => v.id === selectedVersions[1])?.content || ""
      )
    : []

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center gap-2 p-3 border-b">
        <Clock className="size-4" />
        <span className="font-medium text-sm">Version History</span>
        {selectedVersions.length === 2 && (
          <Button variant="outline" size="sm" className="h-7 text-xs ml-auto" onClick={() => setShowDiff(true)}>
            <GitCompare className="size-3" /> Compare
          </Button>
        )}
      </div>

      <ScrollArea className="flex-1">
        <div className="p-3">
          <div className="relative">
            <div className="absolute left-4 top-0 bottom-0 w-px bg-border" />
            {versions.length === 0 ? (
              <div className="text-center text-sm text-muted-foreground py-8">No version history</div>
            ) : (
              <div className="space-y-4">
                {[...versions].reverse().map((v, i) => {
                  const user = users.find((u) => u.id === v.createdBy)
                  const isSelected = selectedVersions.includes(v.id)
                  return (
                    <div key={v.id} className="relative flex gap-3 pl-8">
                      <div className={cn(
                        "absolute left-2.5 top-1 size-3 rounded-full border-2",
                        isSelected ? "bg-primary border-primary" : "bg-background border-border"
                      )} />
                      <div className="flex-1 min-w-0">
                        <div className={cn(
                          "p-3 rounded-lg border cursor-pointer transition-colors",
                          isSelected ? "border-primary bg-primary/5" : "hover:bg-accent/50"
                        )} onClick={() => toggleVersion(v.id)}>
                          <div className="flex items-center gap-2">
                            <Avatar className="size-5">
                              <AvatarFallback className="text-[10px]">{user?.name?.split(" ").map((n) => n[0]).join("") || "?"}</AvatarFallback>
                            </Avatar>
                            <span className="text-xs font-medium">{user?.name || "Unknown"}</span>
                            <Badge variant="outline" className="text-[10px] px-1 py-0 ml-auto">v{v.version}</Badge>
                          </div>
                          {v.changeSummary && <p className="text-xs text-muted-foreground mt-1">{v.changeSummary}</p>}
                          <p className="text-[10px] text-muted-foreground mt-1">{new Date(v.createdAt).toLocaleString()}</p>
                        </div>
                        {i === 0 && (
                          <Button variant="ghost" size="sm" className="h-6 text-xs mt-1 text-muted-foreground" onClick={(e) => { e.stopPropagation(); setShowRollback(v.id) }}>
                            <RotateCcw className="size-3 mr-1" /> Current version
                          </Button>
                        )}
                        {i > 0 && (
                          <Button variant="ghost" size="sm" className="h-6 text-xs mt-1" onClick={(e) => { e.stopPropagation(); setShowRollback(v.id) }}>
                            <RotateCcw className="size-3 mr-1" /> Rollback to this
                          </Button>
                        )}
                      </div>
                    </div>
                  )
                })}
              </div>
            )}
          </div>
        </div>
      </ScrollArea>

      <Dialog open={showDiff} onOpenChange={setShowDiff}>
        <DialogContent className="max-w-3xl max-h-[80vh] overflow-hidden flex flex-col">
          <DialogHeader>
            <DialogTitle>Version Comparison</DialogTitle>
            <DialogDescription>Comparing versions {selectedVersions[0]?.slice(0, 8)} and {selectedVersions[1]?.slice(0, 8)}</DialogDescription>
          </DialogHeader>
          <div className="flex-1 overflow-auto font-mono text-xs">
            {diffData.map((line, i) => (
              <div key={i} className={cn(
                "px-3 py-0.5",
                line.type === "added" && "bg-green-100 dark:bg-green-900/30 text-green-800 dark:text-green-200",
                line.type === "removed" && "bg-red-100 dark:bg-red-900/30 text-red-800 dark:text-red-200 line-through",
              )}>
                <span className="inline-block w-4 text-muted-foreground select-none">{line.type === "added" ? "+" : line.type === "removed" ? "-" : " "}</span>
                {line.text}
              </div>
            ))}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setShowDiff(false)}>Close</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={!!showRollback} onOpenChange={(open) => !open && setShowRollback(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2"><AlertTriangle className="size-5 text-destructive" /> Rollback Confirmation</DialogTitle>
            <DialogDescription>Are you sure you want to rollback to this version? A new version will be created with the old content.</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setShowRollback(null)}>Cancel</Button>
            <Button variant="destructive" onClick={() => showRollback && handleRollback(showRollback)}>Rollback</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
