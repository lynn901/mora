import { useState, useCallback, useEffect, useRef } from "react"
import { Search as SearchIcon, X, Filter, Calendar, Tag, User } from "lucide-react"
import { cn } from "@/lib/utils"
import { Input } from "@/components/ui/input"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { ScrollArea } from "@/components/ui/scroll-area"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import type { SearchResult, SearchFilters } from "@/types"
import { apiSearch, apiGetWorkspaces, apiGetUsers } from "@/api/mock"
import type { Workspace, User as UserType } from "@/types"

function highlightText(text: string, query: string) {
  if (!query) return text
  const parts = text.split(new RegExp(`(${query})`, "gi"))
  return parts.map((part, i) =>
    part.toLowerCase() === query.toLowerCase()
      ? <mark key={i} className="bg-yellow-200 dark:bg-yellow-800 rounded-sm px-0.5">{part}</mark>
      : part
  )
}

export function SearchPanel({ onNavigate }: { onNavigate?: (nodeId: string) => void }) {
  const [query, setQuery] = useState("")
  const [results, setResults] = useState<SearchResult[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [showFilters, setShowFilters] = useState(false)
  const [filters, setFilters] = useState<SearchFilters>({ query: "", sortBy: "relevance" })
  const [workspaces, setWorkspaces] = useState<Workspace[]>([])
  const [users, setUsers] = useState<UserType[]>([])
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    apiGetWorkspaces().then(setWorkspaces)
    apiGetUsers().then(setUsers)
  }, [])

  const doSearch = useCallback(async (q: string, f: SearchFilters) => {
    if (!q.trim()) { setResults([]); return }
    setIsLoading(true)
    try {
      const r = await apiSearch({ ...f, query: q })
      setResults(r)
    } finally { setIsLoading(false) }
  }, [])

  useEffect(() => {
    if (debounceRef.current) clearTimeout(debounceRef.current)
    debounceRef.current = setTimeout(() => {
      setFilters((f) => ({ ...f, query }))
      doSearch(query, filters)
    }, 300)
    return () => { if (debounceRef.current) clearTimeout(debounceRef.current) }
  }, [query])

  return (
    <div className="flex flex-col h-full">
      <div className="p-3 border-b">
        <div className="relative">
          <SearchIcon className="absolute left-2.5 top-1/2 -translate-y-1/2 size-4 text-muted-foreground" />
          <Input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Search knowledge base..."
            className="pl-8 h-9"
            aria-label="Search"
          />
          {query && (
            <Button variant="ghost" size="icon" className="absolute right-1 top-1/2 -translate-y-1/2 size-7" onClick={() => setQuery("")}>
              <X className="size-3.5" />
            </Button>
          )}
        </div>
        <div className="flex items-center gap-2 mt-2">
          <Button variant="outline" size="sm" className="h-7 text-xs" onClick={() => setShowFilters(!showFilters)}>
            <Filter className="size-3" /> Filters
          </Button>
          <Select value={filters.sortBy} onValueChange={(v) => { setFilters((f) => ({ ...f, sortBy: v as SearchFilters["sortBy"] })); doSearch(query, { ...filters, sortBy: v as SearchFilters["sortBy"] }) }}>
            <SelectTrigger className="h-7 w-28 text-xs"><SelectValue /></SelectTrigger>
            <SelectContent>
              <SelectItem value="relevance">Relevance</SelectItem>
              <SelectItem value="date">Date</SelectItem>
              <SelectItem value="title">Title</SelectItem>
            </SelectContent>
          </Select>
        </div>

        {showFilters && (
          <div className="flex flex-wrap gap-2 mt-2">
            <Select value={filters.workspaceId || ""} onValueChange={(v) => { setFilters((f) => ({ ...f, workspaceId: v || undefined })); doSearch(query, { ...filters, workspaceId: v || undefined }) }}>
              <SelectTrigger className="h-7 w-36 text-xs"><SelectValue placeholder="Workspace" /></SelectTrigger>
              <SelectContent>
                {workspaces.map((ws) => <SelectItem key={ws.id} value={ws.id}>{ws.name}</SelectItem>)}
              </SelectContent>
            </Select>
            <Popover>
              <PopoverTrigger asChild>
                <Button variant="outline" size="sm" className="h-7 text-xs"><Tag className="size-3 mr-1" />Tags</Button>
              </PopoverTrigger>
              <PopoverContent className="w-48"><div className="text-sm text-muted-foreground">Tag filter coming soon</div></PopoverContent>
            </Popover>
            <Popover>
              <PopoverTrigger asChild>
                <Button variant="outline" size="sm" className="h-7 text-xs"><User className="size-3 mr-1" />Author</Button>
              </PopoverTrigger>
              <PopoverContent className="w-48">
                {users.map((u) => (
                  <Button key={u.id} variant="ghost" size="sm" className="w-full justify-start text-xs" onClick={() => { setFilters((f) => ({ ...f, createdBy: u.id })); doSearch(query, { ...filters, createdBy: u.id }) }}>
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
          {isLoading && <div className="text-center text-sm text-muted-foreground py-8">Searching...</div>}
          {!isLoading && results.length === 0 && query && (
            <div className="text-center text-sm text-muted-foreground py-8">No results found</div>
          )}
          {!isLoading && results.map((r) => (
            <button
              key={r.id}
              className={cn("w-full text-left p-3 rounded-lg hover:bg-accent/50 transition-colors cursor-pointer")}
              onClick={() => onNavigate?.(r.nodeId)}
            >
              <div className="font-medium text-sm">{highlightText(r.title, query)}</div>
              <div className="text-xs text-muted-foreground mt-1 line-clamp-2">
                {highlightText(r.snippet, query)}
              </div>
              <div className="flex items-center gap-2 mt-2">
                {r.tags.map((t) => <Badge key={t} variant="secondary" className="text-[10px] px-1.5 py-0">{t}</Badge>)}
              </div>
            </button>
          ))}
        </div>
      </ScrollArea>
    </div>
  )
}
