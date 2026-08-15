package eval

// cases_seed.go is the seed baseline case set for the codegraph eval runner
// (design-docs/17 §7.1). It demonstrates the case format the test engineer
// ([@Mora知识库测试工程师]) fills per declared language: one CaseSet per
// language, each carrying the baseline repo's graph_ref + commit + the queries
// with their expected answers.
//
// This seed is intentionally minimal (a few Go cases) so the runner is
// exercisable end-to-end before the full per-language case sets land. The hard
// gate (definition / call 100% hit) and recall gate (impact ≥ 90%) are enforced
// against whatever cases are present; an empty seed vacuously passes (no gate
// to fail) — the test engineer's expansion is what turns it into a real gate.

// SeedGoBaseline is the seed Go baseline case set. GraphRef/Commit are
// placeholders ("go-baseline" / "seed-commit") — the test engineer pins them to
// the real baseline repo when authoring the cases; the runner is provider-
// agnostic (it takes a CaseSet, not a fixture path).
var SeedGoBaseline = CaseSet{
	Language: "go",
	GraphRef: "go-baseline",
	Commit:   "seed-commit",
	Cases: []Case{
		// Definition: code_node must resolve the symbol.
		{Language: "go", Kind: CaseKindDefinition, Symbol: "Serve", ExpectedSymbols: []string{"Serve"}, ExpectedCommit: "seed-commit"},
		// Callers: every expected caller must surface (100% hit).
		{Language: "go", Kind: CaseKindCallers, Symbol: "Serve", ExpectedSymbols: []string{"main"}, ExpectedCommit: "seed-commit"},
		// Callees: every expected callee must surface (100% hit).
		{Language: "go", Kind: CaseKindCallees, Symbol: "main", ExpectedSymbols: []string{"Serve"}, ExpectedCommit: "seed-commit"},
		// Impact: recall ≥ 90% against the reference impact set.
		{Language: "go", Kind: CaseKindImpact, Symbol: "Serve", Depth: 2, ExpectedSymbols: []string{"main"}, ExpectedCommit: "seed-commit"},
	},
}
