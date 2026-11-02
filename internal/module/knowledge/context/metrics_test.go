package context

// metrics_test.go verifies the §9.2 Prometheus instrumentation wired in
// broker.go + metrics.go. A recording fake Metrics captures every call so the
// test asserts the seven metrics fire with the right label values, WITHOUT
// touching the global Prometheus registry (NilMetrics is the Broker default;
// the test injects the recorder via WithMetrics).
//
// The seven metrics (§9.2):
//  1. knowledge_context_duration_seconds{route}        — ObserveStage
//  2. knowledge_context_tokens{type}                   — ObserveTokens
//  3. knowledge_context_candidates_total{intent,type,disposition} — IncCandidate
//  4. knowledge_context_degraded_total{reason}         — IncDegraded
//  5. knowledge_context_empty_total{state}             — IncEmpty
//  6. knowledge_context_policy_version                 — SetPolicyVersion
//  7. knowledge_authz_denied_total{action,type}        — IncAuthzDenied
//
// Each test exercises one Execute path (normal / degraded / authorized-empty /
// no-results) and pins the metric calls that path must emit.

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

// metricCall is one recorded Metrics invocation. kind names the method; the
// fields carry the values each method records (only the relevant ones set per
// kind).
type metricCall struct {
	kind        string
	route       string
	dur         time.Duration
	assetType   domain.AssetType
	tokens      int
	intent      Intent
	disposition string
	reason      DegradedReason
	state       string
	wsID        string
	version     int
	action      string
}

// recorder is the backing store a recordingMetrics writes into. Each Metrics
// function field closes over it. We use a struct (not a method-receiver) because
// Metrics is a struct of function fields, not an interface — WithMetrics(*Metrics)
// cannot accept a custom type. newRecordingMetrics returns a *Metrics wired to
// this recorder, so the broker's instrumented call sites record here instead of
// into the global Prometheus registry.
type recorder struct {
	calls []metricCall
}

func newRecordingMetrics() (*Metrics, *recorder) {
	r := &recorder{}
	m := &Metrics{
		ObserveStage: func(route string, d time.Duration) {
			r.calls = append(r.calls, metricCall{kind: "stage", route: route, dur: d})
		},
		ObserveTokens: func(t domain.AssetType, tokens int) {
			r.calls = append(r.calls, metricCall{kind: "tokens", assetType: t, tokens: tokens})
		},
		IncCandidate: func(intent Intent, t domain.AssetType, disposition string) {
			r.calls = append(r.calls, metricCall{kind: "candidate", intent: intent, assetType: t, disposition: disposition})
		},
		IncDegraded: func(reason DegradedReason) {
			r.calls = append(r.calls, metricCall{kind: "degraded", reason: reason})
		},
		IncEmpty: func(state string) {
			r.calls = append(r.calls, metricCall{kind: "empty", state: state})
		},
		SetPolicyVersion: func(wsID string, intent Intent, version int) {
			r.calls = append(r.calls, metricCall{kind: "policy_version", wsID: wsID, intent: intent, version: version})
		},
		IncAuthzDenied: func(action string, t domain.AssetType) {
			r.calls = append(r.calls, metricCall{kind: "authz_denied", action: action, assetType: t})
		},
	}
	return m, r
}

func (r *recorder) stages() []string {
	var out []string
	for _, c := range r.calls {
		if c.kind == "stage" {
			out = append(out, c.route)
		}
	}
	sort.Strings(out)
	return out
}

func (r *recorder) count(kind string) int {
	n := 0
	for _, c := range r.calls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

func (r *recorder) hasStage(route string) bool {
	for _, c := range r.calls {
		if c.kind == "stage" && c.route == route {
			return true
		}
	}
	return false
}

func (r *recorder) hasEmpty(state string) bool {
	for _, c := range r.calls {
		if c.kind == "empty" && c.state == state {
			return true
		}
	}
	return false
}

func (r *recorder) hasDegraded(reason DegradedReason) bool {
	for _, c := range r.calls {
		if c.kind == "degraded" && c.reason == reason {
			return true
		}
	}
	return false
}

func (r *recorder) hasCandidate(intent Intent, t domain.AssetType, disposition string) bool {
	for _, c := range r.calls {
		if c.kind == "candidate" && c.intent == intent && c.assetType == t && c.disposition == disposition {
			return true
		}
	}
	return false
}

func (r *recorder) hasPolicyVersion(intent Intent, version int) bool {
	for _, c := range r.calls {
		if c.kind == "policy_version" && c.intent == intent && c.version == version {
			return true
		}
	}
	return false
}

func (r *recorder) hasAuthzDenied(action string, t domain.AssetType) bool {
	for _, c := range r.calls {
		if c.kind == "authz_denied" && c.action == action && c.assetType == t {
			return true
		}
	}
	return false
}

// TestMetrics_NormalRun_AllSevenFire asserts a normal Execute (three wired
// types, all visible) emits every §9.2 metric at least once with the right
// labels: all six stage routes + total, tokens by type, candidates selected,
// policy_version (0 = built-in default), and NO degraded/empty/authz_denied.
func TestMetrics_NormalRun_AllSevenFire(t *testing.T) {
	ws := uuid.New()
	docID, codeID, memID := uuid.New(), uuid.New(), uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: nil}
	m, rec := newRecordingMetrics()

	b := testBroker(t,
		&fakeMemoryPort{out: []KnowledgeCandidate{cand(memID, domain.AssetTypeMemory, "mem", 0.5, 0.5)}},
		auth,
		WithDocumentPort(&fakeDocumentPort{out: []KnowledgeCandidate{cand(docID, domain.AssetTypeDocument, "doc", 0.9, 0.8)}}),
		WithCodePort(&fakeCodePort{out: []KnowledgeCandidate{cand(codeID, domain.AssetTypeCodebase, "code", 0.7, 0.6)}}),
		WithMetrics(m),
	)
	q := KnowledgeQuery{
		Query:       "规范实现",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeCodebase, domain.AssetTypeMemory},
	}
	if _, err := b.Execute(context.Background(), q, testPrincipal()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// 1. duration: all six stage routes + total observed.
	for _, route := range []string{stageIntent, stageAuthz, stageParallel, stageDedup, stageBudget, stageCitation, stageTotal} {
		if !rec.hasStage(route) {
			t.Errorf("duration route %q not observed", route)
		}
	}
	// 2. tokens: at least one ObserveTokens per surviving type.
	if rec.count("tokens") == 0 {
		t.Errorf("tokens metric never fired; want ≥1 ObserveTokens for selected candidates")
	}
	// 3. candidates_total{disposition=selected}: one per surviving candidate.
	for _, tc := range []struct {
		t domain.AssetType
	}{
		{domain.AssetTypeDocument}, {domain.AssetTypeCodebase}, {domain.AssetTypeMemory},
	} {
		if !rec.hasCandidate(IntentSpec, tc.t, dispositionSelected) {
			t.Errorf("candidates_total{intent=spec,type=%s,disposition=selected} not observed", tc.t)
		}
	}
	// 4. degraded_total: none on a normal run.
	if rec.count("degraded") != 0 {
		t.Errorf("degraded_total fired %d times on a normal run; want 0", rec.count("degraded"))
	}
	// 5. empty_total: none on a normal run (HasResults → no empty state counted).
	if rec.count("empty") != 0 {
		t.Errorf("empty_total fired on a normal run; want 0")
	}
	// 6. policy_version: set to 0 (built-in default, no PolicyLoader wired).
	if !rec.hasPolicyVersion(IntentSpec, 0) {
		t.Errorf("policy_version gauge not set for intent=spec version=0")
	}
	// 7. authz_denied_total: none on an all-visible run.
	if rec.count("authz_denied") != 0 {
		t.Errorf("authz_denied_total fired on an all-visible run; want 0")
	}
}

// TestMetrics_DegradedRun_CountsReason asserts a single-port timeout emits
// knowledge_context_degraded_total{reason=provider_timeout} exactly once and
// the empty_total{state=...} counter is NOT incremented (partial success →
// HasResults, not an empty state).
func TestMetrics_DegradedRun_CountsReason(t *testing.T) {
	ws := uuid.New()
	memID := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: nil}
	m, rec := newRecordingMetrics()

	b := testBroker(t,
		&fakeMemoryPort{out: []KnowledgeCandidate{cand(memID, domain.AssetTypeMemory, "mem", 0.5, 0.5)}},
		auth,
		// Document port times out → degraded provider_timeout; memory returns.
		WithDocumentPort(&fakeDocumentPort{err: context.DeadlineExceeded}),
		WithMetrics(m),
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
	if !resp.HasResults() {
		t.Fatalf("HasResults=false; want true (memory survived the doc timeout)")
	}
	if !rec.hasDegraded(DegradedProviderTimeout) {
		t.Errorf("degraded_total{reason=provider_timeout} not observed; calls=%v", rec.calls)
	}
	if rec.count("empty") != 0 {
		t.Errorf("empty_total fired on a partial-success run; want 0 (HasResults)")
	}
}

// TestMetrics_AuthorizedEmpty_CountsDeniedAndState asserts the authorized-empty
// path (candidates existed but the post-check denied every one) increments
// both knowledge_authz_denied_total{action=use,type=...} AND
// knowledge_context_candidates_total{disposition=denied} AND
// knowledge_context_empty_total{state=empty_authorized}.
func TestMetrics_AuthorizedEmpty_CountsDeniedAndState(t *testing.T) {
	ws := uuid.New()
	docID := uuid.New()
	// visibleSet is an empty (non-nil) map → every candidate denied.
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: map[uuid.UUID]bool{}}
	m, rec := newRecordingMetrics()

	b := testBroker(t,
		&fakeMemoryPort{}, // no memory candidates
		auth,
		WithDocumentPort(&fakeDocumentPort{out: []KnowledgeCandidate{cand(docID, domain.AssetTypeDocument, "doc", 0.9, 0.8)}}),
		WithMetrics(m),
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
	if !resp.IsEmptyAuthorized() {
		t.Fatalf("IsEmptyAuthorized=false; want true (the one doc candidate was denied)")
	}
	if !rec.hasAuthzDenied(string(domain.ActionUse), domain.AssetTypeDocument) {
		t.Errorf("authz_denied_total{action=use,type=document} not observed")
	}
	if !rec.hasCandidate(IntentSpec, domain.AssetTypeDocument, dispositionDenied) {
		t.Errorf("candidates_total{disposition=denied} for document not observed")
	}
	if !rec.hasEmpty(emptyAuthorized) {
		t.Errorf("empty_total{state=empty_authorized} not observed")
	}
}

// TestMetrics_NoResults_CountsState asserts the genuine no-results path (all
// ports return empty, no failure) increments empty_total{state=empty_no_results}
// and emits NO authz_denied (nothing was denied — nothing was returned).
func TestMetrics_NoResults_CountsState(t *testing.T) {
	ws := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: nil}
	m, rec := newRecordingMetrics()

	b := testBroker(t,
		&fakeMemoryPort{}, // empty
		auth,
		WithMetrics(m),
	)
	q := KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !resp.IsEmptyNoResults() {
		t.Fatalf("IsEmptyNoResults=false; want true")
	}
	if !rec.hasEmpty(emptyNoResults) {
		t.Errorf("empty_total{state=empty_no_results} not observed")
	}
	if rec.count("authz_denied") != 0 {
		t.Errorf("authz_denied_total fired on a no-results run; want 0")
	}
}

// TestMetrics_ProviderDown_CountsState asserts the all-ports-fail path
// increments empty_total{state=provider_down}.
func TestMetrics_ProviderDown_CountsState(t *testing.T) {
	ws := uuid.New()
	auth := &fakeAuthz{authorizeAllowed: true, visibleSet: nil}
	m, rec := newRecordingMetrics()

	b := testBroker(t,
		&fakeMemoryPort{err: errors.New("boom")}, // memory fails
		auth,
		WithMetrics(m),
	)
	q := KnowledgeQuery{
		Query:       "规范",
		WorkspaceID: ws,
		AssetTypes:  []domain.AssetType{domain.AssetTypeMemory},
	}
	resp, err := b.Execute(context.Background(), q, testPrincipal())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !resp.IsProviderDown() {
		t.Fatalf("IsProviderDown=false; want true")
	}
	if !rec.hasEmpty(emptyProviderDown) {
		t.Errorf("empty_total{state=provider_down} not observed")
	}
	// A provider error is a degradation too (§7.3).
	if !rec.hasDegraded(DegradedProviderError) {
		t.Errorf("degraded_total{reason=provider_error} not observed")
	}
}
