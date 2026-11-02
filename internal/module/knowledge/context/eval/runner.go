// Package eval — Context Broker capability evaluation runner (design-docs/19
// §9.1). This is the acceptance-gate carrier for the Phase 6 broker. It loads
// a case set (queries + expected answers per intent), runs them through a
// ContextBroker, and scores:
//   - Recall@K     — recall, K locked by the dataset;
//   - nDCG         — ordering quality;
//   - CitationAccuracy — citation correctness (≥ 95% gate);
//   - P95 Latency  — end-to-end broker latency (P95 ≤ 2s, 12 §14.3 SLO).
//
// Metrics are reported per (intent, asset_type), NOT aggregated into a single
// number — same precedent as codegraph/eval (§9.1: a weak type/intent must not
// be masked by a strong type's mean). The runner is the skeleton 研发
// implements; the concrete case set (expected answers per intent) is delivered
// by the test engineer
// ([@Mora知识库测试工程师](mention://agent/9066a6d6-f66a-45cb-844e-81203f6cd137),
// §9.1) as a slice literal in cases_test.go so a CI gate can run
// `go test ./internal/module/knowledge/context/eval/`.
//
// The runner does NOT spin up providers: it expects a wired ContextBroker
// (real ports in CI; a stub broker short-circuits to degraded_sources — see
// the codegraph/eval NoopProvider precedent). A case asserts no expectation on
// the principal/authz wiring beyond what the broker itself enforces; the
// eval-driven broker is constructed by the test harness with fake ports that
// surface the dataset's seeded candidates.
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
	contextbroker "github.com/lynn901/mora/internal/module/knowledge/context"
)

// IntentKind tags an eval case with the intent it exercises (§4.1). The runner
// groups scoring per intent so recall is reported per-intent, not aggregated
// into a single mean (§9.1 — a weak intent must not be masked by a strong
// one's mean).
//
// IntentKind mirrors contextbroker.Intent (spec/revision/rationale/procedure)
// as a distinct local type so the eval case set is a pure Go literal with no
// import-time coupling to the broker package's Intent string — the runner
// converts at scoring time.
type IntentKind string

const (
	IntentKindSpec      IntentKind = "spec"
	IntentKindRevision  IntentKind = "revision"
	IntentKindRationale IntentKind = "rationale"
	IntentKindProcedure IntentKind = "procedure"
)

// toBrokerIntent maps an eval IntentKind to the broker Intent the case routes
// through. It panics on an unknown intent so a typo in a fixture fails loudly
// at scoring rather than silently falling through to the default policy.
func toBrokerIntent(k IntentKind) contextbroker.Intent {
	switch k {
	case IntentKindSpec:
		return contextbroker.IntentSpec
	case IntentKindRevision:
		return contextbroker.IntentRevision
	case IntentKindRationale:
		return contextbroker.IntentRationale
	case IntentKindProcedure:
		return contextbroker.IntentProcedure
	}
	panic("eval: unknown IntentKind " + string(k))
}

// ContextBroker is the narrow slice of the broker the runner exercises. It is
// declared locally (not an import of the broker package's *Broker type) so:
//   - the runner compiles without the concrete broker type name in its
//     signature (the original design-doc stub referenced an undefined
//     contextbroker.ContextBroker and would not build — YS-211 blocker 1);
//   - a test harness can inject a fake broker that short-circuits to a fixed
//     ContextResponse (the offline case set is scored against deterministic
//     fixtures, not a live fan-out — §9.1 "runner does NOT spin up
//     providers").
//
// The production *contextbroker.Broker satisfies this interface: its Execute
// method signature is
//
//	Execute(ctx, KnowledgeQuery, Principal) (ContextResponse, error)
//
// which matches Execute exactly. The runner never constructs the principal
// itself — Run accepts the principal the harness resolves (an eval-fixed
// owner principal with full use on the seeded assets), keeping the runner
// free of authz wiring concerns (§3.2 — authz is the broker's gate, not the
// runner's).
type ContextBroker interface {
	Execute(ctx context.Context, q contextbroker.KnowledgeQuery, p contextbroker.Principal) (contextbroker.ContextResponse, error)
}

// ExpectedCitation is one case's expected citation anchor. CitationAccuracy
// checks the returned citations' AssetID + VersionOrRevision against the
// expected set (§8.1 per-type anchor semantics: document=version_id,
// codebase=commit, memory=evidence_id, skill=package_version).
type ExpectedCitation struct {
	AssetID           uuid.UUID
	VersionOrRevision string // matched against Citation.VersionOrRevision
}

// ExpectedRelation is one case's expected retained conflict relation (§7.2:
// contradicts/supersedes travel on candidate.Relations as directed edges with
// a TargetID). The runner verifies some returned candidate carries that
// RelationType with a matching TargetID and was NOT dropped by dedup.
type ExpectedRelation struct {
	RelationType string    // contradicts | supersedes
	SourceAsset  uuid.UUID // the candidate AssetID carrying the relation
	TargetAsset  uuid.UUID // RelationSummary.TargetID
}

// Case is one eval case: a query + the expected answer set + the intent it
// exercises. Recall@K = |returned ∩ expected| / |expected|; nDCG uses the
// broker's ordering; CitationAccuracy checks the returned citations point at
// the expected version/evidence.
//
// The three Expected* fields are the DoD-required expected answers (§9.1):
//   - ExpectedCandidates — the asset_ids expected to be selected (Recall@K
//     judge; also the nDCG relevance set);
//   - ExpectedCitations  — the expected citation anchors (CitationAccuracy
//     judge, aligned to §8.1 per-type anchors);
//   - ExpectedRelations  — the conflict relations expected to survive dedup
//     side-by-side (conflict-retention judge, §7.2).
//
// ExpectedIDs is the string form of ExpectedCandidates retained for back-compat
// with the original skeleton's Case literal shape; the runner treats
// ExpectedCandidates as authoritative when non-nil and falls back to
// ExpectedIDs otherwise.
type Case struct {
	Intent      IntentKind
	Query       string
	WorkspaceID string // the workspace the case runs in (authz scope); mapped to uuid by the harness
	AssetTypes  []domain.AssetType

	// ExpectedIDs is the reference answer set as asset_id strings (Recall@K =
	// |returned ∩ expected| / |expected|). Retained for the skeleton's literal
	// shape; ExpectedCandidates is the typed equivalent.
	ExpectedIDs []string
	// ExpectedK is K for Recall@K (0 = len(expected)).
	ExpectedK int
	// MaxTokens is the budget for this case (0 = default).
	MaxTokens int

	// DoD-required expected answers (§9.1). ExpectedCandidates is the typed
	// reference set; when non-empty it is authoritative over ExpectedIDs.
	ExpectedCandidates []uuid.UUID        // expected selected asset_ids (Recall@K + nDCG relevance)
	ExpectedCitations  []ExpectedCitation // expected citation anchors (CitationAccuracy)
	ExpectedRelations  []ExpectedRelation // expected retained conflict relations (conflict retention)
}

// CaseSet is a dataset-tagged collection of cases. The runner pins scoring to
// a dataset_tag so a release can lock the threshold against a specific dataset
// version (§9.1 "发布前锁定当期数据集阈值"). The tag is recorded in
// context_eval_runs alongside the aggregate metrics.
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
	RecallAtK         float64 // min per-case recall (the binding constraint); gate ≥ RecallThreshold
	RecallThreshold   float64 // default 0.9 (first-version gate, §9.1)
	NDCG              float64 // mean per-case nDCG; gate ≥ RecallThreshold (ordering quality)
	CitationCases     int
	CitationAccuracy  float64 // gate ≥ CitationThreshold
	CitationThreshold float64 // default 0.95
	Failed            []CaseFailure
}

// CaseFailure records one case that missed its expectation, with the detail a
// test engineer needs to triage (expected vs actual).
type CaseFailure struct {
	Case    Case
	Got     []string // surfaced asset_ids
	Missing []string // expected asset_ids not surfaced
	Extra   []string // surfaced asset_ids not in expected (precision signal)
	// CitationMisses are expected citation anchors not matched by any returned
	// citation (AssetID + VersionOrRevision).
	CitationMisses []ExpectedCitation
	// RelationMisses are expected conflict relations not found on any returned
	// candidate (the conflict pair was dropped — a §7.2 regression signal).
	RelationMisses []ExpectedRelation
}

// Summary is the aggregate the runner emits for a whole run. Per-type recall
// is kept separate (§9.1: do NOT collapse to one number).
type Summary struct {
	DatasetTag         string
	ByType             []TypeReport
	P95LatencyMS       int  // end-to-end broker P95 (gate ≤ 2000, 12 §14.3)
	LatencyThresholdMS int  // default 2000 (§14.3 SLO)
	LatencyOK          bool // P95 ≤ threshold
	RecallOK           bool // recall ≥ threshold, every type
	CitationOK         bool // citation accuracy ≥ 0.95, every type
}

// Pass reports whether the whole run satisfies all gates (recall + citation +
// latency). Used by the CI gate / release threshold lock (§9.1).
func (s Summary) Pass() bool { return s.RecallOK && s.CitationOK && s.LatencyOK }

// RunOption tunes a run (thresholds). The defaults are the §9.1 first-version
// gates: Recall@K ≥ 0.9, CitationAccuracy ≥ 0.95, P95 ≤ 2000ms. A test harness
// may override (e.g. a release-threshold lock run uses the locked values).
type RunOption func(*runConfig)

type runConfig struct {
	recallThreshold    float64
	citationThreshold  float64
	latencyThresholdMS int
}

// WithRecallThreshold overrides the per-type Recall@K gate (default 0.9).
func WithRecallThreshold(t float64) RunOption {
	return func(c *runConfig) { c.recallThreshold = t }
}

// WithCitationThreshold overrides the CitationAccuracy gate (default 0.95).
func WithCitationThreshold(t float64) RunOption {
	return func(c *runConfig) { c.citationThreshold = t }
}

// WithLatencyThresholdMS overrides the P95 latency gate in ms (default 2000).
func WithLatencyThresholdMS(ms int) RunOption {
	return func(c *runConfig) { c.latencyThresholdMS = ms }
}

func defaultRunConfig() runConfig {
	return runConfig{
		recallThreshold:    0.9,
		citationThreshold:  0.95,
		latencyThresholdMS: 2000,
	}
}

// evalBucket is the per-(intent, asset_type) scoring accumulator the runner
// builds while iterating cases. It is unexported (internal to the scoring
// pass); assembled into a TypeReport by assembleSummary. Kept at package scope
// (not a Run-local type) so the helper functions (addBucketFailure,
// recallSetFor, assembleSummary) share one type with Run.
type evalBucket struct {
	intent    IntentKind
	assetType domain.AssetType
	recallMin float64 // min per-case recall (the binding constraint)
	recallSet bool
	ndcgSum   float64
	ndcgN     int
	citSum    float64
	citN      int
	failures  []CaseFailure
}

// Run executes a case set against a ContextBroker and returns the per-type
// scoring summary. It is the single entrypoint the CI gate / the test
// engineer's harness calls. The broker is the ContextBroker under test (real
// ports in CI, a fake broker short-circuits to a fixed ContextResponse for the
// offline case set).
//
// The runner does NOT build assets: it expects the workspace to already hold
// the dataset's assets + versions. The build path is exercised by the §9.3
// integration tests, not here.
//
// Scoring (§9.1), per case:
//  1. Build the KnowledgeQuery (intent + asset types + budget from the case).
//  2. Call broker.Execute under a per-case timer (end-to-end latency sample).
//  3. Recall@K — |returned_ids ∩ expected| / |expected|, K from ExpectedK.
//  4. nDCG — DCG of the returned order vs an ideal order of the expected set.
//  5. CitationAccuracy — fraction of expected citation anchors matched by a
//     returned citation (AssetID + VersionOrRevision).
//  6. Conflict retention — every ExpectedRelation must appear on a returned
//     candidate; a miss is a CaseFailure (a §7.2 regression).
//
// Results are grouped per (intent, asset_type). A case touching multiple asset
// types contributes to each of its types' reports (a candidate of type T scores
// that type's bucket). Per-type recall is the MIN per-case recall in the bucket
// (the binding constraint, same as codegraph impact recall); nDCG is the mean.
// An empty-expected case (e.g. a low-authority type that should surface
// nothing) is scored as recall=1.0 when the broker returns nothing for that
// type and a failure when it leaks an unexpected candidate.
func Run(ctx context.Context, b ContextBroker, sets []CaseSet, principal contextbroker.Principal, opts ...RunOption) (Summary, error) {
	cfg := defaultRunConfig()
	for _, o := range opts {
		o(&cfg)
	}

	// per-type accumulator keyed by (intent, asset_type).
	buckets := make(map[string]*evalBucket)

	var latencies []int64 // ms, per case, for P95

	for _, set := range sets {
		for _, c := range set.Cases {
			start := time.Now()
			resp, err := b.Execute(ctx, caseQuery(c), principal)
			latencies = append(latencies, time.Since(start).Milliseconds())
			if err != nil {
				// A pipeline-start error (no workspace/principal/policy) is a
				// harness/wiring fault, not a scoring miss — surface it so the
				// run does not silently mark every case as a recall failure
				// (which would mask the real cause). Record a failure for each
				// of the case's type buckets and continue.
				fail := CaseFailure{
					Case:    c,
					Missing: expectedIDStrings(c),
				}
				addBucketFailure(buckets, c, fail)
				continue
			}

			gotIDs := returnedAssetIDs(resp)
			expected := expectedUUIDs(c)
			k := c.ExpectedK
			if k <= 0 || k > len(expected) {
				k = len(expected)
			}
			recall := recallAtK(gotIDs, expected, k)
			ndcg := ndcg(gotIDs, expected)
			citAcc, citMisses := citationAccuracy(resp, c)
			relMisses := relationMisses(resp, c)

			// Per-case failure bookkeeping: a case fails its bucket when recall
			// < threshold, a citation is missed, or a conflict relation was
			// dropped. Empty-expected cases that leak a candidate are also a
			// failure (precision leak).
			var missing, extra []string
			if !subset(expected, gotIDs) {
				missing = diffStrings(expected, gotIDs)
			}
			if len(expected) == 0 && len(gotIDs) > 0 {
				extra = gotIDs
			} else if len(expected) > 0 {
				extra = diffStringsRaw(gotIDs, expected)
			}
			failed := recall < cfg.recallThreshold || len(citMisses) > 0 || len(relMisses) > 0 ||
				(len(expected) == 0 && len(gotIDs) > 0)
			var fail CaseFailure
			if failed {
				fail = CaseFailure{
					Case:           c,
					Got:            gotIDs,
					Missing:        missing,
					Extra:          extra,
					CitationMisses: citMisses,
					RelationMisses: relMisses,
				}
			}

			for _, t := range caseTypes(c) {
				key := bucketKey(c.Intent, t)
				bk := buckets[key]
				if bk == nil {
					bk = &evalBucket{intent: c.Intent, assetType: t}
					buckets[key] = bk
				}
				// Recall: min per-case (binding constraint). A case with no
				// expected candidates of this type and none returned is
				// vacuously 1.0 (no gate to fail).
				if recallSetFor(bk, recall) {
					bk.recallMin = recall
				}
				bk.ndcgSum += ndcg
				bk.ndcgN++
				bk.citSum += citAcc
				bk.citN++
				if failed {
					bk.failures = append(bk.failures, fail)
				}
			}
		}
	}

	summary := assembleSummary(buckets, cfg, latencies)
	summary.DatasetTag = datasetTagOf(sets)
	return summary, nil
}

// caseQuery builds the broker KnowledgeQuery for a case. The WorkspaceID is
// parsed from the case's string (a fixed eval workspace); an invalid id is
// uuid.Nil, which the broker rejects with ErrBrokerNoWorkspace — surfaced as a
// per-case failure (see Run). AssetTypes + budget come from the case literal.
func caseQuery(c Case) contextbroker.KnowledgeQuery {
	q := contextbroker.KnowledgeQuery{
		Query:       c.Query,
		WorkspaceID: parseWorkspaceID(c.WorkspaceID),
		AssetTypes:  c.AssetTypes,
		MaxTokens:   c.MaxTokens,
	}
	return q
}

// parseWorkspaceID parses the case's workspace string. An invalid id → uuid.Nil
// (the broker rejects it; the runner records a per-case failure rather than
// panicking, so a malformed fixture does not abort the whole run).
func parseWorkspaceID(s string) uuid.UUID {
	if s == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// caseTypes returns the asset types a case touches. Falls back to the four
// built-in types when the case declares none (so the run still buckets by
// type; a case with no explicit types is the IntentRouter's default routing).
func caseTypes(c Case) []domain.AssetType {
	if len(c.AssetTypes) > 0 {
		return c.AssetTypes
	}
	return []domain.AssetType{
		domain.AssetTypeDocument,
		domain.AssetTypeCodebase,
		domain.AssetTypeMemory,
		domain.AssetTypeSkill,
	}
}

// returnedAssetIDs collects the asset_ids the broker returned, deduped, as
// strings (for comparison against ExpectedIDs / the failure bookkeeping).
func returnedAssetIDs(resp contextbroker.ContextResponse) []string {
	seen := make(map[uuid.UUID]bool, len(resp.Candidates))
	out := make([]string, 0, len(resp.Candidates))
	for _, cand := range resp.Candidates {
		if seen[cand.AssetID] {
			continue
		}
		seen[cand.AssetID] = true
		out = append(out, cand.AssetID.String())
	}
	return out
}

// expectedUUIDs resolves the case's expected candidate set to uuids.
// ExpectedCandidates (typed) is authoritative when non-empty; otherwise the
// ExpectedIDs strings are parsed. A malformed ExpectedIDs entry is dropped
// (a fixture typo would surface as a recall miss, not a panic).
func expectedUUIDs(c Case) []uuid.UUID {
	if len(c.ExpectedCandidates) > 0 {
		out := make([]uuid.UUID, 0, len(c.ExpectedCandidates))
		out = append(out, c.ExpectedCandidates...)
		return out
	}
	out := make([]uuid.UUID, 0, len(c.ExpectedIDs))
	for _, s := range c.ExpectedIDs {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// expectedIDStrings is the string form of expectedUUIDs (for failure records).
func expectedIDStrings(c Case) []string {
	ids := expectedUUIDs(c)
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// recallAtK = |returned[:K] ∩ expected| / |expected|. K is clamped to
// len(expected). An empty expected set is the precision gate (§9.1 "空预期也
// 判"): returning nothing → 1.0 (vacuous pass), but leaking a candidate → 0.0
// (a leak into a type that should surface nothing is a recall failure — the
// broker surfaced something that should not be there).
func recallAtK(returned []string, expected []uuid.UUID, k int) float64 {
	if len(expected) == 0 {
		if len(returned) == 0 {
			return 1.0 // vacuous pass: expected nothing, got nothing.
		}
		return 0.0 // leak: expected nothing, got something → recall failure.
	}
	if k <= 0 || k > len(returned) {
		k = len(returned)
	}
	exp := make(map[string]bool, len(expected))
	for _, id := range expected {
		exp[id.String()] = true
	}
	hits := 0
	for i := 0; i < k && i < len(returned); i++ {
		if exp[returned[i]] {
			hits++
		}
	}
	return float64(hits) / float64(len(expected))
}

// ndcg computes the normalized DCG of the returned ordering against the
// expected relevance set. Expected ids are relevance 1 (graded), everything
// else 0. DCG = Σ rel_i / log2(i+2); IDCG is the ideal (all expected first).
// An empty expected set returns 1.0 (vacuous).
func ndcg(returned []string, expected []uuid.UUID) float64 {
	if len(expected) == 0 {
		return 1.0
	}
	exp := make(map[string]bool, len(expected))
	for _, id := range expected {
		exp[id.String()] = true
	}
	dcg := 0.0
	for i, id := range returned {
		if !exp[id] {
			continue
		}
		dcg += 1.0 / math.Log2(float64(i+2))
	}
	// IDCG: the expected set ranked first (ideal order).
	m := len(expected)
	idcg := 0.0
	for i := 0; i < m; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}
	return dcg / idcg
}

// citationAccuracy returns the fraction of expected citation anchors matched
// by a returned citation (AssetID + VersionOrRevision), plus the misses.
// An empty expected set returns (1.0, nil) (vacuous).
func citationAccuracy(resp contextbroker.ContextResponse, c Case) (float64, []ExpectedCitation) {
	if len(c.ExpectedCitations) == 0 {
		return 1.0, nil
	}
	// index returned citations by (AssetID, VersionOrRevision).
	type k struct {
		asset uuid.UUID
		ver   string
	}
	got := make(map[k]bool, len(resp.Candidates))
	for _, cand := range resp.Candidates {
		if cand.Citation.VersionOrRevision == "" {
			continue
		}
		got[k{cand.Citation.AssetID, cand.Citation.VersionOrRevision}] = true
	}
	hits := 0
	var misses []ExpectedCitation
	for _, e := range c.ExpectedCitations {
		if got[k{e.AssetID, e.VersionOrRevision}] {
			hits++
		} else {
			misses = append(misses, e)
		}
	}
	return float64(hits) / float64(len(c.ExpectedCitations)), misses
}

// relationMisses returns the expected conflict relations not found on any
// returned candidate (§7.2: a conflict pair the broker dropped — a retention
// regression). A relation matches when a returned candidate with SourceAsset
// carries a RelationSummary{RelationType, TargetID==TargetAsset}.
func relationMisses(resp contextbroker.ContextResponse, c Case) []ExpectedRelation {
	if len(c.ExpectedRelations) == 0 {
		return nil
	}
	byAsset := make(map[uuid.UUID][]contextbroker.RelationSummary, len(resp.Candidates))
	for _, cand := range resp.Candidates {
		if len(cand.Relations) == 0 {
			continue
		}
		byAsset[cand.AssetID] = append(byAsset[cand.AssetID], cand.Relations...)
	}
	var misses []ExpectedRelation
	for _, e := range c.ExpectedRelations {
		rels, ok := byAsset[e.SourceAsset]
		if !ok {
			misses = append(misses, e)
			continue
		}
		found := false
		for _, r := range rels {
			if r.RelationType == e.RelationType && r.TargetID == e.TargetAsset {
				found = true
				break
			}
		}
		if !found {
			misses = append(misses, e)
		}
	}
	return misses
}

// bucketKey is the map key for a (intent, asset_type) scoring bucket.
func bucketKey(intent IntentKind, t domain.AssetType) string {
	return string(intent) + "/" + string(t)
}

// addBucketFailure records a failure (e.g. a pipeline error) against every
// type bucket the case touches, creating the buckets as needed.
func addBucketFailure(buckets map[string]*evalBucket, c Case, fail CaseFailure) {
	for _, t := range caseTypes(c) {
		key := bucketKey(c.Intent, t)
		bk := buckets[key]
		if bk == nil {
			bk = &evalBucket{intent: c.Intent, assetType: t}
			buckets[key] = bk
		}
		bk.failures = append(bk.failures, fail)
	}
}

// recallSetFor records the per-bucket minimum recall. The first case sets it;
// later cases only lower it (the binding constraint is the worst case).
func recallSetFor(bk *evalBucket, recall float64) bool {
	if !bk.recallSet {
		bk.recallMin = recall
		bk.recallSet = true
		return false
	}
	if recall < bk.recallMin {
		bk.recallMin = recall
	}
	return false
}

// assembleSummary builds the per-type TypeReport slice + the gate booleans
// from the accumulated buckets, and computes the P95 latency.
func assembleSummary(buckets map[string]*evalBucket, cfg runConfig, latencies []int64) Summary {
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic report order

	byType := make([]TypeReport, 0, len(keys))
	recallOK := true
	citationOK := true
	for _, k := range keys {
		bk := buckets[k]
		r := TypeReport{
			Intent:            bk.intent,
			AssetType:         bk.assetType,
			RecallThreshold:   cfg.recallThreshold,
			CitationThreshold: cfg.citationThreshold,
			RecallCases:       bk.ndcgN,
			RecallAtK:         bk.recallMin,
			NDCG:              meanOrZero(bk.ndcgSum, bk.ndcgN),
			CitationCases:     bk.citN,
			CitationAccuracy:  meanOrZero(bk.citSum, bk.citN),
			Failed:            bk.failures,
		}
		byType = append(byType, r)
		if r.RecallAtK < cfg.recallThreshold {
			recallOK = false
		}
		if r.CitationAccuracy < cfg.citationThreshold {
			citationOK = false
		}
	}

	p95 := percentile(latencies, 0.95)
	latencyOK := p95 <= int64(cfg.latencyThresholdMS)
	return Summary{
		ByType:             byType,
		P95LatencyMS:       int(p95),
		LatencyThresholdMS: cfg.latencyThresholdMS,
		LatencyOK:          latencyOK,
		RecallOK:           recallOK,
		CitationOK:         citationOK,
	}
}

// datasetTagOf picks the dataset tag from the sets (first non-empty). Empty
// when no set carries one.
func datasetTagOf(sets []CaseSet) string {
	for _, s := range sets {
		if s.DatasetTag != "" {
			return s.DatasetTag
		}
	}
	return ""
}

func meanOrZero(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

// percentile returns the q-th percentile of xs (q in [0,1]). A nil/empty
// slice returns 0. Uses the nearest-rank method (simple, deterministic).
func percentile(xs []int64, q float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := make([]int64, len(xs))
	copy(sorted, xs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	// nearest-rank: ceil(q*N), 1-indexed.
	idx := int(q*float64(len(sorted)) + 0.999999)
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

// subset reports whether every element of a is in b (order-independent).
func subset(a []uuid.UUID, b []string) bool {
	if len(a) == 0 {
		return true
	}
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	for _, id := range a {
		if !set[id.String()] {
			return false
		}
	}
	return true
}

// diffStrings returns elements of a (as strings) not present in b.
func diffStrings(a []uuid.UUID, b []string) []string {
	set := make(map[string]bool, len(b))
	for _, s := range b {
		set[s] = true
	}
	out := make([]string, 0, len(a))
	for _, id := range a {
		if !set[id.String()] {
			out = append(out, id.String())
		}
	}
	return out
}

// diffStringsRaw returns elements of a (already strings) not present in b (the
// expected uuids). Used for the "extra surfaced ids" precision signal.
func diffStringsRaw(a []string, expected []uuid.UUID) []string {
	set := make(map[string]bool, len(expected))
	for _, id := range expected {
		set[id.String()] = true
	}
	out := make([]string, 0, len(a))
	for _, s := range a {
		if !set[s] {
			out = append(out, s)
		}
	}
	return out
}

// FormatReport renders a Summary as a per-type text report (no aggregation into
// a single recall number — §9.1). Used by the CLI / test output.
func FormatReport(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "context eval: dataset=%s recall=%v citation=%v latency=%v (p95=%dms)\n",
		s.DatasetTag, s.RecallOK, s.CitationOK, s.LatencyOK, s.P95LatencyMS)
	for _, r := range s.ByType {
		fmt.Fprintf(&b, "  %s/%s: recall@k %.0f%% (≥%.0f%%) ndcg %.2f citation %.0f%% (≥%.0f%%) failures=%d\n",
			r.Intent, r.AssetType, r.RecallAtK*100, r.RecallThreshold*100,
			r.NDCG, r.CitationAccuracy*100, r.CitationThreshold*100, len(r.Failed))
		for _, f := range r.Failed {
			fmt.Fprintf(&b, "    FAIL %s intent=%s missing=%v extra=%v cit_miss=%d rel_miss=%d\n",
				f.Case.Query, f.Case.Intent, f.Missing, f.Extra, len(f.CitationMisses), len(f.RelationMisses))
		}
	}
	return b.String()
}
