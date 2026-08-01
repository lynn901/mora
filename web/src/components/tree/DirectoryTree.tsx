import { useCallback, useState } from "react"
import { ChevronRight, ChevronDown, Folder, FileText, Plus, GripVertical, SearchX, FilePlus, Trash2 } from "lucide-react"
import { cn } from "@/lib/utils"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Tooltip, TooltipTrigger, TooltipContent } from "@/components/ui/tooltip"
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { EmptyState } from "@/components/ui/empty-state"
import { StatusBadge } from "@/components/ui/status-badge"
import { toast } from "@/components/ui/sonner"
import type { TreeNode } from "@/types"
import { useMoraStore } from "@/stores/mora"

interface TreeNodeItemProps {
  node: TreeNode
  depth: number
  onSelect: (id: string) => void
  selectedId: string | null
  onDelete: (node: TreeNode) => void
}

/** Pure recursive filter over the tree by name. Hoisted out so it has no
 * component closure and stays stable across renders. */
function filterTree(nodes: TreeNode[], query: string): TreeNode[] {
  if (!query) return nodes
  return nodes.reduce<TreeNode[]>((acc, node) => {
    const matches = node.name.toLowerCase().includes(query.toLowerCase())
    const filteredChildren = node.children ? filterTree(node.children, query) : []
    if (matches || filteredChildren.length > 0) {
      acc.push({ ...node, children: filteredChildren.length > 0 ? filteredChildren : node.children })
    }
    return acc
  }, [])
}

function TreeNodeItem({ node, depth, onSelect, selectedId, onDelete }: TreeNodeItemProps) {
  const [expanded, setExpanded] = useState(true)
  const isFolder = node.type === "folder"
  const isSelected = selectedId === node.id
  const hasChildren = node.children && node.children.length > 0

  return (
    <div>
      <div
        className={cn(
          "group flex items-center gap-1 rounded-sm px-2 py-1.5 text-sm cursor-pointer hover:bg-accent/50 transition-colors",
          isSelected && "bg-accent text-accent-foreground font-medium"
        )}
        style={{ paddingLeft: `${depth * 16 + 8}px` }}
        onClick={() => {
          if (isFolder) setExpanded(!expanded)
          onSelect(node.id)
        }}
        role="treeitem"
        aria-expanded={isFolder ? expanded : undefined}
        aria-selected={isSelected}
        tabIndex={0}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault()
            if (isFolder) setExpanded(!expanded)
            onSelect(node.id)
          }
        }}
      >
        <span className="flex size-4 items-center justify-center shrink-0">
          {isFolder && hasChildren && (
            expanded ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />
          )}
        </span>
        <span className="shrink-0">
          {isFolder ? <Folder className="size-4 text-info" /> : <FileText className="size-4 text-muted-foreground" />}
        </span>
        <span className="truncate flex-1">{node.name}</span>
        {!isFolder && node.indexStatus && (
          <StatusBadge status={node.indexStatus} className="shrink-0 opacity-70 group-hover:opacity-100" />
        )}
        {!isFolder && (
          <Tooltip>
            <TooltipTrigger asChild>
              <Button
                variant="ghost"
                size="icon"
                className="size-5 text-muted-foreground hover:text-destructive opacity-0 group-hover:opacity-100 transition-opacity"
                onClick={(e) => { e.stopPropagation(); onDelete(node) }}
                aria-label={`Delete ${node.name}`}
              >
                <Trash2 className="size-3" />
              </Button>
            </TooltipTrigger>
            <TooltipContent>Delete page</TooltipContent>
          </Tooltip>
        )}
        <Tooltip>
          <TooltipTrigger asChild>
            <Button variant="ghost" size="icon" className="size-5 opacity-0 group-hover:opacity-100 transition-opacity" onClick={(e) => { e.stopPropagation() }}>
              <GripVertical className="size-3" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>Drag to reorder</TooltipContent>
        </Tooltip>
      </div>
      {isFolder && expanded && hasChildren && (
        <div role="group">
          {node.children!.map((child) => (
            <TreeNodeItem key={child.id} node={child} depth={depth + 1} onSelect={onSelect} selectedId={selectedId} onDelete={onDelete} />
          ))}
        </div>
      )}
    </div>
  )
}

export function DirectoryTree() {
  const { tree, selectedNodeId, selectNode, currentWorkspace, createDocument, deleteDocument } = useMoraStore()
  const [search, setSearch] = useState("")
  const [deleteTarget, setDeleteTarget] = useState<TreeNode | null>(null)

  const handleSelect = useCallback((nodeId: string) => {
    selectNode(nodeId)
  }, [selectNode])

  const handleCreate = useCallback(() => {
    createDocument("Untitled Document")
  }, [createDocument])

  const handleDelete = useCallback(async (node: TreeNode) => {
    try {
      await deleteDocument(node.id)
      setDeleteTarget(null)
      toast.success("Page deleted", { description: `"${node.name}" was removed.` })
    } catch (e) {
      toast.error("Couldn't delete page", { description: (e as Error).message })
    }
  }, [deleteDocument])

  const filteredTree = filterTree(tree, search)

  return (
    <div className="flex flex-col h-full">
      <div className="flex items-center gap-2 p-3 border-b">
        <Input
          placeholder="Filter pages..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="h-7 text-xs"
          aria-label="Filter directory tree"
        />
        <Tooltip>
          <TooltipTrigger asChild>
            <Button variant="ghost" size="icon" className="size-7 shrink-0" onClick={handleCreate} aria-label="Create new page">
              <Plus className="size-3.5" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>New page</TooltipContent>
        </Tooltip>
      </div>
      <ScrollArea className="flex-1">
        <div className="p-2" role="tree" aria-label={`${currentWorkspace?.name || "Workspace"} directory`}>
          {filteredTree.length === 0 ? (
            <EmptyState
              compact
              className="py-10"
              icon={search ? <SearchX className="size-8" /> : <FileText className="size-8" />}
              title={search ? "No matching pages" : "No pages yet"}
              description={search ? "Try a different filter." : "Create your first page to start building the knowledge base."}
              action={search ? undefined : { label: "New page", icon: <FilePlus className="size-3.5" />, onClick: handleCreate }}
            />
          ) : (
            filteredTree.map((node) => (
              <TreeNodeItem key={node.id} node={node} depth={0} onSelect={handleSelect} selectedId={selectedNodeId} onDelete={setDeleteTarget} />
            ))
          )}
        </div>
      </ScrollArea>

      <AlertDialog open={!!deleteTarget} onOpenChange={(open) => !open && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Delete page?</AlertDialogTitle>
            <AlertDialogDescription>
              {deleteTarget ? `"${deleteTarget.name}" will be permanently deleted. This can't be undone.` : ""}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-destructive text-white hover:bg-destructive/90"
              onClick={(e) => { e.preventDefault(); if (deleteTarget) handleDelete(deleteTarget) }}
            >
              Delete
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}
