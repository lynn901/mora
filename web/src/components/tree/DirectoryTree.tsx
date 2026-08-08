import { useCallback, useState } from "react"
import { ChevronRight, ChevronDown, Folder, FileText, Plus, GripVertical, Upload } from "lucide-react"
import { cn } from "@/lib/utils"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Tooltip, TooltipTrigger, TooltipContent } from "@/components/ui/tooltip"
import type { TreeNode } from "@/types"
import { useMoraStore } from "@/stores/mora"
import { useParseStore } from "@/stores/parse"

interface TreeNodeItemProps {
  node: TreeNode
  depth: number
  onSelect: (id: string) => void
  selectedId: string | null
}

function TreeNodeItem({ node, depth, onSelect, selectedId }: TreeNodeItemProps) {
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
          {isFolder ? <Folder className="size-4 text-muted-foreground" /> : <FileText className="size-4 text-muted-foreground" />}
        </span>
        <span className="truncate flex-1">{node.name}</span>
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
            <TreeNodeItem key={child.id} node={child} depth={depth + 1} onSelect={onSelect} selectedId={selectedId} />
          ))}
        </div>
      )}
    </div>
  )
}

export function DirectoryTree() {
  const { tree, selectedNodeId, selectNode, currentWorkspace, createDocument } = useMoraStore()
  const { setUploadOpen } = useParseStore()
  const [search, setSearch] = useState("")

  const handleSelect = useCallback((nodeId: string) => {
    selectNode(nodeId)
  }, [selectNode])

  const handleCreate = useCallback(() => {
    createDocument("Untitled Document")
  }, [createDocument])

  const filterTree = useCallback((nodes: TreeNode[], query: string): TreeNode[] => {
    if (!query) return nodes
    return nodes.reduce<TreeNode[]>((acc, node) => {
      const matches = node.name.toLowerCase().includes(query.toLowerCase())
      const filteredChildren = node.children ? filterTree(node.children, query) : []
      if (matches || filteredChildren.length > 0) {
        acc.push({ ...node, children: filteredChildren.length > 0 ? filteredChildren : node.children })
      }
      return acc
    }, [])
  }, [])

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
            <Button variant="ghost" size="icon" className="size-7 shrink-0" onClick={() => setUploadOpen(true)} aria-label="Upload and parse document">
              <Upload className="size-3.5" />
            </Button>
          </TooltipTrigger>
          <TooltipContent>上传并解析文档</TooltipContent>
        </Tooltip>
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
            <div className="text-center text-muted-foreground text-sm py-8">
              {search ? "No matching pages" : "No pages yet"}
            </div>
          ) : (
            filteredTree.map((node) => (
              <TreeNodeItem key={node.id} node={node} depth={0} onSelect={handleSelect} selectedId={selectedNodeId} />
            ))
          )}
        </div>
      </ScrollArea>
    </div>
  )
}
