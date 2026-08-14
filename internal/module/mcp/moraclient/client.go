// Package moraclient abstracts the upstream Mora API + RAG search service that
// the MCP module calls. The MCP Server never re-implements Mora/RAG business
// logic (design doc 06 §6.3, 02 §2.2): it delegates via this interface.
//
// Two implementations ship:
//   - Mock: an in-memory Mora + RAG with embedded RBAC, used for tests and
//     standalone development.
//   - HTTP: a REST client for the Mora API and RAG search endpoints.
//
// RBAC semantics (design doc 06 §6.4):
//   - Read operations MUST NOT leak existence. When the caller lacks read
//     permission, methods return a typed "not found" error which the MCP tool
//     layer translates to an empty result (never a 403/404 to the Agent).
//   - Write operations return a typed "forbidden" error on missing write
//     permission; the MCP layer surfaces that as 403.
package moraclient

import (
	"context"
	"encoding/json"
	"time"

	domainerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// AuthContext is the caller identity propagated to every MoraClient call.
// It is constructed by the auth middleware from the resolved API token.
type AuthContext struct {
	TokenID      string
	IdentityType rbac.IdentityType
	IdentityID   string
	IdentityName string
	Scope        rbac.Scope
	Groups       []string
	IsAdmin      bool
}

// Workspace is a Mora workspace visible to the caller.
type Workspace struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description,omitempty"`
	OwnerID     string `json:"owner_id,omitempty"`
}

// DirectoryNode is one node in a workspace's directory tree. Children are
// nested to express the infinite-depth tree (design doc 04 §4 / §15 Directory).
type DirectoryNode struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	SortOrder int             `json:"sort_order"`
	Documents []DocumentMeta  `json:"documents,omitempty"`
	Children  []DirectoryNode `json:"children,omitempty"`
}

// DocumentMeta is document metadata (no body), as exposed by Resources and
// list_documents (design doc 04 §15 Document minus content).
type DocumentMeta struct {
	ID          string   `json:"id"`
	WorkspaceID string   `json:"workspace_id"`
	DirectoryID string   `json:"directory_id,omitempty"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	IndexStatus string   `json:"index_status"`
	VersionNo   int      `json:"version_no"`
	Tags        []string `json:"tags,omitempty"`
	CreatedBy   string   `json:"created_by,omitempty"`
	UpdatedAt   string   `json:"updated_at,omitempty"`
}

// Document is a full document including body, returned by get_document.
type Document struct {
	DocumentMeta
	Content json.RawMessage `json:"content"`
	Format  string          `json:"format"`
}

// VersionSummary is a document version history entry (design doc 04 §6).
type VersionSummary struct {
	VersionNo   int    `json:"version_no"`
	DiffSummary string `json:"diff_summary,omitempty"`
	AuthorID    string `json:"author_id,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// Tag is a workspace tag.
type Tag struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// SearchRequest is the input to semantic hybrid search (design doc 04 §9).
type SearchRequest struct {
	Query       string   `json:"query"`
	WorkspaceID string   `json:"workspace_id,omitempty"`
	DirectoryID string   `json:"directory_id,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	TopK        int      `json:"top_k,omitempty"`
	TopN        int      `json:"top_n,omitempty"`
	Rerank      bool     `json:"rerank,omitempty"`
}

// SearchHit is one ranked result chunk (design doc 04 §9 response item).
type SearchHit struct {
	DocumentID  string  `json:"document_id"`
	Title       string  `json:"title"`
	ChunkText   string  `json:"chunk_text"`
	ChunkIndex  int     `json:"chunk_index"`
	Score       float64 `json:"score"`
	DenseScore  float64 `json:"dense_score,omitempty"`
	BM25Score   float64 `json:"bm25_score,omitempty"`
	WorkspaceID string  `json:"workspace_id"`
	SourceURL   string  `json:"source_url"`
}

// SearchResult is the search response (RBAC-filtered upstream).
type SearchResult struct {
	Items []SearchHit `json:"items"`
	Total int         `json:"total"`
}

// CreateDraftRequest creates a document draft (never publishes directly —
// design doc 06 §5.2.3: write ops enter draft/review state).
type CreateDraftRequest struct {
	WorkspaceID string
	ParentID    string
	Title       string
	Content     string
	Format      string
}

// UpdateDocumentRequest updates a document, producing a new draft version
// pending review (design doc 06 §5.2.4).
type UpdateDocumentRequest struct {
	DocumentID string
	Content    string
	Format     string
	Summary    string
}

// DraftResult is returned by write operations.
type DraftResult struct {
	DraftID    string `json:"draft_id"`
	VersionNo  int    `json:"version_no"`
	ReviewURL  string `json:"review_url"`
	DocumentID string `json:"document_id,omitempty"`
}

// WikiSpaceStatus is the aggregated status returned by wiki_status (design
// doc 16 §7.3 / §7.2). It surfaces the Space's directory (pages), the most
// recent maintenance run, and visible pending proposals — all RBAC-filtered
// upstream so an unauthorized caller gets an empty result, not an error
// (existence-leak prevention, §8.2).
type WikiSpaceStatus struct {
	WikiSpaceID   string            `json:"wiki_space_id"`
	WorkspaceID   string            `json:"workspace_id"`
	Name          string            `json:"name"`
	Status        string            `json:"status"`
	Pages         []WikiPageSummary `json:"pages"`
	LastRun       *MaintenanceRun   `json:"last_run,omitempty"`
	Proposals     []PageProposal    `json:"proposals"`
}

// WikiPageSummary is one page in the wiki_status directory listing.
type WikiPageSummary struct {
	PageKey       string `json:"page_key"`
	PageKind      string `json:"page_kind"`
	StaleReason   string `json:"stale_reason,omitempty"`
	LastMaintained string `json:"last_maintained,omitempty"`
}

// MaintenanceRun is the projection of a wiki_maintenance_runs row surfaced
// by wiki_status (design doc 16 §2.4).
type MaintenanceRun struct {
	ID           string         `json:"id"`
	TriggerType  string         `json:"trigger_type"`
	Status       string         `json:"status"`
	InputSetHash string         `json:"input_set_hash,omitempty"`
	StartedAt    string         `json:"started_at,omitempty"`
	FinishedAt   string         `json:"finished_at,omitempty"`
	ErrorCode    string         `json:"error_code,omitempty"`
}

// PageProposal is one pending/recent proposal surfaced by wiki_status (§2.4).
// Only proposals visible to the caller are returned.
type PageProposal struct {
	ID          string `json:"id"`
	PageKey     string `json:"page_key"`
	Action      string `json:"action"`
	Status      string `json:"status"`
	IsBypass    bool   `json:"is_bypass"`
	ContentHash string `json:"content_hash,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// WikiPageProposeRequest is the wiki_page_propose input (design doc 16 §7.3 /
// §11.3). It triggers a maintenance run that lands a candidate proposal for
// the named page — it never publishes directly.
type WikiPageProposeRequest struct {
	WikiSpaceID string         `json:"wiki_space_id"`
	PageKey     string         `json:"page_key"`
	AnswerRef   map[string]any `json:"answer_ref,omitempty"`
}

// WikiPageProposeResult is the wiki_page_propose response: the created (or
// existing, on idempotent retry) maintenance run id. The actual proposal
// lands asynchronously via the wiki_maintain job; the caller polls
// wiki_status.
type WikiPageProposeResult struct {
	RunID    string `json:"run_id"`
	Status   string `json:"status"`
	PageKey  string `json:"page_key,omitempty"`
}

// ListDocumentsParams filters the document listing.
type ListDocumentsParams struct {
	WorkspaceID string
	DirectoryID string
	Tag         string
	Status      string
	Page        int
	PageSize    int
}

// MoraClient is the upstream Mora + RAG capability surface used by MCP tools
// and resources. All methods receive the caller AuthContext so RBAC is applied
// server-side by the real Mora/RAG services.
type MoraClient interface {
	// ListWorkspaces returns workspaces visible (read) to the caller.
	ListWorkspaces(ctx context.Context, auth *AuthContext) ([]Workspace, error)
	// GetDirectoryTree returns the visible directory tree of a workspace.
	GetDirectoryTree(ctx context.Context, auth *AuthContext, workspaceID string) ([]DirectoryNode, error)
	// GetDocumentMeta returns metadata for one document. Returns ErrNotFound
	// when the document does not exist OR the caller lacks read permission
	// (existence must not leak — see design doc 06 §6.4).
	GetDocumentMeta(ctx context.Context, auth *AuthContext, documentID string) (*DocumentMeta, error)
	// GetDocument returns the document body. Same existence-leak rule as
	// GetDocumentMeta. versionNo<=0 means latest.
	GetDocument(ctx context.Context, auth *AuthContext, documentID string, format string, versionNo int) (*Document, error)
	// ListDocuments lists documents under a workspace/directory, RBAC-filtered.
	ListDocuments(ctx context.Context, auth *AuthContext, p ListDocumentsParams) ([]DocumentMeta, int, error)
	// GetTags returns the tag taxonomy of a workspace.
	GetTags(ctx context.Context, auth *AuthContext, workspaceID string) ([]Tag, error)
	// GetDocumentVersions returns version history summaries (read permission).
	GetDocumentVersions(ctx context.Context, auth *AuthContext, documentID string) ([]VersionSummary, error)
	// Search runs RAG semantic hybrid search (Dense+BM25+Rerank), RBAC-filtered.
	Search(ctx context.Context, auth *AuthContext, req SearchRequest) (*SearchResult, error)
	// CreateDraft creates a document draft (write permission; returns
	// ErrForbidden on missing write perm).
	CreateDraft(ctx context.Context, auth *AuthContext, req CreateDraftRequest) (*DraftResult, error)
	// UpdateDocument updates a document into a new draft version (write perm).
	UpdateDocument(ctx context.Context, auth *AuthContext, req UpdateDocumentRequest) (*DraftResult, error)
	// WikiStatus returns a Wiki Space's directory, last run, and visible
	// proposals (design doc 16 §7.3). Read-only: no-permission/absent Space
	// returns ErrNotExist so the tool layer yields an empty result (§8.2).
	WikiStatus(ctx context.Context, auth *AuthContext, wikiSpaceID string) (*WikiSpaceStatus, error)
	// WikiPagePropose triggers a maintenance run that lands a candidate
	// proposal for a page (§7.3/§11.3). Write: returns ErrForbidden on
	// missing write perm; never publishes directly.
	WikiPagePropose(ctx context.Context, auth *AuthContext, req WikiPageProposeRequest) (*WikiPageProposeResult, error)
}

// ErrNotExist is returned by read methods when the resource is absent or the
// caller has no visibility — callers translate it to an empty result, never an
// error to the Agent, to prevent existence leakage. It is an alias for the
// shared domainerr.ErrNotFound so transport layers map it uniformly.
//
// (Kept as a package-level value via the alias function ErrNotExist() below to
// preserve a stable call site for tool/resource implementations.)

// ErrNotExist returns the sentinel not-found/not-visible error. Callers compare
// with errors.Is(err, domainerr.ErrNotFound).
func ErrNotExist() error { return domainerr.ErrNotFound }

// nowFunc is overridable in tests via the mock; production uses time.Now.
var nowFunc = func() time.Time { return time.Now().UTC() }
