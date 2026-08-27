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

// --- Skill read + propose DTOs (design-docs/19 §6.3 / §6.2, Phase 5-4) ---
// These are the client-facing shapes the MCP skill_* tools return. They
// mirror the §6.2 internal delivery response, trimmed by the agent's binding
// delivery_mode (tool/summary/inline). Existence never leaks: an unbound /
// missing skill yields ErrNotExist (→ empty result, §8.2). No execute
// endpoint: skill_propose lands a candidate, never publishes (§6.3).

// SkillListItem is one entry of skill_list: the agent-visible projection of a
// skill the agent is bound to (the SKILL.md header + effective delivery_mode +
// resolved version, no raw bytes).
type SkillListItem struct {
	AssetID      string `json:"asset_id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Version      string `json:"version,omitempty"`
	VersionNo    int64  `json:"version_no,omitempty"`
	DeliveryMode string `json:"delivery_mode"`
	FormatID     string `json:"format_id,omitempty"`
	ContentHash  string `json:"content_hash,omitempty"`
}

// SkillListResult is the skill_list response: the skills the agent is bound to.
// Empty for an unbound agent / a workspace with no skills (no leak — §8.2).
type SkillListResult struct {
	Items []SkillListItem `json:"items"`
	Total int             `json:"total"`
}

// SkillReadResult is the skill_read response (delivery_mode-trimmed). Header is
// always the SKILL.md frontmatter; Manifest is nil in summary mode; Capability
//Summary is set in summary mode only. CompatibilityReport is always present.
type SkillReadResult struct {
	AssetID             string            `json:"asset_id"`
	AssetVersionID      string            `json:"asset_version_id"`
	VersionNo           int64             `json:"version_no,omitempty"`
	DeliveryMode        string            `json:"delivery_mode"`
	Header              map[string]any    `json:"header"`
	Manifest            *SkillManifest    `json:"manifest,omitempty"`
	CapabilitySummary   map[string]any    `json:"capability_summary,omitempty"`
	CompatibilityReport map[string]any    `json:"compatibility_report"`
	ContentHash         string            `json:"content_hash,omitempty"`
}

// SkillManifest is the trimmed file inventory (mirrors domain.SkillManifest).
// The Path/Hash fields let the agent fetch bytes progressively via
// skill_resources; no bytes are inlined here.
type SkillManifest struct {
	Files             []SkillFileEntry `json:"files"`
	CapabilitySummary map[string]any   `json:"capability_summary,omitempty"`
	ContentHash       string           `json:"content_hash"`
	EntryCount        int              `json:"entry_count"`
	TotalSize         int64            `json:"total_size"`
}

// SkillFileEntry is one file in the skill manifest.
type SkillFileEntry struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Hash    string `json:"hash"`
	ExecBit bool   `json:"exec_bit"`
	Kind    string `json:"kind"`
}

// SkillResourceContent is one progressive resource read (skill_resources).
// Content is the decompressed file bytes; Hash is the manifest's sha256
// (integrity anchor); Kind is metadata only, NEVER an exec hint.
type SkillResourceContent struct {
	Path        string `json:"path"`
	Hash        string `json:"hash"`
	Kind        string `json:"kind"`
	Content     []byte `json:"-"`
	ContentHash string `json:"content_hash"`
}

// SkillProposeRequest is the skill_propose input: the agent drafts a SKILL.md
// body + optional metadata for the human reviewer. WorkspaceID scopes the
// candidate (AC-4); upstream the delegated context's workspace is the real
// gate, but the field lets the mock + tool layer pass it explicitly.
type SkillProposeRequest struct {
	WorkspaceID string          `json:"workspace_id"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Version     string          `json:"version,omitempty"`
	DraftBody   string          `json:"draft_body"`
	SourceRef   map[string]any  `json:"source_ref,omitempty"`
}

// SkillProposeResult is the skill_propose response: the candidate + review
// references. Status is always "candidate" — never published (§6.3).
type SkillProposeResult struct {
	AssetID         string `json:"asset_id"`
	AssetVersionID  string `json:"asset_version_id"`
	ReviewRequestID string `json:"review_request_id"`
	Status          string `json:"status"`
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

// --- CodeGraph DTOs (design-docs/17 §3.2 / §6.2) ---
// These are the client-facing shapes the MCP code_* tools return. They mirror
// the provider/codegraph-service types but live in moraclient so the MCP module
// stays self-contained (no import of the codegraph service package). Every result
// carries its commit (§3.2 CodeLoc) so an expired revision never masquerades as
// the current one.

// CodeLoc is the location anchor every codegraph result MUST carry (§3.2).
type CodeLoc struct {
	Commit    string `json:"commit"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line,omitempty"`
	Symbol    string `json:"symbol,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

// CodeHit is a search/explore/impact hit.
type CodeHit struct {
	Loc     CodeLoc `json:"loc"`
	Score   float64 `json:"score,omitempty"`
	Snippet string  `json:"snippet,omitempty"`
}

// CodeEdge is a directed relationship between two symbols.
type CodeEdge struct {
	From CodeLoc `json:"from"`
	To   CodeLoc `json:"to"`
	Kind string  `json:"kind"` // calls|defines|implements
}

// CodeNodeDef is a single symbol definition (code_node).
type CodeNodeDef struct {
	Loc       CodeLoc `json:"loc"`
	Kind      string  `json:"kind"`
	Signature string  `json:"signature,omitempty"`
	Docstring string  `json:"docstring,omitempty"`
}

// CodeFileNode is one file in a CodeFileTree.
type CodeFileNode struct {
	Path   string `json:"path"`
	Lines  int    `json:"lines,omitempty"`
	Commit string `json:"commit"`
}

// CodeFileTree is the files listing (code_files).
type CodeFileTree struct {
	Path  string          `json:"path"`
	Files []CodeFileNode  `json:"files,omitempty"`
	Dirs  []CodeFileTree  `json:"dirs,omitempty"`
}

// CodeGraphStatus is the code_status result: active graph version metadata.
type CodeGraphStatus struct {
	Commit             string                 `json:"commit"`
	SourceTreeHash     string                 `json:"source_tree_hash"`
	ProviderVersion    string                 `json:"provider_version"`
	IndexSchemaVersion string                 `json:"index_schema_version,omitempty"`
	Stats             CodeGraphBuildStats     `json:"stats,omitempty"`
	Stale             bool                   `json:"stale,omitempty"`
}

// CodeGraphBuildStats are build-time counts surfaced in CodeGraphStatus.
type CodeGraphBuildStats struct {
	Files   int `json:"files"`
	Symbols int `json:"symbols"`
	Edges   int `json:"edges"`
}

// CodeSearchQuery is the code_search input.
type CodeSearchQuery struct {
	Query    string `json:"query"`
	Language string `json:"language,omitempty"`
	PathGlob string `json:"path_glob,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// CodeExploreQuery is the code_explore input.
type CodeExploreQuery struct {
	Query    string `json:"query"`
	Language string `json:"language,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// CodeExploreResult is the code_explore outcome.
type CodeExploreResult struct {
	Hits   []CodeHit    `json:"hits,omitempty"`
	Nodes  []CodeNodeDef `json:"nodes,omitempty"`
	Commit string       `json:"commit"`
}

// CodeSymbolQuery names a symbol to resolve (code_node / code_callers /
// code_callees). symbol is required; language + path disambiguate.
type CodeSymbolQuery struct {
	Symbol   string `json:"symbol"`
	Language string `json:"language,omitempty"`
	Path     string `json:"path,omitempty"`
}

// CodeImpactQuery is the code_impact input.
type CodeImpactQuery struct {
	Symbol   string `json:"symbol"`
	Language string `json:"language,omitempty"`
	Path     string `json:"path,omitempty"`
	Depth    int    `json:"depth,omitempty"`
}

// CodeHits wraps a hit slice for the search/impact tools' empty-safe shape.
type CodeHits struct {
	Items  []CodeHit `json:"items"`
	Commit string    `json:"commit,omitempty"`
}

// CodeEdges wraps an edge slice for the callers/callees tools' empty-safe shape.
type CodeEdges struct {
	Items []CodeEdge `json:"items"`
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

	// --- Skill read + propose surface (design-docs/19 §6.3, Phase 5-4) ---
	// All skill_* methods are scoped to the delegated agent context
	// (AgentID + WorkspaceID, §11.2): the internal service-token alone never
	// authorizes skill delivery or proposal — a caller with no agent context
	// gets ErrNotExist (skill_list/skill_read/skill_resources) or
	// ErrForbidden (skill_propose), which the tool layer translates to an
	// empty result / 403 per §8.2. Existence never leaks: an unbound skill
	// is absent from skill_list and yields not-found on skill_read/skill_
	// resources, indistinguishable from a missing skill. skill_read returns
	// the delivery_mode-trimmed SKILL.md header + manifest; skill_resources
	// progressively reads one declared resource file. skill_propose submits a
	// candidate (never publishes — it lands a pending review_request).

	// SkillList lists the skills the agent is bound to in the delegated
	// workspace (skill_list). Returns ErrNotExist for a missing/no-permission
	// context; the tool layer yields an empty list (no leak — §8.2).
	SkillList(ctx context.Context, auth *AuthContext) (*SkillListResult, error)
	// SkillRead returns the SKILL.md header + manifest, delivery_mode-trimmed
	// (skill_read). assetID is the skill asset id; versionSpec is "latest" /
	// a version id / "". Returns ErrNotExist for an unbound / missing skill
	// (no leak — §8.2).
	SkillRead(ctx context.Context, auth *AuthContext, assetID, versionSpec string) (*SkillReadResult, error)
	// SkillResources progressively reads one declared resource file
	// (skill_resources). assetID is the skill asset id; resourcePath is a
	// manifest entry path; versionSpec is "latest" / a version id / "". The
	// binding's delivery_mode gates raw reads (inline/tool allow; summary
	// refuses — §6.2). Returns ErrNotExist for an unbound / summary-mode /
	// non-manifest path (no leak — §8.2).
	SkillResources(ctx context.Context, auth *AuthContext, assetID, versionSpec, resourcePath string) (*SkillResourceContent, error)
	// SkillPropose submits a candidate skill proposal (skill_propose). Write:
	// a read-only scope / no-agent-context caller is rejected; never publishes
	// directly — it lands a pending review_request. Returns the candidate +
	// review references.
	SkillPropose(ctx context.Context, auth *AuthContext, req SkillProposeRequest) (*SkillProposeResult, error)

	// --- CodeGraph query surface (design-docs/17 §6.2) ---
	// All code_* methods are read-only, scoped to a codebase asset id. RBAC is
	// enforced upstream by the codegraph service (via asset.ReadService.GetAsset):
	// a missing / cross-workspace / no-permission codebase returns ErrNotExist so
	// the tool layer yields an empty result, never an error to the Agent (§8.2
	// no-leak). A provider fault (capability_unavailable /
	// source_snapshot_unavailable / asset_version mismatch) surfaces as a typed
	// error the tool layer maps to an empty result + a diagnostic note, never a
	// faked result (§15). Every result carries its commit so an expired revision
	// never masquerades as current (§3.2 / §4.2).

	// CodeStatus returns the active codegraph version metadata for a codebase
	// (code_status). Returns ErrNotExist for a missing/no-permission codebase.
	CodeStatus(ctx context.Context, auth *AuthContext, codebaseID string) (*CodeGraphStatus, error)
	// CodeFiles returns the source tree listing (code_files).
	CodeFiles(ctx context.Context, auth *AuthContext, codebaseID string, pathPrefix string) (*CodeFileTree, error)
	// CodeSearch runs a code search (code_search). query is required.
	CodeSearch(ctx context.Context, auth *AuthContext, codebaseID string, req CodeSearchQuery) (*CodeHits, error)
	// CodeExplore runs the combined query (code_explore). query is required.
	CodeExplore(ctx context.Context, auth *AuthContext, codebaseID string, req CodeExploreQuery) (*CodeExploreResult, error)
	// CodeNode resolves one symbol (code_node). symbol is required.
	CodeNode(ctx context.Context, auth *AuthContext, codebaseID string, req CodeSymbolQuery) (*CodeNodeDef, error)
	// CodeCallers returns the incoming call edges (code_callers). symbol required.
	CodeCallers(ctx context.Context, auth *AuthContext, codebaseID string, req CodeSymbolQuery) (*CodeEdges, error)
	// CodeCallees returns the outgoing call edges (code_callees). symbol required.
	CodeCallees(ctx context.Context, auth *AuthContext, codebaseID string, req CodeSymbolQuery) (*CodeEdges, error)
	// CodeImpact computes the change-impact set (code_impact). symbol required.
	CodeImpact(ctx context.Context, auth *AuthContext, codebaseID string, req CodeImpactQuery) (*CodeHits, error)
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
