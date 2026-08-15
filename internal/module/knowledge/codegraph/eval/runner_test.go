package eval

// runner_test.go exercises the eval runner (design-docs/17 §7.1) with two
// providers: the NoopProvider (capability_unavailable → every case fails, no
// panic, both gates false) and a fake happy-path provider (100% definition/call
// hit + full recall → both gates pass). Pins the scoring + per-language report
// shape so the test engineer's case sets land against a stable contract.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/infra/codegraph"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// TestRun_NoopProviderFailsClosed asserts the runner degrades cleanly when the
// provider is unconfigured: every query returns capability_unavailable, the
// runner records failures (not a panic), and both gates are false.
func TestRun_NoopProviderFailsClosed(t *testing.T) {
	p := codegraph.NewNoopProvider()
	summary, err := Run(context.Background(), p, []CaseSet{SeedGoBaseline})
	require.NoError(t, err, "runner must not error on a provider fault — it scores it")
	require.Len(t, summary.ByLanguage, 1)
	rep := summary.ByLanguage[0]
	assert.Equal(t, "go", rep.Language)
	assert.Equal(t, 1, rep.DefinitionCases)
	assert.Equal(t, 0, rep.DefinitionHits, "noop must miss every definition")
	assert.Equal(t, 0, rep.CallHits, "noop must miss every call case")
	assert.Equal(t, 0.0, rep.ImpactRecall, "noop impact recall must be 0")
	assert.False(t, summary.HardGateOK)
	assert.False(t, summary.RecallOK)
	assert.NotEmpty(t, rep.Failed, "failures must be recorded for triage")
	assert.False(t, summary.Pass())
}

// fakeProvider is a happy-path provider that resolves the SeedGoBaseline cases
// perfectly: Serve is defined, main calls Serve, and Serve's impact set is
// {main}. Used to prove the runner scores a 100%-hit + full-recall run as PASS.
type fakeProvider struct {
	commit string
}

func (fakeProvider) Capabilities(context.Context) (cgprovider.CodeGraphCapabilities, error) {
	return cgprovider.CodeGraphCapabilities{Languages: []string{"go"}, Operations: []string{"node", "callers", "callees", "impact"}}, nil
}
func (fakeProvider) Build(context.Context, cgprovider.BuildRequest) (cgprovider.BuildResult, error) {
	return cgprovider.BuildResult{}, nil
}
func (fakeProvider) Explore(context.Context, string, cgprovider.ExploreRequest) (cgprovider.ExploreResult, error) {
	return cgprovider.ExploreResult{}, nil
}
func (fakeProvider) Search(context.Context, string, cgprovider.CodeSearchRequest) ([]cgprovider.CodeHit, error) {
	return nil, nil
}
func (fakeProvider) Files(context.Context, string, cgprovider.FilesRequest) (cgprovider.FileTree, error) {
	return cgprovider.FileTree{}, nil
}
func (f fakeProvider) Node(_ context.Context, _ string, req cgprovider.NodeRequest) (cgprovider.CodeNode, error) {
	if req.Symbol == "Serve" {
		return cgprovider.CodeNode{Loc: cgprovider.CodeLoc{Commit: f.commit, Path: "main.go", StartLine: 10, Symbol: "Serve", Kind: "function"}, Signature: "func Serve()"}, nil
	}
	return cgprovider.CodeNode{}, nil
}
func (f fakeProvider) Callers(_ context.Context, _ string, req cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	if req.Symbol == "Serve" {
		return []cgprovider.CodeEdge{{From: cgprovider.CodeLoc{Commit: f.commit, Path: "main.go", StartLine: 20, Symbol: "main"}, To: cgprovider.CodeLoc{Commit: f.commit, Path: "main.go", StartLine: 10, Symbol: "Serve"}, Kind: "calls"}}, nil
	}
	return nil, nil
}
func (f fakeProvider) Callees(_ context.Context, _ string, req cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	if req.Symbol == "main" {
		return []cgprovider.CodeEdge{{From: cgprovider.CodeLoc{Commit: f.commit, Path: "main.go", StartLine: 20, Symbol: "main"}, To: cgprovider.CodeLoc{Commit: f.commit, Path: "main.go", StartLine: 10, Symbol: "Serve"}, Kind: "calls"}}, nil
	}
	return nil, nil
}
func (f fakeProvider) Impact(_ context.Context, _ string, req cgprovider.ImpactRequest) ([]cgprovider.CodeHit, error) {
	if req.Symbol == "Serve" {
		return []cgprovider.CodeHit{{Loc: cgprovider.CodeLoc{Commit: f.commit, Path: "main.go", StartLine: 20, Symbol: "main"}}}, nil
	}
	return nil, nil
}
func (fakeProvider) Status(context.Context, string) (cgprovider.GraphStatus, error) {
	return cgprovider.GraphStatus{}, nil
}
func (fakeProvider) Delete(context.Context, string) error { return nil }
func (fakeProvider) Health(context.Context) error         { return nil }

// TestRun_FakeProviderPassesBothGates asserts a perfect provider scores
// definition 1/1, call 2/2, impact recall 100% → PASS.
func TestRun_FakeProviderPassesBothGates(t *testing.T) {
	p := fakeProvider{commit: "seed-commit"}
	summary, err := Run(context.Background(), p, []CaseSet{SeedGoBaseline})
	require.NoError(t, err)
	rep := summary.ByLanguage[0]
	assert.Equal(t, 1, rep.DefinitionHits)
	assert.Equal(t, 2, rep.CallCases)
	assert.Equal(t, 2, rep.CallHits)
	assert.InDelta(t, 1.0, rep.ImpactRecall, 1e-9)
	assert.Empty(t, rep.Failed)
	assert.True(t, summary.HardGateOK)
	assert.True(t, summary.RecallOK)
	assert.True(t, summary.Pass())
}

// TestRun_ImpactRecallBelowThresholdFailsGate asserts a provider that misses
// one impact symbol drives recall below 1.0. With a single expected symbol the
// recall is binary (0 or 1) — use a two-symbol expected set to get 50% recall
// and confirm the recall gate fails (≥ 90% required).
func TestRun_ImpactRecallBelowThresholdFailsGate(t *testing.T) {
	set := CaseSet{
		Language: "go", GraphRef: "g", Commit: "c",
		Cases: []Case{
			{Language: "go", Kind: CaseKindImpact, Symbol: "Serve",
				ExpectedSymbols: []string{"main", "init"}, ExpectedCommit: "c"},
		},
	}
	p := fakeProvider{commit: "c"} // returns only {main} for Serve's impact
	summary, err := Run(context.Background(), p, []CaseSet{set})
	require.NoError(t, err)
	assert.InDelta(t, 0.5, summary.ByLanguage[0].ImpactRecall, 1e-9)
	assert.False(t, summary.RecallOK, "50% recall must fail the ≥90% gate")
	assert.False(t, summary.Pass())
}

// TestRun_PerLanguageNoAggregation asserts a weak language is NOT masked by a
// strong one: two languages, one passing, one failing recall → the run fails
// (recall is AND across languages, never averaged, §7.1).
func TestRun_PerLanguageNoAggregation(t *testing.T) {
	good := CaseSet{Language: "go", GraphRef: "g", Commit: "seed-commit", Cases: SeedGoBaseline.Cases}
	bad := CaseSet{
		Language: "python", GraphRef: "g2", Commit: "c2",
		Cases: []Case{
			{Language: "python", Kind: CaseKindImpact, Symbol: "X",
				ExpectedSymbols: []string{"a", "b"}, ExpectedCommit: "c2"},
		},
	}
	// fakeProvider returns the seed-correct answers for go (commit seed-commit)
	// and nothing for python/X → go passes every gate, python is 0% recall.
	p := fakeProvider{commit: "seed-commit"}
	summary, err := Run(context.Background(), p, []CaseSet{good, bad})
	require.NoError(t, err)
	require.Len(t, summary.ByLanguage, 2)
	// The go language is fine; python is 0% recall. Aggregation would have
	// averaged to ~50% and possibly masked it; per-language keeps it visible.
	assert.True(t, summary.HardGateOK, "python has no hard-gate cases; def/call vacuously pass")
	assert.False(t, summary.RecallOK, "python's 0% recall must fail the run despite go's 100%")
	assert.False(t, summary.Pass())
}

// TestRun_ErrorFromProviderPropagates asserts a provider returning a non-
// capability error (e.g. a transient sidecar fault) surfaces as a run error,
// not silently scored as a miss.
func TestRun_ErrorFromProviderPropagates(t *testing.T) {
	p := errProvider{}
	_, err := Run(context.Background(), p, []CaseSet{SeedGoBaseline})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sidecar fault")
}

// errProvider returns a transient error from every query method.
type errProvider struct{ fakeProvider }

func (errProvider) Node(context.Context, string, cgprovider.NodeRequest) (cgprovider.CodeNode, error) {
	return cgprovider.CodeNode{}, errors.New("sidecar fault")
}
func (errProvider) Callers(context.Context, string, cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	return nil, errors.New("sidecar fault")
}
func (errProvider) Impact(context.Context, string, cgprovider.ImpactRequest) ([]cgprovider.CodeHit, error) {
	return nil, errors.New("sidecar fault")
}

// TestFormatReport_NoAggregation asserts the text report emits one line per
// language (not a single averaged recall), so a weak layer is legible.
func TestFormatReport_NoAggregation(t *testing.T) {
	p := fakeProvider{commit: "seed-commit"}
	summary, _ := Run(context.Background(), p, []CaseSet{SeedGoBaseline})
	rep := FormatReport(summary)
	assert.Contains(t, rep, "go:")
	assert.Contains(t, rep, "impact_recall 100%")
}
