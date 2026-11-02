package context

// metrics_test.go verifies the §9.2 online quality metrics emission
// (design-docs/19 §9.2). The Broker emits against a MetricsEmitter; the
// default (nil → noopMetricsEmitter) is metric-free so the existing broker
// tests run unchanged. This test installs a recording emitter fake and
// asserts the seven §9.2 metrics fire at the right §7.1 phase boundaries,
// including the §7.3 three-state label completeness (the Phase 6 acceptance
// gate — a fault must never be reported as "no knowledge").
//
// The recording fake does NOT touch the prometheus registry, so this is a pure
// unit test of the Broker's emission wiring (the registered collectors
// themselves are exercised by the observ package + a scrape in integration).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// recordingEmitter records every emission call so the test can assert which
// §9.2 metrics fired, with which labels. Safe for concurrent use (the Broker
// fans out ports concurrently).
type recordingEmitter struct {
	mu sync.Mutex
	phases       []MetricsPhase
	tokens       []tokenObs
	candidates   []candObs
	degraded     []DegradedReason
	emptyStates  []MetricsEmptyState
	policyVers   []int
	authzDenied  []authzObs
}

type tokenObs struct {
	t    domain.AssetType
	n    int
}
type candObs struct {
	intent      Intent
	t           domain.AssetType
	disposition MetricsDisposition
	n           int
}
type authzObs struct {
	action domain.Action
	t      domain.TargetType
}

func (r *recordingEmitter) ObservePhaseDuration(p MetricsPhase, _ time.Duration) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.phases = append(r.phases, p)
}
func (r *recordingEmitter) ObserveTokens(t domain.AssetType, n int) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.tokens = append(r.tokens, tokenObs{t, n})
}
func (r *recordingEmitter) IncCandidates(intent Intent, t domain.AssetType, d MetricsDisposition, n int) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.candidates = append(r.candidates, candObs{intent, t, d, n})
}
func (r *recordingEmitter) IncDegraded(reason DegradedReason) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.degraded = append(r.degraded, reason)
}
func (r *recordingEmitter) IncEmptyState(s MetricsEmptyState) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.emptyStates = append(r.emptyStates, s)
}
func (r *recordingEmitter) SetPolicyVersion(v int) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.policyVers = append(r.policyVers, v)
}
func (r *recordingEmitter) IncAuthzDenied(a domain.Action, t domain.TargetType) {
	r.mu.Lock(); defer r.mu.Unlock()
	r.authzDenied = append(r.authzDenied, authzObs{a, t})
}

// TestBrokerMetrics_HappyPath_AllSevenMetricsFire runs the normal four-type
// happy path with a recording emitter and asserts every §9.2 metric fires:
// duration (6 phases), tokens (per type), candidates (selected), policy
// version, and NO degraded/empty/authz-denied (the happy path has none).
func TestBrokerMetrics_HappyPath_AllSevenMetricsFire(t *testing.T) {
	ws := uuid.New()
	docID, codeID, memID := uuid.New(), uuid.New(), uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: nil}
	rec := &recordingEmitter{}
	b := testBroker(t,
		&fakeMemoryPort{out: []KnowledgeCandidate{cand(memID, domain.AssetTypeMemory, "mem", 0.5, 0.5)}},
		auth,
		WithDocumentPort(&fakeDocumentPort{out: []KnowledgeCandidate{cand(docID, domain.AssetTypeDocument, "doc", 0.9, 0.8)}}),
		WithCodePort(&fakeCodePort{out: []KnowledgeCandidate{cand(codeID, domain.AssetTypeCodebase, "code", 0.7, 0.6)}}),
		WithMetrics(rec),
	)
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
		t.Fatalf("HasResults=false, want true: %+v", resp)
	}

	// §9.2 knowledge_context_duration_seconds{route}: all six §7.1 phases
	// observed (intent/authz/parallel/dedup/budget/citation).
	wantPhases := map[MetricsPhase]bool{
		MetricsPhaseIntent: true, MetricsPhaseAuthz: true, MetricsPhaseParallel: true,
		MetricsPhaseDedup: true, MetricsPhaseBudget: true, MetricsPhaseCitation: true,
	}
	gotPhases := make(map[MetricsPhase]bool, len(rec.phases))
	for _, p := range rec.phases {
		gotPhases[p] = true
	}
	for p := range wantPhases {
		if !gotPhases[p] {
			t.Errorf("duration metric missing phase %q; got %v", p, rec.phases)
		}
	}

	// §9.2 knowledge_context_policy_version (gauge): set once to 0 (built-in
	// default, no PolicyLoader wired).
	if len(rec.policyVers) == 0 || rec.policyVers[0] != 0 {
		t.Errorf("policy_version = %v, want [0] (built-in default)", rec.policyVers)
	}

	// §9.2 knowledge_context_candidates_total{disposition=selected}: at least
	// one selected count fired per surviving type. NO truncated/denied on the
	// happy path.
	hasSelected := false
	for _, c := range rec.candidates {
		if c.disposition == MetricsDispSelected {
			hasSelected = true
		}
		if c.disposition == MetricsDispDenied {
			t.Errorf("happy path must not emit denied candidates; got %+v", c)
		}
	}
	if !hasSelected {
		t.Errorf("no selected candidates emitted; got %+v", rec.candidates)
	}

	// §9.2 knowledge_context_tokens{type}: token spend observed for at least
	// one surviving type.
	if len(rec.tokens) == 0 {
		t.Errorf("no token observations emitted (happy path has spend)")
	}

	// Happy path: NO degraded, NO empty-state, NO authz-denied.
	if len(rec.degraded) != 0 {
		t.Errorf("happy path emitted degraded = %v, want none", rec.degraded)
	}
	if len(rec.emptyStates) != 0 {
		t.Errorf("happy path emitted empty-state = %v, want none (has results)", rec.emptyStates)
	}
	if len(rec.authzDenied) != 0 {
		t.Errorf("happy path emitted authz-denied = %v, want none (allowed)", rec.authzDenied)
	}
}

// TestBrokerMetrics_ProviderDown_EmptyStateLabel pins the §7.3 three-state
// acceptance gate (§0 D7/D8, §11.4): when all ports fail, the empty_total
// metric MUST carry the provider_down state — NOT empty_no_results (a fault
// must never be reported as "no knowledge"). Also asserts degraded_total
// fires per failed source.
func TestBrokerMetrics_ProviderDown_EmptyStateLabel(t *testing.T) {
	ws := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true}
	rec := &recordingEmitter{}
	b := testBroker(t,
		&fakeMemoryPort{err: errors.New("memory engine blew up")},
		auth,
		WithDocumentPort(&fakeDocumentPort{err: errors.New("qdrant down")}),
		WithMetrics(rec),
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
	if !resp.IsProviderDown() {
		t.Fatalf("IsProviderDown=false, want true: %+v", resp)
	}
	// §9.2 knowledge_context_empty_total{state=provider_down}: the three-state
	// label MUST be provider_down (a fault, not "no knowledge").
	found := false
	for _, s := range rec.emptyStates {
		if s == MetricsEmptyProviderDown {
			found = true
		}
	}
	if !found {
		t.Errorf("empty_total state = %v, want provider_down (§7.3 three-state)", rec.emptyStates)
	}
	// §9.2 knowledge_context_degraded_total{reason}: one per failed source.
	if len(rec.degraded) != 2 {
		t.Errorf("degraded count = %d, want 2 (both ports failed): %v", len(rec.degraded), rec.degraded)
	}
}

// TestBrokerMetrics_EmptyAuthorized_DistinctFromNoResults pins the §7.3
// three-state distinguishability on the metric surface: candidates existed
// but all were denied by the post-check → empty_authorized (NOT
// empty_no_results, NOT provider_down). The three labels are mutually
// exclusive on an empty result.
func TestBrokerMetrics_EmptyAuthorized_DistinctFromNoResults(t *testing.T) {
	ws := uuid.New()
	memID := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: map[uuid.UUID]bool{}}
	rec := &recordingEmitter{}
	b := testBroker(t,
		&fakeMemoryPort{out: []KnowledgeCandidate{cand(memID, domain.AssetTypeMemory, "mem", 0.5, 0.5)}},
		auth,
		WithMetrics(rec),
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
		t.Fatalf("IsEmptyAuthorized=false, want true: %+v", resp)
	}
	// §9.2 empty_total{state=empty_authorized} — NOT empty_no_results.
	if len(rec.emptyStates) != 1 || rec.emptyStates[0] != MetricsEmptyAuthorized {
		t.Errorf("empty_total state = %v, want [empty_authorized]", rec.emptyStates)
	}
	// §9.2 candidates_total{disposition=denied}: the denied candidate counted.
	hasDenied := false
	for _, c := range rec.candidates {
		if c.disposition == MetricsDispDenied {
			hasDenied = true
		}
	}
	if !hasDenied {
		t.Errorf("no denied candidates emitted; got %+v", rec.candidates)
	}
}

// TestBrokerMetrics_EmptyNoResults pins the third §7.3 state on the metric
// surface: every port returned empty, no port failed → empty_no_results.
func TestBrokerMetrics_EmptyNoResults(t *testing.T) {
	ws := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true}
	rec := &recordingEmitter{}
	b := testBroker(t, &fakeMemoryPort{out: nil}, auth, WithMetrics(rec))
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
		t.Fatalf("IsEmptyNoResults=false, want true: %+v", resp)
	}
	if len(rec.emptyStates) != 1 || rec.emptyStates[0] != MetricsEmptyNoResults {
		t.Errorf("empty_total state = %v, want [empty_no_results]", rec.emptyStates)
	}
}

// TestBrokerMetrics_WorkspaceDenial_AuthzDenied pins §9.2
// knowledge_authz_denied_total{action=read,type=workspace}: a workspace-scope
// denial (non-member) is counted on the membership gate, leak-safe (counted
// only, never distinguished from "no knowledge" on the response — the response
// is empty-authorized).
func TestBrokerMetrics_WorkspaceDenial_AuthzDenied(t *testing.T) {
	ws := uuid.New()
	// authorizeAllowed=false → workspace membership denied (the principal is
	// not a member). The Broker converts this to Allowed=false (leak-safe,
	// §8.2) and the fan-out proceeds against an empty AllowedAssetIDs.
	auth := &fakeAuthz{authorizeAllowed: false, visibleSet: nil}
	rec := &recordingEmitter{}
	b := testBroker(t, &fakeMemoryPort{out: nil}, auth, WithMetrics(rec))
	q := KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Workspace denial → empty (leak-safe); NOT provider-down.
	if resp.IsProviderDown() {
		t.Errorf("IsProviderDown=true, want false (denial is not a fault)")
	}
	// §9.2 authz_denied_total{action=read,type=workspace}: the membership gate
	// denial counted.
	found := false
	for _, a := range rec.authzDenied {
		if a.action == domain.ActionRead && a.t == domain.TargetWorkspace {
			found = true
		}
	}
	if !found {
		t.Errorf("authz_denied missing read/workspace; got %+v", rec.authzDenied)
	}
}

// TestBrokerMetrics_TruncationEmitted pins §9.2
// candidates_total{disposition=truncated} + tokens{type}: when the budget
// truncates scored candidates, the truncated disposition is counted per type
// and the realized token spend is observed for the selected slice.
func TestBrokerMetrics_TruncationEmitted(t *testing.T) {
	ws := uuid.New()
	// Build 30 memory candidates with distinct ids; the default MaxItems=20 +
	// memory MaxItems=4 quota forces truncation (the Budgeter drops the rest).
	cands := make([]KnowledgeCandidate, 30)
	for i := range cands {
		cands[i] = cand(uuid.New(), domain.AssetTypeMemory, "mem", 0.5, 0.5)
	}
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: nil}
	rec := &recordingEmitter{}
	b := testBroker(t, &fakeMemoryPort{out: cands}, auth, WithMetrics(rec))
	q := KnowledgeQuery{
		Query:       "决策",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(resp.Candidates) == 0 {
		t.Fatalf("no candidates survived; want some (truncation still selects)")
	}
	// Truncation report present (the budget dropped scored candidates).
	if resp.Truncation.Reason == "" {
		t.Fatalf("no truncation; want one (30 cands vs memory quota 4): %+v", resp.Truncation)
	}
	// §9.2 truncated disposition emitted for memory type.
	hasTrunc := false
	for _, c := range rec.candidates {
		if c.disposition == MetricsDispTruncated && c.t == domain.AssetTypeMemory {
			hasTrunc = true
		}
	}
	if !hasTrunc {
		t.Errorf("no truncated candidates emitted for memory; got %+v", rec.candidates)
	}
	// §9.2 tokens{type=memory}: realized spend observed for the selected slice.
	hasTokens := false
	for _, tk := range rec.tokens {
		if tk.t == domain.AssetTypeMemory {
			hasTokens = true
		}
	}
	if !hasTokens {
		t.Errorf("no token observation for memory; got %+v", rec.tokens)
	}
}

// TestPromMetricsEmitter_NoPanic verifies the production emitter (forwarding
// to the registered observ collectors) does not panic on the §9.2 calls. It
// does not assert values (those are scrape-verified in integration); this is
// a wiring smoke test that the registered collectors accept the labels.
func TestPromMetricsEmitter_NoPanic(t *testing.T) {
	e := NewPromMetricsEmitter()
	e.ObservePhaseDuration(MetricsPhaseParallel, 5*time.Millisecond)
	e.ObserveTokens(domain.AssetTypeDocument, 128)
	e.IncCandidates(IntentSpec, domain.AssetTypeDocument, MetricsDispSelected, 1)
	e.IncCandidates(IntentSpec, domain.AssetTypeDocument, MetricsDispTruncated, 2)
	e.IncCandidates(IntentSpec, domain.AssetTypeMemory, MetricsDispDenied, 1)
	e.IncDegraded(DegradedProviderTimeout)
	e.IncEmptyState(MetricsEmptyProviderDown)
	e.IncEmptyState(MetricsEmptyAuthorized)
	e.IncEmptyState(MetricsEmptyNoResults)
	e.SetPolicyVersion(3)
	e.IncAuthzDenied(domain.ActionRead, domain.TargetWorkspace)
	e.IncAuthzDenied(domain.ActionUse, domain.TargetAsset)
	// No panic → the registered collectors accept every §9.2 label vocabulary.
}
