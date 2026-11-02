// Package eval — runner scoring-loop tests (design-docs/19 §9.1).
//
// These exercise Run() against a fake ContextBroker that returns a fixed
// ContextResponse per case, so the scoring math (recall / nDCG / citation /
// conflict-retention / latency) is verified without a live fan-out — same
// offline-eval precedent as codegraph/eval's NoopProvider path. The TE's
// DefaultCaseSets (cases_test.go) are the data; this file adds the harness
// that wires a deterministic broker + asserts the gate booleans.
package eval

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	contextbroker "github.com/lynn901/mora/internal/module/knowledge/context"
)

// fakeBroker implements ContextBroker by returning a canned ContextResponse
// keyed by the case's query (queries are unique across DefaultCaseSets, so the
// query alone is a stable key without re-deriving the intent). It records the
// calls so a test can assert the runner actually invoked Execute.
type fakeBroker struct {
	responses map[string]contextbroker.ContextResponse // key: query
	calls     []contextbroker.KnowledgeQuery
}

func (f *fakeBroker) Execute(_ context.Context, q contextbroker.KnowledgeQuery, _ contextbroker.Principal) (contextbroker.ContextResponse, error) {
	f.calls = append(f.calls, q)
	resp, ok := f.responses[q.Query]
	if !ok {
		// Default: empty response (genuine no-results) — exercises the
		// empty-expected happy path for cases with no canned response.
		return contextbroker.ContextResponse{}, nil
	}
	return resp, nil
}

// mkCand builds a KnowledgeCandidate with a citation anchor (the shape the
// runner's citation + relation checks read).
func mkCand(assetID uuid.UUID, t domain.AssetType, ver string, rels ...contextbroker.RelationSummary) contextbroker.KnowledgeCandidate {
	return contextbroker.KnowledgeCandidate{
		AssetID:   assetID,
		AssetType: t,
		Citation: contextbroker.Citation{
			AssetID:           assetID,
			AssetType:         t,
			VersionOrRevision: ver,
		},
		Relations: rels,
	}
}

// evalPrincipal is the fixed principal the runner scores under. The fake broker
// ignores it; the real broker would authorize it (the harness seeds the eval
// workspace with full use on the dataset assets).
func evalPrincipal() contextbroker.Principal {
	uid := uuid.MustParse("00000000-0000-0000-0000-e00000000001")
	return contextbroker.Principal{
		SubjectType: domain.SubjectUser,
		PrincipalID: uid,
	}
}

// TestRun_HappyPath_AllGatesPass wires a fake broker that returns exactly the
// expected candidates + citations for every case in DefaultCaseSets, and
// asserts the run passes all three gates (recall / citation / latency). This
// is the CI-gate shape: a green run means the broker, the case set, and the
// runner agree.
func TestRun_HappyPath_AllGatesPass(t *testing.T) {
	b := &fakeBroker{responses: happyResponses()}
	summary, err := Run(context.Background(), b, DefaultCaseSets, evalPrincipal())
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if !summary.RecallOK {
		t.Errorf("RecallOK = false; want true\n%s", FormatReport(summary))
	}
	if !summary.CitationOK {
		t.Errorf("CitationOK = false; want true\n%s", FormatReport(summary))
	}
	if !summary.LatencyOK {
		t.Errorf("LatencyOK = false; want true (p95=%dms)", summary.P95LatencyMS)
	}
	if !summary.Pass() {
		t.Errorf("Pass() = false; want true\n%s", FormatReport(summary))
	}
	if got := len(b.calls); got != CaseCount() {
		t.Errorf("broker Execute calls = %d; want %d (one per case)", got, CaseCount())
	}
}

// TestRun_RecallMiss_SurfacesMissing asserts that a candidate the broker omits
// (not in the returned set) is recorded as a CaseFailure.Missing and flips
// RecallOK to false (§9.1: a weak type must not be masked).
func TestRun_RecallMiss_SurfacesMissing(t *testing.T) {
	sets := singleCaseSet(IntentKindSpec, "spec miss", []domain.AssetType{domain.AssetTypeDocument},
		[]uuid.UUID{specDocV2, specDocV1Old}, // expect both
		[]ExpectedCitation{
			{AssetID: specDocV2, VersionOrRevision: "doc-v2"},
			{AssetID: specDocV1Old, VersionOrRevision: "doc-v1"},
		})
	// Broker returns only specDocV2 → specDocV1Old is missing → recall 0.5 < 0.9.
	b := &fakeBroker{responses: map[string]contextbroker.ContextResponse{
		"spec miss": {Candidates: []contextbroker.KnowledgeCandidate{
			mkCand(specDocV2, domain.AssetTypeDocument, "doc-v2"),
		}},
	}}
	summary, err := Run(context.Background(), b, sets, evalPrincipal())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.RecallOK {
		t.Fatalf("RecallOK = true; want false (specDocV1Old missing)\n%s", FormatReport(summary))
	}
	// The spec/document bucket must carry the failure with the missing id.
	var found bool
	for _, r := range summary.ByType {
		if r.Intent != IntentKindSpec || r.AssetType != domain.AssetTypeDocument {
			continue
		}
		for _, f := range r.Failed {
			if contains(f.Missing, specDocV1Old.String()) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected a spec/document failure naming missing %s; report:\n%s", specDocV1Old, FormatReport(summary))
	}
}

// TestRun_CitationMiss asserts a wrong citation anchor is recorded as a
// CitationMiss and flips CitationOK (§9.1 CitationAccuracy ≥ 0.95).
func TestRun_CitationMiss(t *testing.T) {
	sets := singleCaseSet(IntentKindRevision, "cite miss", []domain.AssetType{domain.AssetTypeCodebase},
		[]uuid.UUID{revCodeCommit},
		[]ExpectedCitation{{AssetID: revCodeCommit, VersionOrRevision: "commit-a1b2c3d"}})
	// Broker returns the right asset but a wrong version anchor.
	b := &fakeBroker{responses: map[string]contextbroker.ContextResponse{
		"cite miss": {Candidates: []contextbroker.KnowledgeCandidate{
			mkCand(revCodeCommit, domain.AssetTypeCodebase, "commit-WRONG"),
		}},
	}}
	summary, err := Run(context.Background(), b, sets, evalPrincipal())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.CitationOK {
		t.Fatalf("CitationOK = true; want false (wrong version anchor)\n%s", FormatReport(summary))
	}
}

// TestRun_ConflictRelationDropped asserts a conflict relation the broker drops
// (the §7.2 retention invariant) is recorded as a RelationMiss — the conflict
// pair was silently chosen instead of kept side-by-side.
func TestRun_ConflictRelationDropped(t *testing.T) {
	sets := singleCaseSetWithRels(IntentKindRationale, "conflict drop",
		[]domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
		[]uuid.UUID{ratDocDecision, ratMemSuperseded},
		[]ExpectedCitation{
			{AssetID: ratDocDecision, VersionOrRevision: "doc-decision-1"},
			{AssetID: ratMemSuperseded, VersionOrRevision: "evidence-old"},
		},
		[]ExpectedRelation{
			{RelationType: "contradicts", SourceAsset: ratDocDecision, TargetAsset: ratMemSuperseded},
		})
	// Broker returns both candidates but DROPS the contradicts relation on
	// ratDocDecision → a §7.2 retention regression.
	b := &fakeBroker{responses: map[string]contextbroker.ContextResponse{
		"conflict drop": {Candidates: []contextbroker.KnowledgeCandidate{
			mkCand(ratDocDecision, domain.AssetTypeDocument, "doc-decision-1"), // no relations
			mkCand(ratMemSuperseded, domain.AssetTypeMemory, "evidence-old"),
		}},
	}}
	summary, err := Run(context.Background(), b, sets, evalPrincipal())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Recall + citation pass (both candidates + anchors present), but the
	// dropped relation must still flip the bucket to failed.
	var hasRelMiss bool
	for _, r := range summary.ByType {
		for _, f := range r.Failed {
			if len(f.RelationMisses) > 0 {
				hasRelMiss = true
			}
		}
	}
	if !hasRelMiss {
		t.Fatalf("expected a RelationMiss; report:\n%s", FormatReport(summary))
	}
}

// TestRun_EmptyExpected_LeakIsFailure asserts that a case expecting NO
// candidates (a low-authority type that should surface nothing, e.g. spec/
// memory) but where the broker leaks one is recorded as a precision failure
// (§9.1 — a leak must not be masked as a vacuous pass).
func TestRun_EmptyExpected_LeakIsFailure(t *testing.T) {
	sets := singleCaseSet(IntentKindSpec, "leak", []domain.AssetType{domain.AssetTypeMemory},
		nil, nil) // expect nothing
	b := &fakeBroker{responses: map[string]contextbroker.ContextResponse{
		"leak": {Candidates: []contextbroker.KnowledgeCandidate{
			mkCand(specMemLow, domain.AssetTypeMemory, "ev-leak"),
		}},
	}}
	summary, err := Run(context.Background(), b, sets, evalPrincipal())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if summary.RecallOK {
		t.Fatalf("RecallOK = true; want false (leaked candidate into an empty-expected case)\n%s", FormatReport(summary))
	}
}

// TestRun_PolicyError_PipelineStartFailure asserts a pipeline-start error (the
// broker returns ErrBrokerNoWorkspace because the case's WorkspaceID is
// invalid) is recorded as a per-case failure rather than aborting the whole
// run (so one bad fixture does not mask the rest).
func TestRun_PolicyError_PipelineStartFailure(t *testing.T) {
	sets := []CaseSet{{
		DatasetTag: "phase6-v1-err",
		Cases: []Case{
			{
				Intent:             IntentKindSpec,
				Query:              "bad ws",
				WorkspaceID:        "not-a-uuid", // → uuid.Nil → ErrBrokerNoWorkspace
				AssetTypes:         []domain.AssetType{domain.AssetTypeDocument},
				ExpectedCandidates: []uuid.UUID{specDocV2},
			},
		},
	}}
	b := &fakeBroker{responses: map[string]contextbroker.ContextResponse{}}
	// The fake broker ignores workspace validity (returns empty); the real
	// broker would error. To exercise the error path against the fake, we wrap.
	wb := &erroringBroker{inner: b, err: contextbroker.ErrBrokerNoWorkspace}
	summary, err := Run(context.Background(), wb, sets, evalPrincipal())
	if err != nil {
		t.Fatalf("Run: a per-case pipeline error must not abort the run: %v", err)
	}
	if summary.RecallOK {
		t.Errorf("RecallOK = true; want false (the case errored, its expected set unsurfaced)")
	}
}

// erroringBroker wraps a fakeBroker and forces an error on every Execute (used
// to exercise the runner's per-case-error path without a real broker).
type erroringBroker struct {
	inner ContextBroker
	err   error
}

func (e *erroringBroker) Execute(ctx context.Context, q contextbroker.KnowledgeQuery, p contextbroker.Principal) (contextbroker.ContextResponse, error) {
	_, _ = e.inner.Execute(ctx, q, p)
	return contextbroker.ContextResponse{}, e.err
}

// happyResponses builds the canned broker responses that make every
// DefaultCaseSets case pass: each returns exactly its ExpectedCandidates with
// matching citation anchors and (for the conflict cases) the expected
// relations. This is the "broker agrees with the case set" baseline.
func happyResponses() map[string]contextbroker.ContextResponse {
	out := make(map[string]contextbroker.ContextResponse, CaseCount())
	for _, set := range DefaultCaseSets {
		for _, c := range set.Cases {
			var cands []contextbroker.KnowledgeCandidate
			for _, id := range expectedUUIDs(c) {
				ver := citationVerFor(c, id)
				rels := relationsFor(c, id)
				cands = append(cands, mkCand(id, typeOf(c, id), ver, rels...))
			}
			out[c.Query] = contextbroker.ContextResponse{
				Candidates: cands,
				Intent:     toBrokerIntent(c.Intent),
			}
		}
	}
	return out
}

// citationVerFor looks up the expected VersionOrRevision for asset id in the
// case's ExpectedCitations (so the happy-path broker returns matching anchors).
func citationVerFor(c Case, id uuid.UUID) string {
	for _, e := range c.ExpectedCitations {
		if e.AssetID == id {
			return e.VersionOrRevision
		}
	}
	return ""
}

// relationsFor returns the ExpectedRelations whose SourceAsset == id, mapped to
// the broker's RelationSummary shape (so the happy-path broker keeps the
// conflict pair side-by-side).
func relationsFor(c Case, id uuid.UUID) []contextbroker.RelationSummary {
	var out []contextbroker.RelationSummary
	for _, e := range c.ExpectedRelations {
		if e.SourceAsset != id {
			continue
		}
		out = append(out, contextbroker.RelationSummary{
			RelationType: e.RelationType,
			TargetID:     e.TargetAsset,
		})
	}
	return out
}

// typeOf infers the asset type for an expected id from the fixture's well-known
// id→type mapping (so multi-type cases attribute each candidate to the right
// type for the runner's per-(intent,type) bucketing). Falls back to the case's
// first declared type for unknown ids.
func typeOf(c Case, id uuid.UUID) domain.AssetType {
	switch id {
	case specDocV2, specDocV1Old, revDocIncon, ratDocDecision, procDocRunbook:
		return domain.AssetTypeDocument
	case specCodeDrift, revCodeCommit:
		return domain.AssetTypeCodebase
	case specMemLow, revMemEvidence, ratMemEvidence, ratMemSuperseded:
		return domain.AssetTypeMemory
	case procSkillV2, procSkillV1MM:
		return domain.AssetTypeSkill
	}
	if len(c.AssetTypes) > 0 {
		return c.AssetTypes[0]
	}
	return domain.AssetTypeDocument
}

// singleCaseSet builds a one-case CaseSet with expected candidates + citations
// (no relations) for the focused tests.
func singleCaseSet(intent IntentKind, query string, types []domain.AssetType,
	ids []uuid.UUID, cits []ExpectedCitation) []CaseSet {
	c := Case{Intent: intent, Query: query, WorkspaceID: "ws-eval-focus", AssetTypes: types,
		ExpectedCandidates: ids, ExpectedCitations: cits}
	c.ExpectedIDs = idsToStrings(ids)
	return []CaseSet{{DatasetTag: "phase6-v1-focus", Cases: []Case{c}}}
}

// singleCaseSetWithRels is singleCaseSet + expected conflict relations.
func singleCaseSetWithRels(intent IntentKind, query string, types []domain.AssetType,
	ids []uuid.UUID, cits []ExpectedCitation, rels []ExpectedRelation) []CaseSet {
	c := Case{Intent: intent, Query: query, WorkspaceID: "ws-eval-focus", AssetTypes: types,
		ExpectedCandidates: ids, ExpectedCitations: cits, ExpectedRelations: rels}
	c.ExpectedIDs = idsToStrings(ids)
	return []CaseSet{{DatasetTag: "phase6-v1-focus", Cases: []Case{c}}}
}

func idsToStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestCaseCount_Eighteen asserts the DoD coverage: 16 base + 2 conflict = 18.
func TestCaseCount_Eighteen(t *testing.T) {
	if got := CaseCount(); got != 18 {
		t.Errorf("CaseCount() = %d; want 18 (16 base + 2 conflict)", got)
	}
}
