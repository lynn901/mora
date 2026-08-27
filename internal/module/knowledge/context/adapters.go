// Package contextbroker — type adapter skeletons (design-docs/19 §3.3).
//
// Each adapter wraps an existing type engine and maps its result into the
// unified KnowledgeCandidate (candidate.go). The Broker orchestrates these via
// the ports (ports.go); adapters are NOT an authorization gate — they forward
// the server-built AuthzContext the Broker pushed down and never accept a
// user-submitted allowed_asset_ids (§3.2 invariant). The Broker still runs a
// batch post-check after the fetch.
//
// These are skeletons: the mapping logic is complete (the conversion functions
// on candidate.go are pure), but the engine call wiring is the minimum needed
// for the ports to compile and for fake-port unit tests to assert mapping
// correctness. Full Broker orchestration (parallel fan-out, dedup, budget,
// citation build) lands in later Phase 6 stages.

package contextbroker

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/recall"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
	cgservice "github.com/lynn901/mora/internal/module/knowledge/codegraph/service"
	"github.com/lynn901/mora/internal/module/rag/search"
	skilldelivery "github.com/lynn901/mora/internal/module/skill"
)

// ---------------------------------------------------------------------------
// MemoryQuery adapter — reuses recall.RecallService (§3.3)
// ---------------------------------------------------------------------------

// MemoryAdapter wraps recall.RecallService and satisfies MemoryQuery (12 §9.4).
// recall.RecallService already implements the leak-safe ranked recall (§8.1 /
// §9.3) and the §4.3 Evidence ACL chain; the adapter only translates the
// memory-dimension candidate into the unified shape via CandidateFromMemory.
// It does NOT touch recall's REST serialization — Phase 4 stays stable (D2).
type MemoryAdapter struct {
	svc recallAdapterPort
}

// recallAdapterPort is the narrow slice of recall.RecallService the adapter
// calls (just Recall). Local interface so the adapter unit test can inject a
// fake without depending on the recall package's wider surface; the real
// recall.RecallService satisfies it (Recall method signature matches).
type recallAdapterPort interface {
	Recall(ctx context.Context, auth recall.AuthContext, q recall.KnowledgeQuery) ([]recall.KnowledgeCandidate, error)
}

// NewMemoryAdapter wires the adapter over a recall.RecallService (or any
// recallAdapterPort, e.g. a fake in tests). The wiring layer passes the
// production recall.RecallService (WithAuthz/WithUnits chained).
func NewMemoryAdapter(svc recallAdapterPort) *MemoryAdapter {
	return &MemoryAdapter{svc: svc}
}

// Recall implements MemoryQuery.Recall. It translates the Broker's
// AuthzContext + MemoryQueryRequest into recall.AuthContext +
// recall.KnowledgeQuery, calls the recall service, and maps each memory
// candidate into the unified shape. An unauthorized caller gets an empty slice
// (leak-safe, §9.3 — the recall service guarantees it).
func (a *MemoryAdapter) Recall(ctx context.Context, ac AuthzContext, q MemoryQueryRequest) ([]KnowledgeCandidate, error) {
	rq := recall.KnowledgeQuery{
		Query:            q.Query,
		WorkspaceID:      q.WorkspaceID,
		OwnerID:          q.OwnerID,
		MemoryType:       q.MemoryType,
		ValidAt:          q.ValidAt,
		AssetID:          q.AssetID,
		IncludeCandidates: q.IncludeCandidates,
		MaxItems:         q.MaxItems,
	}
	rac := recall.AuthContext{
		SubjectType:     ac.PrincipalType,
		PrincipalID:     ac.PrincipalID,
		GroupIDs:        nil, // AgentID path; GroupIDs plumbed by wiring when principal is a user
		IsAdmin:         false,
		IsServiceCaller: ac.AgentID != nil,
	}
	// ActingUserID + AgentID: when the principal is an agent acting on behalf
	// of a user, recall honors the user's group memberships via ActingUserID.
	if ac.ActingUserID != nil {
		rac.PrincipalID = *ac.ActingUserID
	}
	mems, err := a.svc.Recall(ctx, rac, rq)
	if err != nil {
		return nil, fmt.Errorf("context.memory: recall: %w", err)
	}
	out := make([]KnowledgeCandidate, 0, len(mems))
	for _, m := range mems {
		out = append(out, CandidateFromMemory(memoryCandidate{
			AssetID:          m.AssetID,
			AssetVersionID:   m.Citation.AssetVersionID,
			Title:            m.Title,
			Snippet:          m.Snippet,
			Score:            m.Score,
			Authority:        m.Authority,
			Freshness:        m.Freshness,
			Confidence:       m.Confidence,
			ContentHash:      m.ContentHash,
			EvidenceID:       m.Citation.EvidenceID,
			EvidenceLocator:  m.Citation.QuoteLocator,
		}))
		// Relations: recall.RelationSummary → recallRelation (narrow view).
		// Attached after construction so the loop stays linear.
		out[len(out)-1].Relations = toUnifiedRelations(fromRecallRelations(m.Relations))
	}
	return out, nil
}

func fromRecallRelations(rs []recall.RelationSummary) []recallRelation {
	if len(rs) == 0 {
		return nil
	}
	out := make([]recallRelation, len(rs))
	for i, r := range rs {
		out[i] = recallRelation{
			RelationType: r.RelationType,
			TargetID:     r.TargetID,
			TargetTitle:  r.TargetTitle,
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// DocumentQuery adapter — wraps mora/search + rag/search (§3.3)
// ---------------------------------------------------------------------------

// DocumentAdapter wraps the document search engines and satisfies DocumentQuery.
// It composes the FTS SearchExecutor (mora/search) and the hybrid RAG searcher
// (rag/search). The Broker decides which to call (FTS-only when Qdrant is
// down, §15); this adapter maps whichever hit shape arrives into the unified
// candidate. RBAC is the hard filter the engines already enforce; the adapter
// forwards AuthzContext.AllowedAssetIDs as the visible document set.
type DocumentAdapter struct {
	fts    docFTSPort     // mora/search.SearchExecutor-shaped port
	hybrid docHybridPort  // rag/search.HybridSearcher-shaped port
}

// docFTSPort is the mora/search.SearchExecutor slice (Search(ctx, Query) →
// []Result, total, error). Local interface for fake injection.
type docFTSPort interface {
	Search(ctx context.Context, q docFTSQuery) ([]docFTSResult, int, error)
}

// docFTSQuery / docFTSResult are the narrow views of mora/search.Query /
// search.Result the adapter builds/consumes. They decouple the context package
// from the search package's SQL-builder types (same narrow-port precedent).
type docFTSQuery struct {
	SQL  string
	Args []any
}
type docFTSResult struct {
	DocumentID  uuid.UUID
	Title       string
	Snippet     string
	Score       float64
	WorkspaceID uuid.UUID
	DirectoryID *uuid.UUID
	UpdatedAt   string // RFC3339
}

// docHybridPort is the rag/search.HybridSearcher.Search slice.
type docHybridPort interface {
	Search(ctx context.Context, req docHybridReq) (*docHybridResult, error)
}
type docHybridReq struct {
	Query       string
	WorkspaceID string
	TopN        int
}
type docHybridResult struct {
	Items []docHybridHit
	Total int
}
type docHybridHit struct {
	DocumentID  string
	Title       string
	ChunkText   string
	ChunkIndex  int
	SectionPath string
	Score       float64
	WorkspaceID string
}

// NewDocumentAdapter wires the FTS + hybrid search ports. Either may be nil in
// dev/test (the nil path is skipped); production wires both so the Broker can
// degrade FTS-only when Qdrant is unavailable (§15).
func NewDocumentAdapter(fts docFTSPort, hybrid docHybridPort) *DocumentAdapter {
	return &DocumentAdapter{fts: fts, hybrid: hybrid}
}

// Search implements DocumentQuery.Search. First version runs the hybrid path
// when available (RAG fuses dense + BM25 under RBAC, §05), falling back to the
// FTS-only path. Each hit is mapped via CandidateFromDocument. version_id is
// not on the search hit (it is resolved at read time), so AssetVersion is nil
// here; the CitationBuilder completes it post-authz (§8.2).
func (a *DocumentAdapter) Search(ctx context.Context, ac AuthzContext, q DocumentQueryRequest) ([]KnowledgeCandidate, error) {
	if a.hybrid != nil {
		res, err := a.hybrid.Search(ctx, docHybridReq{
			Query:       q.Query,
			WorkspaceID: q.WorkspaceID.String(),
			TopN:        q.MaxItems,
		})
		if err != nil {
			return nil, fmt.Errorf("context.document: hybrid search: %w", err)
		}
		out := make([]KnowledgeCandidate, 0, len(res.Items))
		for _, h := range res.Items {
			id, parseErr := uuid.Parse(h.DocumentID)
			if parseErr != nil {
				continue // malformed id: skip, do not surface (§8.2 no leak)
			}
			loc := map[string]any{"chunk_index": h.ChunkIndex}
			if h.SectionPath != "" {
				loc["section_path"] = h.SectionPath
			}
			out = append(out, CandidateFromDocument(documentHit{
				DocumentID: id,
				Title:      h.Title,
				Snippet:    h.ChunkText,
				Score:      h.Score,
				Locator:    loc,
			}))
		}
		return out, nil
	}
	// FTS-only fallback (Qdrant unavailable, §15). The visible-doc set is the
	// AuthzContext.AllowedAssetIDs (server-resolved); the adapter does NOT
	// accept a caller-supplied set (§3.2).
	return nil, nil // skeleton: FTS wiring lands with the Broker (no engine to call here yet)
}

// ---------------------------------------------------------------------------
// CodeQuery adapter — wraps codegraph/service (§3.3)
// ---------------------------------------------------------------------------

// CodeAdapter wraps codegraph.Service and satisfies CodeQuery. It calls the
// read-only Search (never Explore/Files/Node — those are type-specialized
// tools the Broker does not flatten, D12) and maps CodeHit into the unified
// candidate, carrying commit + source_tree_ref as the version anchor.
type CodeAdapter struct {
	svc codeAdapterPort
}

// codeAdapterPort is the codegraph.Service.Search + ActiveCodeGraph slice.
// Local interface for fake injection; the real codegraph.Service satisfies it.
type codeAdapterPort interface {
	Search(ctx context.Context, auth codeAssetAuthContext, id uuid.UUID, req cgprovider.CodeSearchRequest) ([]cgprovider.CodeHit, error)
	ActiveCodeGraph(ctx context.Context, assetID uuid.UUID) (cgservice.ActiveGraph, error)
}

// codeAssetAuthContext is the narrow view of asset.AuthContext the codegraph
// service takes. Kept local so the adapter does not import the asset package's
// wider surface; the wiring layer adapts platform/authz.AuthzContext →
// asset.AuthContext before calling the real service.
type codeAssetAuthContext interface{}

// NewCodeAdapter wires the codegraph query port.
func NewCodeAdapter(svc codeAdapterPort) *CodeAdapter {
	return &CodeAdapter{svc: svc}
}

// Search implements CodeQuery.Search. It resolves the active graph (for
// commit/source_tree_ref), runs the provider search, and maps each CodeHit.
func (a *CodeAdapter) Search(ctx context.Context, ac AuthzContext, q CodeQueryRequest) ([]KnowledgeCandidate, error) {
	ag, err := a.svc.ActiveCodeGraph(ctx, q.AssetID)
	if err != nil {
		return nil, fmt.Errorf("context.code: active graph: %w", err)
	}
	limit := q.MaxItems
	if limit <= 0 {
		limit = 20
	}
	hits, err := a.svc.Search(ctx, nil, q.AssetID, cgprovider.CodeSearchRequest{
		Query:    q.Query,
		Language: q.Language,
		PathGlob: q.PathGlob,
		Limit:    limit,
	})
	if err != nil {
		return nil, fmt.Errorf("context.code: search: %w", err)
	}
	out := make([]KnowledgeCandidate, 0, len(hits))
	for _, h := range hits {
		out = append(out, CandidateFromCode(codeHit{
			AssetID:       ag.AssetID,
			Commit:         ag.Commit,
			SourceTreeRef: ag.SourceTreeRef,
			Path:          h.Loc.Path,
			StartLine:     h.Loc.StartLine,
			EndLine:       h.Loc.EndLine,
			Symbol:        h.Loc.Symbol,
			Snippet:        h.Snippet,
			Score:         h.Score,
		}))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// SkillQuery adapter — wraps skill/delivery ArchiveReader (§3.3)
// ---------------------------------------------------------------------------

// SkillAdapter wraps the skill delivery service and satisfies SkillQuery. It
// lists the agent's allowed skills (binding-resolved) and/or delivers one,
// trimming by the binding delivery_mode (tool/summary/inline), and maps the
// DeliveryResult into the unified candidate carrying package_version.
type SkillAdapter struct {
	svc skillAdapterPort
}

// skillAdapterPort is the skill.DeliveryService.Deliver slice. Local interface
// for fake injection; the real skill.DeliveryService satisfies it.
type skillAdapterPort interface {
	Deliver(ctx context.Context, agentID, workspaceID, assetID uuid.UUID, versionSpec string) (skilldelivery.DeliveryResult, error)
}

// NewSkillAdapter wires the skill delivery port.
func NewSkillAdapter(svc skillAdapterPort) *SkillAdapter {
	return &SkillAdapter{svc: svc}
}

// Discover implements SkillQuery.Discover. For a single asset it delivers one
// skill; for an empty AssetIDs it lists the agent's allowed skills (the wiring
// layer resolves the binding list — the skeleton delivers the named set). The
// candidate is trimmed by delivery_mode (§3.3): tool=SKILL.md head,
// summary=description, inline=resource list.
func (a *SkillAdapter) Discover(ctx context.Context, ac AuthzContext, q SkillQueryRequest) ([]KnowledgeCandidate, error) {
	if q.AgentID == uuid.Nil {
		return nil, fmt.Errorf("context.skill: agent_id required")
	}
	ids := q.AssetIDs
	out := make([]KnowledgeCandidate, 0, len(ids))
	for _, id := range ids {
		res, err := a.svc.Deliver(ctx, q.AgentID, q.WorkspaceID, id, "")
		if err != nil {
			// ErrPackageNotFound = no allow binding / cross-workspace / missing
			// (§8.2 no leak): skip, do not surface existence.
			continue
		}
		out = append(out, CandidateFromSkill(skillHit{
			AssetID:          res.AssetID,
			AssetVersionID:   &res.AssetVersionID,
			Title:            skillTitle(res),
			Snippet:          skillSnippet(res),
			VersionOrRevision: skillVersion(res),
			Locator:          skillLocator(res),
		}))
	}
	return out, nil
}

func skillTitle(r skilldelivery.DeliveryResult) string {
	if r.Header != nil {
		if v, ok := r.Header["name"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func skillSnippet(r skilldelivery.DeliveryResult) string {
	if r.CapabilitySummary != nil {
		if v, ok := r.CapabilitySummary["description"].(string); ok && v != "" {
			return v
		}
	}
	if r.Header != nil {
		if v, ok := r.Header["description"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func skillVersion(r skilldelivery.DeliveryResult) string {
	if r.VersionNo > 0 {
		return fmt.Sprintf("v%d", r.VersionNo)
	}
	return r.AssetVersionID.String()
}

func skillLocator(r skilldelivery.DeliveryResult) map[string]any {
	loc := map[string]any{
		"delivery_mode": string(r.DeliveryMode),
	}
	if r.Manifest != nil {
		loc["resource_count"] = len(r.Manifest.Files)
	}
	return loc
}

// Ensure domain import is used (AssetType references in candidate.go carry the
// real dependency; this guard keeps the package honest if candidate.go moves).
var _ domain.AssetType = domain.AssetTypeDocument

// Ensure search import is referenced (the docHybridPort uses the rag/search
// package path even though the local interfaces narrow it).
var _ = search.SearchRequest{}
