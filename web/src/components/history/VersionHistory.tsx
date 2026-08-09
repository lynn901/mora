import { useEffect, useState } from "react"
import { Clock, RotateCcw, GitCompare, AlertTriangle } from "lucide-react"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
  DialogDescription,
} from "@/components/ui/dialog"
import type { DocumentVersion, User } from "@/types"
import { apiGetVersions, apiRollbackVersion, apiGetUsers } from "@/api"
import { useMoraStore } from "@/stores/mora"

function computeDiff(
  oldText: string,
  newText: string
): { type: "added" | "removed" | "unchanged"; text: string }[] {
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
  const currentDocumentId = currentDocument?.id
  const [versions, setVersions] = useState<DocumentVersion[]>([])
  const [users, setUsers] = useState<User[]>([])
  const [selectedVersions, setSelectedVersions] = useState<string[]>([])
  const [showDiff, setShowDiff] = useState(false)
  const [showRollback, setShowRollback] = useState<string | null>(null)

  useEffect(() => {
    if (currentDocumentId) {
      apiGetVersions(currentDocumentId).then(setVersions)
    }
    apiGetUsers().then(setUsers)
  }, [currentDocumentId])

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

  const diffData =
    selectedVersions.length === 2
      ? computeDiff(
          versions.find((v) => v.id === selectedVersions[0])?.content || "",
          versions.find((v) => v.id === selectedVersions[1])?.content || ""
        )
      : []

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2 border-b p-3">
        <Clock className="size-4" />
        <span className="text-sm font-medium">Version History</span>
        {selectedVersions.length === 2 && (
          <Button
            variant="outline"
            size="sm"
            className="ml-auto h-7 text-xs"
            onClick={() => setShowDiff(true)}
          >
            <GitCompare className="size-3" /> Compare
          </Button>
        )}
      </div>

      <ScrollArea className="flex-1">
        <div className="p-3">
          <div className="relative">
            <div className="absolute top-0 bottom-0 left-4 w-px bg-border" />
            {versions.length === 0 ? (
              <div className="py-8 text-center text-sm text-muted-foreground">
                No version history
              </div>
            ) : (
              <div className="space-y-4">
                {[...versions].reverse().map((v, i) => {
                  const user = users.find((u) => u.id === v.createdBy)
                  const isSelected = selectedVersions.includes(v.id)
                  return (
                    <div key={v.id} className="relative flex gap-3 pl-8">
                      <div
                        className={cn(
                          "absolute top-1 left-2.5 size-3 rounded-full border-2",
                          isSelected
                            ? "border-primary bg-primary"
                            : "border-border bg-background"
                        )}
                      />
                      <div className="min-w-0 flex-1">
                        <div
                          className={cn(
                            "cursor-pointer rounded-lg border p-3 transition-colors",
                            isSelected
                              ? "border-primary bg-primary/5"
                              : "hover:bg-accent/50"
                          )}
                          onClick={() => toggleVersion(v.id)}
                        >
                          <div className="flex items-center gap-2">
                            <Avatar className="size-5">
                              <AvatarFallback className="text-[10px]">
                                {user?.name
                                  ?.split(" ")
                                  .map((n) => n[0])
                                  .join("") || "?"}
                              </AvatarFallback>
                            </Avatar>
                            <span className="text-xs font-medium">
                              {user?.name || "Unknown"}
                            </span>
                            <Badge
                              variant="outline"
                              className="ml-auto px-1 py-0 text-[10px]"
                            >
                              v{v.version}
                            </Badge>
                          </div>
                          {v.changeSummary && (
                            <p className="mt-1 text-xs text-muted-foreground">
                              {v.changeSummary}
                            </p>
                          )}
                          <p className="mt-1 text-[10px] text-muted-foreground">
                            {new Date(v.createdAt).toLocaleString()}
                          </p>
                        </div>
                        {i === 0 && (
                          <Button
                            variant="ghost"
                            size="sm"
                            className="mt-1 h-6 text-xs text-muted-foreground"
                            onClick={(e) => {
                              e.stopPropagation()
                              setShowRollback(v.id)
                            }}
                          >
                            <RotateCcw className="mr-1 size-3" /> Current
                            version
                          </Button>
                        )}
                        {i > 0 && (
                          <Button
                            variant="ghost"
                            size="sm"
                            className="mt-1 h-6 text-xs"
                            onClick={(e) => {
                              e.stopPropagation()
                              setShowRollback(v.id)
                            }}
                          >
                            <RotateCcw className="mr-1 size-3" /> Rollback to
                            this
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
        <DialogContent className="flex max-h-[80vh] max-w-3xl flex-col overflow-hidden">
          <DialogHeader>
            <DialogTitle>Version Comparison</DialogTitle>
            <DialogDescription>
              Comparing versions {selectedVersions[0]?.slice(0, 8)} and{" "}
              {selectedVersions[1]?.slice(0, 8)}
            </DialogDescription>
          </DialogHeader>
          <div className="flex-1 overflow-auto font-mono text-xs">
            {diffData.map((line, i) => (
              <div
                key={i}
                className={cn(
                  "px-3 py-0.5",
                  line.type === "added" &&
                    "bg-success/10 text-success dark:text-success",
                  line.type === "removed" &&
                    "bg-destructive/10 text-destructive line-through dark:text-destructive"
                )}
              >
                <span className="inline-block w-4 text-muted-foreground select-none">
                  {line.type === "added"
                    ? "+"
                    : line.type === "removed"
                      ? "-"
                      : " "}
                </span>
                {line.text}
              </div>
            ))}
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setShowDiff(false)}>
              Close
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog
        open={!!showRollback}
        onOpenChange={(open) => !open && setShowRollback(null)}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <AlertTriangle className="size-5 text-destructive" /> Rollback
              Confirmation
            </DialogTitle>
            <DialogDescription>
              Are you sure you want to rollback to this version? A new version
              will be created with the old content.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <Button variant="outline" onClick={() => setShowRollback(null)}>
              Cancel
            </Button>
            <Button
              variant="destructive"
              onClick={() => showRollback && handleRollback(showRollback)}
            >
              Rollback
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
