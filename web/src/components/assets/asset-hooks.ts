// Cursor-paginated list hooks for the Phase 1 asset UI. Pure logic, no JSX —
// lives in a .ts so react-refresh/only-export-components does not apply.
//
// Honors §11.4: a 404 (no permission / not found) flips `notFound` rather
// than `error`, so callers render an empty state and existence is not leaked.
import { useCallback, useEffect, useRef, useState } from "react"
import {
  apiGetAssets,
  apiGetAssetVersions,
  apiGetAssetRelations,
  apiGetReviews,
  isNotFoundOrForbidden,
} from "@/api/assets"
import { ApiError } from "@/api/client"
import { errMsg } from "./asset-utils"
import type {
  AssetStatus,
  AssetType,
  KnowledgeAsset,
  KnowledgeAssetVersion,
  KnowledgeRelation,
  ReviewRequest,
} from "@/types/assets"

interface ListState<T> {
  items: T[]
  nextCursor: string | null
  isLoading: boolean // initial load
  isLoadingMore: boolean // "load more" fetch
  error: string | null
  notFound: boolean // §11.4: render as empty, not error
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
 * Cursor-paginated list hook shared by the asset / version / relations /
 * review-inbox pages. `fetchPage(cursor)` must return a CursorPage. `deps`
 * re-runs the initial load when the page identity changes (e.g. a new
 * workspace or asset id).
 */
export function useCursorList<T>(
  fetchPage: (cursor: string | null) => Promise<CursorPageShape<T>>,
  deps: ReadonlyArray<unknown>,
): ListState<T> & {
  loadMore: () => Promise<void>
  reload: () => Promise<void>
} {
  const [state, setState] = useState<ListState<T>>(EMPTY as ListState<T>)
  const fetchRef = useRef(fetchPage)
  fetchRef.current = fetchPage
  const cancelledRef = useRef(false)
  // nextCursor lives in a ref too so loadMore can read it synchronously
  // without poking a value out of a setState updater (an anti-pattern).
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

// ---- Convenience fetchers bound to the api layer --------------------------

export function useAssetList(
  workspaceId: string | null,
  filters: { asset_type?: AssetType | null; status?: AssetStatus | null },
) {
  return useCursorList<KnowledgeAsset>(
    async (cursor) => {
      if (!workspaceId) throw new ApiError("No workspace", 0)
      return apiGetAssets(workspaceId, {
        cursor,
        page_size: 20,
        asset_type: filters.asset_type ?? undefined,
        status: filters.status ?? undefined,
      })
    },
    [workspaceId, filters.asset_type, filters.status],
  )
}

export function useAssetVersions(assetId: string | null) {
  return useCursorList<KnowledgeAssetVersion>(
    async (cursor) => {
      if (!assetId) throw new ApiError("No asset", 0)
      return apiGetAssetVersions(assetId, cursor, 50)
    },
    [assetId],
  )
}

export function useAssetRelations(assetId: string | null) {
  return useCursorList<KnowledgeRelation>(
    async (cursor) => {
      if (!assetId) throw new ApiError("No asset", 0)
      return apiGetAssetRelations(assetId, null, cursor, 50)
    },
    [assetId],
  )
}

export function useReviewInbox(workspaceId: string | null) {
  return useCursorList<ReviewRequest>(
    async (cursor) => {
      if (!workspaceId) throw new ApiError("No workspace", 0)
      return apiGetReviews(workspaceId, cursor, 20)
    },
    [workspaceId],
  )
}
