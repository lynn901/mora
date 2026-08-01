export interface User {
  id: string
  name: string
  email: string
  avatar?: string
  role: "admin" | "editor" | "viewer"
}

/** Document indexing state, mirrored from the backend `index_status` enum. */
export type IndexStatusValue = "pending" | "processing" | "indexed" | "failed"

export interface Workspace {
  id: string
  name: string
  description?: string
  icon?: string
  createdAt: string
  updatedAt: string
}

export interface TreeNode {
  id: string
  workspaceId: string
  parentId: string | null
  name: string
  type: "folder" | "document"
  order: number
  children?: TreeNode[]
  /** Index status for document nodes; undefined for folders. */
  indexStatus?: IndexStatusValue
  createdAt: string
  updatedAt: string
}

export interface Document {
  id: string
  workspaceId: string
  nodeId: string
  title: string
  content: string
  contentFormat: "markdown" | "json"
  createdBy: string
  updatedBy: string
  createdAt: string
  updatedAt: string
  tags: string[]
  status: "draft" | "published" | "archived"
  indexStatus: IndexStatusValue
  versionNo: number
}

export interface DocumentVersion {
  id: string
  documentId: string
  version: number
  content: string
  contentFormat: "markdown" | "json"
  createdBy: string
  createdAt: string
  changeSummary?: string
}

export interface Permission {
  id: string
  workspaceId?: string
  nodeId?: string
  documentId?: string
  userId?: string
  groupId?: string
  role: "read" | "write" | "admin"
  scope: "workspace" | "directory" | "page"
  inherited: boolean
}

export interface SearchResult {
  id: string
  documentId: string
  title: string
  snippet: string
  highlights: string[]
  score: number
  workspaceId: string
  nodeId: string
  tags: string[]
  createdBy: string
  createdAt: string
  updatedAt: string
}

export interface CollaboratorPresence {
  userId: string
  userName: string
  userAvatar?: string
  color: string
  cursorPosition?: { line: number; ch: number }
  selectionRange?: { from: number; to: number }
  lastSeen: string
}

export interface Comment {
  id: string
  documentId: string
  blockId?: string
  content: string
  createdBy: string
  createdAt: string
  updatedAt: string
  resolved: boolean
  resolvedBy?: string
  resolvedAt?: string
  mentions: string[]
  replies?: Comment[]
}

export interface SearchFilters {
  query: string
  workspaceId?: string
  nodeId?: string
  tags?: string[]
  createdBy?: string
  dateRange?: { from: string; to: string }
  sortBy?: "relevance" | "date" | "title"
}
