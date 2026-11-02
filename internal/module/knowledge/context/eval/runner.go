// Package eval — Context Broker capability evaluation runner (design-docs/19
// §9.1 / D11). This is the acceptance-gate carrier for the Phase 6 broker. It
// loads a case set (queries + expected answers per intent), runs them through
// a wired ContextBroker, and scores:
//   - Recall@K     — recall, K locked by the dataset;
//   - nDCG         — ordering quality;
//   - CitationAccuracy — citation correctness (≥ 95% gate, §9.1);
//   - P95 Latency  — end-to-end broker latency (P95 ≤ 2s, 12 §14.3 SLO).
//
// Metrics are reported per (document/code/memory/skill) type, NOT aggregated
// into a single number — same precedent as codegraph/eval (§9.1: a weak type
// must not be masked by a strong type's mean). The runner is the skeleton
// 研发 implements; the concrete case set (expected answers per intent) lands
// collaboratively with the test engineer
// ([@Mora知识库测试工程师](mention://agent/9066a6d6-f66a-45cb-844e-81203f6cd137),
// §9.1). Cases are plain Go (a slice literal) so a CI gate can run
// `go test ./internal/module/knowledge/context/eval/` once a broker + ports
// are wired.
//
// The runner does NOT spin up providers: it expects a wired *ContextBroker
// (real ports in CI, a stub broker short-circuits to degraded_sources — see
// the codegraph/eval NoopProvider precedent). It runs the Broker's Execute
// (POST /knowledge/context path) — the same ten-step pipeline a real request
// walks — so the eval exercises the production path, not a parallel one.
package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	knowledgectx "github.com/lynn901/mora/internal/module/knowledge/context"
)

// IntentKind tags an eval case with the intent it exercises (§4.1). The runner
// groups scoring per (intent, asset_type) so recall is reported per-layer, not
// aggregated into a single mean (§9.1 — a weak layer must not be masked by a
// strong one's mean).
type IntentKind string

const (
	IntentKindSpec      IntentKind = "spec"
	IntentKindRevision  IntentKind = "revision"
	IntentKindRationale IntentKind = "rationale"
	IntentKindProcedure IntentKind = "procedure"
)

// ToContextIntent maps an eval IntentKind to the Broker's Intent (policy.go).
// Unknown kinds fall back to IntentSpec (the §4.2 rule-6 fallback policy).
func ToContextIntent(k IntentKind) knowledgectx.Intent {
	switch k {
	case IntentKindRevision:
		return knowledgectx.IntentRevision
	case IntentKindRationale:
		return knowledgectx.IntentRationale
	case IntentKindProcedure:
		return knowledgectx.IntentProcedure
	default:
		return knowledgectx.IntentSpec
	}
}

// Case is one eval case: a query + the expected asset ids (the reference
// answer set) + the intent it exercises. Recall@K = |returned ∩ expected| /
// |expected|; nDCG uses the broker's ordering; CitationAccuracy checks the
// returned citations point at the expected version/evidence.
type Case struct {
	Intent       IntentKind
	Query        string
	WorkspaceID  string // the workspace the case runs in (authz scope)
	AssetTypes   []domain.AssetType
	ExpectedIDs  []string // the reference answer set (asset ids)
	ExpectedK    int      // K for Recall@K (0 = len(ExpectedIDs))
	MaxTokens    int      // budget for this case (0 = default)
	// ExpectedVersions maps an expected asset id → the version/revision anchor
	// the citation MUST carry (§8.1: document=version_id, code=commit, memory=
	// evidence_id, skill=package_version). Empty entry = do not assert the
	// anchor for that id (recall-only). Used by CitationAccuracy.
	ExpectedVersions map[string]string
}

// CaseSet is a dataset-tagged collection of cases. The runner pins scoring to
// a dataset_tag so a release can lock the threshold against a specific dataset
// version (§9.1 "发布前锁定当期数据集阈值").
type CaseSet struct {
	DatasetTag string
	Cases      []Case
}

// TypeReport is the per-(intent, asset_type) scoring breakdown (§9.1: do NOT
// collapse types into one number). The runner emits one of these per
// (intent, asset_type) so a weak layer is visible on its own.
type TypeReport struct {
	Intent            IntentKind
	AssetType         domain.AssetType
	RecallCases       int
	RecallAtK         float64 // min per-case recall (the binding constraint)
	RecallThreshold   float64 // default 0.9 (first-version gate, §9.1)
	NDCG              float64
	CitationCases     int
	CitationAccuracy  float64    // gate ≥ 0.95 (§9.1)
	CitationThreshold float64    // default 0.95
	Failed            []CaseFailure
}

// CaseFailure records one case that missed its expectation, with the detail a
// test engineer needs to triage (expected vs actual).
type CaseFailure struct {
	Case    Case
	Got     []string
	Missing []string // expected ids not surfaced
	Extra   []string // surfaced ids not in expected (precision signal)
}

// Summary is the aggregate the runner emits for a whole run. Per-type recall
// is kept separate (§9.1: do NOT collapse to one number).
type Summary struct {
	DatasetTag    string
	ByType        []TypeReport
	P95LatencyMS  int64 // end-to-end broker P95 (gate ≤ 2000, 12 §14.3)
	LatencyOK     bool
	RecallOK      bool // recall ≥ threshold, every type
	CitationOK    bool // citation accuracy ≥ 95%, every type
}

// Pass reports whether the whole run satisfies all gates (recall + citation +
// latency). Used by the CI gate / release threshold lock (§9.1).
func (s Summary) Pass() bool { return s.RecallOK && s.CitationOK && s.LatencyOK }

// Run executes a case set against a ContextBroker and returns the per-type
// scoring summary. It is the single entrypoint the CI gate / the test
// engineer's harness calls. The broker is the *ContextBroker under test (real
// ports in CI, a stub broker short-circuits to degraded_sources).
//
// The runner does NOT build assets: it expects the workspace to already hold
// the dataset's assets + versions. The build path is exercised by the §9.3
// integration tests, not here.
//
// Per §9.1, recall/nDCG/citation are reported per (intent, asset_type) and
// NEVER averaged into a single number — a weak type must fail the run on its
// own even when a strong type would mask it under a mean. The gates are AND
// across types: one type below threshold fails the whole run.
func Run(ctx context.Context, b *knowledgectx.Broker, principal knowledgectx.Principal, sets []CaseSet) (Summary, error) {
	var summary Summary
	if b == nil {
		return summary, fmt.Errorf("context eval: broker is nil")
	}
	latencies := make([]int64, 0, 64) // per-case end-to-end latencies for P95

	for _, set := range sets {
		if summary.DatasetTag == "" {
			summary.DatasetTag = set.DatasetTag
		}
		for _, c := range set.Cases {
			caseStart := time.Now()
			resp, err := b.Execute(ctx, c.toQuery(), principal)
			caseMS := time.Since(caseStart).Milliseconds()
			latencies = append(latencies, caseMS)
			if err != nil {
				// A pipeline-start error (no workspace / no principal / no
				// policy) is a runner error, not a scored miss — it means the
				// case could not run at all (§7.1). Record every expected id
				// as missing so the case is visible as a failure, but do not
				// abort the run (one bad case should not hide the rest).
				summary.recordFailure(c, nil, c.ExpectedIDs, nil)
				continue
			}
			summary.scoreCase(c, resp)
		}
	}

	// P95 latency (§14.3 SLO: ≤ 2000ms). Computed from the per-case end-to-end
	// broker latencies; the gate is the §14.3 bound.
	summary.P95LatencyMS = p95(latencies)
	summary.LatencyOK = summary.P95LatencyMS <= 2000

	// Gates are AND across types (§9.1 — a weak type must not be masked). A
	// type with zero cases is vacuously passing for that gate (no gate to
	// fail); the runner emits no TypeReport for it.
	summary.RecallOK = true
	summary.CitationOK = true
	for _, r := range summary.ByType {
		if r.RecallCases > 0 && r.RecallAtK < r.RecallThreshold {
			summary.RecallOK = false
		}
		if r.CitationCases > 0 && r.CitationAccuracy < r.CitationThreshold {
			summary.CitationOK = false
		}
	}
	return summary, nil
}

// toQuery builds the Broker's KnowledgeQuery from a Case. AssetTypes carry
// through so the IntentRouter's rule 1 (explicit type set) applies; the Intent
// is keyword-inferred by the router unless the case pins it via the query text
// (the eval author writes a query that matches the intent's keywords, §4.2).
func (c Case) toQuery() knowledgectx.KnowledgeQuery {
	q := knowledgectx.KnowledgeQuery{
		Query:     c.Query,
		MaxTokens: c.MaxTokens,
	}
	if c.WorkspaceID != "" {
		if id, err := uuid.Parse(c.WorkspaceID); err == nil {
			q.WorkspaceID = id
		}
	}
	if len(c.AssetTypes) > 0 {
		q.AssetTypes = append([]domain.AssetType(nil), c.AssetTypes...)
	}
	return q
}

// scoreCase scores one case against its broker response, attributing per
// (intent, asset_type). Recall@K = |returned ∩ expected| / |expected|; nDCG
// uses the broker's ordering of the expected ids; CitationAccuracy checks the
// returned citations point at the expected version/evidence anchor.
func (s *Summary) scoreCase(c Case, resp knowledgectx.ContextResponse) {
	expectedSet := toSet(c.ExpectedIDs)
	got := returnedIDs(resp)

	// Recall@K (§9.1): |returned ∩ expected| / |expected|. K is the case's
	// ExpectedK (default len(expected)); only the top-K returned count.
	k := c.ExpectedK
	if k <= 0 || k > len(got) {
		k = len(got)
	}
	topK := got
	if len(topK) > k {
		topK = topK[:k]
	}
	hits := 0
	for _, id := range topK {
		if expectedSet[id] {
			hits++
		}
	}
	var recall float64
	if len(c.ExpectedIDs) > 0 {
		recall = float64(hits) / float64(len(c.ExpectedIDs))
	} else {
		recall = 1.0 // vacuous: no expected set → no recall gate
	}

	// nDCG (§9.1): ordering quality. Expected ids are the relevance set; a
	// returned expected id at rank i contributes 1/log2(i+2) (DCG). IDCG is
	// the ideal (all expected ids ranked first). 0 when no expected ids.
	ndcg := computeNDCG(got, expectedSet)

	// CitationAccuracy (§9.1, gate ≥ 0.95): for each returned candidate whose
	// id is in ExpectedVersions, the citation's VersionOrRevision MUST match
	// the expected anchor. Accurate/checked; 1.0 when none are checked.
	accurate, checked := 0, 0
	for _, cand := range resp.Candidates {
		idStr := cand.AssetID.String()
		wantAnchor, ok := c.ExpectedVersions[idStr]
		if !ok || wantAnchor == "" {
			continue
		}
		checked++
		if cand.Citation.VersionOrRevision == wantAnchor {
			accurate++
		}
	}
	var citationAcc float64
	if checked > 0 {
		citationAcc = float64(accurate) / float64(checked)
	} else {
		citationAcc = 1.0 // vacuous: no anchor assertions → no citation gate
	}

	// Attribute to every (intent, asset_type) the case targets. A case that
	// spans types updates each targeted type's report so per-type recall stays
	// visible (§9.1 — do NOT aggregate).
	types := c.AssetTypes
	if len(types) == 0 {
		// No explicit types → attribute to all four (the router picked); the
		// per-type report is then coarse but still non-aggregated.
		types = []domain.AssetType{
			domain.AssetTypeDocument, domain.AssetTypeCodebase,
			domain.AssetTypeMemory, domain.AssetTypeSkill,
		}
	}
	missing := diff(c.ExpectedIDs, got)
	extra := diff(got, c.ExpectedIDs)
	var fail *CaseFailure
	if recall < 1.0 || len(missing) > 0 {
		fail = &CaseFailure{Case: c, Got: got, Missing: missing, Extra: extra}
	}
	for _, t := range types {
		r := s.typeReport(c.Intent, t)
		r.RecallCases++
		// track the minimum per-case recall (the binding constraint, §9.1).
		if r.RecallCases == 1 || recall < r.RecallAtK {
			r.RecallAtK = recall
		}
		// nDCG: track the min per-case nDCG as the binding ordering signal
		// (consistent with recall's min-tracking; a weak ordering is visible).
		if r.NDCG == 0 || ndcg < r.NDCG {
			r.NDCG = ndcg
		}
		if checked > 0 {
			r.CitationCases++
			// citation accuracy is the min per-case (binding constraint).
			if r.CitationCases == 1 || citationAcc < r.CitationAccuracy {
				r.CitationAccuracy = citationAcc
			}
		}
		if fail != nil {
			r.Failed = append(r.Failed, *fail)
		}
	}
}

// typeReport returns the mutable TypeReport for (intent, asset_type),
// creating it with the first-version gates (§9.1: recall ≥ 0.9, citation ≥
// 0.95) if absent. The thresholds are the §9.1 first-version gates; a release
// locks the actual threshold against the dataset (§9.1).
func (s *Summary) typeReport(intent IntentKind, t domain.AssetType) *TypeReport {
	for i := range s.ByType {
		if s.ByType[i].Intent == intent && s.ByType[i].AssetType == t {
			return &s.ByType[i]
		}
	}
	s.ByType = append(s.ByType, TypeReport{
		Intent:            intent,
		AssetType:         t,
		RecallThreshold:   0.9,
		CitationThreshold: 0.95,
	})
	return &s.ByType[len(s.ByType)-1]
}

// recordFailure records a case that could not run (broker error) as a miss
// across every targeted type, with every expected id missing. Used when
// Execute returns a pipeline-start error (§7.1).
func (s *Summary) recordFailure(c Case, got, missing, extra []string) {
	types := c.AssetTypes
	if len(types) == 0 {
		types = []domain.AssetType{
			domain.AssetTypeDocument, domain.AssetTypeCodebase,
			domain.AssetTypeMemory, domain.AssetTypeSkill,
		}
	}
	fail := CaseFailure{Case: c, Got: got, Missing: missing, Extra: extra}
	for _, t := range types {
		r := s.typeReport(c.Intent, t)
		r.RecallCases++
		if r.RecallCases == 1 || 0 < r.RecallAtK {
			r.RecallAtK = 0
		}
		r.Failed = append(r.Failed, fail)
	}
}

// returnedIDs extracts the ordered asset-id strings from a broker response.
// Order is the broker's ranking (post-Score, post-Budget) — the order nDCG
// scores.
func returnedIDs(resp knowledgectx.ContextResponse) []string {
	out := make([]string, 0, len(resp.Candidates))
	for _, c := range resp.Candidates {
		out = append(out, c.AssetID.String())
	}
	return out
}

// computeNDCG returns the nDCG of the returned ordering against the expected
// relevance set (§9.1). DCG = Σ rel_i / log2(i+2) where rel_i ∈ {0,1} (an
// expected id is relevant). IDCG is the ideal (all relevant ranked first).
// Returns 0 when the expected set is empty (no relevance to order).
func computeNDCG(got []string, expected map[string]bool) float64 {
	if len(expected) == 0 {
		return 0
	}
	dcg := 0.0
	for i, id := range got {
		if expected[id] {
			dcg += 1.0 / log2(float64(i+2))
		}
	}
	// IDCG: rank len(expected) relevant items first.
	nRel := len(expected)
	idcg := 0.0
	for i := 0; i < nRel; i++ {
		idcg += 1.0 / log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// p95 returns the 95th-percentile latency from a slice of per-case
// milliseconds (§14.3 SLO). Returns 0 when empty.
func p95(latencies []int64) int64 {
	if len(latencies) == 0 {
		return 0
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	// nearest-rank method: idx = ceil(0.95 * n) - 1.
	idx := int(float64(len(latencies))*0.95) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(latencies) {
		idx = len(latencies) - 1
	}
	return latencies[idx]
}

// toSet converts a string slice to a set for membership checks.
func toSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// diff returns elements of a that are not in b (a minus b).
func diff(a, b []string) []string {
	bs := toSet(b)
	out := make([]string, 0, len(a))
	for _, x := range a {
		if !bs[x] {
			out = append(out, x)
		}
	}
	return out
}

// log2 is the base-2 logarithm (math.Log2 would pull math; nDCG only needs
// log2 of small integers, so a lookup-free impl keeps the eval package
// dependency-light). Returns 0 for x ≤ 0 (guard).
func log2(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// ln(x)/ln(2); use the standard identity. A small, dependency-free impl.
	return ln(x) / 0.6931471805599453
}

// ln is a natural-log approximation via the Newton/halving series on the
// exponent + mantissa split. Adequate for nDCG (small integer inputs); not a
// general-purpose math function.
func ln(x float64) float64 {
	if x <= 0 {
		return 0
	}
	// Reduce x to [1,2) by dividing/multiplying by 2, accumulating ln2.
	var e int
	for x >= 2 {
		x /= 2
		e++
	}
	for x < 1 {
		x *= 2
		e--
	}
	// Series for ln on [1,2): ln(x) = 2 * artanh((x-1)/(x+1)).
	y := (x - 1) / (x + 1)
	sum := 0.0
	pow := y
	for k := 1; k <= 9; k += 2 {
		sum += pow / float64(k)
		pow *= y * y
	}
	return 2*sum + float64(e)*0.6931471805599453
}

// FormatReport renders a Summary as a per-type text report (no aggregation
// into a single recall number — §9.1). Used by the CLI / test output.
func FormatReport(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "context eval: dataset=%s recall=%v citation=%v latency=%v (p95=%dms)\n",
		s.DatasetTag, s.RecallOK, s.CitationOK, s.LatencyOK, s.P95LatencyMS)
	for _, r := range s.ByType {
		fmt.Fprintf(&b, "  %s/%s: recall@k %.0f%% (≥%.0f%%) ndcg %.2f citation %.0f%% (≥%.0f%%) failures=%d\n",
			r.Intent, r.AssetType, r.RecallAtK*100, r.RecallThreshold*100,
			r.NDCG, r.CitationAccuracy*100, r.CitationThreshold*100, len(r.Failed))
		for _, f := range r.Failed {
			fmt.Fprintf(&b, "    FAIL intent=%s query=%q missing=%v extra=%v\n",
				f.Case.Intent, f.Case.Query, f.Missing, f.Extra)
		}
	}
	return b.String()
}
