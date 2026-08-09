import { useState, useCallback, useEffect, useRef } from "react"
import { Search as SearchIcon, X, Filter, Tag, User } from "lucide-react"
import { cn } from "@/lib/utils"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover"
import type { SearchResult, SearchFilters } from "@/types"
import { apiSearch, apiGetWorkspaces, apiGetUsers } from "@/api"
import type { Workspace, User as UserType } from "@/types"

function highlightText(text: string, query: string) {
  if (!query) return text
  const escapedQuery = query.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")
  const parts = text.split(new RegExp(`(${escapedQuery})`, "gi"))
  return parts.map((part, i) =>
    part.toLowerCase() === query.toLowerCase() ? (
      <mark
        key={i}
        className="rounded-sm bg-yellow-200 px-0.5 dark:bg-yellow-800"
      >
        {part}
      </mark>
    ) : (
      part
    )
  )
}

export function SearchPanel({
  onNavigate,
}: {
  onNavigate?: (nodeId: string) => void
}) {
  const [query, setQuery] = useState("")
  const [results, setResults] = useState<SearchResult[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [showFilters, setShowFilters] = useState(false)
  const [filters, setFilters] = useState<SearchFilters>({
    query: "",
    sortBy: "relevance",
  })
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [users, setUsers] = useState<UserType[]>([])
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    apiGetWorkspaces().then(setWorkspaces)
    apiGetUsers().then(setUsers)
  }, [])

  const doSearch = useCallback(async (q: string, f: SearchFilters) => {
    if (!q.trim()) {
      setResults([])
      return
    }
    setIsLoading(true)
    try {
      const r = await apiSearch({ ...f, query: q })
      setResults(r)
    } finally {
      setIsLoading(false)
    }
  }, [])

  useEffect(() => {
    if (debounceRef.current) clearTimeout(debounceRef.current)
    debounceRef.current = setTimeout(() => {
      doSearch(query, filters)
    }, 300)
    return () => {
      if (debounceRef.current) clearTimeout(debounceRef.current)
    }
  }, [doSearch, filters, query])

  return (
    <div className="flex h-full flex-col">
      <div className="border-b p-3">
        <div className="relative">
          <SearchIcon className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search knowledge base..."
            className="h-9 pl-8"
            aria-label="Search"
          />
          {query && (
            <Button
              variant="ghost"
              size="icon"
              className="absolute top-1/2 right-1 size-7 -translate-y-1/2"
              onClick={() => setQuery("")}
            >
              <X className="size-3.5" />
            </Button>
          )}
        </div>
        <div className="mt-2 flex items-center gap-2">
          <Button
            variant="outline"
            size="sm"
            className="h-7 text-xs"
            onClick={() => setShowFilters(!showFilters)}
          >
            <Filter className="size-3" /> Filters
          </Button>
          <Select
            value={filters.sortBy}
            onValueChange={(v) =>
              setFilters((current) => ({
                ...current,
                sortBy: v as SearchFilters["sortBy"],
              }))
            }
          >
            <SelectTrigger className="h-7 w-28 text-xs">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="relevance">Relevance</SelectItem>
              <SelectItem value="date">Date</SelectItem>
              <SelectItem value="title">Title</SelectItem>
            </SelectContent>
          </Select>
        </div>

        {showFilters && (
          <div className="mt-2 flex flex-wrap gap-2">
            <Select
              value={filters.workspaceId || ""}
              onValueChange={(v) =>
                setFilters((current) => ({
                  ...current,
                  workspaceId: v || undefined,
                }))
              }
            >
              <SelectTrigger className="h-7 w-36 text-xs">
                <SelectValue placeholder="Workspace" />
              </SelectTrigger>
              <SelectContent>
                {workspaces.map((ws) => (
                  <SelectItem key={ws.id} value={ws.id}>
                    {ws.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Popover>
              <PopoverTrigger asChild>
                <Button variant="outline" size="sm" className="h-7 text-xs">
                  <Tag className="mr-1 size-3" />
                  Tags
                </Button>
              </PopoverTrigger>
              <PopoverContent className="w-48">
                <div className="text-sm text-muted-foreground">
                  Tag filter coming soon
                </div>
              </PopoverContent>
            </Popover>
            <Popover>
              <PopoverTrigger asChild>
                <Button variant="outline" size="sm" className="h-7 text-xs">
                  <User className="mr-1 size-3" />
                  Author
                </Button>
              </PopoverTrigger>
              <PopoverContent className="w-48">
                {users.map((u) => (
                  <Button
                    key={u.id}
                    variant="ghost"
                    size="sm"
                    className="w-full justify-start text-xs"
                    onClick={() =>
                      setFilters((current) => ({ ...current, createdBy: u.id }))
                    }
                  >
                    {u.name}
                  </Button>
                ))}
              </PopoverContent>
            </Popover>
          </div>
        )}
      </div>

      <ScrollArea className="flex-1">
        <div className="p-2">
          {isLoading && (
            <div className="py-8 text-center text-sm text-muted-foreground">
              Searching...
            </div>
          )}
          {!isLoading && results.length === 0 && query && (
            <div className="py-8 text-center text-sm text-muted-foreground">
              No results found
            </div>
          )}
          {!isLoading &&
            results.map((r) => (
              <button
                key={r.id}
                className={cn(
                  "w-full cursor-pointer rounded-lg p-3 text-left transition-colors hover:bg-accent/50"
                )}
                onClick={() => onNavigate?.(r.nodeId)}
              >
                <div className="text-sm font-medium">
                  {highlightText(r.title, query)}
                </div>
                <div className="mt-1 line-clamp-2 text-xs text-muted-foreground">
                  {highlightText(r.snippet, query)}
                </div>
                <div className="mt-2 flex items-center gap-2">
                  {r.tags.map((t) => (
                    <Badge
                      key={t}
                      variant="secondary"
                      className="px-1.5 py-0 text-[10px]"
                    >
                      {t}
                    </Badge>
                  ))}
                </div>
              </button>
            ))}
        </div>
      </ScrollArea>
    </div>
  )
}
