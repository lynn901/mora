import { create } from "zustand"
import type { Workspace, TreeNode, Document, User } from "@/types"
import { apiGetWorkspaces, apiGetTree, apiGetDocument, apiSaveDocument, apiCreateDocument } from "@/api"
import { ApiError } from "@/api/client"

interface WikiState {
  currentWorkspace: Workspace | null
  workspaces: Workspace[]
  tree: TreeNode[]
  selectedNodeId: string | null
  currentDocument: Document | null
  editorMode: "wysiwyg" | "markdown"
  isDirty: boolean
  isLoading: boolean
  error: string | null
  users: User[]

  loadWorkspaces: () => Promise<void>
  setWorkspace: (ws: Workspace) => Promise<void>
  selectNode: (nodeId: string) => Promise<void>
  setEditorMode: (mode: "wysiwyg" | "markdown") => void
  updateDocument: (doc: Partial<Document>) => void
  saveDocument: () => Promise<void>
  createDocument: (title: string, directoryId?: string | null) => Promise<void>
  setError: (error: string | null) => void
  setUsers: (users: User[]) => void
}

export const useWikiStore = create<WikiState>((set, get) => ({
  currentWorkspace: null,
  workspaces: [],
  tree: [],
  selectedNodeId: null,
  currentDocument: null,
  editorMode: "wysiwyg",
  isDirty: false,
  isLoading: false,
  error: null,
  users: [],

  loadWorkspaces: async () => {
    set({ isLoading: true, error: null })
    try {
      const workspaces = await apiGetWorkspaces()
      set({ workspaces, isLoading: false })
      if (workspaces.length > 0 && !get().currentWorkspace) {
        await get().setWorkspace(workspaces[0])
      }
    } catch (e) {
      set({ error: (e as Error).message, isLoading: false })
    }
  },

  setWorkspace: async (ws) => {
    set({ currentWorkspace: ws, isLoading: true, error: null, selectedNodeId: null, currentDocument: null })
    try {
      const tree = await apiGetTree(ws.id)
      set({ tree, isLoading: false })
    } catch (e) {
      set({ error: (e as Error).message, isLoading: false })
    }
  },

  selectNode: async (nodeId) => {
    set({ selectedNodeId: nodeId, isLoading: true, error: null, isDirty: false })
    try {
      const doc = await apiGetDocument(nodeId)
      set({ currentDocument: doc, isLoading: false })
    } catch (e) {
      set({ error: (e as Error).message, isLoading: false })
    }
  },

  setEditorMode: (mode) => set({ editorMode: mode }),

  updateDocument: (partial) => {
    const { currentDocument } = get()
    if (!currentDocument) return
    set({ currentDocument: { ...currentDocument, ...partial }, isDirty: true })
  },

  saveDocument: async () => {
    const { currentDocument } = get()
    if (!currentDocument) return
    set({ isLoading: true, error: null })
    try {
      const saved = await apiSaveDocument(currentDocument)
      set({ currentDocument: saved, isDirty: false, isLoading: false })
    } catch (e) {
      if (e instanceof ApiError && e.status === 409) {
        set({ error: "Document has been modified by another user. Please refresh to load the latest version.", isLoading: false })
        const doc = await apiGetDocument(currentDocument.id)
        set({ currentDocument: doc })
      } else {
        set({ error: (e as Error).message, isLoading: false })
      }
    }
  },

  createDocument: async (title, directoryId) => {
    const { currentWorkspace } = get()
    if (!currentWorkspace) return
    set({ isLoading: true, error: null })
    try {
      const doc = await apiCreateDocument(currentWorkspace.id, title, directoryId || null, "")
      const tree = await apiGetTree(currentWorkspace.id)
      set({ tree, currentDocument: doc, selectedNodeId: doc.id, isLoading: false, isDirty: false })
    } catch (e) {
      set({ error: (e as Error).message, isLoading: false })
    }
  },

  setError: (error) => set({ error }),
  setUsers: (users) => set({ users }),
}))
