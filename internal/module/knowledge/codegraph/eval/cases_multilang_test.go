package eval

// cases_multilang_test.go expands the §7.1 layered case set beyond the seed Go
// baseline: it pins a Go + TypeScript + Python case set so the eval runner's
// per-language non-aggregation contract (§7.1 — recall is NEVER averaged across
// languages) is exercised with more than one language. The case answers are
// deterministic against a fixed fake provider (cases_multilang_provider) so the
// test does not depend on a live sidecar; the point is to pin the runner's
// scoring + non-aggregation, which is what §7.1 mandates.
//
// This is T5's eval-side coverage + a regression guard for §7.1: a future
// change that collapsed per-language recall into a single mean would surface
// here because a weak language (python, 50% recall) must fail the run even
// when a strong language (go, 100%) would mask it under averaging.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// multilangProvider resolves the Go + TypeScript cases perfectly and the
// Python cases at 50% recall (one of two expected impact symbols missing),
// so the per-language gate fails for python but passes for go.
type multilangProvider struct{}

func (multilangProvider) Capabilities(context.Context) (cgprovider.CodeGraphCapabilities, error) {
	return cgprovider.CodeGraphCapabilities{Languages: []string{"go", "typescript", "python"}}, nil
}
func (multilangProvider) Build(context.Context, cgprovider.BuildRequest) (cgprovider.BuildResult, error) {
	return cgprovider.BuildResult{}, nil
}
func (multilangProvider) Explore(context.Context, string, cgprovider.ExploreRequest) (cgprovider.ExploreResult, error) {
	return cgprovider.ExploreResult{}, nil
}
func (multilangProvider) Search(context.Context, string, cgprovider.CodeSearchRequest) ([]cgprovider.CodeHit, error) {
	return nil, nil
}
func (multilangProvider) Files(context.Context, string, cgprovider.FilesRequest) (cgprovider.FileTree, error) {
	return cgprovider.FileTree{}, nil
}
func (multilangProvider) Node(_ context.Context, _ string, req cgprovider.NodeRequest) (cgprovider.CodeNode, error) {
	// Both go and TS resolve their definition cases.
	switch req.Symbol {
	case "Serve":
		return cgprovider.CodeNode{Loc: cgprovider.CodeLoc{Commit: "go-commit", Path: "main.go", StartLine: 10, Symbol: "Serve", Kind: "function"}}, nil
	case "render":
		return cgprovider.CodeNode{Loc: cgprovider.CodeLoc{Commit: "ts-commit", Path: "app.tsx", StartLine: 5, Symbol: "render", Kind: "function"}}, nil
	}
	return cgprovider.CodeNode{}, nil
}
func (multilangProvider) Callers(_ context.Context, _ string, req cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	if req.Symbol == "Serve" { // go
		return []cgprovider.CodeEdge{{From: cgprovider.CodeLoc{Commit: "go-commit", Path: "main.go", StartLine: 20, Symbol: "main"}, To: cgprovider.CodeLoc{Commit: "go-commit", Path: "main.go", StartLine: 10, Symbol: "Serve"}, Kind: "calls"}}, nil
	}
	if req.Symbol == "render" { // TS
		return []cgprovider.CodeEdge{{From: cgprovider.CodeLoc{Commit: "ts-commit", Path: "index.tsx", StartLine: 8, Symbol: "App"}, To: cgprovider.CodeLoc{Commit: "ts-commit", Path: "app.tsx", StartLine: 5, Symbol: "render"}, Kind: "calls"}}, nil
	}
	return nil, nil
}
func (multilangProvider) Callees(_ context.Context, _ string, req cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	if req.Symbol == "main" { // go
		return []cgprovider.CodeEdge{{From: cgprovider.CodeLoc{Commit: "go-commit", Path: "main.go", StartLine: 20, Symbol: "main"}, To: cgprovider.CodeLoc{Commit: "go-commit", Path: "main.go", StartLine: 10, Symbol: "Serve"}, Kind: "calls"}}, nil
	}
	if req.Symbol == "App" { // TS
		return []cgprovider.CodeEdge{{From: cgprovider.CodeLoc{Commit: "ts-commit", Path: "index.tsx", StartLine: 8, Symbol: "App"}, To: cgprovider.CodeLoc{Commit: "ts-commit", Path: "app.tsx", StartLine: 5, Symbol: "render"}, Kind: "calls"}}, nil
	}
	return nil, nil
}
func (multilangProvider) Impact(_ context.Context, _ string, req cgprovider.ImpactRequest) ([]cgprovider.CodeHit, error) {
	switch req.Symbol {
	case "Serve": // go: full recall {main}
		return []cgprovider.CodeHit{{Loc: cgprovider.CodeLoc{Commit: "go-commit", Path: "main.go", StartLine: 20, Symbol: "main"}}}, nil
	case "render": // TS: full recall {App}
		return []cgprovider.CodeHit{{Loc: cgprovider.CodeLoc{Commit: "ts-commit", Path: "index.tsx", StartLine: 8, Symbol: "App"}}}, nil
	case "parse": // python: 50% recall — expected {tokenize, validate}, returns only {tokenize}
		return []cgprovider.CodeHit{{Loc: cgprovider.CodeLoc{Commit: "py-commit", Path: "parser.py", StartLine: 12, Symbol: "tokenize"}}}, nil
	}
	return nil, nil
}
func (multilangProvider) Status(context.Context, string) (cgprovider.GraphStatus, error) {
	return cgprovider.GraphStatus{}, nil
}
func (multilangProvider) Delete(context.Context, string) error { return nil }
func (multilangProvider) Health(context.Context) error        { return nil }

// multilangCases is the Go + TS + Python case set. The Go + TS layers pass
// every gate; the Python layer has a 50% impact recall that must fail the run
// (≥90% required) — and per-language non-aggregation means go/TS cannot mask it.
var multilangCases = []CaseSet{
	{
		Language: "go", GraphRef: "g-go", Commit: "go-commit",
		Cases: []Case{
			{Language: "go", Kind: CaseKindDefinition, Symbol: "Serve", ExpectedSymbols: []string{"Serve"}, ExpectedCommit: "go-commit"},
			{Language: "go", Kind: CaseKindCallers, Symbol: "Serve", ExpectedSymbols: []string{"main"}, ExpectedCommit: "go-commit"},
			{Language: "go", Kind: CaseKindCallees, Symbol: "main", ExpectedSymbols: []string{"Serve"}, ExpectedCommit: "go-commit"},
			{Language: "go", Kind: CaseKindImpact, Symbol: "Serve", Depth: 2, ExpectedSymbols: []string{"main"}, ExpectedCommit: "go-commit"},
		},
	},
	{
		Language: "typescript", GraphRef: "g-ts", Commit: "ts-commit",
		Cases: []Case{
			{Language: "typescript", Kind: CaseKindDefinition, Symbol: "render", ExpectedSymbols: []string{"render"}, ExpectedCommit: "ts-commit"},
			{Language: "typescript", Kind: CaseKindCallers, Symbol: "render", ExpectedSymbols: []string{"App"}, ExpectedCommit: "ts-commit"},
			{Language: "typescript", Kind: CaseKindCallees, Symbol: "App", ExpectedSymbols: []string{"render"}, ExpectedCommit: "ts-commit"},
		},
	},
	{
		Language: "python", GraphRef: "g-py", Commit: "py-commit",
		Cases: []Case{
			// Python has only an impact case, at 50% recall → fails the gate.
			{Language: "python", Kind: CaseKindImpact, Symbol: "parse", Depth: 2, ExpectedSymbols: []string{"tokenize", "validate"}, ExpectedCommit: "py-commit"},
		},
	},
}

// TestMultilang_WeakPythonNotMaskedByGoOrTS asserts the §7.1 non-aggregation
// contract: the run fails because python's 50% recall is below the 90% gate,
// even though go and typescript pass every gate. Averaging would have yielded
// ~83% and masked the weak language; per-language scoring keeps it visible.
func TestMultilang_WeakPythonNotMaskedByGoOrTS(t *testing.T) {
	summary, err := Run(context.Background(), multilangProvider{}, multilangCases)
	require.NoError(t, err)
	require.Len(t, summary.ByLanguage, 3)

	// go: all gates pass.
	goRep := reportFor(summary, "go")
	assert.Equal(t, 1, goRep.DefinitionHits)
	assert.Equal(t, 2, goRep.CallHits)
	assert.InDelta(t, 1.0, goRep.ImpactRecall, 1e-9)

	// typescript: all gates pass (no impact case → recall vacuously 1.0).
	tsRep := reportFor(summary, "typescript")
	assert.Equal(t, 1, tsRep.DefinitionHits)
	assert.Equal(t, 2, tsRep.CallHits)
	assert.InDelta(t, 1.0, tsRep.ImpactRecall, 1e-9)

	// python: 50% recall → fails.
	pyRep := reportFor(summary, "python")
	assert.InDelta(t, 0.5, pyRep.ImpactRecall, 1e-9, "python recall must be 0.5 (one of two missing)")

	// The run as a whole fails on python's recall despite go/TS passing.
	assert.True(t, summary.HardGateOK, "go + TS hard gates pass; python has no hard-gate cases")
	assert.False(t, summary.RecallOK, "python's 50% recall must fail the run despite go/TS at 100% (§7.1 no aggregation)")
	assert.False(t, summary.Pass())
}

// TestMultilang_ReportHasOneLinePerLanguage asserts FormatReport emits one
// line per language (§7.1 — a weak layer must be legible on its own, not
// collapsed into an averaged recall).
func TestMultilang_ReportHasOneLinePerLanguage(t *testing.T) {
	summary, _ := Run(context.Background(), multilangProvider{}, multilangCases)
	rep := FormatReport(summary)
	for _, lang := range []string{"go:", "typescript:", "python:"} {
		assert.Contains(t, rep, lang, "report must have one line per language (§7.1 no aggregation)")
	}
	assert.Contains(t, rep, "impact_recall 50%")
}

// TestMultilang_AllLanguagesPassWhenPythonComplete asserts that when python
// reaches full recall, the whole run passes — a guard against an over-broad
// failure that would reject a clean multilang run.
func TestMultilang_AllLanguagesPassWhenPythonComplete(t *testing.T) {
	complete := []CaseSet{
		multilangCases[0], // go
		multilangCases[1], // typescript
		{
			Language: "python", GraphRef: "g-py", Commit: "py-commit",
			Cases: []Case{
				// Single expected symbol → the provider returns {tokenize} → 100% recall.
				{Language: "python", Kind: CaseKindImpact, Symbol: "parse", Depth: 2, ExpectedSymbols: []string{"tokenize"}, ExpectedCommit: "py-commit"},
			},
		},
	}
	summary, err := Run(context.Background(), multilangProvider{}, complete)
	require.NoError(t, err)
	assert.True(t, summary.HardGateOK)
	assert.True(t, summary.RecallOK, "all languages at ≥90% recall → run passes")
	assert.True(t, summary.Pass())
}

// reportFor returns the LanguageReport for a language (helper).
func reportFor(s Summary, lang string) LanguageReport {
	for _, r := range s.ByLanguage {
		if r.Language == lang {
			return r
		}
	}
	return LanguageReport{}
}

// Compile-time: multilangProvider satisfies the port.
var _ cgprovider.CodeGraphProvider = multilangProvider{}
