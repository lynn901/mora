// Package eval — codegraph capability evaluation runner (design-docs/17 §7.1).
//
// This is the acceptance-gate carrier for the codegraph provider. It loads a
// case set (one baseline repo subset per declared language, each with a set of
// queries + expected answers), runs them through a provider.CodeGraphProvider,
// and scores:
//   - definition / call queries: 100% hit (hard gate — every expected symbol
//     must resolve, every expected call edge must surface);
//   - impact recall: ≥ 90% per language (first version); a per-language report
//     is emitted WITHOUT aggregating into a single number, so a weak language
//     cannot be masked by a strong one's mean (§7.1).
//
// The runner is the skeleton研发 implements; the concrete case set (expected
// answers per language) is landed collaboratively by the test engineer
// ([@Mora知识库测试工程师], §12). Cases are plain Go (a slice literal) so a CI
// gate can run `go test ./internal/module/knowledge/codegraph/eval/` once a
// sidecar provider is wired.
package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"

	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// CaseKind is the query category a case exercises. The hard-gate categories
// (Definition, Callers, Callees) must be 100% hit; Impact is the recall gate.
type CaseKind string

const (
	CaseKindDefinition CaseKind = "definition" // code_node must resolve the symbol
	CaseKindCallers    CaseKind = "callers"    // code_callers must surface every expected caller
	CaseKindCallees    CaseKind = "callees"    // code_callees must surface every expected callee
	CaseKindImpact     CaseKind = "impact"     // code_impact recall ≥ threshold (default 90%)
)

// Case is one eval case. For Definition, ExpectedSymbols is the set the
// code_node result must belong to (single-element for a unique symbol, or the
// allowed set when a name is ambiguous). For Callers/Callees, ExpectedSymbols
// is the set of caller/callee symbols that MUST surface (100% hit). For Impact,
// ExpectedSymbols is the reference impact set; recall = |returned ∩ expected| /
// |expected|.
type Case struct {
	Language     string   // the provider-declared language this case targets
	Kind         CaseKind
	Symbol       string   // the query symbol (code_node / callers / callees / impact)
	Path         string   // optional disambiguator
	Depth        int      // impact depth (0 = provider default)
	ExpectedSymbols []string // expected answer set (see Case doc)
	// ExpectedCommit is the pinned commit the results MUST carry (§3.2: an
	// expired revision never masquerades as current). "" = do not assert.
	ExpectedCommit string
}

// CaseSet is a baseline repo subset + its query cases for one language. The
// runner groups scoring per language so recall is reported per-layer, not
// aggregated into a single mean (§7.1).
type CaseSet struct {
	Language  string
	GraphRef  string // the provider graph handle for this baseline repo
	Commit    string // the pinned commit answers are authored against
	Cases     []Case
}

// LanguageReport is the per-language scoring breakdown. Recall is never
// averaged across languages (§7.1) — the runner emits one of these per
// language so a weak layer is visible on its own.
type LanguageReport struct {
	Language        string
	DefinitionCases int
	DefinitionHits  int // must equal DefinitionCases (hard gate)
	CallCases       int // callers + callees
	CallHits        int // must equal CallCases (hard gate)
	ImpactCases     int
	ImpactRecall    float64 // min per-case recall across impact cases; gate ≥ threshold
	ImpactThreshold float64 // default 0.9
	Failed          []CaseFailure
}

// CaseFailure records one case that missed its expectation, with the detail a
// test engineer needs to triage (expected vs actual).
type CaseFailure struct {
	Case    Case
	Got      []string
	Missing  []string // expected symbols not surfaced
	Extra    []string // surfaced symbols not in expected (precision signal)
	CommitOK bool    // whether the result commit matched ExpectedCommit (§3.2)
}

// Summary is the aggregate the runner emits for a whole run. Per-language
// recall is kept separate (§7.1: do NOT collapse to one number).
type Summary struct {
	ByLanguage []LanguageReport
	HardGateOK bool // definition + call 100% hit, every language
	RecallOK   bool // impact recall ≥ threshold, every language
}

// Pass reports whether the whole run satisfies both gates.
func (s Summary) Pass() bool { return s.HardGateOK && s.RecallOK }

// Run executes a case set against a provider and returns the per-language
// scoring summary. It is the single entrypoint the CI gate / the test
// engineer's harness calls. The provider is the CodeGraphProvider under test
// (a sidecar in CI, the NoopProvider short-circuits to capability_unavailable
// — see IsCapabilityUnavailable).
//
// The runner does NOT build graphs: it expects CaseSet.GraphRef to already be a
// built, ready graph handle. The build path (§4.1) is exercised by the §17.3
// integration tests, not here.
func Run(ctx context.Context, p cgprovider.CodeGraphProvider, sets []CaseSet) (Summary, error) {
	var summary Summary
	for _, set := range sets {
		rep, err := runSet(ctx, p, set)
		if err != nil {
			return summary, fmt.Errorf("eval language %s: %w", set.Language, err)
		}
		summary.ByLanguage = append(summary.ByLanguage, rep)
	}
	// Hard gate + recall gate are AND across languages: a single language
	// failing either fails the run (§7.1 — a weak language must not be masked
	// by a strong one's mean). Recall for a language with zero impact cases is
	// vacuously passing (runSet sets it to 1.0).
	hardOK := true
	recallOK := true
	for _, r := range summary.ByLanguage {
		if r.DefinitionHits != r.DefinitionCases || r.CallHits != r.CallCases {
			hardOK = false
		}
		if r.ImpactCases > 0 && r.ImpactRecall < r.ImpactThreshold {
			recallOK = false
		}
	}
	summary.HardGateOK = hardOK
	summary.RecallOK = recallOK
	return summary, nil
}

// runSet scores one language's case set. A provider error that is NOT
// ErrCapabilityUnavailable propagates as a run error (a transient sidecar fault
// must not be silently scored as a miss — it would mask a flaky provider).
// ErrCapabilityUnavailable (provider unconfigured) is scored as a miss: the
// runner stays exercisable against the NoopProvider so a CI gate observes the
// "not wired" state without erroring.
func runSet(ctx context.Context, p cgprovider.CodeGraphProvider, set CaseSet) (LanguageReport, error) {
	rep := LanguageReport{
		Language:        set.Language,
		ImpactThreshold: 0.9, // §7.1 first-version gate
	}
	for _, c := range set.Cases {
		switch c.Kind {
		case CaseKindDefinition:
			rep.DefinitionCases++
			ok, fail, err := evalDefinition(ctx, p, set, c)
			if err != nil {
				return rep, err
			}
			if ok {
				rep.DefinitionHits++
			} else if fail != nil {
				rep.Failed = append(rep.Failed, *fail)
			}
		case CaseKindCallers, CaseKindCallees:
			rep.CallCases++
			ok, fail, err := evalCall(ctx, p, set, c)
			if err != nil {
				return rep, err
			}
			if ok {
				rep.CallHits++
			} else if fail != nil {
				rep.Failed = append(rep.Failed, *fail)
			}
		case CaseKindImpact:
			rep.ImpactCases++
			recall, fail, err := evalImpact(ctx, p, set, c)
			if err != nil {
				return rep, err
			}
			// track the minimum per-case recall (the binding constraint).
			if rep.ImpactCases == 1 || recall < rep.ImpactRecall {
				rep.ImpactRecall = recall
			}
			if fail != nil {
				rep.Failed = append(rep.Failed, *fail)
			}
		}
	}
	// If there were no impact cases, recall is vacuously 1.0 (no gate to fail).
	if rep.ImpactCases == 0 {
		rep.ImpactRecall = 1.0
	}
	return rep, nil
}

// evalDefinition runs code_node for the case's symbol and checks the resolved
// symbol ∈ ExpectedSymbols. A nil node (symbol not found) is a miss. A provider
// error that is ErrCapabilityUnavailable is a miss; any other error propagates.
func evalDefinition(ctx context.Context, p cgprovider.CodeGraphProvider, set CaseSet, c Case) (bool, *CaseFailure, error) {
	node, err := p.Node(ctx, set.GraphRef, cgprovider.NodeRequest{
		Symbol: c.Symbol, Language: c.Language, Path: c.Path,
	})
	if err != nil {
		if errors.Is(err, cgprovider.ErrCapabilityUnavailable) {
			return false, &CaseFailure{Case: c, Missing: c.ExpectedSymbols}, nil
		}
		return false, nil, err
	}
	if node.Loc.Symbol == "" {
		return false, &CaseFailure{Case: c, Missing: c.ExpectedSymbols}, nil
	}
	commitOK := c.ExpectedCommit == "" || node.Loc.Commit == c.ExpectedCommit
	got := node.Loc.Symbol
	if contains(c.ExpectedSymbols, got) {
		return commitOK, nil, nil
	}
	return false, &CaseFailure{Case: c, Got: []string{got}, Missing: c.ExpectedSymbols, CommitOK: commitOK}, nil
}

// evalCall runs code_callers / code_callees and checks every ExpectedSymbols
// entry surfaces (100% hit). Extra surfaced symbols are recorded as precision
// signal (not a gate failure). capability_unavailable is a miss; other errors
// propagate.
func evalCall(ctx context.Context, p cgprovider.CodeGraphProvider, set CaseSet, c Case) (bool, *CaseFailure, error) {
	var (
		edges []cgprovider.CodeEdge
		err   error
	)
	req := cgprovider.NodeRequest{Symbol: c.Symbol, Language: c.Language, Path: c.Path}
	if c.Kind == CaseKindCallers {
		edges, err = p.Callers(ctx, set.GraphRef, req)
	} else {
		edges, err = p.Callees(ctx, set.GraphRef, req)
	}
	if err != nil {
		if errors.Is(err, cgprovider.ErrCapabilityUnavailable) {
			return false, &CaseFailure{Case: c, Missing: c.ExpectedSymbols}, nil
		}
		return false, nil, err
	}
	got := callEdgeSymbols(c.Kind, edges)
	missing := diff(c.ExpectedSymbols, got)
	extra := diff(got, c.ExpectedSymbols)
	commitOK := c.ExpectedCommit == "" || allCommitMatch(edges, c.ExpectedCommit)
	if len(missing) == 0 {
		return commitOK, nil, nil
	}
	return false, &CaseFailure{Case: c, Got: got, Missing: missing, Extra: extra, CommitOK: commitOK}, nil
}

// callEdgeSymbols extracts the relevant-side symbol names from a call edge set.
// For callers the relevant side is From (the caller); for callees it is To.
func callEdgeSymbols(kind CaseKind, edges []cgprovider.CodeEdge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if kind == CaseKindCallers {
			out = append(out, e.From.Symbol)
		} else {
			out = append(out, e.To.Symbol)
		}
	}
	return out
}

// evalImpact runs code_impact and computes recall = |returned ∩ expected| /
// |expected|. recall is 0 when expected is empty (guard against div-by-zero;
// an empty expected set is an authoring error, surfaced as a failure).
// capability_unavailable is a miss (recall 0); any other provider error
// propagates so a transient sidecar fault is not silently scored.
func evalImpact(ctx context.Context, p cgprovider.CodeGraphProvider, set CaseSet, c Case) (float64, *CaseFailure, error) {
	hits, err := p.Impact(ctx, set.GraphRef, cgprovider.ImpactRequest{
		Symbol: c.Symbol, Language: c.Language, Path: c.Path, Depth: c.Depth,
	})
	if err != nil {
		if errors.Is(err, cgprovider.ErrCapabilityUnavailable) {
			return 0, &CaseFailure{Case: c, Missing: c.ExpectedSymbols}, nil
		}
		return 0, nil, err
	}
	got := make([]string, 0, len(hits))
	for _, h := range hits {
		got = append(got, h.Loc.Symbol)
	}
	if len(c.ExpectedSymbols) == 0 {
		return 0, &CaseFailure{Case: c, Got: got, Missing: nil, Extra: got}, nil
	}
	hitsCount := 0
	for _, s := range c.ExpectedSymbols {
		if contains(got, s) {
			hitsCount++
		}
	}
	recall := float64(hitsCount) / float64(len(c.ExpectedSymbols))
	missing := diff(c.ExpectedSymbols, got)
	extra := diff(got, c.ExpectedSymbols)
	commitOK := c.ExpectedCommit == "" || allHitCommitMatch(hits, c.ExpectedCommit)
	if recall >= 1.0 && len(missing) == 0 {
		return recall, nil, nil
	}
	return recall, &CaseFailure{Case: c, Got: got, Missing: missing, Extra: extra, CommitOK: commitOK}, nil
}

// --- small set helpers ---

func contains(set []string, s string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// diff returns elements of a that are not in b (a minus b).
func diff(a, b []string) []string {
	out := make([]string, 0)
	for _, x := range a {
		if !contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func allCommitMatch(edges []cgprovider.CodeEdge, commit string) bool {
	for _, e := range edges {
		if e.From.Commit != "" && e.From.Commit != commit {
			return false
		}
		if e.To.Commit != "" && e.To.Commit != commit {
			return false
		}
	}
	return true
}

func allHitCommitMatch(hits []cgprovider.CodeHit, commit string) bool {
	for _, h := range hits {
		if h.Loc.Commit != "" && h.Loc.Commit != commit {
			return false
		}
	}
	return true
}

// FormatReport renders a Summary as a per-language text report (no aggregation
// into a single recall number — §7.1). Used by the CLI / test output.
func FormatReport(s Summary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "codegraph eval: hard_gate=%v recall_gate=%v\n", s.HardGateOK, s.RecallOK)
	for _, r := range s.ByLanguage {
		fmt.Fprintf(&b, "  %s: def %d/%d call %d/%d impact_recall %.0f%% (≥%.0f%%) failures=%d\n",
			r.Language, r.DefinitionHits, r.DefinitionCases,
			r.CallHits, r.CallCases,
			r.ImpactRecall*100, r.ImpactThreshold*100, len(r.Failed))
		for _, f := range r.Failed {
			fmt.Fprintf(&b, "    FAIL %s/%s symbol=%s missing=%v extra=%v commit_ok=%v\n",
				f.Case.Language, f.Case.Kind, f.Case.Symbol, f.Missing, f.Extra, f.CommitOK)
		}
	}
	return b.String()
}
