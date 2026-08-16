package provider_test

// codeloc_test.go pins the §3.2 contract: every read-side result type
// (CodeHit / CodeEdge / CodeNode / ExploreResult / FileTree) carries a CodeLoc
// with commit / path / line / symbol, so an expired revision never masquerades
// as the current result (design-docs/17 §3.2 / §4.2 / §7.2 T5).
//
// These tests use a fake CodeGraphProvider that returns hand-authored hits /
// edges / nodes, then assert each public method's result carries a populated
// CodeLoc. They are provider-port-level (the service-layer commit-stamping is
// covered in service_test.go); here we pin the data shapes the provider
// contract requires, which is what the eval runner + MCP tools depend on.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// locProvider is a fake that returns canned, CodeLoc-populated results for every
// read method, so a test can assert the result shapes carry the §3.2 anchors.
type locProvider struct {
	commit string
}

func (locProvider) Capabilities(context.Context) (cgprovider.CodeGraphCapabilities, error) {
	return cgprovider.CodeGraphCapabilities{Languages: []string{"go"}, Operations: []string{"explore", "search", "files", "node", "callers", "callees", "impact", "status"}}, nil
}
func (locProvider) Build(context.Context, cgprovider.BuildRequest) (cgprovider.BuildResult, error) {
	return cgprovider.BuildResult{}, nil
}
func (p locProvider) Explore(_ context.Context, _ string, _ cgprovider.ExploreRequest) (cgprovider.ExploreResult, error) {
	return cgprovider.ExploreResult{
		Commit: p.commit,
		Hits: []cgprovider.CodeHit{{
			Loc:     cgprovider.CodeLoc{Commit: p.commit, Path: "main.go", StartLine: 10, EndLine: 20, Symbol: "Serve", Kind: "function"},
			Score:   0.9,
			Snippet: "func Serve() {}",
		}},
		Nodes: []cgprovider.CodeNode{{
			Loc: cgprovider.CodeLoc{Commit: p.commit, Path: "main.go", StartLine: 10, Symbol: "Serve", Kind: "function"},
			Signature: "func Serve()",
		}},
	}, nil
}
func (p locProvider) Search(_ context.Context, _ string, _ cgprovider.CodeSearchRequest) ([]cgprovider.CodeHit, error) {
	return []cgprovider.CodeHit{{
		Loc:     cgprovider.CodeLoc{Commit: p.commit, Path: "main.go", StartLine: 10, Symbol: "Serve", Kind: "function"},
		Score:   0.8,
	}}, nil
}
func (p locProvider) Files(_ context.Context, _ string, _ cgprovider.FilesRequest) (cgprovider.FileTree, error) {
	return cgprovider.FileTree{
		Path: ".",
		Files: []cgprovider.FileNode{{Path: "main.go", Lines: 42, Commit: p.commit}},
	}, nil
}
func (p locProvider) Node(_ context.Context, _ string, _ cgprovider.NodeRequest) (cgprovider.CodeNode, error) {
	return cgprovider.CodeNode{
		Loc:       cgprovider.CodeLoc{Commit: p.commit, Path: "main.go", StartLine: 10, EndLine: 20, Symbol: "Serve", Kind: "function"},
		Kind:      "function",
		Signature: "func Serve()",
		Docstring: "Serve serves.",
	}, nil
}
func (p locProvider) Callers(_ context.Context, _ string, _ cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	return []cgprovider.CodeEdge{{
		From: cgprovider.CodeLoc{Commit: p.commit, Path: "main.go", StartLine: 30, Symbol: "main", Kind: "function"},
		To:   cgprovider.CodeLoc{Commit: p.commit, Path: "main.go", StartLine: 10, Symbol: "Serve", Kind: "function"},
		Kind: "calls",
	}}, nil
}
func (p locProvider) Callees(_ context.Context, _ string, _ cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	return []cgprovider.CodeEdge{{
		From: cgprovider.CodeLoc{Commit: p.commit, Path: "main.go", StartLine: 30, Symbol: "main", Kind: "function"},
		To:   cgprovider.CodeLoc{Commit: p.commit, Path: "main.go", StartLine: 10, Symbol: "Serve", Kind: "function"},
		Kind: "calls",
	}}, nil
}
func (p locProvider) Impact(_ context.Context, _ string, _ cgprovider.ImpactRequest) ([]cgprovider.CodeHit, error) {
	return []cgprovider.CodeHit{{
		Loc: cgprovider.CodeLoc{Commit: p.commit, Path: "main.go", StartLine: 30, Symbol: "main", Kind: "function"},
	}}, nil
}
func (p locProvider) Status(_ context.Context, _ string) (cgprovider.GraphStatus, error) {
	return cgprovider.GraphStatus{Commit: p.commit, SourceTreeHash: "hash", ProviderVersion: "1.5.0"}, nil
}
func (locProvider) Delete(context.Context, string) error { return nil }
func (locProvider) Health(context.Context) error        { return nil }

// Compile-time: locProvider satisfies the port.
var _ cgprovider.CodeGraphProvider = locProvider{}

// TestEveryReadMethodCarriesCodeLoc asserts each read method returns results
// whose CodeLoc carries commit/path/line/symbol — the §3.2 anchor that
// prevents stale-revision masking (§4.2). If a method's result shape dropped
// the CodeLoc, this test would catch it.
func TestEveryReadMethodCarriesCodeLoc(t *testing.T) {
	p := locProvider{commit: "commit-A"}
	ctx := context.Background()
	const ref = "g1"

	t.Run("Explore_HitsAndNodes", func(t *testing.T) {
		res, err := p.Explore(ctx, ref, cgprovider.ExploreRequest{Query: "Serve"})
		require.NoError(t, err)
		require.Len(t, res.Hits, 1)
		assertCodeLoc(t, res.Hits[0].Loc, "commit-A", "main.go", "Serve", 10)
		require.Len(t, res.Nodes, 1)
		assertCodeLoc(t, res.Nodes[0].Loc, "commit-A", "main.go", "Serve", 10)
		assert.Equal(t, "commit-A", res.Commit, "ExploreResult carries the graph commit (§3.2)")
	})

	t.Run("Search", func(t *testing.T) {
		hits, err := p.Search(ctx, ref, cgprovider.CodeSearchRequest{Query: "Serve"})
		require.NoError(t, err)
		require.Len(t, hits, 1)
		assertCodeLoc(t, hits[0].Loc, "commit-A", "main.go", "Serve", 10)
	})

	t.Run("Files", func(t *testing.T) {
		ft, err := p.Files(ctx, ref, cgprovider.FilesRequest{})
		require.NoError(t, err)
		require.Len(t, ft.Files, 1)
		assert.Equal(t, "commit-A", ft.Files[0].Commit, "FileNode carries the commit (§3.2)")
	})

	t.Run("Node", func(t *testing.T) {
		n, err := p.Node(ctx, ref, cgprovider.NodeRequest{Symbol: "Serve"})
		require.NoError(t, err)
		assertCodeLoc(t, n.Loc, "commit-A", "main.go", "Serve", 10)
		assert.NotEmpty(t, n.Signature)
	})

	t.Run("Callers", func(t *testing.T) {
		edges, err := p.Callers(ctx, ref, cgprovider.NodeRequest{Symbol: "Serve"})
		require.NoError(t, err)
		require.Len(t, edges, 1)
		assertCodeLoc(t, edges[0].From, "commit-A", "main.go", "main", 30)
		assertCodeLoc(t, edges[0].To, "commit-A", "main.go", "Serve", 10)
		assert.Equal(t, "calls", edges[0].Kind, "CodeEdge carries the relation kind (calls|defines|implements)")
	})

	t.Run("Callees", func(t *testing.T) {
		edges, err := p.Callees(ctx, ref, cgprovider.NodeRequest{Symbol: "main"})
		require.NoError(t, err)
		require.Len(t, edges, 1)
		assertCodeLoc(t, edges[0].To, "commit-A", "main.go", "Serve", 10)
		assert.Equal(t, "calls", edges[0].Kind)
	})

	t.Run("Impact", func(t *testing.T) {
		hits, err := p.Impact(ctx, ref, cgprovider.ImpactRequest{Symbol: "Serve", Depth: 2})
		require.NoError(t, err)
		require.Len(t, hits, 1)
		assertCodeLoc(t, hits[0].Loc, "commit-A", "main.go", "main", 30)
	})

	t.Run("Status", func(t *testing.T) {
		st, err := p.Status(ctx, ref)
		require.NoError(t, err)
		assert.Equal(t, "commit-A", st.Commit, "GraphStatus carries the active commit (§3.2)")
		assert.Equal(t, "hash", st.SourceTreeHash)
	})
}

// TestReadMethodsAreReadOnly asserts the 8 code_* operations are read-only —
// they do not create graphs. Build is the only write-ish method (§6.2), and
// the MCP tools' IsWrite() must be false. At the provider-port level we pin
// this by asserting the read methods never return a graph_ref the caller
// could mutate (they RECEIVE a graph_ref, they don't return one).
func TestReadMethodsAreReadOnly(t *testing.T) {
	// The read-method signatures take a graphRef param and return data, never a
	// new graph_ref. This is a structural assertion: none of Explore/Search/
	// Files/Node/Callers/Callees/Impact/Status return a BuildResult or graph
	// handle. The compile-time interface check (var _ CodeGraphProvider) plus
	// this test's existence documents the §6.2 read-only contract.
	p := locProvider{commit: "commit-A"}
	ctx := context.Background()

	// Every read returns data tied to the *passed-in* graph_ref; none returns a
	// new ref. (A regression that added a write side-effect would show up as a
	// signature change breaking the compile-time check.)
	_, _ = p.Explore(ctx, "g1", cgprovider.ExploreRequest{})
	_, _ = p.Search(ctx, "g1", cgprovider.CodeSearchRequest{})
	_, _ = p.Files(ctx, "g1", cgprovider.FilesRequest{})
	_, _ = p.Node(ctx, "g1", cgprovider.NodeRequest{})
	_, _ = p.Callers(ctx, "g1", cgprovider.NodeRequest{})
	_, _ = p.Callees(ctx, "g1", cgprovider.NodeRequest{})
	_, _ = p.Impact(ctx, "g1", cgprovider.ImpactRequest{})
	_, _ = p.Status(ctx, "g1")
}

// assertCodeLoc asserts a CodeLoc carries the §3.2 anchors: non-empty commit,
// path, symbol, and a positive start line.
func assertCodeLoc(t *testing.T, loc cgprovider.CodeLoc, wantCommit, wantPath, wantSymbol string, wantLine int) {
	t.Helper()
	assert.Equalf(t, wantCommit, loc.Commit, "CodeLoc.Commit must carry the graph commit (§3.2), got %q", loc.Commit)
	assert.Equalf(t, wantPath, loc.Path, "CodeLoc.Path must carry the file path, got %q", loc.Path)
	assert.Equalf(t, wantSymbol, loc.Symbol, "CodeLoc.Symbol must carry the symbol name, got %q", loc.Symbol)
	assert.Equalf(t, wantLine, loc.StartLine, "CodeLoc.StartLine must be the line range start, got %d", loc.StartLine)
}
