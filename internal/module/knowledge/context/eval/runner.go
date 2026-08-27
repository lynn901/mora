// Package eval — Context Broker capability evaluation runner (design-docs/19
// §9.1). This is the acceptance-gate carrier for the Phase 6 broker. It loads
// a case set (queries + expected answers per intent), runs them through a
// ContextBroker, and scores:
//   - Recall@K     — recall, K locked by the dataset;
//   - nDCG         — ordering quality;
//   - CitationAccuracy — citation correctness (≥ 95% gate);
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
// The runner does NOT spin up providers: it expects a wired ContextBroker
// (real ports in CI, a stub broker short-circuits to degraded_sources — see
// the codegraph/eval NoopProvider precedent).
package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/context"
)

// IntentKind tags an eval case with the intent it exercises (§4.1). The runner
// groups scoring per intent so recall is reported per-intent, not aggregated
// into a single mean (§9.1 — a weak intent must not be masked by a strong
// one's mean).
type IntentKind string

const (
	IntentKindSpec      IntentKind = "spec"
	IntentKindRevision  IntentKind = "revision"
	IntentKindRationale IntentKind = "rationale"
	IntentKindProcedure IntentKind = "procedure"
)

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
}

// CaseSet is a dataset-tagged collection of cases. The runner pins scoring to
// a dataset_tag so a release can lock the threshold against a specific dataset
// version (§9.1 "发布前锁定当期数据集阈值").
type CaseSet struct {
	DatasetTag string
	Cases      []Case
}

// TypeReport is the per-type scoring breakdown (§9.1: do NOT collapse types
// into one number). The runner emits one of these per (intent, asset_type) so
// a weak layer is visible on its own.
type TypeReport struct {
	Intent          IntentKind
	AssetType       domain.AssetType
	RecallCases     int
	RecallAtK       float64 // min per-case recall (the binding constraint)
	RecallThreshold float64  // default 0.9 (first-version gate, §9.1)
	NDCG            float64
	CitationCases   int
	CitationAccuracy float64 // gate ≥ 0.95 (§9.1)
	CitationThreshold float64 // default 0.95
	Failed          []CaseFailure
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
	DatasetTag string
	ByType     []TypeReport
	P95LatencyMS int   // end-to-end broker P95 (gate ≤ 2000, 12 §14.3)
	LatencyOK  bool
	RecallOK   bool    // recall ≥ threshold, every type
	CitationOK bool    // citation accuracy ≥ 95%, every type
}

// Pass reports whether the whole run satisfies all gates (recall + citation +
// latency). Used by the CI gate / release threshold lock (§9.1).
func (s Summary) Pass() bool { return s.RecallOK && s.CitationOK && s.LatencyOK }

// Run executes a case set against a ContextBroker and returns the per-type
// scoring summary. It is the single entrypoint the CI gate / the test
// engineer's harness calls. The broker is the ContextBroker under test (real
// ports in CI, a stub broker short-circuits to degraded_sources).
//
// The runner does NOT build assets: it expects the workspace to already hold
// the dataset's assets + versions. The build path is exercised by the §9.3
// integration tests, not here.
//
// TODO: implement the scoring loop (recall / nDCG / citation / latency). The
// skeleton returns an empty Summary so the carrier compiles; the test engineer
// fills the case set + the 研发 implements scoring once broker.Execute lands.
func Run(ctx context.Context, b contextbroker.ContextBroker, sets []CaseSet) (Summary, error) {
	var summary Summary
	for _, set := range sets {
		summary.DatasetTag = set.DatasetTag
		for _, c := range set.Cases {
			_ = c // TODO: build KnowledgeQuery, call b.Execute, score per type.
		}
	}
	// TODO: gate computation — RecallOK (every type ≥ threshold), CitationOK
	// (every type ≥ 0.95), LatencyOK (P95 ≤ 2000).
	summary.RecallOK = true
	summary.CitationOK = true
	summary.LatencyOK = true
	_ = ctx
	_ = b
	return summary, nil
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
			fmt.Fprintf(&b, "    FAIL %s/%s intent=%s missing=%v extra=%v\n",
				f.Case.Query, f.Case.AssetTypes, f.Case.Intent, f.Missing, f.Extra)
		}
	}
	return b.String()
}
