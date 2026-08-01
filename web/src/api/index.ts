import { http, login as httpLogin, setToken, clearToken, getToken } from "./client"
import { blocksToMarkdown } from "@/lib/blocksToMarkdown"
import type {
  User,
  Workspace,
  TreeNode,
  Document,
  DocumentVersion,
  Permission,
  SearchResult,
  CollaboratorPresence,
  Comment,
  SearchFilters,
  IndexStatusValue,
} from "@/types"

interface Paginated<T> {
  items: T[]
  total: number
  page: number
  page_size: number
}

interface BackendWorkspace {
  id: string
  name: string
  slug: string
  description: string
  owner_id: string
  settings: Record<string, unknown>
  created_at: string
  updated_at: string
}

interface BackendDirectory {
  id: string
  workspace_id: string
  parent_id: string | null
  name: string
  path: string
  sort_order: number
  children: BackendDirectory[]
  created_at: string
  updated_at: string
}

interface BackendBlock {
  type: string
  attrs?: Record<string, unknown>
  content?: BackendBlock[]
  text?: string
  marks?: { type: string; attrs?: Record<string, unknown> }[]
}

interface BackendDocument {
  id: string
  workspace_id: string
  directory_id: string | null
  title: string
  content: BackendBlock[]
  format: string
  status: string
  index_status: string
  version_no: number
  tags: string[]
  created_by: string
  updated_by: string | null
  created_at: string
  updated_at: string
}

interface BackendVersion {
  id: string
  document_id: string
  version_no: number
  content: BackendBlock[]
  diff_summary: string
  author_id: string
  created_at: string
}

interface BackendComment {
  id: string
  document_id: string
  block_id: string | null
  parent_id: string | null
  author_id: string
  content: string
  mentions: string[]
  resolved: boolean
  resolved_by: string | null
  resolved_at: string | null
  created_at: string
  updated_at: string
}

interface BackendPermission {
  id: string
  subject_type: string
  subject_id: string
  role_id: string
  target_type: string
  target_id: string
  effect: string
  inherit_scope: string
  created_at: string
  created_by: string | null
}

interface BackendSearchResult {
  document_id: string
  title: string
  snippet: string
  highlight: string[]
  score: number
  workspace_id: string
  directory_id: string | null
  updated_at: string
}

function mapWorkspace(ws: BackendWorkspace): Workspace {
  return {
    id: ws.id,
    name: ws.name,
    description: ws.description,
    createdAt: ws.created_at,
    updatedAt: ws.updated_at || ws.created_at,
  }
}

function mapDirectoryToTreeNode(dir: BackendDirectory): TreeNode {
  return {
    id: dir.id,
    workspaceId: dir.workspace_id,
    parentId: dir.parent_id,
    name: dir.name,
    type: "folder",
    order: dir.sort_order,
    children: dir.children?.map(mapDirectoryToTreeNode) || [],
    createdAt: dir.created_at,
    updatedAt: dir.updated_at || dir.created_at,
  }
}

function mapDocumentToTreeNode(doc: BackendDocument): TreeNode {
  return {
    id: doc.id,
    workspaceId: doc.workspace_id,
    parentId: doc.directory_id,
    name: doc.title,
    type: "document",
    order: 0,
    indexStatus: toIndexStatus(doc.index_status),
    createdAt: doc.created_at,
    updatedAt: doc.updated_at || doc.created_at,
  }
}

const INDEX_STATUS_VALUES: IndexStatusValue[] = ["pending", "processing", "indexed", "failed"]

function toIndexStatus(value: string | undefined | null): IndexStatusValue {
  if (value && INDEX_STATUS_VALUES.includes(value as IndexStatusValue)) {
    return value as IndexStatusValue
  }
  return "pending"
}

function mapDocument(doc: BackendDocument): Document {
  let content: string
  if (doc.format === "markdown" || doc.format === "blocks") {
    content = blocksToMarkdown(doc.content || [])
  } else {
    content = blocksToMarkdown(doc.content || [])
  }

  return {
    id: doc.id,
    workspaceId: doc.workspace_id,
    nodeId: doc.directory_id || doc.id,
    title: doc.title,
    content,
    contentFormat: doc.format === "markdown" ? "markdown" : "markdown",
    createdBy: doc.created_by,
    updatedBy: doc.updated_by || doc.created_by,
    createdAt: doc.created_at,
    updatedAt: doc.updated_at || doc.created_at,
    tags: doc.tags || [],
    status: (doc.status as Document["status"]) || "draft",
    indexStatus: toIndexStatus(doc.index_status),
    versionNo: doc.version_no,
  }
}

function mapVersion(v: BackendVersion): DocumentVersion {
  return {
    id: v.id,
    documentId: v.document_id,
    version: v.version_no,
    content: blocksToMarkdown(v.content || []),
    contentFormat: "markdown",
    createdBy: v.author_id,
    createdAt: v.created_at,
    changeSummary: v.diff_summary,
  }
}

function mapComment(c: BackendComment): Comment {
  return {
    id: c.id,
    documentId: c.document_id,
    blockId: c.block_id || undefined,
    content: c.content,
    createdBy: c.author_id,
    createdAt: c.created_at,
    updatedAt: c.updated_at || c.created_at,
    resolved: c.resolved,
    resolvedBy: c.resolved_by || undefined,
    resolvedAt: c.resolved_at || undefined,
    mentions: c.mentions || [],
  }
}

function mapPermission(p: BackendPermission): Permission {
  const roleMap: Record<string, "read" | "write" | "admin"> = {
    viewer: "read",
    editor: "write",
    workspace_admin: "admin",
    super_admin: "admin",
  }

  let role: "read" | "write" | "admin" = "read"
  for (const [name, r] of Object.entries(roleMap)) {
    if (p.role_id.includes(name) || p.role_id === name) {
      role = r
      break
    }
  }

  const scopeMap: Record<string, "workspace" | "directory" | "page"> = {
    workspace: "workspace",
    directory: "directory",
    document: "page",
  }

  const perm: Permission = {
    id: p.id,
    role,
    scope: scopeMap[p.target_type] || "workspace",
    inherited: p.inherit_scope === "subtree",
  }

  if (p.subject_type === "user") perm.userId = p.subject_id
  if (p.subject_type === "group") perm.groupId = p.subject_id
  if (p.target_type === "workspace") perm.workspaceId = p.target_id
  if (p.target_type === "directory") perm.nodeId = p.target_id
  if (p.target_type === "document") perm.documentId = p.target_id

  return perm
}

function mapSearchResult(sr: BackendSearchResult): SearchResult {
  return {
    id: `sr-${sr.document_id}`,
    documentId: sr.document_id,
    title: sr.title,
    snippet: sr.snippet,
    highlights: sr.highlight || [],
    score: sr.score,
    workspaceId: sr.workspace_id,
    nodeId: sr.directory_id || sr.document_id,
    tags: [],
    createdBy: "",
    createdAt: "",
    updatedAt: sr.updated_at,
  }
}

function buildTree(directories: BackendDirectory[], documents: BackendDocument[]): TreeNode[] {
  const dirNodes = directories.map(mapDirectoryToTreeNode)
  const docNodes = documents.map(mapDocumentToTreeNode)

  const nodeMap = new Map<string, TreeNode>()
  for (const dn of dirNodes) {
    nodeMap.set(dn.id, dn)
  }

  const roots: TreeNode[] = []

  for (const docNode of docNodes) {
    const parentId = docNode.parentId
    if (parentId && nodeMap.has(parentId)) {
      const parent = nodeMap.get(parentId)!
      if (!parent.children) parent.children = []
      parent.children.push(docNode)
    } else {
      roots.push(docNode)
    }
  }

  for (const dirNode of dirNodes) {
    if (!dirNode.parentId) {
      roots.push(dirNode)
    } else if (nodeMap.has(dirNode.parentId)) {
      const parent = nodeMap.get(dirNode.parentId)!
      if (!parent.children) parent.children = []
      parent.children.push(dirNode)
    } else {
      roots.push(dirNode)
    }
  }

  const sortNodes = (nodes: TreeNode[]): TreeNode[] => {
    return nodes.sort((a, b) => a.order - b.order).map((n) => {
      if (n.children) n.children = sortNodes(n.children)
      return n
    })
  }

  return sortNodes(roots)
}

export async function apiGetWorkspaces(): Promise<Workspace[]> {
  const data = await http.get<{ items: BackendWorkspace[] }>("/workspaces")
  return data.items.map(mapWorkspace)
}

export async function apiGetTree(workspaceId: string): Promise<TreeNode[]> {
  const [dirData, docData] = await Promise.all([
    http.get<{ items: BackendDirectory[] }>(`/workspaces/${workspaceId}/directories`),
    http.get<Paginated<BackendDocument>>(`/workspaces/${workspaceId}/documents?page_size=100`),
  ])
  return buildTree(dirData.items || [], docData.items || [])
}

export async function apiGetDocument(documentId: string): Promise<Document> {
  const doc = await http.get<BackendDocument>(`/documents/${documentId}`)
  return mapDocument(doc)
}

export async function apiSaveDocument(doc: Document): Promise<Document> {
  const updated = await http.patch<BackendDocument>(`/documents/${doc.id}`, {
    markdown: doc.content,
    title: doc.title,
    status: doc.status,
    tags: doc.tags,
  }, {
    "If-Match": String(doc.versionNo),
  })
  return mapDocument(updated)
}

export async function apiCreateDocument(
  workspaceId: string,
  title: string,
  directoryId: string | null,
  content: string,
): Promise<Document> {
  const doc = await http.post<BackendDocument>(`/workspaces/${workspaceId}/documents`, {
    title,
    directory_id: directoryId,
    markdown: content,
    format: "markdown",
  })
  return mapDocument(doc)
}

export async function apiDeleteDocument(documentId: string): Promise<void> {
  await http.delete(`/documents/${documentId}`)
}

export interface DocumentIndexStatus {
  indexStatus: IndexStatusValue
  lastIndexedAt: string | null
  chunkCount: number
  error: string | null
}

/** Fetch a document's indexing status (GET /documents/:id/index-status). */
export async function apiGetIndexStatus(documentId: string): Promise<DocumentIndexStatus> {
  const data = await http.get<{
    index_status: string
    last_indexed_at: string | null
    chunk_count: number
    error: string | null
  }>(`/documents/${documentId}/index-status`)
  return {
    indexStatus: toIndexStatus(data.index_status),
    lastIndexedAt: data.last_indexed_at,
    chunkCount: data.chunk_count ?? 0,
    error: data.error,
  }
}

export async function apiGetVersions(documentId: string): Promise<DocumentVersion[]> {
  const data = await http.get<{ items: BackendVersion[] }>(`/documents/${documentId}/versions`)
  return (data.items || []).map(mapVersion)
}

export async function apiRollbackVersion(documentId: string, versionId: string): Promise<Document> {
  const versions = await apiGetVersions(documentId)
  const version = versions.find((v) => v.id === versionId)
  if (!version) throw new Error("Version not found")
  const updated = await http.post<BackendDocument>(
    `/documents/${documentId}/versions/${version.version}/rollback`,
  )
  return mapDocument(updated)
}

export async function apiGetPermissions(workspaceId: string): Promise<Permission[]> {
  const data = await http.get<{ items: BackendPermission[] }>(
    `/permissions?target_type=workspace&target_id=${workspaceId}`,
  )
  return (data.items || []).map(mapPermission)
}

export async function apiSetPermission(perm: Omit<Permission, "id">): Promise<Permission> {
  let targetType: string
  let targetId: string

  if (perm.workspaceId) {
    targetType = "workspace"
    targetId = perm.workspaceId
  } else if (perm.nodeId) {
    targetType = "directory"
    targetId = perm.nodeId
  } else if (perm.documentId) {
    targetType = "document"
    targetId = perm.documentId
  } else {
    targetType = "workspace"
    targetId = ""
  }

  const roleToId: Record<string, string> = {
    read: "viewer",
    write: "editor",
    admin: "workspace_admin",
  }

  const result = await http.post<BackendPermission>("/permissions", {
    subject_type: perm.userId ? "user" : "group",
    subject_id: perm.userId || perm.groupId || "",
    role_id: roleToId[perm.role] || perm.role,
    target_type: targetType,
    target_id: targetId,
    effect: "allow",
    inherit_scope: perm.inherited ? "subtree" : "node_only",
  })
  return mapPermission(result)
}

export async function apiDeletePermission(id: string): Promise<void> {
  await http.delete(`/permissions/${id}`)
}

export async function apiSearch(filters: SearchFilters): Promise<SearchResult[]> {
  const params = new URLSearchParams()
  params.set("q", filters.query)
  if (filters.workspaceId) params.set("workspace_id", filters.workspaceId)
  if (filters.nodeId) params.set("directory_id", filters.nodeId)
  if (filters.createdBy) params.set("created_by", filters.createdBy)
  if (filters.sortBy === "date") params.set("sort", "updated")
  else if (filters.sortBy === "title") params.set("sort", "updated")
  else params.set("sort", "relevance")
  if (filters.dateRange?.from) params.set("updated_after", filters.dateRange.from)
  if (filters.dateRange?.to) params.set("updated_before", filters.dateRange.to)

  const data = await http.get<{ items: BackendSearchResult[]; total: number }>(
    `/search?${params.toString()}`,
  )
  return (data.items || []).map(mapSearchResult)
}

export async function apiGetComments(documentId: string): Promise<Comment[]> {
  const data = await http.get<{ items: BackendComment[] }>(`/documents/${documentId}/comments`)
  const comments = (data.items || []).map(mapComment)

  const rootComments = comments.filter((c) => !c.id.includes("reply"))
  const replyMap = new Map<string, Comment[]>()
  for (const c of comments) {
    const parentId = (c as unknown as { parentId?: string }).parentId
    if (parentId) {
      if (!replyMap.has(parentId)) replyMap.set(parentId, [])
      replyMap.get(parentId)!.push(c)
    }
  }

  for (const c of rootComments) {
    c.replies = replyMap.get(c.id) || []
  }

  return rootComments
}

export async function apiAddComment(comment: Omit<Comment, "id" | "createdAt" | "updatedAt">): Promise<Comment> {
  const result = await http.post<BackendComment>(`/documents/${comment.documentId}/comments`, {
    content: comment.content,
    block_id: comment.blockId || null,
    mentions: comment.mentions || [],
  })
  return mapComment(result)
}

export async function apiResolveComment(commentId: string): Promise<void> {
  await http.post(`/comments/${commentId}/resolve`)
}

export async function apiGetPresences(documentId: string): Promise<CollaboratorPresence[]> {
  void documentId
  return []
}

export async function apiGetUsers(): Promise<User[]> {
  try {
    const data = await http.get<{ items: Array<{ id: string; email: string; name: string; avatar_url?: string; status: string }> }>("/users")
    return (data.items || []).map((u) => ({
      id: u.id,
      name: u.name,
      email: u.email,
      avatar: u.avatar_url,
      role: "editor" as const,
    }))
  } catch {
    return []
  }
}

export { httpLogin as login, setToken, clearToken, getToken }
