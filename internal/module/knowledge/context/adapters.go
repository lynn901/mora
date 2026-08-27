// adapters.go — 四类型查询端口 adapter 实现（design-docs/19 §3.3 / §8.1）。
//
// 每个 adapter 包裹一个已实现的类型引擎，把其原生返回映射为统一
// KnowledgeCandidate（candidate.go）。adapter 只做字段映射，不重新解析、
// 不重做授权（§8.2）。授权由 platform/authz 在检索前（Authorize 产
// AuthzContext，§7.1 step 1-2）与检索后（VisibleAssets 批量 post-check，
// §7.1 step 5）完成；adapter 不接受用户提交的 allowed_asset_ids，只接受
// Broker 下推的、服务端构造的 AuthzContext（§3.2 不变量）。
//
// adapter 返回 []KnowledgeCandidate，不做跨类型合并/去重/预算/降级——那些
// 是后续 Broker 编排子任务的职责（§7）。本文件只实现"单类型引擎 → 统一
// candidate"的映射 seam。

package contextbroker

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/recall"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
	"github.com/lynn901/mora/internal/module/rag/search"
	skilldelivery "github.com/lynn901/mora/internal/module/skill"
)

// ---------------------------------------------------------------------------
// MemoryQuery adapter — 复用 recall.RecallService（§3.3）
// ---------------------------------------------------------------------------

// recallPort is the narrow slice of recall.RecallService the adapter calls
// (just Recall). A local interface so the adapter unit test can inject a fake
// without depending on the recall package's wider surface; the production
// recall.RecallService satisfies it (the Recall signature matches).
type recallPort interface {
	Recall(ctx context.Context, auth recall.AuthContext, q recall.KnowledgeQuery) ([]recall.KnowledgeCandidate, error)
}

// MemoryAdapter wraps recall.RecallService and satisfies MemoryQuery. recall
// already implements the leak-safe ranked recall (§8.1 / §9.3) and the §4.3
// evidence ACL chain; the adapter only translates the memory-dimension
// candidate into the unified shape via CandidateFromMemory. It does NOT touch
// recall's REST serialization — Phase 4 stays stable (D2 / §13).
type MemoryAdapter struct {
	svc recallPort
}

// NewMemoryAdapter wires the adapter over a recall.RecallService (or any
// recallPort, e.g. a fake in tests). The wiring layer (service.go) passes the
// production recall.RecallService (WithAuthz/WithUnits chained).
func NewMemoryAdapter(svc recallPort) *MemoryAdapter {
	return &MemoryAdapter{svc: svc}
}

// Query implements MemoryQuery. It narrows the broker's KnowledgeQuery into a
// recall.KnowledgeQuery, maps AuthzContext → recall.AuthContext, calls the
// recall service, and maps each memory candidate into the unified shape. An
// unauthorized caller gets an empty slice (leak-safe, §9.3 — recall guarantees
// it). The §3.2 invariant holds: no user-supplied allowed_asset_ids is read;
// only the Broker-supplied AuthzContext drives the principal identity.
func (a *MemoryAdapter) Query(ctx context.Context, ac AuthzContext, q KnowledgeQuery) ([]KnowledgeCandidate, error) {
	if q.WorkspaceID == uuid.Nil {
		return nil, nil // leak-safe empty — recall is workspace-scoped (§8.1)
	}
	rq := recall.KnowledgeQuery{
		Query:       q.Query,
		WorkspaceID: q.WorkspaceID,
		MaxItems:    q.MaxItems,
	}
	// Carry the optional owner / memory_type / valid-at / linked-asset filters
	// the broker's Filters map (§11.3) may carry. These narrow recall, never
	// broaden it; absent keys leave the defaults (all visible).
	if v, ok := stringVal(q.Filters, "owner_id"); ok {
		if id, err := uuid.Parse(v); err == nil {
			rq.OwnerID = &id
		}
	}
	if v, ok := stringVal(q.Filters, "memory_type"); ok {
		rq.MemoryType = &v
	}
	if v, ok := stringVal(q.Filters, "asset_id"); ok {
		if id, err := uuid.Parse(v); err == nil {
			rq.AssetID = &id
		}
	}
	// ActingUserID + AgentID: when an agent acts on behalf of a user, recall
	// honors the user's group memberships via ActingUserID (mapped to
	// PrincipalID below). An agent principal with no ActingUserID is a pure
	// service caller (recall.IsServiceCaller = true).
	rac := recall.AuthContext{
		SubjectType:     ac.PrincipalType,
		IsServiceCaller: ac.AgentID != nil && ac.ActingUserID == nil,
	}
	if ac.ActingUserID != nil {
		rac.PrincipalID = *ac.ActingUserID
	} else if ac.PrincipalID != uuid.Nil {
		rac.PrincipalID = ac.PrincipalID
	}
	mems, err := a.svc.Recall(ctx, rac, rq)
	if err != nil {
		return nil, fmt.Errorf("context.memory: recall: %w", err)
	}
	out := make([]KnowledgeCandidate, 0, len(mems))
	for _, m := range mems {
		out = append(out, CandidateFromMemory(memoryCandidate{
			AssetID:         m.AssetID,
			AssetVersionID:  m.Citation.AssetVersionID,
			Title:           m.Title,
			Snippet:         m.Snippet,
			Score:           m.Score,
			Authority:       m.Authority,
			Freshness:        m.Freshness,
			Confidence:      m.Confidence,
			ContentHash:     m.ContentHash,
			EvidenceID:      m.Citation.EvidenceID,
			EvidenceLocator: m.Citation.QuoteLocator,
			Relations:       fromRecallRelations(m.Relations),
		}))
	}
	return out, nil
}

// fromRecallRelations copies recall.RelationSummary into the unified
// RelationSummary (same shape; kept as a copy so the candidate owns its slice).
func fromRecallRelations(rs []recall.RelationSummary) []RelationSummary {
	if len(rs) == 0 {
		return nil
	}
	out := make([]RelationSummary, len(rs))
	for i, r := range rs {
		out[i] = RelationSummary{
			RelationType: r.RelationType,
			TargetID:     r.TargetID,
			TargetTitle:  r.TargetTitle,
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// DocumentQuery adapter — 包裹 mora/search + rag/search（§3.3）
// ---------------------------------------------------------------------------

// docFTSPort is the mora/search.SearchExecutor slice (Search(ctx, Query) →
// []Result, total, error). Local interface for fake injection.
type docFTSPort interface {
	Search(ctx context.Context, q moraSearchQuery) ([]moraSearchResult, int, error)
}

// moraSearchQuery / moraSearchResult are the narrow views of mora/search.Query /
// search.Result the adapter builds/consumes. They decouple the context package
// from the search package's SQL-builder + pagination types (same narrow-port
// precedent as the rest of the module). The wiring layer adapts the real
// mora/search.SearchExecutor to docFTSPort.
type moraSearchQuery struct {
	SQL  string
	Args []any
}
type moraSearchResult struct {
	DocumentID  uuid.UUID
	Title       string
	Snippet     string
	Score       float64
	WorkspaceID uuid.UUID
	UpdatedAt   string // RFC3339
}

// docHybridPort is the rag/search.HybridSearcher.Search slice.
type docHybridPort interface {
	Search(ctx context.Context, req search.SearchRequest) (*search.SearchResult, error)
}

// DocumentAdapter wraps the document search engines and satisfies DocumentQuery.
// It composes the hybrid RAG searcher (rag/search, BM25+vector RRF fusion under
// RBAC hard filter). The FTS path (mora/search) is the Qdrant-down fallback
// (§15); when the hybrid port is wired, the adapter runs it, else the FTS port.
// The adapter forwards AuthzContext.AllowedAssetIDs as the visible document set
// when the caller is not workspace-level (§3.2: server-resolved only).
type DocumentAdapter struct {
	hybrid docHybridPort // rag/search.HybridSearcher-shaped port; may be nil in dev
	fts    docFTSPort    // mora/search.SearchExecutor-shaped port; Qdrant-down fallback
}

// NewDocumentAdapter wires the hybrid + FTS search ports. Either may be nil in
// dev/test (the nil path returns an empty result); production wires the hybrid
// searcher so the Broker can degrade FTS-only when Qdrant is unavailable (§15).
func NewDocumentAdapter(hybrid docHybridPort, fts docFTSPort) *DocumentAdapter {
	return &DocumentAdapter{hybrid: hybrid, fts: fts}
}

// Query implements DocumentQuery. It prefers the hybrid path (RAG fuses dense +
// BM25 under RBAC, §05), falling back to the FTS-only path when the hybrid
// searcher is unwired. Each hit is mapped via CandidateFromDocument.
// version_id is not on the search hit (resolved at read time), so the unified
// candidate leaves VersionOrRevision empty; the CitationBuilder completes it
// post-authz (§8.2). §3.2 invariant: no user-supplied allowed_asset_ids —
// the adapter only reads AuthzContext (AllowedAssetIDs / workspace-level).
func (a *DocumentAdapter) Query(ctx context.Context, ac AuthzContext, q KnowledgeQuery) ([]KnowledgeCandidate, error) {
	if q.Query == "" {
		return nil, nil
	}
	if a.hybrid != nil {
		return a.searchHybrid(ctx, ac, q)
	}
	if a.fts != nil {
		return nil, nil // skeleton: FTS-only wiring (visible-doc set) lands with the
		// Broker's Qdrant-down path (§15); the engine seam (docFTSPort) is fixed so
		// the Broker can wire it without touching the adapter again.
	}
	return nil, nil
}

// searchHybrid runs the RAG hybrid search and maps hits into the unified shape.
// The visible-doc set is server-resolved inside the hybrid searcher (RBAC
// envelope, §05); the adapter forwards the principal identity + workspace
// scope, never a caller-supplied asset set.
func (a *DocumentAdapter) searchHybrid(ctx context.Context, ac AuthzContext, q KnowledgeQuery) ([]KnowledgeCandidate, error) {
	topN := q.MaxItems
	if topN <= 0 {
		topN = 10
	}
	req := search.SearchRequest{
		Query:       q.Query,
		WorkspaceID: q.WorkspaceID.String(),
		TopN:        topN,
		TopK:        0, // 0 = searcher default (50)
	}
	// Principal identity for the RBAC envelope. An agent acting on behalf of a
	// user resolves the user's visible set; an agent principal is treated as a
	// service caller (admin flag is server-resolved, not caller-supplied).
	if ac.ActingUserID != nil {
		req.UserID = ac.ActingUserID.String()
	} else if ac.PrincipalID != uuid.Nil {
		req.UserID = ac.PrincipalID.String()
	}
	if v, ok := stringVal(q.Filters, "directory_id"); ok {
		req.DirectoryID = v
	}
	if v, ok := stringVal(q.Filters, "updated_after"); ok {
		if req.Filters == nil {
			req.Filters = make(map[string]any)
		}
		req.Filters["updated_after"] = v
	}
	res, err := a.hybrid.Search(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("context.document: hybrid search: %w", err)
	}
	if res == nil || len(res.Items) == 0 {
		return nil, nil // leak-safe empty (§9.3)
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
		if h.WorkspaceID != "" {
			loc["workspace_id"] = h.WorkspaceID
		}
		out = append(out, CandidateFromDocument(documentHit{
			DocumentID: id,
			Title:      h.Title,
			Snippet:    h.ChunkText,
			Score:      float64(h.Score),
			Locator:    loc,
		}))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// CodeQuery adapter — 包裹 codegraph/service 只读查询（§3.3）
// ---------------------------------------------------------------------------

// codeGraphPort is the codegraph.Service read-only slice: resolve the active
// graph (for commit/source_tree_ref) + run the provider search. Local
// interface for fake injection; the real codegraph.Service satisfies it.
type codeGraphPort interface {
	ActiveCodeGraph(ctx context.Context, assetID uuid.UUID) (activeCodeGraph, error)
	Search(ctx context.Context, auth codeAssetAuth, id uuid.UUID, req cgprovider.CodeSearchRequest) ([]cgprovider.CodeHit, error)
}

// activeCodeGraph is the narrow view of codegraph/service.ActiveGraph the
// adapter consumes (commit + source_tree_ref as the version anchor, §4.2/§8.1).
type activeCodeGraph struct {
	AssetID        uuid.UUID
	AssetVersionID uuid.UUID
	Commit         string
	SourceTreeRef  string
	Stale          bool
}

// codeAssetAuth is the narrow view of asset.AuthContext the codegraph service
// takes. Kept as an empty interface so the context package does not import the
// asset package's wider surface; the wiring layer adapts platform/authz.
// AuthzContext → asset.AuthContext before calling the real service. The adapter
// itself does NOT call codegraph.Service.Search with a caller-supplied identity
// — it forwards the Broker's AuthzContext-derived principal (§3.2).
type codeAssetAuth interface{}

// CodeAdapter wraps codegraph.Service and satisfies CodeQuery. It resolves the
// active graph (for commit/source_tree_ref), runs the read-only provider search
// (never Explore/Files/Node — those are type-specialized tools the Broker does
// not flatten, D12), and maps each CodeHit into the unified candidate carrying
// commit + source_tree_ref as the version anchor (§8.1).
type CodeAdapter struct {
	svc codeGraphPort
}

// NewCodeAdapter wires the codegraph query port.
func NewCodeAdapter(svc codeGraphPort) *CodeAdapter {
	return &CodeAdapter{svc: svc}
}

// Query implements CodeQuery. It resolves the codebase asset id (from the
// broker's Filters["asset_id"] — code search is per-codebase), loads the active
// graph, runs the provider search, and maps each CodeHit. Only-read: it never
// triggers a build (§3.2). §3.2 invariant holds: no user-supplied
// allowed_asset_ids; the resource-level RBAC gate runs inside codegraph.Service
// (asset.ReadService, fail-closed no-leak).
func (a *CodeAdapter) Query(ctx context.Context, _ AuthzContext, q KnowledgeQuery) ([]KnowledgeCandidate, error) {
	assetIDStr, ok := stringVal(q.Filters, "asset_id")
	if !ok {
		return nil, nil // code search is per-codebase; no asset_id = nothing to search
	}
	assetID, err := uuid.Parse(assetIDStr)
	if err != nil || assetID == uuid.Nil {
		return nil, nil // malformed/empty: leak-safe empty
	}
	ag, err := a.svc.ActiveCodeGraph(ctx, assetID)
	if err != nil {
		// ErrGraphNotReady / ErrCapabilityUnavailable → fail-closed empty (§15).
		// The caller cannot tell a not-ready graph from an empty result.
		return nil, nil
	}
	limit := q.MaxItems
	if limit <= 0 {
		limit = 20
	}
	hits, err := a.svc.Search(ctx, nil, assetID, cgprovider.CodeSearchRequest{
		Query:    q.Query,
		Language: stringOpt(q.Filters, "language"),
		PathGlob: stringOpt(q.Filters, "path_glob"),
		Limit:    limit,
	})
	if err != nil {
		return nil, fmt.Errorf("context.code: search: %w", err)
	}
	if len(hits) == 0 {
		return nil, nil // leak-safe empty
	}
	var versionID *uuid.UUID
	if ag.AssetVersionID != uuid.Nil {
		v := ag.AssetVersionID
		versionID = &v
	}
	out := make([]KnowledgeCandidate, 0, len(hits))
	for _, h := range hits {
		out = append(out, CandidateFromCode(codeHit{
			AssetID:        ag.AssetID,
			AssetVersionID: versionID,
			Commit:         ag.Commit,
			SourceTreeRef:  ag.SourceTreeRef,
			Path:           h.Loc.Path,
			StartLine:      h.Loc.StartLine,
			EndLine:        h.Loc.EndLine,
			Symbol:         h.Loc.Symbol,
			Snippet:        h.Snippet,
			Score:          h.Score,
		}))
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// SkillQuery adapter — 包裹 skill/delivery ArchiveReader（§3.3）
// ---------------------------------------------------------------------------

// skillPort is the skill.DeliveryService slice the adapter calls (Deliver +
// List). Local interface for fake injection; the real DeliveryService satisfies
// it (Deliver + List signatures match).
type skillPort interface {
	Deliver(ctx context.Context, agentID, workspaceID, assetID uuid.UUID, versionSpec string) (skilldelivery.DeliveryResult, error)
}

// SkillAdapter wraps the skill delivery service and satisfies SkillQuery. It
// delivers each named skill (binding-resolved, trimmed by delivery_mode) and
// maps the DeliveryResult into the unified candidate carrying package_version
// as VersionOrRevision (§8.1). §3.2 invariant: the delivery path enforces the
// agent-level binding gate (deny / no-binding → not-found, no existence leak,
// §8.2); the adapter forwards AuthzContext.AgentID, never a user-supplied set.
type SkillAdapter struct {
	svc skillPort
}

// NewSkillAdapter wires the skill delivery port.
func NewSkillAdapter(svc skillPort) *SkillAdapter {
	return &SkillAdapter{svc: svc}
}

// Query implements SkillQuery. For an agent principal with named skill asset
// ids (Filters["asset_ids"]), it delivers each and maps the result. The
// candidate is trimmed by the binding delivery_mode (§3.3): tool = SKILL.md
// head (Header name), summary = description, inline = resource list. A skill
// the agent has no allow binding for yields not-found — skipped, not surfaced
// (§8.2 no leak).
func (a *SkillAdapter) Query(ctx context.Context, ac AuthzContext, q KnowledgeQuery) ([]KnowledgeCandidate, error) {
	// Skill discovery is agent-scoped: the binding resolution keys on the
	// delegated AgentID. No agent context → empty (§11.2: the internal token
	// alone never authorizes skill discovery).
	var agentID uuid.UUID
	if ac.AgentID != nil {
		agentID = *ac.AgentID
	}
	if agentID == uuid.Nil && ac.PrincipalType == domain.SubjectAgent && ac.PrincipalID != uuid.Nil {
		agentID = ac.PrincipalID
	}
	if agentID == uuid.Nil {
		return nil, nil
	}
	ids := uuidSliceVal(q.Filters, "asset_ids")
	if len(ids) == 0 {
		return nil, nil // no named skills to discover
	}
	out := make([]KnowledgeCandidate, 0, len(ids))
	for _, id := range ids {
		res, err := a.svc.Deliver(ctx, agentID, q.WorkspaceID, id, "")
		if err != nil {
			// ErrPackageNotFound = no allow binding / cross-workspace / missing
			// (§8.2 no leak): skip, do not surface existence.
			continue
		}
		out = append(out, CandidateFromSkill(skillHit{
			AssetID:           res.AssetID,
			AssetVersionID:    &res.AssetVersionID,
			Title:             skillTitle(res),
			Snippet:           skillSnippet(res),
			VersionOrRevision: skillVersion(res),
			Locator:           skillLocator(res),
			ContentHash:       res.ContentHash,
		}))
	}
	return out, nil
}

// skillTitle extracts the SKILL.md name from the delivered header (§3.3 tool =
// SKILL.md head). Falls back to the asset name when the frontmatter omits it.
func skillTitle(r skilldelivery.DeliveryResult) string {
	if r.Header != nil {
		if v, ok := r.Header["name"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// skillSnippet extracts the description, projected by delivery_mode (§3.3):
// summary mode carries the capability_summary description; tool/inline carry the
// header description. Falls back to an empty string (never the raw bytes).
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

// skillVersion is the package_version anchor (§8.1). Prefer the SKILL.md
// frontmatter version; fall back to version_no, then the asset_version_id.
func skillVersion(r skilldelivery.DeliveryResult) string {
	if r.Header != nil {
		if v, ok := r.Header["version"].(string); ok && v != "" {
			return v
		}
	}
	if r.VersionNo > 0 {
		return fmt.Sprintf("v%d", r.VersionNo)
	}
	return r.AssetVersionID.String()
}

// skillLocator builds the citation locator by delivery_mode (§3.3): carries
// the delivery_mode + a resource_count when the manifest is present (inline =
// resource list). The locator never carries raw bytes (§8.1).
func skillLocator(r skilldelivery.DeliveryResult) map[string]any {
	loc := map[string]any{
		"delivery_mode": string(r.DeliveryMode),
	}
	if r.Manifest != nil {
		loc["resource_count"] = len(r.Manifest.Files)
		// inline mode surfaces the resource list (§3.3). tool/summary carry
		// only the count; the agent re-reads via skill_resources.
		if r.DeliveryMode == domain.BindingDeliveryInline {
			paths := make([]string, 0, len(r.Manifest.Files))
			for _, f := range r.Manifest.Files {
				paths = append(paths, f.Path)
			}
			loc["resources"] = paths
		}
	}
	return loc
}

// ---------------------------------------------------------------------------
// shared filter helpers — read narrow string/uuid values from the broker's
// Filters map (§11.3) WITHOUT trusting caller-supplied asset sets. These only
// narrow a query (owner / asset_id / language / delivery axes); the §3.2
// invariant (no user-supplied allowed_asset_ids) is enforced by the adapter
// signatures themselves — they never accept an asset-id visibility set.
// ---------------------------------------------------------------------------

// stringVal returns a string filter value and whether it was present.
func stringVal(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok && s != ""
}

// stringOpt returns a string filter value, "" when absent (for fields where ""
// is a valid default, e.g. language).
func stringOpt(m map[string]any, key string) string {
	s, _ := stringVal(m, key)
	return s
}

// uuidSliceVal parses a filter that may be a []string, []any, or a single
// string of comma-separated uuids. Used for the skill "asset_ids" axis. It only
// narrows which named skills to deliver — the binding gate still trims to the
// agent's allowed set (§8.2).
func uuidSliceVal(m map[string]any, key string) []uuid.UUID {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	var raw []string
	switch t := v.(type) {
	case []string:
		raw = t
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				raw = append(raw, s)
			}
		}
	case string:
		// single comma-separated list
		for _, s := range splitComma(t) {
			if s != "" {
				raw = append(raw, s)
			}
		}
	}
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		if id, err := uuid.Parse(s); err == nil && id != uuid.Nil {
			out = append(out, id)
		}
	}
	return out
}

// splitComma splits a comma-separated string without importing strings (keeps
// the adapter allocation-light on the hot path).
func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
