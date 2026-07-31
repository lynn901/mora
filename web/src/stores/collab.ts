import { create } from "zustand"
import type { Comment, CollaboratorPresence } from "@/types"
import { apiGetComments, apiAddComment, apiResolveComment } from "@/api"
import { WikiCollabProvider, type CollabProviderStatus } from "@/lib/collab-provider"

const USER_COLORS = [
  "#3b82f6", "#ef4444", "#10b981", "#f59e0b", "#8b5cf6",
  "#ec4899", "#06b6d4", "#84cc16",
]

function pickColor(userId: string): string {
  let hash = 0
  for (let i = 0; i < userId.length; i++) {
    hash = (hash << 5) - hash + userId.charCodeAt(i)
    hash |= 0
  }
  return USER_COLORS[Math.abs(hash) % USER_COLORS.length]
}

interface CollabState {
  provider: WikiCollabProvider | null
  status: CollabProviderStatus
  localMode: boolean
  presences: CollaboratorPresence[]
  comments: Comment[]
  showComments: boolean
  isReadOnly: boolean

  initCollab: (documentId: string, userId: string, userName: string) => void
  destroyCollab: () => void
  loadComments: (documentId: string) => Promise<void>
  addComment: (comment: Omit<Comment, "id" | "createdAt" | "updatedAt">) => Promise<void>
  resolveComment: (commentId: string) => Promise<void>
  toggleComments: () => void
}

export const useCollabStore = create<CollabState>((set, get) => ({
  provider: null,
  status: "disconnected",
  localMode: false,
  presences: [],
  comments: [],
  showComments: false,
  isReadOnly: false,

  initCollab: (documentId, userId, userName) => {
    const existing = get().provider
    if (existing) {
      existing.destroy()
    }

    const wsProtocol = window.location.protocol === "https:" ? "wss:" : "ws:"
    const serverUrl = `${wsProtocol}//${window.location.host}/api/v1/ws/collab/${documentId}`
    const token = localStorage.getItem("wiki_token") || "dev-token"

    const provider = new WikiCollabProvider({
      serverUrl,
      documentId,
      token,
      userId,
      userName,
      userColor: pickColor(userId),
    })

    provider.on("status", (newStatus: CollabProviderStatus) => {
      set({
        status: newStatus,
        localMode: newStatus === "local-only",
        isReadOnly: newStatus === "degraded",
      })
    })

    provider.awareness.on("change", () => {
      const presences: CollaboratorPresence[] = []
      provider.awareness.getStates().forEach((state, clientId) => {
        if (clientId === provider.doc.clientID) return
        if (!state.user) return
        presences.push({
          userId: state.user.id || String(clientId),
          userName: state.user.name || "Unknown",
          userAvatar: state.user.avatar,
          color: state.user.color || "#999",
          lastSeen: new Date().toISOString(),
        })
      })
      set({ presences })
    })

    provider.connect()
    set({ provider, status: "connecting", localMode: false, presences: [], isReadOnly: false })
  },

  destroyCollab: () => {
    const { provider } = get()
    if (provider) {
      provider.destroy()
      set({ provider: null, status: "disconnected", localMode: false, presences: [], isReadOnly: false })
    }
  },

  loadComments: async (documentId) => {
    const comments = await apiGetComments(documentId)
    set({ comments })
  },

  addComment: async (comment) => {
    const newComment = await apiAddComment(comment)
    set({ comments: [...get().comments, newComment] })
  },

  resolveComment: async (commentId) => {
    await apiResolveComment(commentId)
    set({
      comments: get().comments.map((c) =>
        c.id === commentId ? { ...c, resolved: true, resolvedAt: new Date().toISOString() } : c
      ),
    })
  },

  toggleComments: () => set({ showComments: !get().showComments }),
}))
