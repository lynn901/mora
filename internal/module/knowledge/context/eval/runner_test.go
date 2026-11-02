package eval

// runner_test.go exercises the Context Broker eval runner (design-docs/19 §9.1)
// with a fake wired *Broker. It pins the per-type non-aggregation contract
// (§9.1 — recall is NEVER averaged into a single number; a weak type must fail
// the run on its own) + the recall/nDCG/citation/latency scoring shape, so the
// test engineer's case set lands against a stable contract.
//
// The fake broker is built with the context package's own test fakes
// (fakeAuthz/fakeMemoryPort/...) via the same testBroker helper the broker
// tests use — the eval runner exercises the REAL Execute path, not a parallel
// one. A nil-emitter broker (no WithMetrics) is used so the eval is metric-free
// and does not touch the prometheus registry.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/authz"
	knowledgectx "github.com/lynn901/mora/internal/module/knowledge/context"
)

// buildEvalBroker constructs a real *Broker over fake ports that return the
// given candidates for the memory + document ports, with an all-visible fake
// authz. The eval runner calls Execute on this — the production ten-step path.
func buildEvalBroker(t *testing.T, memOut, docOut []knowledgectx.KnowledgeCandidate) *knowledgectx.Broker {
	t.Helper()
	auth := &evalFakeAuthz{allowed: true}
	return knowledgectx.NewBroker(
		&evalFakeMemoryPort{out: memOut},
		knowledgectx.NewIntentRouter(nil),
		auth,
		nil, // built-in default policy (policy_version=0)
		knowledgectx.NewBudgeter(),
		knowledgectx.NewCitationBuilder(nil),
		knowledgectx.DefaultBudgetDefaults,
		knowledgectx.WithDocumentPort(&evalFakeDocumentPort{out: docOut}),
	)
}

// evalPrincipal is a user principal for the eval tests.
func evalPrincipal() knowledgectx.Principal {
	return knowledgectx.Principal{
		SubjectType:  domain.SubjectUser,
		PrincipalID:  uuid.New(),
	}
}

// TestRun_HappyPath_PassAllGates runs the seed baseline against a broker whose
// fake ports return exactly the expected candidates (id + version anchor
// match). All gates pass: recall 100%, citation 100%, P95 well under 2s.
func TestRun_HappyPath_PassAllGates(t *testing.T) {
	ws := "00000000-0000-0000-0000-000000000001"
	// Document port returns the spec doc + the runbook doc (IntentSpec +
	// IntentProcedure cases); memory port returns the rationale memory; the
	// codebase case expects a code asset but the code port is unwired → that
	// case degrades and fails recall. To pin the happy path, route the
	// revision case to document only by giving the fake doc port the code id.
	docID := uuid.MustParse("00000000-0000-0000-0000-000000000020")
	specID := uuid.MustParse("00000000-0000-0000-0000-000000000010")
	procID := uuid.MustParse("00000000-0000-0000-0000-000000000040")
	memID := uuid.MustParse("00000000-0000-0000-0000-000000000031")
	decID := uuid.MustParse("00000000-0000-0000-0000-000000000030")

	docOut := []knowledgectx.KnowledgeCandidate{
		candWithVersion(specID, domain.AssetTypeDocument, "spec", "00000000-0000-0000-0000-000000000100"),
		candWithVersion(docID, domain.AssetTypeDocument, "impl", "a1b2c3d"),
		candWithVersion(procID, domain.AssetTypeDocument, "runbook", "1.2.0"),
		candWithVersion(decID, domain.AssetTypeDocument, "decision", ""),
	}
	memOut := []knowledgectx.KnowledgeCandidate{
		candWithVersion(memID, domain.AssetTypeMemory, "memory", "00000000-0000-0000-0000-000000000310"),
	}
	b := buildEvalBroker(t, memOut, docOut)

	// Rewrite the seed cases so the revision case targets document (the fake
	// port is document-only); the expected ids + anchors match the fakes.
	sets := []CaseSet{{
		DatasetTag: SeedDatasetTag,
		Cases: []Case{
			{Intent: IntentKindSpec, Query: "规范：分页", WorkspaceID: ws,
				AssetTypes: []domain.AssetType{domain.AssetTypeDocument},
				ExpectedIDs: []string{specID.String()},
				ExpectedVersions: map[string]string{specID.String(): "00000000-0000-0000-0000-000000000100"}},
			{Intent: IntentKindRevision, Query: "实现 commit", WorkspaceID: ws,
				AssetTypes: []domain.AssetType{domain.AssetTypeDocument},
				ExpectedIDs: []string{docID.String()},
				ExpectedVersions: map[string]string{docID.String(): "a1b2c3d"}},
			{Intent: IntentKindRationale, Query: "为什么决策", WorkspaceID: ws,
				AssetTypes: []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
				ExpectedIDs: []string{decID.String(), memID.String()},
				ExpectedVersions: map[string]string{memID.String(): "00000000-0000-0000-0000-000000000310"}},
			{Intent: IntentKindProcedure, Query: "如何执行 runbook", WorkspaceID: ws,
				AssetTypes: []domain.AssetType{domain.AssetTypeDocument},
				ExpectedIDs: []string{procID.String()},
				ExpectedVersions: map[string]string{procID.String(): "1.2.0"}},
		},
	}}

	summary, err := Run(context.Background(), b, evalPrincipal(), sets)
	require.NoError(t, err)
	// Every targeted type reached 100% recall + citation; P95 ≪ 2s.
	assert.True(t, summary.RecallOK, "recall gate should pass: %s", FormatReport(summary))
	assert.True(t, summary.CitationOK, "citation gate should pass")
	assert.True(t, summary.LatencyOK, "latency gate should pass (P95=%dms)", summary.P95LatencyMS)
	assert.True(t, summary.Pass())
	assert.Less(t, summary.P95LatencyMS, int64(2000))
	// Per-type reports exist (non-aggregated, §9.1).
	assert.NotEmpty(t, summary.ByType)
}

// TestRun_WeakTypeFailsRun_NonAggregated pins §9.1's non-aggregation: a weak
// type (0% recall) must fail the run even when a strong type (100%) would mask
// it under a mean. The document type returns the expected id (100% recall);
// the memory type returns nothing (0% recall). RecallOK must be FALSE.
func TestRun_WeakTypeFailsRun_NonAggregated(t *testing.T) {
	ws := "00000000-0000-0000-0000-000000000001"
	docID := uuid.MustParse("00000000-0000-0000-0000-000000000010")
	memID := uuid.MustParse("00000000-0000-0000-0000-000000000031")

	// Document port returns the expected doc; memory port returns NOTHING
	// (the rationale case expects a memory candidate that never surfaces).
	b := buildEvalBroker(t,
		nil, // memory port returns no candidates → memory recall 0
		[]knowledgectx.KnowledgeCandidate{
			candWithVersion(docID, domain.AssetTypeDocument, "spec", "v1"),
		},
	)
	sets := []CaseSet{{
		DatasetTag: SeedDatasetTag,
		Cases: []Case{
			{Intent: IntentKindSpec, Query: "规范", WorkspaceID: ws,
				AssetTypes: []domain.AssetType{domain.AssetTypeDocument},
				ExpectedIDs: []string{docID.String()}},
			{Intent: IntentKindRationale, Query: "为什么决策", WorkspaceID: ws,
				AssetTypes: []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
				ExpectedIDs: []string{memID.String()}}, // memory never surfaces → 0 recall
		},
	}}
	summary, err := Run(context.Background(), b, evalPrincipal(), sets)
	require.NoError(t, err)
	// The memory type's 0 recall must fail the run, even though document was
	// 100% — §9.1 non-aggregation (a weak type must not be masked).
	assert.False(t, summary.RecallOK, "weak memory type (0 recall) must fail the run: %s", FormatReport(summary))
	// The memory type report records the failure with the missing id.
	var memRep *TypeReport
	for i := range summary.ByType {
		if summary.ByType[i].AssetType == domain.AssetTypeMemory {
			memRep = &summary.ByType[i]
		}
	}
	require.NotNil(t, memRep, "memory type report must exist (non-aggregated)")
	assert.Equal(t, 0.0, memRep.RecallAtK)
	assert.NotEmpty(t, memRep.Failed)
}

// TestRun_NilBrokerErrors asserts the runner surfaces a nil broker as an
// error rather than panicking (the skeleton's guard).
func TestRun_NilBrokerErrors(t *testing.T) {
	_, err := Run(context.Background(), nil, evalPrincipal(), []CaseSet{SeedBaseline})
	require.Error(t, err)
}

// TestRun_EmptyCaseSet_VacuousPass asserts an empty case set vacuously passes
// (no gate to fail) — the seed is a carrier until the test engineer fills it.
func TestRun_EmptyCaseSet_VacuousPass(t *testing.T) {
	b := buildEvalBroker(t, nil, nil)
	summary, err := Run(context.Background(), b, evalPrincipal(), []CaseSet{{DatasetTag: "empty"}})
	require.NoError(t, err)
	assert.True(t, summary.Pass(), "empty case set vacuously passes")
	assert.Equal(t, int64(0), summary.P95LatencyMS)
}

// TestComputeNDCG pins the nDCG ordering formula (§9.1): a perfect ordering
// (all expected first) is 1.0; a reversed ordering is < 1.0; empty expected is 0.
func TestComputeNDCG(t *testing.T) {
	expected := map[string]bool{"a": true, "b": true}
	// Perfect: both expected ranked first.
	assert.InDelta(t, 1.0, computeNDCG([]string{"a", "b", "c"}, expected), 1e-9)
	// Reversed: expected at the tail → < 1.0.
	ndcgRev := computeNDCG([]string{"c", "a", "b"}, expected)
	assert.Less(t, ndcgRev, 1.0)
	assert.Greater(t, ndcgRev, 0.0)
	// None of the expected surfaced → 0.
	assert.InDelta(t, 0.0, computeNDCG([]string{"c", "d"}, expected), 1e-9)
	// Empty expected → 0.
	assert.InDelta(t, 0.0, computeNDCG([]string{"a"}, map[string]bool{}), 1e-9)
}

// TestP95 pins the nearest-rank P95 (§14.3 SLO).
func TestP95(t *testing.T) {
	// 20 samples 1..20ms; P95 ≈ 19 (nearest-rank ceil(0.95*20)=19 → idx 18).
	lats := make([]int64, 20)
	for i := range lats {
		lats[i] = int64(i + 1)
	}
	assert.Equal(t, int64(19), p95(lats))
	assert.Equal(t, int64(0), p95(nil))
}

// TestToContextIntent pins the IntentKind → Broker Intent mapping (§4.1).
func TestToContextIntent(t *testing.T) {
	assert.Equal(t, knowledgectx.IntentSpec, ToContextIntent(IntentKindSpec))
	assert.Equal(t, knowledgectx.IntentRevision, ToContextIntent(IntentKindRevision))
	assert.Equal(t, knowledgectx.IntentRationale, ToContextIntent(IntentKindRationale))
	assert.Equal(t, knowledgectx.IntentProcedure, ToContextIntent(IntentKindProcedure))
	// Unknown → IntentSpec (the §4.2 rule-6 fallback).
	assert.Equal(t, knowledgectx.IntentSpec, ToContextIntent("bogus"))
}

// --- shared fakes (mirror the context package broker_test fakes, but local
// so the eval test is self-contained — the context package's fakes are
// unexported and live in its _test files). ---

type evalFakeAuthz struct{ allowed bool }

func (f *evalFakeAuthz) Authorize(_ context.Context, req authz.AuthzRequest) (authz.AuthzContext, error) {
	return authz.AuthzContext{
		WorkspaceID:   req.WorkspaceID,
		AuthzRevision: 1,
		PrincipalType: req.PrincipalType,
		PrincipalID:   req.PrincipalID,
		AgentID:       req.AgentID,
		Allowed:       f.allowed,
	}, nil
}

func (f *evalFakeAuthz) VisibleAssets(_ context.Context, _ authz.ListScope, candidates []uuid.UUID) ([]uuid.UUID, error) {
	// all-visible: return the candidates unchanged so the post-check passes.
	return candidates, nil
}

type evalFakeMemoryPort struct {
	out []knowledgectx.KnowledgeCandidate
	err error
	d   time.Duration
}

func (f *evalFakeMemoryPort) Recall(ctx context.Context, _ knowledgectx.AuthzContext, _ knowledgectx.MemoryQueryRequest) ([]knowledgectx.KnowledgeCandidate, error) {
	if f.d > 0 {
		select {
		case <-time.After(f.d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.out, f.err
}

type evalFakeDocumentPort struct {
	out []knowledgectx.KnowledgeCandidate
	err error
	d   time.Duration
}

func (f *evalFakeDocumentPort) Search(ctx context.Context, _ knowledgectx.AuthzContext, _ knowledgectx.DocumentQueryRequest) ([]knowledgectx.KnowledgeCandidate, error) {
	if f.d > 0 {
		select {
		case <-time.After(f.d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.out, f.err
}

// candWithVersion builds a candidate with a citation carrying the version
// anchor (so CitationAccuracy can assert it).
func candWithVersion(id uuid.UUID, atype domain.AssetType, title, version string) knowledgectx.KnowledgeCandidate {
	return knowledgectx.KnowledgeCandidate{
		AssetID:   id,
		AssetType: atype,
		Title:     title,
		Snippet:   title + " snippet",
		Score:     0.9,
		Authority: 0.9,
		Citation:  knowledgectx.Citation{AssetID: id, AssetType: atype, VersionOrRevision: version},
	}
}
