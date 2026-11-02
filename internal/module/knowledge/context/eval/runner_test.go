package eval

// runner_test.go exercises the eval runner (design-docs/19 §9.1) with a fake
// broker that returns the seed cases' expected shapes: normal cases get the
// expected candidates with correct citations; the three-state cases get the
// matching empty/fault response; the conflict-preservation case gets
// candidates carrying the must-surface relation. Pins the per-type Summary
// shape so the test engineer's case sets land against a stable contract.
//
// The fake broker models the Broker's leak-safe + three-state contract
// (§7.3 / §11.5) — not the ten-step pipeline. It returns a fixed response per
// case keyed by the query, so the runner's scoring loop is exercised without a
// real Broker wiring.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	ctxmod "github.com/lynn901/mora/internal/module/knowledge/context"
)

// fakeBroker returns a canned ContextResponse per query, modeling the §11.5
// three-state contract + §7.2 conflict preservation. It implements Broker
// (Execute) so the runner scores it without a real wiring.
type fakeBroker struct {
	responses map[string]ctxmod.ContextResponse
	err       error // when set, Execute returns this error for every query
}

func (f fakeBroker) Execute(_ context.Context, q ctxmod.KnowledgeQuery, _ ctxmod.Principal) (ctxmod.ContextResponse, error) {
	if f.err != nil {
		return ctxmod.ContextResponse{}, f.err
	}
	if resp, ok := f.responses[q.Query]; ok {
		return resp, nil
	}
	// Default: no-results (a query not in the map is "nothing matched").
	return ctxmod.ContextResponse{Intent: ctxmod.IntentSpec}, nil
}

// newSeedFakeBroker builds a fake broker that returns the seed cases' expected
// shapes: the four normal cases get their expected candidates (with citations
// for the two that carry ExpectedCitations), the authorized-empty case gets an
// IsEmptyAuthorized response, the no-results case gets IsEmptyNoResults, the
// provider-down case gets IsProviderDown, and the conflict-preservation case
// gets candidates carrying a "contradicts" relation.
func newSeedFakeBroker() fakeBroker {
	// asset ids + version anchors mirror cases_seed.go.
	docID := uuid.MustParse("00000000-0000-0000-0000-00000000d0c1")
	codeID := uuid.MustParse("00000000-0000-0000-0000-00000000c0de")
	memID := uuid.MustParse("00000000-0000-0000-0000-000000006d6e")
	skillID := uuid.MustParse("00000000-0000-0000-0000-000000005d11")

	doc := func(id uuid.UUID, atype domain.AssetType, rev string, relations []ctxmod.RelationSummary) ctxmod.KnowledgeCandidate {
		return ctxmod.KnowledgeCandidate{
			AssetID:   id,
			AssetType: atype,
			Title:     "seed-" + string(atype),
			Snippet:   "seed snippet",
			Relations: relations,
			Citation: ctxmod.Citation{
				AssetID:           id,
				AssetType:         atype,
				VersionOrRevision: rev,
			},
		}
	}

	return fakeBroker{responses: map[string]ctxmod.ContextResponse{
		// spec → document (citation v1).
		"API 分页规范要求": {Candidates: []ctxmod.KnowledgeCandidate{doc(docID, domain.AssetTypeDocument, "v1", nil)}, Intent: ctxmod.IntentSpec},
		// revision → codebase (citation seed-commit).
		"Serve 函数实现 commit": {Candidates: []ctxmod.KnowledgeCandidate{doc(codeID, domain.AssetTypeCodebase, "seed-commit", nil)}, Intent: ctxmod.IntentRevision},
		// rationale → memory (no citation anchor asserted in the seed).
		"为什么选择 BM25+向量 RAG 决策": {Candidates: []ctxmod.KnowledgeCandidate{doc(memID, domain.AssetTypeMemory, "", nil)}, Intent: ctxmod.IntentRationale},
		// procedure → skill.
		"如何执行 runbook 部署流程": {Candidates: []ctxmod.KnowledgeCandidate{doc(skillID, domain.AssetTypeSkill, "", nil)}, Intent: ctxmod.IntentProcedure},
		// authorized-empty: candidates existed (preCheckCount=3) but all denied.
		"机密规范要求": ctxmod.NewAuthorizedEmptyResponse(ctxmod.IntentSpec, 3),
		// no-results: nothing fetched, no fault.
		"完全不存在的术语xyz": {Intent: ctxmod.IntentSpec},
		// provider-down: a port faulted, no candidate survived.
		"codegraph sidecar 故障查询": {
			Intent:          ctxmod.IntentRevision,
			DegradedSources: []ctxmod.DegradedSource{{AssetType: domain.AssetTypeCodebase, Reason: ctxmod.DegradedProviderError, Detail: "sidecar timeout"}},
		},
		// conflict-preservation: two candidates, one contradicts the other.
		"存在冲突的规范要求": {
			Intent: ctxmod.IntentSpec,
			Candidates: []ctxmod.KnowledgeCandidate{
				doc(docID, domain.AssetTypeDocument, "v1", []ctxmod.RelationSummary{{RelationType: "contradicts", TargetID: uuid.New(), TargetTitle: "rival spec"}}),
				doc(uuid.New(), domain.AssetTypeDocument, "v2", nil),
			},
		},
	}}
}

// TestRun_SeedFakeBrokerPasses asserts the runner scores the seed case set
// against the fake broker as PASS: every normal case 100% recall + correct
// citations, every three-state case classified correctly, the conflict-
// preservation case surfaces "contradicts", P95 ≤ 2s, CitationAccuracy ≥ 95%.
func TestRun_SeedFakeBrokerPasses(t *testing.T) {
	b := newSeedFakeBroker()
	summary, err := Run(context.Background(), b, ctxmod.Principal{}, []CaseSet{SeedCaseSet})
	require.NoError(t, err)
	assert.Equal(t, "seed-baseline", summary.DatasetTag)
	// Four asset types appear (document/code/memory/skill).
	assert.Len(t, summary.ByType, 4)
	// Citation gate: the seed asserts citations on document + codebase; both
	// match → CitationAccuracy 100% → CitationOK true.
	assert.True(t, summary.CitationOK, "citation gate must pass (≥95%%)")
	assert.True(t, summary.LatencyOK, "latency gate must pass (P95 ≤ 2s)")
	assert.True(t, summary.Pass())
	// No failures across the whole run.
	for _, r := range summary.ByType {
		assert.Empty(t, r.Failed, "%s must have no failures", r.AssetType)
	}
	// The document type carries: 1 normal (recall+nDCG+citation), 1
	// authorized-empty, 1 no-results, 1 conflict-preservation.
	var docRep *TypeReport
	for i := range summary.ByType {
		if summary.ByType[i].AssetType == domain.AssetTypeDocument {
			docRep = &summary.ByType[i]
		}
	}
	require.NotNil(t, docRep, "document type report must exist")
	assert.Equal(t, 1, docRep.RecallCases)
	assert.InDelta(t, 1.0, docRep.RecallAtK, 1e-9)
	assert.InDelta(t, 1.0, docRep.NDCG, 1e-9)
	assert.Equal(t, 1, docRep.CitationCases) // the seed asserts one citation on the doc
	assert.InDelta(t, 1.0, docRep.CitationAccuracy, 1e-9)
	assert.Equal(t, 2, docRep.ThreeStateCases) // authorized-empty + no-results
	assert.Equal(t, 2, docRep.ThreeStateOK)
	assert.Equal(t, 1, docRep.ConflictCases)
	assert.Equal(t, 1, docRep.ConflictOK)
}

// TestRun_ProviderDownNotScoredAsRecallMiss asserts a provider-down case is
// scored as a three-state classification, NOT a recall miss: the codebase type
// has one normal case (100% recall) + one provider-down case, and the run
// still passes (the fault is not a recall failure).
func TestRun_ProviderDownNotScoredAsRecallMiss(t *testing.T) {
	b := newSeedFakeBroker()
	summary, err := Run(context.Background(), b, ctxmod.Principal{}, []CaseSet{SeedCaseSet})
	require.NoError(t, err)
	var codeRep *TypeReport
	for i := range summary.ByType {
		if summary.ByType[i].AssetType == domain.AssetTypeCodebase {
			codeRep = &summary.ByType[i]
		}
	}
	require.NotNil(t, codeRep)
	assert.Equal(t, 1, codeRep.RecallCases, "codebase has one normal case")
	assert.InDelta(t, 1.0, codeRep.RecallAtK, 1e-9)
	assert.Equal(t, 1, codeRep.ThreeStateCases, "codebase has one provider-down case")
	assert.Equal(t, 1, codeRep.ThreeStateOK, "provider-down classified correctly")
	assert.Empty(t, codeRep.Failed)
}

// TestRun_CitationGateFailsBelowThreshold asserts the §9.1 hard gate
// (CitationAccuracy ≥ 95%) fails when a citation anchor is wrong. Mutate the
// fake broker to return a wrong version for the spec doc → CitationAccuracy
// drops to 0 → Pass() false.
func TestRun_CitationGateFailsBelowThreshold(t *testing.T) {
	b := newSeedFakeBroker()
	// Flip the spec doc's citation version to the wrong anchor.
	resp := b.responses["API 分页规范要求"]
	resp.Candidates[0].Citation.VersionOrRevision = "wrong-version"
	b.responses["API 分页规范要求"] = resp

	summary, err := Run(context.Background(), b, ctxmod.Principal{}, []CaseSet{SeedCaseSet})
	require.NoError(t, err)
	assert.False(t, summary.CitationOK, "a wrong citation must fail the ≥95%% gate")
	assert.False(t, summary.Pass())
}

// TestRun_PerTypeNoAggregation asserts metrics are reported per type, not
// collapsed into one number (§9.1): the Summary carries one TypeReport per
// asset type, and a weak type's low metric is visible on its own line.
func TestRun_PerTypeNoAggregation(t *testing.T) {
	b := newSeedFakeBroker()
	summary, err := Run(context.Background(), b, ctxmod.Principal{}, []CaseSet{SeedCaseSet})
	require.NoError(t, err)
	types := make(map[domain.AssetType]bool)
	for _, r := range summary.ByType {
		types[r.AssetType] = true
	}
	assert.True(t, types[domain.AssetTypeDocument])
	assert.True(t, types[domain.AssetTypeCodebase])
	assert.True(t, types[domain.AssetTypeMemory])
	assert.True(t, types[domain.AssetTypeSkill])
}

// TestRun_ErrorFromBrokerPropagates asserts a broker Execute error (a caller/
// config fault — not a provider fault, which is a DegradedSource) surfaces as
// a run error, not silently scored as a miss.
func TestRun_ErrorFromBrokerPropagates(t *testing.T) {
	b := fakeBroker{err: errBrokerFault}
	_, err := Run(context.Background(), b, ctxmod.Principal{}, []CaseSet{SeedCaseSet})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "broker fault")
}

// TestRun_EmptyCaseSetVacuouslyPasses asserts an empty case set vacuously
// passes (no gate to fail) — the runner stays exercisable before the test
// engineer's cases land (same codegraph/eval precedent).
func TestRun_EmptyCaseSetVacuouslyPasses(t *testing.T) {
	b := fakeBroker{}
	summary, err := Run(context.Background(), b, ctxmod.Principal{}, []CaseSet{{DatasetTag: "empty"}})
	require.NoError(t, err)
	assert.Empty(t, summary.ByType)
	assert.True(t, summary.CitationOK, "no citation cases → vacuously passes")
	assert.True(t, summary.Pass())
}

// TestFormatReport_NoAggregation asserts the text report emits one line per
// asset type (not a single averaged metric), so a weak type is legible.
func TestFormatReport_NoAggregation(t *testing.T) {
	b := newSeedFakeBroker()
	summary, err := Run(context.Background(), b, ctxmod.Principal{}, []CaseSet{SeedCaseSet})
	require.NoError(t, err)
	rep := FormatReport(summary)
	assert.Contains(t, rep, "document:")
	assert.Contains(t, rep, "codebase:")
	assert.Contains(t, rep, "memory:")
	assert.Contains(t, rep, "skill:")
	assert.Contains(t, rep, "citation=true")
}

// errBrokerFault is a non-provider error a broken wiring surfaces (distinct
// from a DegradedSource, which is a port fault scored as a three-state case).
var errBrokerFault = brokerFault{}

type brokerFault struct{}

func (brokerFault) Error() string { return "broker fault" }
