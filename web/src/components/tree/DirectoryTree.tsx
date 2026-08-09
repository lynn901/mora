import { useCallback, useMemo, useState } from "react"
import {
  ChevronRight,
  ChevronDown,
  Folder,
  FileText,
  Plus,
  GripVertical,
} from "lucide-react"
import { cn } from "@/lib/utils"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import {
  Tooltip,
  TooltipTrigger,
  TooltipContent,
} from "@/components/ui/tooltip"
import type { TreeNode } from "@/types"
import { useMoraStore } from "@/stores/mora"

interface TreeNodeItemProps {
  node: TreeNode
  depth: number
  onSelect: (id: string) => void
  selectedId: string | null
}

function filterTree(nodes: TreeNode[], query: string): TreeNode[] {
  if (!query) return nodes
  const normalizedQuery = query.toLowerCase()
  return nodes.reduce<TreeNode[]>((result, node) => {
    const filteredChildren = node.children
      ? filterTree(node.children, query)
      : []
    if (
      node.name.toLowerCase().includes(normalizedQuery) ||
      filteredChildren.length > 0
    ) {
      result.push({
        ...node,
        children:
          filteredChildren.length > 0 ? filteredChildren : node.children,
      })
    }
    return result
  }, [])
}

function TreeNodeItem({
  node,
  depth,
  onSelect,
  selectedId,
}: TreeNodeItemProps) {
  const [expanded, setExpanded] = useState(true)
  const isFolder = node.type === "folder"
  const isSelected = selectedId === node.id
  const hasChildren = node.children && node.children.length > 0

  return (
    <div>
      <div
        className={cn(
          "group flex cursor-pointer items-center gap-1 rounded-sm px-2 py-1.5 text-sm transition-colors hover:bg-accent/50",
          isSelected && "bg-accent font-medium text-accent-foreground"
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
        <span className="flex size-4 shrink-0 items-center justify-center">
          {isFolder &&
            hasChildren &&
            (expanded ? (
              <ChevronDown className="size-3.5" />
            ) : (
              <ChevronRight className="size-3.5" />
            ))}
        </span>
        <span className="shrink-0">
          {isFolder ? (
            <Folder className="size-4 text-muted-foreground" />
          ) : (
            <FileText className="size-4 text-muted-foreground" />
          )}
        </span>
        <span className="flex-1 truncate">{node.name}</span>
        <Tooltip>
          <TooltipTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className="size-5 opacity-0 transition-opacity group-hover:opacity-100"
              onClick={(e) => {
                e.stopPropagation()
              }}
            >
              <GripVertical className="size-3" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>Drag to reorder</TooltipContent>
        </Tooltip>
      </div>
      {isFolder && expanded && hasChildren && (
        <div role="group">
          {node.children!.map((child) => (
            <TreeNodeItem
              key={child.id}
              node={child}
              depth={depth + 1}
              onSelect={onSelect}
              selectedId={selectedId}
            />
          ))}
        </div>
      )}
    </div>
  )
}

export function DirectoryTree() {
  const { tree, selectedNodeId, selectNode, currentWorkspace, createDocument } =
    useMoraStore()
  const [search, setSearch] = useState("")

  const handleSelect = useCallback(
    (nodeId: string) => {
      selectNode(nodeId)
    },
    [selectNode]
  )

  const handleCreate = useCallback(() => {
    createDocument("Untitled Document")
  }, [createDocument])

  const filteredTree = useMemo(() => filterTree(tree, search), [search, tree])

  return (
    <div className="flex h-full flex-col">
      <div className="flex items-center gap-2 border-b p-3">
        <Input
          placeholder="Filter pages..."
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          className="h-7 text-xs"
          aria-label="Filter directory tree"
        />
        <Tooltip>
          <TooltipTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              className="size-7 shrink-0"
              onClick={handleCreate}
              aria-label="Create new page"
            >
              <Plus className="size-3.5" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>New page</TooltipContent>
        </Tooltip>
      </div>
      <ScrollArea className="flex-1">
        <div
          className="p-2"
          role="tree"
          aria-label={`${currentWorkspace?.name || "Workspace"} directory`}
        >
          {filteredTree.length === 0 ? (
            <div className="py-8 text-center text-sm text-muted-foreground">
              {search ? "No matching pages" : "No pages yet"}
            </div>
          ) : (
            filteredTree.map((node) => (
              <TreeNodeItem
                key={node.id}
                node={node}
                depth={0}
                onSelect={handleSelect}
                selectedId={selectedNodeId}
              />
            ))
          )}
        </div>
      </ScrollArea>
    </div>
  )
}
