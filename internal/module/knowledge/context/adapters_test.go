package contextbroker

// adapters_test.go — 四类型 adapter 字段映射正确性单测（YS-207 DoD）。
//
// 每个 adapter 包裹一个 fake 端口（无需 DB/infra），断言：
//   - Memory: recall.KnowledgeCandidate → 统一 candidate，evidence locator 落到
//     Citation.Locator，evidence_id 落到 VersionOrRevision（§8.1）。
//   - Document: rag/search.SearchHit → 统一 candidate，chunk/section_path 落到
//     Citation.Locator（block_id 风格），version_id 留空由 Builder 补（§8.2）。
//   - Code: codegraph CodeHit → 统一 candidate，commit 落 VersionOrRevision，
//     file:line/symbol 落 Locator（§8.1），source_tree_ref 携带。
//   - Skill: skill DeliveryResult → 统一 candidate，按 delivery_mode 裁剪
//     snippet/locator，package_version 落 VersionOrRevision（§3.3/§8.1）。
//   - §3.2 不变量：adapter 签名不接受用户提交的 allowed_asset_ids（仅 AuthzContext）。

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/recall"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
	"github.com/lynn901/mora/internal/module/rag/search"
	skilldelivery "github.com/lynn901/mora/internal/module/skill"
)

// ---------------------------------------------------------------------------
// fake ports
// ---------------------------------------------------------------------------

type fakeRecall struct {
	gotAuth recall.AuthContext
	gotQ    recall.KnowledgeQuery
	out    []recall.KnowledgeCandidate
	err    error
}

func (f *fakeRecall) Recall(_ context.Context, auth recall.AuthContext, q recall.KnowledgeQuery) ([]recall.KnowledgeCandidate, error) {
	f.gotAuth = auth
	f.gotQ = q
	return f.out, f.err
}

type fakeHybrid struct {
	gotReq search.SearchRequest
	out    *search.SearchResult
	err    error
}

func (f *fakeHybrid) Search(_ context.Context, req search.SearchRequest) (*search.SearchResult, error) {
	f.gotReq = req
	return f.out, f.err
}

type fakeCodeGraph struct {
	graph  activeCodeGraph
	gotID  uuid.UUID
	gotReq cgprovider.CodeSearchRequest
	hits   []cgprovider.CodeHit
	gErr   error
	sErr   error
}

func (f *fakeCodeGraph) ActiveCodeGraph(_ context.Context, id uuid.UUID) (activeCodeGraph, error) {
	f.gotID = id
	return f.graph, f.gErr
}
func (f *fakeCodeGraph) Search(_ context.Context, _ codeAssetAuth, _ uuid.UUID, req cgprovider.CodeSearchRequest) ([]cgprovider.CodeHit, error) {
	f.gotReq = req
	return f.hits, f.sErr
}

type fakeSkillDelivery struct {
	gotAgentID uuid.UUID
	gotAssetID uuid.UUID
	outByAsset map[uuid.UUID]skilldelivery.DeliveryResult
	errByAsset map[uuid.UUID]error
	calls      int
}

func (f *fakeSkillDelivery) Deliver(_ context.Context, agentID, _, assetID uuid.UUID, _ string) (skilldelivery.DeliveryResult, error) {
	f.gotAgentID = agentID
	f.gotAssetID = assetID
	f.calls++
	if e, ok := f.errByAsset[assetID]; ok {
		return skilldelivery.DeliveryResult{}, e
	}
	return f.outByAsset[assetID], nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Memory adapter — evidence locator → Citation.Locator (§8.1)
// ---------------------------------------------------------------------------

func TestMemoryAdapter_MapsEvidenceLocatorAndVersion(t *testing.T) {
	assetID := uuid.New()
	evidenceID := uuid.New()
	versionID := uuid.New()
	conf := 0.8
	quoteLoc := map[string]any{"quote": "ltree beats adjacency", "offset": 42}
	mem := []recall.KnowledgeCandidate{{
		AssetID:    assetID,
		Title:      "目录选型决策",
		Snippet:    "选 ltree 而非 adjacency list",
		Score:      0.9,
		Authority:  0.85,
		Freshness:  0.7,
		Confidence: &conf,
		Citation: recall.Citation{
			AssetID:        assetID,
			AssetVersionID: &versionID,
			EvidenceID:     evidenceID,
			QuoteLocator:   quoteLoc,
		},
		Relations: []recall.RelationSummary{{
			RelationType: "contradicts",
			TargetID:     uuid.New(),
			TargetTitle:  "旧方案 adjacency list",
		}},
	}}
	fake := &fakeRecall{out: mem}
	a := NewMemoryAdapter(fake)

	wsID := uuid.New()
	ac := AuthzContext{WorkspaceID: wsID, PrincipalType: domain.SubjectAgent, PrincipalID: uuid.New()}
	got, err := a.Query(context.Background(), ac, KnowledgeQuery{Query: "为什么选 ltree", WorkspaceID: wsID, MaxItems: 5})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(got))
	}
	c := got[0]
	if c.AssetID != assetID {
		t.Errorf("AssetID = %v, want %v", c.AssetID, assetID)
	}
	if c.AssetType != domain.AssetTypeMemory {
		t.Errorf("AssetType = %v, want %v", c.AssetType, domain.AssetTypeMemory)
	}
	// §8.1: evidence_id is memory's VersionOrRevision.
	if c.VersionOrRevision != evidenceID.String() {
		t.Errorf("VersionOrRevision = %q, want %q", c.VersionOrRevision, evidenceID.String())
	}
	if c.Citation.VersionOrRevision != evidenceID.String() {
		t.Errorf("Citation.VersionOrRevision = %q, want %q", c.Citation.VersionOrRevision, evidenceID.String())
	}
	// §8.1: evidence locator → Citation.Locator (verbatim, not re-resolved).
	if c.Citation.Locator["quote"] != "ltree beats adjacency" {
		t.Errorf("Citation.Locator.quote = %v, want 'ltree beats adjacency'", c.Citation.Locator["quote"])
	}
	if c.Citation.Locator["offset"] != 42 {
		t.Errorf("Citation.Locator.offset = %v, want 42", c.Citation.Locator["offset"])
	}
	// asset_version_id stashed for the CitationBuilder (§8.2), NOT the version anchor.
	if c.Citation.Locator["asset_version_id"] != versionID.String() {
		t.Errorf("Citation.Locator.asset_version_id = %v, want %q", c.Citation.Locator["asset_version_id"], versionID.String())
	}
	// relations (contradicts) pass through so conflicts surface (§7.2).
	if len(c.Relations) != 1 || c.Relations[0].RelationType != "contradicts" {
		t.Errorf("Relations = %+v, want one contradicts", c.Relations)
	}
	if c.Confidence == nil || *c.Confidence != 0.8 {
		t.Errorf("Confidence = %v, want 0.8", c.Confidence)
	}
	// the recall.AuthContext carries the agent principal (§3.2: no user-supplied
	// asset set — only the Broker's AuthzContext drives identity).
	if fake.gotAuth.SubjectType != domain.SubjectAgent {
		t.Errorf("recall.AuthContext.SubjectType = %v, want agent", fake.gotAuth.SubjectType)
	}
	if fake.gotQ.WorkspaceID != wsID {
		t.Errorf("recall.KnowledgeQuery.WorkspaceID = %v, want %v", fake.gotQ.WorkspaceID, wsID)
	}
	if fake.gotQ.MaxItems != 5 {
		t.Errorf("MaxItems = %d, want 5", fake.gotQ.MaxItems)
	}
}

func TestMemoryAdapter_LeakSafeEmptyOnNoWorkspace(t *testing.T) {
	a := NewMemoryAdapter(&fakeRecall{})
	got, err := a.Query(context.Background(), AuthzContext{}, KnowledgeQuery{Query: "x"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != nil {
		t.Errorf("want nil (leak-safe), got %d", len(got))
	}
}

func TestMemoryAdapter_LeakSafeEmptyOnRecallErr(t *testing.T) {
	a := NewMemoryAdapter(&fakeRecall{err: errors.New("boom")})
	got, err := a.Query(context.Background(), AuthzContext{}, KnowledgeQuery{WorkspaceID: uuid.New()})
	if err == nil {
		t.Fatalf("want error propagation, got nil")
	}
	if got != nil {
		t.Errorf("want nil candidates on error, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Document adapter — chunk/section → Citation.Locator, version_id empty (§8.2)
// ---------------------------------------------------------------------------

func TestDocumentAdapter_MapsChunkLocatorAndLeavesVersionEmpty(t *testing.T) {
	docID := uuid.New()
	wsID := uuid.New()
	res := &search.SearchResult{Items: []search.SearchHit{{
		DocumentID:  docID.String(),
		Title:       "目录树存储选型决策",
		ChunkText:   "ltree 支持无限极目录",
		ChunkIndex:  3,
		SectionPath: "architecture/storage",
		Score:       0.91,
		WorkspaceID: wsID.String(),
	}}}
	fake := &fakeHybrid{out: res}
	a := NewDocumentAdapter(fake, nil)

	ac := AuthzContext{WorkspaceID: wsID, PrincipalID: uuid.New()}
	got, err := a.Query(context.Background(), ac, KnowledgeQuery{Query: "ltree", WorkspaceID: wsID, MaxItems: 7})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(got))
	}
	c := got[0]
	if c.AssetID != docID {
		t.Errorf("AssetID = %v, want %v", c.AssetID, docID)
	}
	if c.AssetType != domain.AssetTypeDocument {
		t.Errorf("AssetType = %v, want document", c.AssetType)
	}
	// SearchHit.Score is float32; the adapter widens it to float64, which
	// introduces float32->float64 rounding — compare with a tolerance.
	if diff := c.Score - 0.91; diff > 1e-5 || diff < -1e-5 {
		t.Errorf("Score = %v, want ~0.91", c.Score)
	}
	// §8.2: version_id is not on the hit; candidate leaves it empty for the
	// CitationBuilder to complete post-authz.
	if c.VersionOrRevision != "" {
		t.Errorf("VersionOrRevision = %q, want empty (builder finalizes)", c.VersionOrRevision)
	}
	// §8.1 / §11.3: block/chunk locator → Citation.Locator.
	if c.Citation.Locator["chunk_index"] != 3 {
		t.Errorf("Locator.chunk_index = %v, want 3", c.Citation.Locator["chunk_index"])
	}
	if c.Citation.Locator["section_path"] != "architecture/storage" {
		t.Errorf("Locator.section_path = %v, want 'architecture/storage'", c.Citation.Locator["section_path"])
	}
	// TopN forwarded.
	if fake.gotReq.TopN != 7 {
		t.Errorf("SearchRequest.TopN = %d, want 7", fake.gotReq.TopN)
	}
	if fake.gotReq.WorkspaceID != wsID.String() {
		t.Errorf("WorkspaceID = %q, want %q", fake.gotReq.WorkspaceID, wsID.String())
	}
}

func TestDocumentAdapter_EmptyQueryReturnsNil(t *testing.T) {
	a := NewDocumentAdapter(&fakeHybrid{out: &search.SearchResult{}}, nil)
	got, err := a.Query(context.Background(), AuthzContext{}, KnowledgeQuery{Query: "", WorkspaceID: uuid.New()})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != nil {
		t.Errorf("want nil for empty query, got %d", len(got))
	}
}

func TestDocumentAdapter_SkipsMalformedDocID(t *testing.T) {
	res := &search.SearchResult{Items: []search.SearchHit{
		{DocumentID: "not-a-uuid", Title: "bad", ChunkText: "x", Score: 0.5},
		{DocumentID: uuid.New().String(), Title: "good", ChunkText: "y", Score: 0.6},
	}}
	a := NewDocumentAdapter(&fakeHybrid{out: res}, nil)
	got, err := a.Query(context.Background(), AuthzContext{}, KnowledgeQuery{Query: "q", WorkspaceID: uuid.New()})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 (malformed skipped), got %d", len(got))
	}
	if got[0].Title != "good" {
		t.Errorf("Title = %q, want 'good'", got[0].Title)
	}
}

func TestDocumentAdapter_LeakSafeEmptyOnSearchErr(t *testing.T) {
	a := NewDocumentAdapter(&fakeHybrid{err: errors.New("boom")}, nil)
	got, err := a.Query(context.Background(), AuthzContext{}, KnowledgeQuery{Query: "q", WorkspaceID: uuid.New()})
	if err == nil {
		t.Fatalf("want error propagation, got nil")
	}
	if got != nil {
		t.Errorf("want nil on error, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Code adapter — commit → VersionOrRevision, file:line → Locator (§8.1)
// ---------------------------------------------------------------------------

func TestCodeAdapter_MapsCommitAndFileLine(t *testing.T) {
	assetID := uuid.New()
	versionID := uuid.New()
	commit := "abc1234"
	graph := activeCodeGraph{
		AssetID:        assetID,
		AssetVersionID: versionID,
		Commit:         commit,
		SourceTreeRef:  "refs/heads/main",
	}
	hits := []cgprovider.CodeHit{{
		Loc: cgprovider.CodeLoc{
			Commit:    commit,
			Path:      "internal/tree/ltree.go",
			StartLine: 42,
			EndLine:   60,
			Symbol:    "BuildPath",
		},
		Snippet: "func BuildPath(...) string",
		Score:   0.88,
	}}
	fake := &fakeCodeGraph{graph: graph, hits: hits}
	a := NewCodeAdapter(fake)

	wsID := uuid.New()
	q := KnowledgeQuery{
		Query:      "BuildPath",
		WorkspaceID: wsID,
		MaxItems:   3,
		Filters:    map[string]any{"asset_id": assetID.String(), "language": "go"},
	}
	got, err := a.Query(context.Background(), AuthzContext{}, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(got))
	}
	c := got[0]
	if c.AssetID != assetID {
		t.Errorf("AssetID = %v, want %v", c.AssetID, assetID)
	}
	if c.AssetType != domain.AssetTypeCodebase {
		t.Errorf("AssetType = %v, want codebase", c.AssetType)
	}
	// §8.1: commit is codebase's VersionOrRevision.
	if c.VersionOrRevision != commit {
		t.Errorf("VersionOrRevision = %q, want %q", c.VersionOrRevision, commit)
	}
	if c.Citation.VersionOrRevision != commit {
		t.Errorf("Citation.VersionOrRevision = %q, want %q", c.Citation.VersionOrRevision, commit)
	}
	// §8.1: file:line locator.
	if c.Citation.Locator["path"] != "internal/tree/ltree.go" {
		t.Errorf("Locator.path = %v, want 'internal/tree/ltree.go'", c.Citation.Locator["path"])
	}
	if c.Citation.Locator["start_line"] != 42 {
		t.Errorf("Locator.start_line = %v, want 42", c.Citation.Locator["start_line"])
	}
	if c.Citation.Locator["end_line"] != 60 {
		t.Errorf("Locator.end_line = %v, want 60", c.Citation.Locator["end_line"])
	}
	if c.Citation.Locator["symbol"] != "BuildPath" {
		t.Errorf("Locator.symbol = %v, want 'BuildPath'", c.Citation.Locator["symbol"])
	}
	if c.Citation.Locator["source_tree_ref"] != "refs/heads/main" {
		t.Errorf("Locator.source_tree_ref = %v, want 'refs/heads/main'", c.Citation.Locator["source_tree_ref"])
	}
	// asset_version_id stashed for the Builder (§8.2).
	if c.Citation.Locator["asset_version_id"] != versionID.String() {
		t.Errorf("Locator.asset_version_id = %v, want %q", c.Citation.Locator["asset_version_id"], versionID.String())
	}
	// title = symbol @ path (§8.1 code title convention).
	if c.Title != "BuildPath @ internal/tree/ltree.go" {
		t.Errorf("Title = %q, want 'BuildPath @ internal/tree/ltree.go'", c.Title)
	}
	// the search request carries language + limit.
	if fake.gotReq.Language != "go" {
		t.Errorf("CodeSearchRequest.Language = %q, want 'go'", fake.gotReq.Language)
	}
	if fake.gotReq.Limit != 3 {
		t.Errorf("CodeSearchRequest.Limit = %d, want 3", fake.gotReq.Limit)
	}
}

func TestCodeAdapter_NoAssetIDReturnsEmpty(t *testing.T) {
	a := NewCodeAdapter(&fakeCodeGraph{})
	got, err := a.Query(context.Background(), AuthzContext{}, KnowledgeQuery{Query: "x", WorkspaceID: uuid.New()})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != nil {
		t.Errorf("want nil (no asset_id), got %d", len(got))
	}
}

func TestCodeAdapter_GraphNotReadyReturnsEmpty(t *testing.T) {
	a := NewCodeAdapter(&fakeCodeGraph{gErr: errors.New("graph not ready")})
	q := KnowledgeQuery{Query: "x", WorkspaceID: uuid.New(), Filters: map[string]any{"asset_id": uuid.New().String()}}
	got, err := a.Query(context.Background(), AuthzContext{}, q)
	if err != nil {
		t.Fatalf("Query: %v (want nil, fail-closed empty)", err)
	}
	if got != nil {
		t.Errorf("want nil (graph not ready → leak-safe empty), got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// Skill adapter — delivery_mode trims snippet/locator, package_version (§3.3)
// ---------------------------------------------------------------------------

func TestSkillAdapter_ToolMode(t *testing.T) {
	agentID := uuid.New()
	wsID := uuid.New()
	assetID := uuid.New()
	versionID := uuid.New()
	tool := skilldelivery.DeliveryResult{
		AssetID:        assetID,
		AssetVersionID: versionID,
		VersionNo:      3,
		DeliveryMode:   domain.BindingDeliveryTool,
		Header: map[string]any{
			"name":        "tree-builder",
			"description": "builds ltree paths",
			"version":     "1.2.0",
		},
		Manifest: &domain.SkillManifest{Files: []domain.SkillFileEntry{
			{Path: "SKILL.md", Kind: "skill_md"},
			{Path: "build.sh", Kind: "script"},
		}},
		ContentHash: "abc123",
	}
	fake := &fakeSkillDelivery{outByAsset: map[uuid.UUID]skilldelivery.DeliveryResult{assetID: tool}}
	a := NewSkillAdapter(fake)

	q := KnowledgeQuery{
		Query:      "build tree",
		WorkspaceID: wsID,
		Filters:    map[string]any{"asset_ids": []string{assetID.String()}},
	}
	ac := AuthzContext{WorkspaceID: wsID, PrincipalType: domain.SubjectAgent, PrincipalID: agentID}
	got, err := a.Query(context.Background(), ac, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 candidate, got %d", len(got))
	}
	c := got[0]
	if c.AssetID != assetID {
		t.Errorf("AssetID = %v, want %v", c.AssetID, assetID)
	}
	if c.AssetType != domain.AssetTypeSkill {
		t.Errorf("AssetType = %v, want skill", c.AssetType)
	}
	if c.Title != "tree-builder" {
		t.Errorf("Title = %q, want 'tree-builder'", c.Title)
	}
	// tool mode: description from header.
	if c.Snippet != "builds ltree paths" {
		t.Errorf("Snippet = %q, want 'builds ltree paths'", c.Snippet)
	}
	// §8.1: package_version anchor — prefer SKILL.md frontmatter version.
	if c.VersionOrRevision != "1.2.0" {
		t.Errorf("VersionOrRevision = %q, want '1.2.0'", c.VersionOrRevision)
	}
	if c.ContentHash != "abc123" {
		t.Errorf("ContentHash = %q, want 'abc123'", c.ContentHash)
	}
	// tool mode: locator carries delivery_mode + resource_count, NO resource
	// list (§3.3: tool = SKILL.md head, the agent re-reads via skill_resources).
	if c.Citation.Locator["delivery_mode"] != "tool" {
		t.Errorf("Locator.delivery_mode = %v, want 'tool'", c.Citation.Locator["delivery_mode"])
	}
	if c.Citation.Locator["resource_count"] != 2 {
		t.Errorf("Locator.resource_count = %v, want 2", c.Citation.Locator["resource_count"])
	}
	if _, hasList := c.Citation.Locator["resources"]; hasList {
		t.Errorf("Locator.resources present in tool mode, want absent (inline-only)")
	}
	// agent identity forwarded (§3.2).
	if fake.gotAgentID != agentID {
		t.Errorf("Deliver agentID = %v, want %v", fake.gotAgentID, agentID)
	}
}

func TestSkillAdapter_InlineModeSurfacesResourceList(t *testing.T) {
	agentID := uuid.New()
	wsID := uuid.New()
	assetID := uuid.New()
	inline := skilldelivery.DeliveryResult{
		AssetID:        assetID,
		AssetVersionID: uuid.New(),
		DeliveryMode:   domain.BindingDeliveryInline,
		Header:         map[string]any{"name": "inline-skill"},
		Manifest: &domain.SkillManifest{Files: []domain.SkillFileEntry{
			{Path: "SKILL.md", Kind: "skill_md"},
			{Path: "data.json", Kind: "asset"},
		}},
	}
	a := NewSkillAdapter(&fakeSkillDelivery{outByAsset: map[uuid.UUID]skilldelivery.DeliveryResult{assetID: inline}})
	q := KnowledgeQuery{WorkspaceID: wsID, Filters: map[string]any{"asset_ids": []string{assetID.String()}}}
	ac := AuthzContext{WorkspaceID: wsID, PrincipalType: domain.SubjectAgent, PrincipalID: agentID}
	got, err := a.Query(context.Background(), ac, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	c := got[0]
	if c.Citation.Locator["delivery_mode"] != "inline" {
		t.Errorf("delivery_mode = %v, want 'inline'", c.Citation.Locator["delivery_mode"])
	}
	// §3.3: inline = resource list.
	resources, ok := c.Citation.Locator["resources"].([]string)
	if !ok {
		t.Fatalf("Locator.resources = %T, want []string", c.Citation.Locator["resources"])
	}
	if len(resources) != 2 || resources[0] != "SKILL.md" || resources[1] != "data.json" {
		t.Errorf("resources = %v, want [SKILL.md data.json]", resources)
	}
}

func TestSkillAdapter_SummaryModeNoManifest(t *testing.T) {
	agentID := uuid.New()
	wsID := uuid.New()
	assetID := uuid.New()
	summary := skilldelivery.DeliveryResult{
		AssetID:        assetID,
		AssetVersionID: uuid.New(),
		VersionNo:      2,
		DeliveryMode:   domain.BindingDeliverySummary,
		Header:         map[string]any{"name": "summary-skill", "description": "from header"},
		CapabilitySummary: map[string]any{"description": "from capability summary"},
	}
	a := NewSkillAdapter(&fakeSkillDelivery{outByAsset: map[uuid.UUID]skilldelivery.DeliveryResult{assetID: summary}})
	q := KnowledgeQuery{WorkspaceID: wsID, Filters: map[string]any{"asset_ids": []string{assetID.String()}}}
	ac := AuthzContext{WorkspaceID: wsID, PrincipalType: domain.SubjectAgent, PrincipalID: agentID}
	got, err := a.Query(context.Background(), ac, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	c := got[0]
	// summary mode: snippet from capability_summary (§3.3).
	if c.Snippet != "from capability summary" {
		t.Errorf("Snippet = %q, want 'from capability summary'", c.Snippet)
	}
	// summary mode: no manifest, so resource_count absent (no leak of file list).
	if _, has := c.Citation.Locator["resource_count"]; has {
		t.Errorf("resource_count present in summary mode, want absent")
	}
	if c.Citation.Locator["delivery_mode"] != "summary" {
		t.Errorf("delivery_mode = %v, want 'summary'", c.Citation.Locator["delivery_mode"])
	}
	// no frontmatter version → fall back to version_no (§8.1 package_version).
	if c.VersionOrRevision != "v2" {
		t.Errorf("VersionOrRevision = %q, want 'v2'", c.VersionOrRevision)
	}
}

func TestSkillAdapter_DenyBindingSkippedNoLeak(t *testing.T) {
	agentID := uuid.New()
	wsID := uuid.New()
	allowed := uuid.New()
	denied := uuid.New()
	allowRes := skilldelivery.DeliveryResult{AssetID: allowed, AssetVersionID: uuid.New(), DeliveryMode: domain.BindingDeliveryTool, Header: map[string]any{"name": "allowed"}}
	fake := &fakeSkillDelivery{
		outByAsset: map[uuid.UUID]skilldelivery.DeliveryResult{allowed: allowRes},
		errByAsset: map[uuid.UUID]error{denied: skilldelivery.ErrPackageNotFound},
	}
	a := NewSkillAdapter(fake)
	q := KnowledgeQuery{WorkspaceID: wsID, Filters: map[string]any{"asset_ids": []string{allowed.String(), denied.String()}}}
	ac := AuthzContext{WorkspaceID: wsID, PrincipalType: domain.SubjectAgent, PrincipalID: agentID}
	got, err := a.Query(context.Background(), ac, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// §8.2 no-leak: denied skill absent, not surfaced; allowed skill present.
	if len(got) != 1 {
		t.Fatalf("want 1 (denied skipped), got %d", len(got))
	}
	if got[0].AssetID != allowed {
		t.Errorf("AssetID = %v, want %v", got[0].AssetID, allowed)
	}
}

func TestSkillAdapter_NoAgentReturnsEmpty(t *testing.T) {
	a := NewSkillAdapter(&fakeSkillDelivery{})
	q := KnowledgeQuery{WorkspaceID: uuid.New(), Filters: map[string]any{"asset_ids": []string{uuid.New().String()}}}
	// no AgentID + non-agent principal → empty (§11.2).
	got, err := a.Query(context.Background(), AuthzContext{}, q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got != nil {
		t.Errorf("want nil (no agent context), got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// §3.2 invariant: adapter signatures never accept user-submitted allowed_asset_ids
// ---------------------------------------------------------------------------

// TestAdapters_AcceptOnlyAuthzContext is a compile-time check that every
// adapter satisfies its port with the (ctx, AuthzContext, KnowledgeQuery)
// signature — there is no parameter for a caller-supplied asset-visibility set.
// The §3.2 invariant is enforced structurally: AllowedAssetIDs live inside the
// server-constructed AuthzContext, never a separate adapter argument.
func TestAdapters_AcceptOnlyAuthzContext(t *testing.T) {
	var (
		_ DocumentQuery = (*DocumentAdapter)(nil)
		_ CodeQuery     = (*CodeAdapter)(nil)
		_ MemoryQuery   = (*MemoryAdapter)(nil)
		_ SkillQuery    = (*SkillAdapter)(nil)
	)
	// If these compile, the port signatures carry only AuthzContext — a
	// caller cannot pass a user-supplied allowed_asset_ids to any adapter.
}
