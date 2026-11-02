// Package eval — Context Broker capability evaluation runner (design-docs/19
// §9.1). This is the acceptance-gate carrier for the Phase 6 broker. It loads
// a case set (queries + expected answers per intent and asset type), runs them
// through a ContextBroker, and scores:
//   - Recall@K     — recall, K locked by the dataset;
//   - nDCG         — ordering quality;
//   - CitationAccuracy — citation correctness (≥ 95% gate);
//   - P95 Latency  — end-to-end broker latency (P95 ≤ 2s, 12 §14.3 SLO).
//
// Metrics are reported per (document/code/memory/skill) asset type, NOT
// aggregated into a single number — same precedent as codegraph/eval (§9.1: a
// weak type must not be masked by a strong type's mean). The runner is the
// skeleton 研发 implements; the concrete case set (expected answers per
// intent × type) lands collaboratively with the test engineer
// ([@Mora知识库测试工程师](mention://agent/9066a6d6-f66a-45cb-844e-81203f6cd137),
// §9.1 / D11). Cases are plain Go (a slice literal) so a CI gate can run
// `go test ./internal/module/knowledge/context/eval/` once a broker + ports
// are wired.
//
// The runner does NOT spin up providers: it expects an injected ContextBroker
// (real ports in CI, a fake broker in the skeleton self-test). The broker is
// taken as an INTERFACE, not the concrete *context.Broker — same NoopProvider
// precedent as codegraph/eval, so the runner scores a stub without depending
// on the Broker implementation package.
package eval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	ctxmod "github.com/lynn901/mora/internal/module/knowledge/context"
)

// CaseKind tags an eval case with the scenario it exercises (§11.5 three-state
// distinguishability + §7.2 conflict preservation). The four three-state kinds
// MUST be distinguishable through the ContextResponse methods; the conflict-
// preservation kind asserts conflicting candidates survive side-by-side.
type CaseKind string

const (
	// CaseKindNormal: a query with expected hits. Recall@K / nDCG / citation
	// are scored against ExpectedCandidates/ExpectedCitations.
	CaseKindNormal CaseKind = "normal"
	// CaseKindAuthorizedEmpty: the principal has no use on the matching assets;
	// the Broker must surface an authorized-empty state (IsEmptyAuthorized,
	// §11.5) — never a 403, never a faked candidate. ExpectedCandidates stays
	// empty; the assertion is purely the three-state classification.
	CaseKindAuthorizedEmpty CaseKind = "authorized_empty"
	// CaseKindNoResults: the knowledge base genuinely has nothing for the query.
	// The Broker must surface IsEmptyNoResults (§11.5) — no DegradedSources, no
	// candidates. Distinguishable from authorized-empty and provider-down.
	CaseKindNoResults CaseKind = "no_results"
	// CaseKindProviderDown: a typed port faulted. The Broker must surface
	// IsProviderDown (§11.5) with DegradedSources non-empty — the empty result
	// is a fault, NOT "no knowledge". The runner must never score a fault as a
	// recall miss (that would mask a flaky provider).
	CaseKindProviderDown CaseKind = "provider_down"
	// CaseKindConflictPreservation: high-authority candidates conflict; the
	// Broker must keep both side-by-side (§7.2) instead of picking one.
	// ExpectedRelations asserts the must-surface conflict relation survives.
	CaseKindConflictPreservation CaseKind = "conflict_preservation"
)

// Case is one eval case: a query, the intent + asset type it targets, and the
// expected answer set the Broker response is scored against.
//
//   - For CaseKindNormal: ExpectedCandidates is the reference asset-id set
//     (Recall@K = |returned ∩ expected| / |expected|); ExpectedCitations is the
//     set of (asset_id → version_or_revision) anchors CitationAccuracy checks;
//     ExpectedRelations asserts specific conflict relations survive (§7.2).
//   - For the three-state kinds (authorized_empty/no_results/provider_down):
//     ExpectedCandidates stays empty; the case asserts the ContextResponse
//     classifies into exactly the named state.
//   - For CaseKindConflictPreservation: ExpectedRelations is the conflict
//     relation types that MUST surface side-by-side (§7.2 must-surface).
type Case struct {
	Kind        CaseKind
	Intent      ctxmod.Intent // spec/revision/rationale/procedure (§4.1)
	AssetType   domain.AssetType
	Query       string
	WorkspaceID uuid.UUID
	// AssetTypes is the caller-declared type set; empty = the IntentRouter picks
	// (§4.2). Set it to pin a single type so the per-type report is unambiguous.
	AssetTypes []domain.AssetType
	// ExpectedCandidates is the reference answer set (asset ids) for normal
	// cases. Recall@K is computed against it. Empty for the three-state kinds.
	ExpectedCandidates []uuid.UUID
	// ExpectedK is K for Recall@K (0 = len(ExpectedCandidates)).
	ExpectedK int
	// ExpectedCitations maps asset_id → expected version_or_revision anchor
	// (document=version_id, codebase=commit, memory=evidence_id, skill=
	// package_version). A returned citation with a matching asset_id whose
	// VersionOrRevision equals the expected value counts as accurate.
	ExpectedCitations map[uuid.UUID]string
	// ExpectedRelations is the set of relation types that MUST surface
	// (CaseKindConflictPreservation / §7.2 must-surface). The runner asserts
	// each appears on at least one returned candidate's Relations.
	ExpectedRelations []string
	// MaxItems / MaxTokens pin the budget for this case (0 = default).
	MaxItems  int
	MaxTokens int
}

// CaseSet is a dataset-tagged collection of cases. The runner pins scoring to a
// DatasetTag so a release can lock the threshold against a specific dataset
// version (§9.1 "发布前锁定当期数据集阈值").
type CaseSet struct {
	DatasetTag string
	Cases      []Case
}

// TypeReport is the per-asset-type scoring breakdown (§9.1: do NOT collapse
// types into one number). The runner emits one of these per asset type so a
// weak type is visible on its own — recall/citation/latency are reported per
// type, never averaged across types.
type TypeReport struct {
	AssetType         domain.AssetType
	RecallCases       int     // normal cases that scored recall against this type
	RecallAtK         float64 // mean per-case recall@K across the type's cases
	NDCG              float64 // mean nDCG across the type's cases
	CitationCases     int     // cases with citation anchors to check
	CitationAccuracy  float64 // accurate citations / checked citations (gate ≥ 0.95)
	CitationThreshold float64 // default 0.95 (§9.1)
	ThreeStateCases   int     // authorized-empty/no-results/provider-down cases
	ThreeStateOK      int     // classified into the expected state
	ConflictCases     int     // conflict-preservation cases
	ConflictOK        int     // expected relations surfaced
	P95LatencyMS      int64   // end-to-end P95 latency for the type's cases (gate ≤ 2000)
	Failed            []CaseFailure
}

// CaseFailure records one case that missed its expectation, with the detail a
// test engineer needs to triage (expected vs actual).
type CaseFailure struct {
	Case    Case
	Got     []uuid.UUID // returned candidate asset ids
	Missing []uuid.UUID // expected ids not surfaced
	Extra   []uuid.UUID // surfaced ids not in expected (precision signal)
	Reason  string      // human-readable miss reason (recall / citation / state / conflict)
}

// Summary is the aggregate the runner emits for a whole run. Per-type metrics
// are kept separate (§9.1: do NOT collapse to one number).
type Summary struct {
	DatasetTag   string
	ByType       []TypeReport
	P95LatencyMS int64 // overall end-to-end P95 (gate ≤ 2000, 12 §14.3)
	LatencyOK    bool  // P95 ≤ 2000ms across every type
	RecallOK     bool  // every type scored recall (≥0 cases) — vacuously true when no recall cases exist
	CitationOK   bool  // citation accuracy ≥ 95%, every type with citation cases
}

// Pass reports whether the whole run satisfies the gates. §9.1 hard gate is
// CitationAccuracy ≥ 95%; latency (P95 ≤ 2s) and recall are reported alongside
// but the binding pass/fail is the citation gate AND'd across every type that
// has citation cases (a type with no citation cases vacuously passes citation;
// a type with citation cases below 95% fails the run). Same AND-across-layers
// shape as codegraph/eval (§7.1): a weak type must not be masked by a strong
// one's mean.
func (s Summary) Pass() bool { return s.CitationOK && s.LatencyOK }

// Broker is the narrow slice of the context.Broker the runner needs (just
// Execute). Defined here, consumer-side, so the runner takes the real
// *context.Broker in CI or a fake broker that short-circuits to a fixed
// response — same NoopProvider precedent as codegraph/eval. The production
// *ctxmod.Broker satisfies this interface implicitly (its Execute signature
// matches exactly).
type Broker interface {
	Execute(ctx context.Context, q ctxmod.KnowledgeQuery, principal ctxmod.Principal) (ctxmod.ContextResponse, error)
}

// Run executes a case set against a ContextBroker and returns the per-type
// scoring summary. It is the single entrypoint the CI gate / the test
// engineer's harness calls. The broker is the ContextBroker under test (real
// ports in CI, a fake broker in the skeleton self-test).
//
// principal is the evaluator identity — a caller authorized to see the
// dataset's assets so the test measures retrieval quality, not authz
// filtering. The runner does NOT build assets: it expects the workspace to
// already hold the dataset's assets + versions. The build path is exercised
// by the §9.3 integration tests, not here.
//
// P95 latency is sampled per case end-to-end (the wall time from Execute call
// to response), reported per type and overall.
//
// typeAcc is the per-asset-type accumulator Run builds while scoring: the
// in-progress TypeReport plus the sample slices the per-type means/P95 are
// computed from. It is package-scoped so scoreCase can append to it directly
// (keeping the scoring loop in one function).
type typeAcc struct {
	rep             TypeReport
	recallSamples   []float64
	ndcgSamples     []float64
	citationHit     int
	citationTotal   int
	threeStateHit   int
	threeStateTotal int
	conflictHit     int
	conflictTotal   int
	latencies       []time.Duration
}

func Run(ctx context.Context, b Broker, principal ctxmod.Principal, sets []CaseSet) (Summary, error) {
	var summary Summary
	accs := make(map[domain.AssetType]*typeAcc)
	ensure := func(t domain.AssetType) *typeAcc {
		if a, ok := accs[t]; ok {
			return a
		}
		a := &typeAcc{rep: TypeReport{
			AssetType:         t,
			CitationThreshold: 0.95,
		}}
		accs[t] = a
		return a
	}

	for _, set := range sets {
		if summary.DatasetTag == "" {
			summary.DatasetTag = set.DatasetTag
		}
		for _, c := range set.Cases {
			acc := ensure(c.AssetType)
			start := time.Now()
			q := ctxmod.KnowledgeQuery{
				Query:       c.Query,
				WorkspaceID: c.WorkspaceID,
				AssetTypes:  append([]domain.AssetType(nil), c.AssetTypes...),
				MaxItems:    c.MaxItems,
				MaxTokens:   c.MaxTokens,
				Timeout:     2 * time.Second, // §14.3 SLO
			}
			resp, err := b.Execute(ctx, q, principal)
			latency := time.Since(start)
			if err != nil {
				// A provider fault surfaces as DegradedSources, NOT a run error
				// (§7.1). An error here is a caller/config fault (no workspace /
				// no principal / authz pre-fetch failure) — propagate it so a
				// broken wiring is visible, not silently scored as a miss.
				return summary, fmt.Errorf("eval case %q: %w", c.Query, err)
			}
			acc.latencies = append(acc.latencies, latency)
			scoreCase(acc, c, resp)
		}
	}

	// Finalize per-type aggregates: means, P95, gate booleans.
	allLatencies := make([]time.Duration, 0)
	citationOK := true
	recallReported := false
	for _, a := range accs {
		a.rep.RecallAtK = mean(a.recallSamples)
		a.rep.NDCG = mean(a.ndcgSamples)
		if a.citationTotal > 0 {
			a.rep.CitationAccuracy = float64(a.citationHit) / float64(a.citationTotal)
			if a.rep.CitationAccuracy < a.rep.CitationThreshold {
				citationOK = false
			}
		}
		a.rep.ThreeStateCases = a.threeStateTotal
		a.rep.ThreeStateOK = a.threeStateHit
		a.rep.ConflictCases = a.conflictTotal
		a.rep.ConflictOK = a.conflictHit
		a.rep.P95LatencyMS = percentileMS(a.latencies, 0.95)
		if len(a.recallSamples) > 0 {
			recallReported = true // recall is reported, not gated in the first version (§9.1)
		}
		allLatencies = append(allLatencies, a.latencies...)
		summary.ByType = append(summary.ByType, a.rep)
	}
	// Stable order by asset type for deterministic reporting.
	sort.Slice(summary.ByType, func(i, j int) bool {
		return string(summary.ByType[i].AssetType) < string(summary.ByType[j].AssetType)
	})

	summary.P95LatencyMS = percentileMS(allLatencies, 0.95)
	summary.LatencyOK = summary.P95LatencyMS <= 2000
	summary.RecallOK = recallReported
	summary.CitationOK = citationOK
	return summary, nil
}

// scoreCase scores one case against the Broker response and accumulates into
// the per-type report + accumulator. Normal cases contribute recall/nDCG/
// citation; three-state cases contribute the state-classification assertion;
// conflict-preservation cases contribute the must-surface-relation assertion.
func scoreCase(a *typeAcc, c Case, resp ctxmod.ContextResponse) {
	rep := &a.rep
	gotIDs := candidateIDs(resp.Candidates)

	switch c.Kind {
	case CaseKindNormal:
		rep.RecallCases++
		expected := c.ExpectedCandidates
		k := c.ExpectedK
		if k <= 0 || k > len(expected) {
			k = len(expected)
		}
		recall := recallAtK(gotIDs, expected, k)
		a.recallSamples = append(a.recallSamples, recall)
		a.ndcgSamples = append(a.ndcgSamples, ndcg(gotIDs, expected))
		missing := diffIDs(expected, gotIDs)
		extra := diffIDs(gotIDs, expected)
		if recall < 1.0 || len(missing) > 0 {
			rep.Failed = append(rep.Failed, CaseFailure{Case: c, Got: gotIDs, Missing: missing, Extra: extra, Reason: "recall miss"})
		}
		// Citation accuracy: each expected citation anchor that the response
		// carries must point at the expected version_or_revision. A case with
		// at least one ExpectedCitations entry counts as a citation case.
		if len(c.ExpectedCitations) > 0 {
			rep.CitationCases++
		}
		for assetID, wantRev := range c.ExpectedCitations {
			a.citationTotal++
			if cit, ok := citationFor(resp.Candidates, assetID); ok && cit.VersionOrRevision == wantRev {
				a.citationHit++
			} else if len(missing) == 0 {
				rep.Failed = append(rep.Failed, CaseFailure{Case: c, Reason: fmt.Sprintf("citation mismatch for %s (want %q)", assetID, wantRev)})
			}
		}

	case CaseKindAuthorizedEmpty:
		a.threeStateTotal++
		if resp.IsEmptyAuthorized() {
			a.threeStateHit++
		} else {
			rep.Failed = append(rep.Failed, CaseFailure{Case: c, Reason: fmt.Sprintf("expected authorized-empty, got state=%s", classifyState(resp))})
		}

	case CaseKindNoResults:
		a.threeStateTotal++
		if resp.IsEmptyNoResults() {
			a.threeStateHit++
		} else {
			rep.Failed = append(rep.Failed, CaseFailure{Case: c, Reason: fmt.Sprintf("expected no-results, got state=%s", classifyState(resp))})
		}

	case CaseKindProviderDown:
		a.threeStateTotal++
		if resp.IsProviderDown() {
			a.threeStateHit++
		} else {
			rep.Failed = append(rep.Failed, CaseFailure{Case: c, Reason: fmt.Sprintf("expected provider-down, got state=%s", classifyState(resp))})
		}

	case CaseKindConflictPreservation:
		a.conflictTotal++
		surfaced := surfacedRelationTypes(resp.Candidates)
		ok := true
		var missing []string
		for _, want := range c.ExpectedRelations {
			if !containsStr(surfaced, want) {
				ok = false
				missing = append(missing, want)
			}
		}
		if ok {
			a.conflictHit++
		} else {
			rep.Failed = append(rep.Failed, CaseFailure{Case: c, Reason: fmt.Sprintf("must-surface relations missing: %v", missing)})
		}
	}
}

// classifyState returns the three-state label of a response for failure
// messages (§11.5). Exactly one of HasResults / IsEmptyAuthorized /
// IsEmptyNoResults / IsProviderDown holds.
func classifyState(r ctxmod.ContextResponse) string {
	switch {
	case r.HasResults():
		return "has_results"
	case r.IsProviderDown():
		return "provider_down"
	case r.IsEmptyAuthorized():
		return "authorized_empty"
	case r.IsEmptyNoResults():
		return "no_results"
	default:
		return "unknown"
	}
}

// --- scoring helpers ---

// recallAtK = |topK(returned) ∩ expected| / |expected|. expected empty → 0
// (an authoring error; surfaced as a failure, not a silent pass).
func recallAtK(returned, expected []uuid.UUID, k int) float64 {
	if len(expected) == 0 {
		return 0
	}
	if k <= 0 {
		k = len(expected)
	}
	topK := returned
	if len(topK) > k {
		topK = topK[:k]
	}
	hits := 0
	for _, id := range topK {
		if containsID(expected, id) {
			hits++
		}
	}
	return float64(hits) / float64(len(expected))
}

// ndcg computes normalized DCG over the returned ordering against the expected
// relevance set. A returned id is relevant iff it is in expected. DCG uses
// the standard 1/log2(rank+1) discount; IDCG is the ideal ordering (all
// relevant first). nDCG = DCG / IDCG; empty expected → 0.
func ndcg(returned, expected []uuid.UUID) float64 {
	if len(expected) == 0 {
		return 0
	}
	dcg := 0.0
	for i, id := range returned {
		if containsID(expected, id) {
			dcg += 1.0 / math.Log2(float64(i+2)) // rank i is 0-indexed → position i+1 → log2(i+2)
		}
	}
	idealHits := len(expected)
	if idealHits > len(returned) {
		idealHits = len(returned)
	}
	idcg := 0.0
	for i := 0; i < idealHits; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// candidateIDs extracts the asset ids from a candidate slice, preserving order.
func candidateIDs(cs []ctxmod.KnowledgeCandidate) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.AssetID)
	}
	return out
}

// citationFor looks up a candidate by asset id and returns its Citation.
func citationFor(cs []ctxmod.KnowledgeCandidate, assetID uuid.UUID) (ctxmod.Citation, bool) {
	for _, c := range cs {
		if c.AssetID == assetID {
			return c.Citation, true
		}
	}
	return ctxmod.Citation{}, false
}

// surfacedRelationTypes collects the union of RelationType values across all
// candidates (§7.2 — the must-surface relations the Broker kept side-by-side).
func surfacedRelationTypes(cs []ctxmod.KnowledgeCandidate) []string {
	seen := make(map[string]bool)
	var out []string
	for _, c := range cs {
		for _, r := range c.Relations {
			if !seen[r.RelationType] {
				seen[r.RelationType] = true
				out = append(out, r.RelationType)
			}
		}
	}
	return out
}

func containsID(set []uuid.UUID, id uuid.UUID) bool {
	for _, x := range set {
		if x == id {
			return true
		}
	}
	return false
}

func containsStr(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// diffIDs returns elements of a that are not in b (a minus b).
func diffIDs(a, b []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0)
	for _, x := range a {
		if !containsID(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// percentileMS returns the p-th percentile (0..1) of a latency sample set in
// milliseconds. p=0.95 is the §14.3 SLO percentile. Returns 0 for an empty set.
func percentileMS(samples []time.Duration, p float64) int64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// Nearest-rank percentile: rank = ceil(p * n), clamped to [1, n].
	n := len(sorted)
	rank := int(math.Ceil(p * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1].Milliseconds()
}

// FormatReport renders a Summary as a per-type text report (no aggregation
// into a single recall number — §9.1). Used by the CLI / test output.
func FormatReport(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "context eval: dataset=%s citation=%v latency=%v (p95=%dms)\n",
		s.DatasetTag, s.CitationOK, s.LatencyOK, s.P95LatencyMS)
	for _, r := range s.ByType {
		fmt.Fprintf(&b, "  %s: recall@k %.0f%% ndcg %.2f citation %.0f%% (≥%.0f%%) three_state %d/%d conflict %d/%d p95=%dms failures=%d\n",
			r.AssetType, r.RecallAtK*100, r.NDCG, r.CitationAccuracy*100, r.CitationThreshold*100,
			r.ThreeStateOK, r.ThreeStateCases, r.ConflictOK, r.ConflictCases, r.P95LatencyMS, len(r.Failed))
		for _, f := range r.Failed {
			fmt.Fprintf(&b, "    FAIL %s/%s query=%q reason=%s missing=%v extra=%v\n",
				f.Case.Kind, f.Case.AssetType, f.Case.Query, f.Reason, f.Missing, f.Extra)
		}
	}
	return b.String()
}
