package contextbroker

// candidate_test.go verifies the four type adapters' candidate conversion
// correctness (design-docs/19 §3.3, YS-202 验收门禁 "四个 adapter 单测覆盖
// candidate 转换正确性（用 fake 端口）"). Each test feeds a representative
// engine return value through the adapter and asserts the unified
// KnowledgeCandidate shape is mapped field-by-field, including the
// type-specific version anchors (memory=evidence_id, code=commit,
// skill=package_version, document=version_id) and locators.
//
// The Memory adapter test exercises the real recall.KnowledgeCandidate →
// unified shape path (CandidateFromMemory) — the D2 guarantee that Phase 4's
// recall candidate maps without mutation.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/recall"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
	cgservice "github.com/lynn901/mora/internal/module/knowledge/codegraph/service"
	skilldelivery "github.com/lynn901/mora/internal/module/skill"
)

// ---------------------------------------------------------------------------
// Memory adapter — recall.KnowledgeCandidate → unified shape (D2)
// ---------------------------------------------------------------------------

func TestCandidateFromMemory(t *testing.T) {
	assetID := uuid.New()
	versionID := uuid.New()
	evidenceID := uuid.New()
	conf := 0.8
	m := memoryCandidate{
		AssetID:         assetID,
		AssetVersionID:  &versionID,
		Title:           "ltree 选型原因",
		Snippet:         "采用 ltree 支持无限级目录",
		Score:           0.9,
		Authority:       0.7,
		Freshness:       0.6,
		Confidence:      &conf,
		ContentHash:      "abc123",
		EvidenceID:      evidenceID,
		EvidenceLocator: map[string]any{"quote": "..."},
		Relations: []recallRelation{
			{RelationType: "contradicts", TargetID: uuid.New(), TargetTitle: "adjacency list"},
		},
	}
	c := CandidateFromMemory(m)

	if c.AssetID != assetID {
		t.Errorf("AssetID = %v, want %v", c.AssetID, assetID)
	}
	if c.AssetType != domain.AssetTypeMemory {
		t.Errorf("AssetType = %q, want %q", c.AssetType, domain.AssetTypeMemory)
	}
	if c.Title != m.Title || c.Snippet != m.Snippet {
		t.Errorf("Title/Snippet not mapped: %+v", c)
	}
	if c.Score != m.Score || c.Authority != m.Authority || c.Freshness != m.Freshness {
		t.Errorf("scores not mapped: got score=%v auth=%v fresh=%v", c.Score, c.Authority, c.Freshness)
	}
	if c.Confidence == nil || *c.Confidence != conf {
		t.Errorf("Confidence = %v, want %v", c.Confidence, conf)
	}
	if c.ContentHash != m.ContentHash {
		t.Errorf("ContentHash = %q, want %q", c.ContentHash, m.ContentHash)
	}
	// version anchor: memory = evidence_id (§8.1).
	if c.Citation.VersionOrRevision != evidenceID.String() {
		t.Errorf("Citation.VersionOrRevision = %q, want evidence id %q", c.Citation.VersionOrRevision, evidenceID)
	}
	if c.AssetVersion == nil || *c.AssetVersion != versionID {
		t.Errorf("AssetVersion not mapped: %v", c.AssetVersion)
	}
	// evidence locator preserved (§8.1).
	if c.Citation.Locator["quote"] != "..." {
		t.Errorf("EvidenceLocator not preserved: %v", c.Citation.Locator)
	}
	// relations preserved (contradicts surfaces, §7.2).
	if len(c.Relations) != 1 || c.Relations[0].RelationType != "contradicts" {
		t.Errorf("Relations not mapped: %+v", c.Relations)
	}
	// citation identity denormalized (§9.3).
	if c.Citation.AssetID != assetID || c.Citation.AssetType != domain.AssetTypeMemory {
		t.Errorf("Citation identity not denormalized: %+v", c.Citation)
	}
}

func TestCandidateFromMemory_NoEvidence(t *testing.T) {
	// A unit with no surviving evidence link (evidence_missing): the citation
	// still names the unit, just without an evidence_id (§8.4).
	m := memoryCandidate{AssetID: uuid.New(), EvidenceID: uuid.Nil}
	c := CandidateFromMemory(m)
	if c.Citation.VersionOrRevision != "" {
		t.Errorf("VersionOrRevision = %q, want empty when no evidence", c.Citation.VersionOrRevision)
	}
}

// fakeRecallService is a recallAdapterPort fake returning canned candidates.
type fakeRecallService struct {
	out []recall.KnowledgeCandidate
	err error
}

func (f *fakeRecallService) Recall(ctx context.Context, auth recall.AuthContext, q recall.KnowledgeQuery) ([]recall.KnowledgeCandidate, error) {
	return f.out, f.err
}

func TestMemoryAdapter_Recall(t *testing.T) {
	assetID := uuid.New()
	evidenceID := uuid.New()
	conf := 0.9
	fake := &fakeRecallService{
		out: []recall.KnowledgeCandidate{
			{
				AssetID:    assetID,
				AssetType:  "memory",
				Title:      "决策",
				Snippet:    "采用 Valkey Streams",
				Score:      0.91,
				Authority:  0.9,
				Freshness:  0.8,
				Confidence: &conf,
				Citation: recall.Citation{
					AssetID:      assetID,
					EvidenceID:   evidenceID,
					QuoteLocator: map[string]any{"quote": "x"},
				},
				Relations: []recall.RelationSummary{
					{RelationType: "contradicts", TargetID: uuid.New(), TargetTitle: "旧方案"},
				},
			},
		},
	}
	a := NewMemoryAdapter(fake)
	out, err := a.Recall(context.Background(), AuthzContext{PrincipalID: uuid.New()}, MemoryQueryRequest{WorkspaceID: uuid.New()})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d candidates, want 1", len(out))
	}
	c := out[0]
	if c.AssetID != assetID {
		t.Errorf("AssetID = %v, want %v", c.AssetID, assetID)
	}
	if c.AssetType != domain.AssetTypeMemory {
		t.Errorf("AssetType = %q, want memory", c.AssetType)
	}
	if c.Citation.VersionOrRevision != evidenceID.String() {
		t.Errorf("version anchor = %q, want %q", c.Citation.VersionOrRevision, evidenceID)
	}
	if len(c.Relations) != 1 || c.Relations[0].RelationType != "contradicts" {
		t.Errorf("contradicts not surfaced: %+v", c.Relations)
	}
}

// ---------------------------------------------------------------------------
// Document adapter — rag/search.SearchHit → unified shape
// ---------------------------------------------------------------------------

func TestCandidateFromDocument(t *testing.T) {
	docID := uuid.New()
	versionID := uuid.New()
	d := documentHit{
		DocumentID: docID,
		VersionID:  &versionID,
		Title:      "目录树存储选型决策",
		Snippet:    "ltree 支持...",
		Score:      0.91,
		Authority:  0.9,
		Freshness:  0.8,
		Locator:    map[string]any{"block_id": 12},
	}
	c := CandidateFromDocument(d)
	if c.AssetID != docID || c.AssetType != domain.AssetTypeDocument {
		t.Errorf("identity: %+v", c)
	}
	if c.Citation.VersionOrRevision != versionID.String() {
		t.Errorf("version anchor = %q, want %q", c.Citation.VersionOrRevision, versionID)
	}
	if c.Citation.Locator["block_id"] != 12 {
		t.Errorf("block_id locator not preserved: %v", c.Citation.Locator)
	}
}

func TestDocumentAdapter_Search(t *testing.T) {
	docID := uuid.New()
	fake := &fakeHybridSearch{
		result: &docHybridResult{
			Items: []docHybridHit{
				{DocumentID: docID.String(), Title: "doc", ChunkText: "snippet", ChunkIndex: 3, SectionPath: "§2", Score: 0.7},
			},
		},
	}
	a := NewDocumentAdapter(nil, fake)
	out, err := a.Search(context.Background(), AuthzContext{}, DocumentQueryRequest{WorkspaceID: uuid.New()})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(out) != 1 || out[0].AssetID != docID {
		t.Fatalf("document not mapped: %+v", out)
	}
	if out[0].Citation.Locator["chunk_index"] != 3 || out[0].Citation.Locator["section_path"] != "§2" {
		t.Errorf("locator not preserved: %v", out[0].Citation.Locator)
	}
}

type fakeHybridSearch struct {
	result *docHybridResult
	err    error
}

func (f *fakeHybridSearch) Search(ctx context.Context, req docHybridReq) (*docHybridResult, error) {
	return f.result, f.err
}

func TestDocumentAdapter_Search_MalformedIDSkipped(t *testing.T) {
	// A malformed document_id is skipped, not surfaced (§8.2 no leak).
	fake := &fakeHybridSearch{
		result: &docHybridResult{
			Items: []docHybridHit{
				{DocumentID: "not-a-uuid", Title: "bad"},
				{DocumentID: uuid.New().String(), Title: "good"},
			},
		},
	}
	a := NewDocumentAdapter(nil, fake)
	out, _ := a.Search(context.Background(), AuthzContext{}, DocumentQueryRequest{WorkspaceID: uuid.New()})
	if len(out) != 1 {
		t.Fatalf("got %d, want 1 (malformed skipped)", len(out))
	}
	if out[0].Title != "good" {
		t.Errorf("expected 'good' survived, got %q", out[0].Title)
	}
}

// ---------------------------------------------------------------------------
// Code adapter — codegraph.CodeHit → unified shape
// ---------------------------------------------------------------------------

func TestCandidateFromCode(t *testing.T) {
	assetID := uuid.New()
	ch := codeHit{
		AssetID:       assetID,
		Commit:        "abc1234",
		SourceTreeRef: "refs/heads/main",
		Path:          "internal/module/mora/search/builder.go",
		StartLine:     55,
		EndLine:       60,
		Symbol:        "Build",
		Snippet:        "func (f Filter) Build() Query",
		Score:         0.5,
	}
	c := CandidateFromCode(ch)
	if c.AssetID != assetID || c.AssetType != domain.AssetTypeCodebase {
		t.Errorf("identity: %+v", c)
	}
	// version anchor: codebase = commit (§8.1).
	if c.Citation.VersionOrRevision != "abc1234" {
		t.Errorf("version anchor = %q, want commit abc1234", c.Citation.VersionOrRevision)
	}
	if c.Citation.SourceRef != "refs/heads/main" {
		t.Errorf("SourceRef = %q, want refs/heads/main", c.Citation.SourceRef)
	}
	if c.Citation.Locator["file"] != ch.Path || c.Citation.Locator["start_line"] != 55 {
		t.Errorf("file:line locator not preserved: %v", c.Citation.Locator)
	}
	if c.Citation.Locator["end_line"] != 60 {
		t.Errorf("end_line not preserved: %v", c.Citation.Locator)
	}
	if c.Citation.Locator["symbol"] != "Build" {
		t.Errorf("symbol not preserved: %v", c.Citation.Locator)
	}
	// title falls back to symbol (codeTitle).
	if c.Title != "Build" {
		t.Errorf("Title = %q, want Build", c.Title)
	}
}

func TestCandidateFromCode_NoSymbol_TitleIsPath(t *testing.T) {
	ch := codeHit{AssetID: uuid.New(), Commit: "c", Path: "a/b.go", StartLine: 1}
	c := CandidateFromCode(ch)
	if c.Title != "a/b.go" {
		t.Errorf("Title = %q, want path when no symbol", c.Title)
	}
}

type fakeCodeService struct {
	graph  cgservice.ActiveGraph
	hits   []cgprovider.CodeHit
	err    error
}

func (f *fakeCodeService) Search(ctx context.Context, _ codeAssetAuthContext, _ uuid.UUID, _ cgprovider.CodeSearchRequest) ([]cgprovider.CodeHit, error) {
	return f.hits, f.err
}
func (f *fakeCodeService) ActiveCodeGraph(ctx context.Context, _ uuid.UUID) (cgservice.ActiveGraph, error) {
	return f.graph, nil
}

func TestCodeAdapter_Search(t *testing.T) {
	assetID := uuid.New()
	fake := &fakeCodeService{
		graph: cgservice.ActiveGraph{AssetID: assetID, Commit: "deadbeef", SourceTreeRef: "refs/heads/main"},
		hits: []cgprovider.CodeHit{
			{Loc: cgprovider.CodeLoc{Path: "main.go", StartLine: 10, EndLine: 20, Symbol: "main"}, Score: 0.6, Snippet: "func main()"},
		},
	}
	a := NewCodeAdapter(fake)
	out, err := a.Search(context.Background(), AuthzContext{}, CodeQueryRequest{AssetID: assetID})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d, want 1", len(out))
	}
	if out[0].Citation.VersionOrRevision != "deadbeef" {
		t.Errorf("commit anchor = %q, want deadbeef", out[0].Citation.VersionOrRevision)
	}
	if out[0].Citation.Locator["start_line"] != 10 {
		t.Errorf("start_line locator: %v", out[0].Citation.Locator)
	}
}

// ---------------------------------------------------------------------------
// Skill adapter — skill.DeliveryResult → unified shape
// ---------------------------------------------------------------------------

func TestCandidateFromSkill(t *testing.T) {
	assetID := uuid.New()
	versionID := uuid.New()
	s := skillHit{
		AssetID:          assetID,
		AssetVersionID:   &versionID,
		Title:            "code-review",
		Snippet:          "Review the current diff for correctness bugs",
		VersionOrRevision: "v3",
		Locator:          map[string]any{"delivery_mode": "tool"},
	}
	c := CandidateFromSkill(s)
	if c.AssetID != assetID || c.AssetType != domain.AssetTypeSkill {
		t.Errorf("identity: %+v", c)
	}
	if c.Citation.VersionOrRevision != "v3" {
		t.Errorf("version anchor = %q, want v3", c.Citation.VersionOrRevision)
	}
}

type fakeSkillDelivery struct {
	results map[uuid.UUID]skilldelivery.DeliveryResult
	err     error
}

func (f *fakeSkillDelivery) Deliver(ctx context.Context, agentID, workspaceID, assetID uuid.UUID, versionSpec string) (skilldelivery.DeliveryResult, error) {
	if f.err != nil {
		return skilldelivery.DeliveryResult{}, f.err
	}
	if r, ok := f.results[assetID]; ok {
		return r, nil
	}
	return skilldelivery.DeliveryResult{}, skilldelivery.ErrPackageNotFound
}

func TestSkillAdapter_Discover(t *testing.T) {
	agentID := uuid.New()
	wsID := uuid.New()
	assetID := uuid.New()
	versionID := uuid.New()
	fake := &fakeSkillDelivery{
		results: map[uuid.UUID]skilldelivery.DeliveryResult{
			assetID: {
				AssetID:        assetID,
				AssetVersionID: versionID,
				DeliveryMode:   domain.BindingDeliveryTool,
				Header:         map[string]any{"name": "code-review", "description": "Review the diff"},
				VersionNo:      3,
				Manifest:       &domain.SkillManifest{Files: make([]domain.SkillFileEntry, 5)},
			},
		},
	}
	a := NewSkillAdapter(fake)
	out, err := a.Discover(context.Background(), AuthzContext{}, SkillQueryRequest{AgentID: agentID, WorkspaceID: wsID, AssetIDs: []uuid.UUID{assetID}})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d, want 1", len(out))
	}
	if out[0].Title != "code-review" {
		t.Errorf("Title = %q, want code-review", out[0].Title)
	}
	if out[0].Citation.VersionOrRevision != "v3" {
		t.Errorf("version = %q, want v3", out[0].Citation.VersionOrRevision)
	}
	if out[0].Citation.Locator["resource_count"] != 5 {
		t.Errorf("resource_count locator: %v", out[0].Citation.Locator)
	}
}

func TestSkillAdapter_Discover_NoAllowBindingSkipped(t *testing.T) {
	// A skill with no allow binding → ErrPackageNotFound → skipped, not surfaced
	// (§8.2 no existence leak).
	fake := &fakeSkillDelivery{results: map[uuid.UUID]skilldelivery.DeliveryResult{}}
	a := NewSkillAdapter(fake)
	out, err := a.Discover(context.Background(), AuthzContext{}, SkillQueryRequest{
		AgentID:    uuid.New(),
		WorkspaceID: uuid.New(),
		AssetIDs:   []uuid.UUID{uuid.New()},
	})
	if err != nil {
		t.Fatalf("Discover: %v (err expected nil — skipped, not errored)", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d candidates, want 0 (no binding → skipped)", len(out))
	}
}

func TestSkillAdapter_Discover_RequiresAgentID(t *testing.T) {
	a := NewSkillAdapter(&fakeSkillDelivery{})
	_, err := a.Discover(context.Background(), AuthzContext{}, SkillQueryRequest{WorkspaceID: uuid.New()})
	if err == nil {
		t.Fatal("expected error when agent_id missing")
	}
}
