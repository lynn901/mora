import { create } from "zustand"
import type { CollaboratorPresence, Comment } from "@/types"
import { apiGetPresences, apiGetComments, apiAddComment, apiResolveComment } from "@/api/mock"

interface CollabState {
  presences: CollaboratorPresence[]
  comments: Comment[]
  showComments: boolean
  loadPresences: (documentId: string) => Promise<void>
  loadComments: (documentId: string) => Promise<void>
  addComment: (comment: Omit<Comment, "id" | "createdAt" | "updatedAt">) => Promise<void>
  resolveComment: (commentId: string) => Promise<void>
  toggleComments: () => void
}

export const useCollabStore = create<CollabState>((set, get) => ({
  presences: [],
  comments: [],
  showComments: false,

  loadPresences: async (documentId) => {
    const presences = await apiGetPresences(documentId)
    set({ presences })
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
