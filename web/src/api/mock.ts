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
} from "@/types"

const delay = (ms: number) => new Promise((r) => setTimeout(r, ms))

const mockUsers: User[] = [
  { id: "u1", name: "Alice Chen", email: "alice@mora.dev", role: "admin" },
  { id: "u2", name: "Bob Wang", email: "bob@mora.dev", role: "editor" },
  { id: "u3", name: "Carol Li", email: "carol@mora.dev", role: "viewer" },
]

const mockWorkspaces: Workspace[] = [
  { id: "ws1", name: "Engineering", description: "Engineering docs", icon: "🔧", createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z" },
  { id: "ws2", name: "Product", description: "Product specs", icon: "📦", createdAt: "2026-02-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z" },
]

const mockTree: TreeNode[] = [
  {
    id: "n1", workspaceId: "ws1", parentId: null, name: "Getting Started", type: "folder", order: 0,
    createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z",
    children: [
      { id: "n2", workspaceId: "ws1", parentId: "n1", name: "Introduction", type: "document", order: 0, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z" },
      { id: "n3", workspaceId: "ws1", parentId: "n1", name: "Quick Start", type: "document", order: 1, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z" },
    ],
  },
  {
    id: "n4", workspaceId: "ws1", parentId: null, name: "Architecture", type: "folder", order: 1,
    createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z",
    children: [
      { id: "n5", workspaceId: "ws1", parentId: "n4", name: "System Design", type: "document", order: 0, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z" },
      {
        id: "n6", workspaceId: "ws1", parentId: "n4", name: "API Reference", type: "folder", order: 1,
        createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z",
        children: [
          { id: "n7", workspaceId: "ws1", parentId: "n6", name: "REST API", type: "document", order: 0, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z" },
          { id: "n8", workspaceId: "ws1", parentId: "n6", name: "GraphQL API", type: "document", order: 1, createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z" },
        ],
      },
    ],
  },
]

const mockDocuments: Record<string, Document> = {
  n2: {
    id: "d1", workspaceId: "ws1", nodeId: "n2", title: "Introduction",
    content: "# Introduction\n\nWelcome to the Mora platform.\n\n## Overview\n\nThis is a **collaborative** knowledge base with:\n- Real-time editing\n- Version history\n- Full-text search\n\n```typescript\nconst greeting = 'Hello, Mora!'\nconsole.log(greeting)\n```\n\n```mermaid\ngraph TD\n    A[User] --> B[Editor]\n    B --> C[Save]\n    C --> D[Version History]\n```",
    contentFormat: "markdown", createdBy: "u1", updatedBy: "u1",
    createdAt: "2026-01-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z",
    tags: ["guide", "intro"], status: "published", versionNo: 3,
  },
  n3: {
    id: "d2", workspaceId: "ws1", nodeId: "n3", title: "Quick Start",
    content: "# Quick Start\n\nGet started in 3 steps:\n\n1. Create a workspace\n2. Add pages\n3. Invite team members",
    contentFormat: "markdown", createdBy: "u1", updatedBy: "u2",
    createdAt: "2026-01-02T00:00:00Z", updatedAt: "2026-07-02T00:00:00Z",
    tags: ["guide"], status: "published", versionNo: 1,
  },
  n5: {
    id: "d3", workspaceId: "ws1", nodeId: "n5", title: "System Design",
    content: "# System Design\n\n## Architecture\n\nModular monolith with clear boundaries.\n\n## Components\n\n- **Frontend**: React + TypeScript + shadcn/ui\n- **Backend**: Go\n- **Database**: PostgreSQL\n- **Vector DB**: Qdrant",
    contentFormat: "markdown", createdBy: "u1", updatedBy: "u1",
    createdAt: "2026-02-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z",
    tags: ["architecture", "design"], status: "published", versionNo: 1,
  },
  n7: {
    id: "d4", workspaceId: "ws1", nodeId: "n7", title: "REST API",
    content: "# REST API\n\n## Endpoints\n\n### GET /api/documents\n\nReturns a list of documents.",
    contentFormat: "markdown", createdBy: "u2", updatedBy: "u2",
    createdAt: "2026-03-01T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z",
    tags: ["api", "rest"], status: "published", versionNo: 1,
  },
  n8: {
    id: "d5", workspaceId: "ws1", nodeId: "n8", title: "GraphQL API",
    content: "# GraphQL API\n\n## Schema\n\n```graphql\ntype Document {\n  id: ID!\n  title: String!\n  content: String!\n}\n```",
    contentFormat: "markdown", createdBy: "u2", updatedBy: "u2",
    createdAt: "2026-03-02T00:00:00Z", updatedAt: "2026-07-01T00:00:00Z",
    tags: ["api", "graphql"], status: "draft", versionNo: 1,
  },
}

const mockVersions: Record<string, DocumentVersion[]> = {
  d1: [
    { id: "v1", documentId: "d1", version: 1, content: "# Introduction\n\nInitial draft.", contentFormat: "markdown", createdBy: "u1", createdAt: "2026-01-01T00:00:00Z", changeSummary: "Initial version" },
    { id: "v2", documentId: "d1", version: 2, content: "# Introduction\n\nWelcome to the Mora platform.\n\n## Overview\n\nThis is a collaborative knowledge base.", contentFormat: "markdown", createdBy: "u1", createdAt: "2026-03-01T00:00:00Z", changeSummary: "Added overview section" },
    { id: "v3", documentId: "d1", version: 3, content: mockDocuments.n2!.content, contentFormat: "markdown", createdBy: "u2", createdAt: "2026-07-01T00:00:00Z", changeSummary: "Added code examples and diagram" },
  ],
}

const mockPermissions: Permission[] = [
  { id: "p1", workspaceId: "ws1", role: "admin", scope: "workspace", inherited: false, userId: "u1" },
  { id: "p2", workspaceId: "ws1", role: "write", scope: "workspace", inherited: false, userId: "u2" },
  { id: "p3", workspaceId: "ws1", role: "read", scope: "workspace", inherited: false, userId: "u3" },
  { id: "p4", nodeId: "n5", role: "write", scope: "directory", inherited: false, userId: "u3" },
]

const mockComments: Comment[] = [
  {
    id: "c1", documentId: "d1", blockId: "heading-1", content: "Should we add more examples?",
    createdBy: "u2", createdAt: "2026-06-01T00:00:00Z", updatedAt: "2026-06-01T00:00:00Z",
    resolved: false, mentions: ["u1"],
    replies: [
      {
        id: "c1r1", documentId: "d1", blockId: "heading-1", content: "Good idea, I'll add some.",
        createdBy: "u1", createdAt: "2026-06-02T00:00:00Z", updatedAt: "2026-06-02T00:00:00Z",
        resolved: false, mentions: [],
      },
    ],
  },
  {
    id: "c2", documentId: "d1", content: "Overall looks great!",
    createdBy: "u3", createdAt: "2026-06-03T00:00:00Z", updatedAt: "2026-06-03T00:00:00Z",
    resolved: true, resolvedBy: "u1", resolvedAt: "2026-06-04T00:00:00Z", mentions: [],
  },
]

const mockPresences: CollaboratorPresence[] = [
  { userId: "u1", userName: "Alice Chen", color: "#3b82f6", cursorPosition: { line: 3, ch: 10 }, lastSeen: new Date().toISOString() },
  { userId: "u2", userName: "Bob Wang", color: "#10b981", cursorPosition: { line: 7, ch: 5 }, lastSeen: new Date().toISOString() },
]

export async function apiGetWorkspaces(): Promise<Workspace[]> {
  await delay(300)
  return mockWorkspaces
}

export async function apiGetTree(workspaceId: string): Promise<TreeNode[]> {
  await delay(300)
  return mockTree.filter((n) => n.workspaceId === workspaceId)
}

export async function apiGetDocument(nodeId: string): Promise<Document> {
  await delay(200)
  const doc = mockDocuments[nodeId]
  if (!doc) throw new Error("Document not found")
  return doc
}

export async function apiSaveDocument(doc: Document): Promise<Document> {
  await delay(500)
  return { ...doc, updatedAt: new Date().toISOString() }
}

export async function apiGetVersions(documentId: string): Promise<DocumentVersion[]> {
  await delay(300)
  return mockVersions[documentId] || []
}

export async function apiRollbackVersion(documentId: string, versionId: string): Promise<Document> {
  await delay(500)
  const versions = mockVersions[documentId] || []
  const v = versions.find((v) => v.id === versionId)
  if (!v) throw new Error("Version not found")
  return { ...mockDocuments.n2!, content: v.content, updatedAt: new Date().toISOString() }
}

export async function apiGetPermissions(workspaceId: string): Promise<Permission[]> {
  await delay(200)
  return mockPermissions.filter((p) => p.workspaceId === workspaceId)
}

export async function apiSetPermission(perm: Omit<Permission, "id">): Promise<Permission> {
  await delay(300)
  return { ...perm, id: `p-${Date.now()}` }
}

export async function apiDeletePermission(id: string): Promise<void> {
  await delay(200)
}

export async function apiSearch(filters: SearchFilters): Promise<SearchResult[]> {
  await delay(400)
  const results: SearchResult[] = Object.values(mockDocuments).map((d) => ({
    id: `sr-${d.id}`,
    documentId: d.id,
    title: d.title,
    snippet: d.content.slice(0, 200),
    highlights: filters.query ? [d.title, d.content.slice(0, 100)] : [],
    score: Math.random(),
    workspaceId: d.workspaceId,
    nodeId: d.nodeId,
    tags: d.tags,
    createdBy: d.createdBy,
    createdAt: d.createdAt,
    updatedAt: d.updatedAt,
  }))
  return results.filter((r) => {
    if (filters.query && !r.title.toLowerCase().includes(filters.query.toLowerCase()) && !r.snippet.toLowerCase().includes(filters.query.toLowerCase())) return false
    if (filters.workspaceId && r.workspaceId !== filters.workspaceId) return false
    return true
  })
}

export async function apiGetComments(documentId: string): Promise<Comment[]> {
  await delay(200)
  return mockComments.filter((c) => c.documentId === documentId)
}

export async function apiAddComment(comment: Omit<Comment, "id" | "createdAt" | "updatedAt">): Promise<Comment> {
  await delay(300)
  return { ...comment, id: `c-${Date.now()}`, createdAt: new Date().toISOString(), updatedAt: new Date().toISOString() }
}

export async function apiResolveComment(commentId: string): Promise<void> {
  await delay(200)
}

export async function apiGetPresences(documentId: string): Promise<CollaboratorPresence[]> {
  await delay(100)
  return mockPresences
}

export async function apiGetUsers(): Promise<User[]> {
  await delay(200)
  return mockUsers
}
