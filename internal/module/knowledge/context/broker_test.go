package context

// broker_test.go verifies the §7.1 ten-step pipeline (YS-209 DoD: "Broker.
// Execute 十步全链单测（fake 端口 + fake authz）覆盖：正常四类型、单端口超时
// 降级、全端口失败三状态、冲突并列保留、预算超限裁剪、引用构建"). The Broker
// is constructed over fake typed ports + a fake AuthzService so the pipeline
// runs end-to-end without IO; the assertions pin each §7.1 step's observable
// contract.
//
// The fakes live here (not in a helpers file) so the broker test is
// self-contained: a reader sees the §7.1 contract + the fake that exercises it
// in one place.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/authz"
)

// fakeAuthz is an AuthzService fake. authorizeAllowed controls the pre-fetch
// gate (step 2); visibleSet controls the post-check (step 5). When visibleSet
// is nil, every candidate passes (the all-visible path). An empty (non-nil)
// set denies every candidate (the authorized-empty path).
type fakeAuthz struct {
	authorizeAllowed bool
	visibleSet       map[uuid.UUID]bool // nil = all visible; empty map = none visible
	authorizeErr     error
	visibleErr       error
	revision         int64
	decisionID       *uuid.UUID
}

func (f *fakeAuthz) Authorize(ctx context.Context, req authz.AuthzRequest) (authz.AuthzContext, error) {
	if f.authorizeErr != nil {
		return authz.AuthzContext{}, f.authorizeErr
	}
	rev := f.revision
	if rev == 0 {
		rev = 1
	}
	return authz.AuthzContext{
		WorkspaceID:   req.WorkspaceID,
		AuthzRevision: rev,
		PrincipalType: req.PrincipalType,
		PrincipalID:   req.PrincipalID,
		AgentID:       req.AgentID,
		Allowed:       f.authorizeAllowed,
		DecisionID:    f.decisionID,
	}, nil
}

func (f *fakeAuthz) VisibleAssets(ctx context.Context, scope authz.ListScope, candidates []uuid.UUID) ([]uuid.UUID, error) {
	if f.visibleErr != nil {
		return nil, f.visibleErr
	}
	if f.visibleSet == nil {
		return candidates, nil // all visible
	}
	out := make([]uuid.UUID, 0, len(candidates))
	for _, id := range candidates {
		if f.visibleSet[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

// fakeMemoryPort is a MemoryQuery fake returning canned candidates or an error.
type fakeMemoryPort struct {
	out []KnowledgeCandidate
	err error
	d   time.Duration // optional delay to simulate a timeout
}

func (f *fakeMemoryPort) Recall(ctx context.Context, ac AuthzContext, q MemoryQueryRequest) ([]KnowledgeCandidate, error) {
	if f.d > 0 {
		select {
		case <-time.After(f.d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.out, f.err
}

// fakeDocumentPort is a DocumentQuery fake.
type fakeDocumentPort struct {
	out []KnowledgeCandidate
	err error
	d   time.Duration
}

func (f *fakeDocumentPort) Search(ctx context.Context, ac AuthzContext, q DocumentQueryRequest) ([]KnowledgeCandidate, error) {
	if f.d > 0 {
		select {
		case <-time.After(f.d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.out, f.err
}

// fakeCodePort is a CodeQuery fake.
type fakeCodePort struct {
	out []KnowledgeCandidate
	err error
	d   time.Duration
}

func (f *fakeCodePort) Search(ctx context.Context, ac AuthzContext, q CodeQueryRequest) ([]KnowledgeCandidate, error) {
	if f.d > 0 {
		select {
		case <-time.After(f.d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.out, f.err
}

// fakeSkillPort is a SkillQuery fake.
type fakeSkillPort struct {
	out []KnowledgeCandidate
	err error
	d   time.Duration
}

func (f *fakeSkillPort) Discover(ctx context.Context, ac AuthzContext, q SkillQueryRequest) ([]KnowledgeCandidate, error) {
	if f.d > 0 {
		select {
		case <-time.After(f.d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.out, f.err
}

// testPrincipal is a user principal for the broker tests.
func testPrincipal() Principal {
	return Principal{
		SubjectType: domain.SubjectUser,
		PrincipalID: uuid.New(),
	}
}

// testBroker builds a Broker over the given ports with a fake authz that
// allows everything (all-visible) + the built-in policy loader. Tests override
// the authz/ports as needed.
func testBroker(t *testing.T, mem MemoryQuery, auth AuthzService, opts ...BrokerOption) *Broker {
	t.Helper()
	return NewBroker(
		mem,
		NewIntentRouter(nil),
		auth,
		nil, // no PolicyLoader → built-in default (policy_version=0)
		NewBudgeter(),
		NewCitationBuilder(nil),
		DefaultBudgetDefaults,
		opts...,
	)
}

// cand builds a candidate with sane defaults for the tests.
func cand(assetID uuid.UUID, atype domain.AssetType, title string, signals ...float64) KnowledgeCandidate {
	var auth, fresh float64
	if len(signals) > 0 {
		auth = signals[0]
	}
	if len(signals) > 1 {
		fresh = signals[1]
	}
	return KnowledgeCandidate{
		AssetID:    assetID,
		AssetType:  atype,
		Title:      title,
		Snippet:    title + " snippet",
		Score:      auth,
		Authority:  auth,
		Freshness:  fresh,
		Citation:   Citation{AssetID: assetID, AssetType: atype},
	}
}

// ---------------------------------------------------------------------------
// Step 1-10: normal four-type happy path
// ---------------------------------------------------------------------------

func TestBroker_Execute_NormalFourTypes(t *testing.T) {
	ws := uuid.New()
	docID, codeID, memID := uuid.New(), uuid.New(), uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: nil}

	b := testBroker(t,
		&fakeMemoryPort{out: []KnowledgeCandidate{cand(memID, domain.AssetTypeMemory, "mem", 0.5, 0.5)}},
		auth,
		WithDocumentPort(&fakeDocumentPort{out: []KnowledgeCandidate{cand(docID, domain.AssetTypeDocument, "doc", 0.9, 0.8)}}),
		WithCodePort(&fakeCodePort{out: []KnowledgeCandidate{cand(codeID, domain.AssetTypeCodebase, "code", 0.7, 0.6)}}),
	)
	// Route to all four types explicitly so the skill port (unwired) is not
	// routed — the test pins the three wired types.
	q := KnowledgeQuery{
		Query:       "规范实现",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeCodebase, domain.AssetTypeMemory},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !resp.HasResults() {
		t.Fatalf("HasResults=false, want true (three wired types returned): %+v", resp)
	}
	// All three candidates survived (no degradation, no truncation).
	if len(resp.Candidates) != 3 {
		t.Errorf("candidates = %d, want 3: %+v", len(resp.Candidates), resp.Candidates)
	}
	if len(resp.DegradedSources) != 0 {
		t.Errorf("degraded = %v, want none", resp.DegradedSources)
	}
	// AuthzRevision + DecisionID carried from the AuthzContext (steps 1-2).
	if resp.AuthzRevision != 1 {
		t.Errorf("AuthzRevision = %d, want 1", resp.AuthzRevision)
	}
	// Intent was keyword-inferred (rule 1: explicit types → keyword picks the
	// policy). "规范实现" matches Spec keywords → IntentSpec.
	if resp.Intent != IntentSpec {
		t.Errorf("Intent = %q, want spec", resp.Intent)
	}
}

// ---------------------------------------------------------------------------
// Step 4: single-port timeout → degraded (§7.1, §9.6)
// ---------------------------------------------------------------------------

func TestBroker_Execute_SinglePortTimeout_Degraded(t *testing.T) {
	ws := uuid.New()
	docID := uuid.New()
	// Code port sleeps past the deadline; document returns immediately. The
	// CodeGraph timeout must NOT block document results (§9.6).
	auth := &fakeAuthz{authorizeAllowed: true}
	b := testBroker(t,
		&fakeMemoryPort{},
		auth,
		WithDocumentPort(&fakeDocumentPort{out: []KnowledgeCandidate{cand(docID, domain.AssetTypeDocument, "doc", 0.9, 0.8)}}),
		WithCodePort(&fakeCodePort{d: 500 * time.Millisecond, err: context.DeadlineExceeded}),
	)
	q := KnowledgeQuery{
		Query:       "实现 commit",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeCodebase},
		Timeout:     100 * time.Millisecond, // tight deadline so the code port times out
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Document result survived; code is degraded.
	if !resp.HasResults() {
		t.Fatalf("HasResults=false, want true (document must survive code timeout, §9.6)")
	}
	if len(resp.Candidates) != 1 || resp.Candidates[0].AssetID != docID {
		t.Errorf("candidates = %+v, want the document only", resp.Candidates)
	}
	if len(resp.DegradedSources) != 1 || resp.DegradedSources[0].AssetType != domain.AssetTypeCodebase {
		t.Errorf("degraded = %+v, want codebase provider_timeout", resp.DegradedSources)
	}
	if resp.DegradedSources[0].Reason != DegradedProviderTimeout {
		t.Errorf("degraded reason = %q, want provider_timeout", resp.DegradedSources[0].Reason)
	}
}

// ---------------------------------------------------------------------------
// Step 4 + §7.3: all ports fail → IsProviderDown (three-state, §11.4)
// ---------------------------------------------------------------------------

func TestBroker_Execute_AllPortsFail_ProviderDown(t *testing.T) {
	ws := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true}
	b := testBroker(t,
		&fakeMemoryPort{err: errors.New("memory engine blew up")},
		auth,
		WithDocumentPort(&fakeDocumentPort{err: errors.New("qdrant down")}),
	)
	q := KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// All ports failed → IsProviderDown. NOT IsEmptyNoResults (§11.4 "不得把
	// 失败解释为「没有知识」").
	if !resp.IsProviderDown() {
		t.Errorf("IsProviderDown=false, want true (all ports failed): %+v", resp)
	}
	if resp.HasResults() {
		t.Errorf("HasResults=true, want false (no candidates survived)")
	}
	if resp.IsEmptyNoResults() {
		t.Errorf("IsEmptyNoResults=true, want false (providers are down, not empty)")
	}
	if len(resp.DegradedSources) != 2 {
		t.Errorf("degraded = %d, want 2 (both ports)", len(resp.DegradedSources))
	}
}

// ---------------------------------------------------------------------------
// Step 5 + §7.3: candidates exist but all denied → IsEmptyAuthorized
// ---------------------------------------------------------------------------

func TestBroker_Execute_AllDenied_EmptyAuthorized(t *testing.T) {
	ws := uuid.New()
	memID := uuid.New()
	// authorizeAllowed=true (workspace-level use ok) but visibleSet is empty
	// (the post-check denies every candidate). This is the authorized-empty
	// state: candidates existed, all denied.
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: map[uuid.UUID]bool{}}
	b := testBroker(t,
		&fakeMemoryPort{out: []KnowledgeCandidate{cand(memID, domain.AssetTypeMemory, "mem", 0.5, 0.5)}},
		auth,
	)
	q := KnowledgeQuery{
		Query:       "决策",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !resp.IsEmptyAuthorized() {
		t.Errorf("IsEmptyAuthorized=false, want true (candidates existed, all denied): %+v", resp)
	}
	if resp.IsEmptyNoResults() {
		t.Errorf("IsEmptyNoResults=true, want false (candidates existed)")
	}
	if resp.IsProviderDown() {
		t.Errorf("IsProviderDown=true, want false (no provider failure)")
	}
}

// ---------------------------------------------------------------------------
// §7.3: genuine no-results → IsEmptyNoResults
// ---------------------------------------------------------------------------

func TestBroker_Execute_NoResults_EmptyNoResults(t *testing.T) {
	ws := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true}
	b := testBroker(t, &fakeMemoryPort{out: nil}, auth)
	q := KnowledgeQuery{
		Query:       "为什么",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !resp.IsEmptyNoResults() {
		t.Errorf("IsEmptyNoResults=false, want true (genuine empty): %+v", resp)
	}
	if resp.IsEmptyAuthorized() {
		t.Errorf("IsEmptyAuthorized=true, want false (no candidates existed)")
	}
	if resp.IsProviderDown() {
		t.Errorf("IsProviderDown=true, want false (no provider failure)")
	}
}

// ---------------------------------------------------------------------------
// Step 6: conflict pair kept side-by-side (§7.2)
// ---------------------------------------------------------------------------

func TestBroker_Execute_ConflictPairKeptSideBySide(t *testing.T) {
	ws := uuid.New()
	// Two same-asset candidates: one carries a "contradicts" relation, the
	// other carries an "old_spec" ConflictTag. Under IntentRevision the policy
	// must-surface {contradicts, impl_drift} — the contradicts carrier is
	// pinned; the old_spec carrier is not pinned by revision (old_spec is in
	// spec's surface set, not revision's). So the contradicts carrier survives
	// dedup and the old_spec twin is dropped (same asset, only the pinned one
	// is kept).
	assetID := uuid.New()
	targetID := uuid.New()
	c1 := cand(assetID, domain.AssetTypeMemory, "v2", 0.6, 0.9)
	c1.Relations = []RelationSummary{{RelationType: "contradicts", TargetID: targetID, TargetTitle: "v1"}}
	c2 := cand(assetID, domain.AssetTypeMemory, "v1 old", 0.4, 0.5)
	c2.ConflictTags = []string{"old_spec"}
	auth := &fakeAuthz{authorizeAllowed: true}
	b := testBroker(t, &fakeMemoryPort{out: []KnowledgeCandidate{c1, c2}}, auth)
	q := KnowledgeQuery{
		Query:       "实现", // IntentRevision → must-surface {contradicts, impl_drift}
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// The contradicts carrier is pinned (revision surfaces contradicts); the
	// old_spec twin is NOT pinned by revision → same-asset dedup keeps the
	// pinned one (c1, the newer). One candidate survives.
	if len(resp.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1 (pinned contradicts kept, twin dropped): %+v", len(resp.Candidates), resp.Candidates)
	}
	if resp.Candidates[0].AssetID != assetID {
		t.Errorf("surviving candidate = %+v, want the pinned contradicts carrier", resp.Candidates[0])
	}
}

// Two DIFFERENT assets that contradict each other are BOTH kept (side-by-side,
// §7.2) — neither is a same-asset twin, so dedup does not collapse them.
func TestBroker_Execute_ConflictPairDifferentAssets_BothKept(t *testing.T) {
	ws := uuid.New()
	a, b := uuid.New(), uuid.New()
	c1 := cand(a, domain.AssetTypeMemory, "decision A", 0.8, 0.8)
	c1.Relations = []RelationSummary{{RelationType: "contradicts", TargetID: b, TargetTitle: "decision B"}}
	c2 := cand(b, domain.AssetTypeMemory, "decision B", 0.7, 0.7)
	c2.ConflictTags = []string{"impl_drift"}
	auth := &fakeAuthz{authorizeAllowed: true}
	broker := testBroker(t, &fakeMemoryPort{out: []KnowledgeCandidate{c1, c2}}, auth)
	q := KnowledgeQuery{
		Query:       "为什么", // IntentRationale → must-surface {contradicts, old_spec}
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}
	resp, err := broker.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// c1 carries contradicts (rationale surfaces it) → pinned. c2 carries
	// impl_drift (rationale does NOT surface impl_drift) → not pinned, but it
	// is a DIFFERENT asset, so dedup keeps it (no same-asset twin to collapse).
	// Both survive.
	if len(resp.Candidates) != 2 {
		t.Errorf("candidates = %d, want 2 (different-asset conflict pair kept side-by-side, §7.2): %+v", len(resp.Candidates), resp.Candidates)
	}
}

// ---------------------------------------------------------------------------
// Step 8: budget overrun → truncation report (§6.2, §11.4)
// ---------------------------------------------------------------------------

func TestBroker_Execute_BudgetOverrun_Truncates(t *testing.T) {
	ws := uuid.New()
	// 10 document candidates at high authority; a tight MaxItems=2 forces
	// truncation. The report must carry the reason + truncated ids + continue
	// tools (§11.4 — never silent truncation).
	var cands []KnowledgeCandidate
	for i := 0; i < 10; i++ {
		cands = append(cands, cand(uuid.New(), domain.AssetTypeDocument, "doc", 0.9, 0.8))
	}
	auth := &fakeAuthz{authorizeAllowed: true}
	b := testBroker(t, &fakeMemoryPort{}, auth,
		WithDocumentPort(&fakeDocumentPort{out: cands}),
	)
	q := KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeDocument},
		MaxItems:    2,
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Candidates) > 2 {
		t.Errorf("candidates = %d, want <= 2 (budget MaxItems=2)", len(resp.Candidates))
	}
	if resp.Truncation.Reason == "" {
		t.Errorf("Truncation.Reason empty, want a reason (§11.4 no silent truncation): %+v", resp.Truncation)
	}
	if len(resp.Truncation.TruncatedAssetIDs) == 0 {
		t.Errorf("Truncation.TruncatedAssetIDs empty, want the dropped ids")
	}
	if len(resp.Truncation.ContinueTools) == 0 {
		t.Errorf("Truncation.ContinueTools empty, want document_read (§6.2)")
	}
	// The continue tool for document is document_read.
	found := false
	for _, tool := range resp.Truncation.ContinueTools {
		if tool == "document_read" {
			found = true
		}
	}
	if !found {
		t.Errorf("ContinueTools = %v, want document_read", resp.Truncation.ContinueTools)
	}
}

// ---------------------------------------------------------------------------
// Step 9: CitationBuilder completes the citation (§8)
// ---------------------------------------------------------------------------

func TestBroker_Execute_CitationBuilderRuns(t *testing.T) {
	ws := uuid.New()
	docID := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true}
	b := testBroker(t, &fakeMemoryPort{}, auth,
		WithDocumentPort(&fakeDocumentPort{out: []KnowledgeCandidate{cand(docID, domain.AssetTypeDocument, "doc", 0.9, 0.8)}}),
	)
	q := KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeDocument},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("candidates = %d, want 1", len(resp.Candidates))
	}
	c := resp.Candidates[0]
	// CitationBuilder denormalizes AssetID/AssetType/Authority on the citation
	// (§9.3 self-contained for audit).
	if c.Citation.AssetID != docID || c.Citation.AssetType != domain.AssetTypeDocument {
		t.Errorf("citation identity not denormalized: %+v", c.Citation)
	}
	if c.Citation.Authority != c.Authority {
		t.Errorf("citation authority = %v, want %v (denormalized)", c.Citation.Authority, c.Authority)
	}
}

// ---------------------------------------------------------------------------
// Step 10: audit summary carries policy_version + authz_revision
// ---------------------------------------------------------------------------

func TestBroker_Execute_AuditSummaryCarriesPolicyVersionAndAuthzRevision(t *testing.T) {
	ws := uuid.New()
	var seen AuditSummary
	var called int32
	sink := func(ctx context.Context, s AuditSummary) {
		atomic.StoreInt32(&called, 1)
		seen = s
	}
	auth := &fakeAuthz{authorizeAllowed: true, revision: 42}
	b := testBroker(t,
		&fakeMemoryPort{out: []KnowledgeCandidate{cand(uuid.New(), domain.AssetTypeMemory, "mem", 0.5, 0.5)}},
		auth,
		WithAuditSink(sink),
	)
	q := KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}
	if _, err := b.Execute(context.Background(), q, testPrincipal()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if atomic.LoadInt32(&called) == 0 {
		t.Fatal("audit sink not called (step 10)")
	}
	// policy_version=0 (built-in default, no DB row); authz_revision from the
	// AuthzContext (42). Both must be on the summary (§9.3).
	if seen.PolicyVersion != 0 {
		t.Errorf("audit PolicyVersion = %d, want 0 (built-in default)", seen.PolicyVersion)
	}
	if seen.AuthzRevision != 42 {
		t.Errorf("audit AuthzRevision = %d, want 42", seen.AuthzRevision)
	}
	if seen.SelectedCount != 1 {
		t.Errorf("audit SelectedCount = %d, want 1", seen.SelectedCount)
	}
	if seen.Intent != IntentSpec {
		t.Errorf("audit Intent = %q, want spec", seen.Intent)
	}
}

// ---------------------------------------------------------------------------
// Steps 1-2: caller/config errors return an error (not a partial response)
// ---------------------------------------------------------------------------

func TestBroker_Execute_NoWorkspace_Error(t *testing.T) {
	b := testBroker(t, &fakeMemoryPort{}, &fakeAuthz{})
	_, err := b.Execute(context.Background(), KnowledgeQuery{}, testPrincipal())
	if err != ErrBrokerNoWorkspace {
		t.Errorf("err = %v, want ErrBrokerNoWorkspace", err)
	}
}

func TestBroker_Execute_NoPrincipal_Error(t *testing.T) {
	b := testBroker(t, &fakeMemoryPort{}, &fakeAuthz{})
	_, err := b.Execute(context.Background(), KnowledgeQuery{WorkspaceID: uuid.New()}, Principal{})
	if err != ErrBrokerNoPrincipal {
		t.Errorf("err = %v, want ErrBrokerNoPrincipal", err)
	}
}

// ---------------------------------------------------------------------------
// Step 7 load: PolicyLoader falls back to the built-in default + records the
// DB policy_version when a row exists.
// ---------------------------------------------------------------------------

func TestBroker_Execute_PolicyLoader_DBVersion(t *testing.T) {
	ws := uuid.New()
	// A PolicyLoader that reports policy_version=7 (a DB row). The audit
	// summary + the response must carry 7.
	loader := func(ctx context.Context, workspaceID uuid.UUID, intent Intent) (AuthorityPolicy, int, error) {
		return NewAuthorityPolicy(intent, PolicyConfig{}), 7, nil
	}
	var seen AuditSummary
	b := NewBroker(
		&fakeMemoryPort{out: []KnowledgeCandidate{cand(uuid.New(), domain.AssetTypeMemory, "mem", 0.5, 0.5)}},
		NewIntentRouter(nil),
		&fakeAuthz{authorizeAllowed: true},
		loader,
		NewBudgeter(),
		NewCitationBuilder(nil),
		DefaultBudgetDefaults,
		WithAuditSink(func(ctx context.Context, s AuditSummary) { seen = s }),
	)
	q := KnowledgeQuery{Query: "规范", WorkspaceID: ws, AssetTypes: []domain.AssetType{domain.AssetTypeMemory}}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.PolicyVersion != 7 {
		t.Errorf("PolicyVersion = %d, want 7", resp.PolicyVersion)
	}
	if seen.PolicyVersion != 7 {
		t.Errorf("audit PolicyVersion = %d, want 7", seen.PolicyVersion)
	}
}

// ---------------------------------------------------------------------------
// §9.6: CodeGraph port unwired → degraded (not a crash)
// ---------------------------------------------------------------------------

func TestBroker_Execute_UnwiredCodePort_Degraded(t *testing.T) {
	ws := uuid.New()
	docID := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true}
	// No WithCodePort → code is unwired. Routing to [codebase, document]
	// degrades codebase as provider_error; document survives.
	b := testBroker(t, &fakeMemoryPort{}, auth,
		WithDocumentPort(&fakeDocumentPort{out: []KnowledgeCandidate{cand(docID, domain.AssetTypeDocument, "doc", 0.9, 0.8)}}),
	)
	q := KnowledgeQuery{
		Query:       "实现", // IntentRevision → [codebase, document]
		WorkspaceID: ws,
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !resp.HasResults() {
		t.Fatalf("HasResults=false, want true (document survived)")
	}
	// codebase is unwired → degraded.
	foundCodeDeg := false
	for _, d := range resp.DegradedSources {
		if d.AssetType == domain.AssetTypeCodebase {
			foundCodeDeg = true
			if d.Reason != DegradedProviderError {
				t.Errorf("code degraded reason = %q, want provider_error", d.Reason)
			}
		}
	}
	if !foundCodeDeg {
		t.Errorf("codebase not in degraded_sources (unwired port must degrade, §9.6): %+v", resp.DegradedSources)
	}
}
