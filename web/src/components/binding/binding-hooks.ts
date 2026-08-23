// Phase 5-4 — cursor-paginated binding list hook + agent list loader. Pure
// logic, no JSX — lives in a .ts so react-refresh/only-export-components does
// not apply. Mirrors the useCursorList pattern in asset-hooks.ts.
//
// §11.4 leak-safe: a 404 (no permission / not found) flips `notFound` rather
// than `error`, so callers render an empty state and existence is not leaked.
import { useCallback, useEffect, useRef, useState } from "react"
import {
  apiGetBindings,
  isNotFoundOrForbidden,
} from "@/api/binding"
import { ApiError } from "@/api/client"
import { errMsg } from "./binding-utils"
import type { AgentBinding } from "@/types/binding"

interface ListState<T> {
  items: T[]
  nextCursor: string | null
  isLoading: boolean
  isLoadingMore: boolean
  error: string | null
  notFound: boolean
  hasMore: boolean
}

const EMPTY: ListState<never> = {
  items: [],
  nextCursor: null,
  isLoading: false,
  isLoadingMore: false,
  error: null,
  notFound: false,
  hasMore: false,
}

interface CursorPageShape<T> {
  items: T[]
  next_cursor: string | null
  total: number | null
}

/**
 * Cursor-paginated list hook for the binding list. `fetchPage(cursor)` must
 * return a CursorPage. `deps` re-run the initial load when the page identity
 * changes (e.g. a new agent id). Mirrors useCursorList in asset-hooks.ts.
 */
export function useCursorBindings(
  fetchPage: (cursor: string | null) => Promise<CursorPageShape<AgentBinding>>,
  deps: ReadonlyArray<unknown>,
): ListState<AgentBinding> & {
  loadMore: () => Promise<void>
  reload: () => Promise<void>
} {
  const [state, setState] = useState<ListState<AgentBinding>>(
    EMPTY as ListState<AgentBinding>,
  )
  const fetchRef = useRef(fetchPage)
  fetchRef.current = fetchPage
  const cancelledRef = useRef(false)
  const nextCursorRef = useRef<string | null>(null)

  const reload = useCallback(async () => {
    cancelledRef.current = false
    setState((s) => ({ ...s, isLoading: true, error: null, notFound: false }))
    try {
      const page = await fetchRef.current(null)
      if (cancelledRef.current) return
      setState({
        items: page.items,
        nextCursor: page.next_cursor,
        isLoading: false,
        isLoadingMore: false,
        error: null,
        notFound: false,
        hasMore: !!page.next_cursor,
      })
      nextCursorRef.current = page.next_cursor
    } catch (e) {
      if (cancelledRef.current) return
      if (isNotFoundOrForbidden(e)) {
        setState({
          items: [],
          nextCursor: null,
          isLoading: false,
          isLoadingMore: false,
          error: null,
          notFound: true,
          hasMore: false,
        })
        nextCursorRef.current = null
        return
      }
      setState((s) => ({
        ...s,
        isLoading: false,
        isLoadingMore: false,
        error: errMsg(e),
        notFound: false,
      }))
    }
  }, [])

  const loadMore = useCallback(async () => {
    const cursor = nextCursorRef.current
    if (cursor === null || state.isLoading || state.isLoadingMore) return
    setState((s) => ({ ...s, isLoadingMore: true, error: null }))
    try {
      const page = await fetchRef.current(cursor)
      setState((s) => ({
        items: [...s.items, ...page.items],
        nextCursor: page.next_cursor,
        isLoading: false,
        isLoadingMore: false,
        error: null,
        notFound: false,
        hasMore: !!page.next_cursor,
      }))
      nextCursorRef.current = page.next_cursor
    } catch (e) {
      setState((s) => ({
        ...s,
        isLoadingMore: false,
        error: errMsg(e),
      }))
    }
  }, [state.isLoading, state.isLoadingMore])

  // Re-run initial load when identity deps change.
  useEffect(() => {
    void reload()
    return () => {
      cancelledRef.current = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps)

  return { ...state, loadMore, reload }
}

/** Bindings for one agent (§6.1 GET /agents/{id}/bindings). */
export function useBindings(agentId: string | null) {
  return useCursorBindings(
    async (cursor) => {
      if (!agentId) throw new ApiError("No agent", 0)
      return apiGetBindings(agentId, { cursor, page_size: 50 })
    },
    [agentId],
  )
}
