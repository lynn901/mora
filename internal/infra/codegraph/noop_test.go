package codegraph

// noop_test.go pins the §3.3 NoopProvider fail-closed contract: when no
// sidecar is configured, every surface returns capability_unavailable and
// NEVER synthesizes a result (design-docs/17 §15 row 3 — the system MUST NOT
// confuse a provider fault, an authorized-empty set, and genuine no-results).
// This is capability-gating contract case T6 (§7.2 "Provider 未启用 →
// capability_unavailable, 文档/MCP 不退化").
//
// The NoopProvider is the default wiring; a code_* MCP tool / REST handler
// that consults it MUST surface capability_unavailable distinctly from a real
// graph with empty results. These tests pin that no path fabricates data.

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// TestNoopProvider_EverySurfaceFailsClosed asserts each provider method on the
// unconfigured provider returns the capability_unavailable sentinel and a
// zero/nil value — never a fabricated graph, hit, edge, or status.
func TestNoopProvider_EverySurfaceFailsClosed(t *testing.T) {
	p := NewNoopProvider()
	ctx := context.Background()

	t.Run("Capabilities", func(t *testing.T) {
		caps, err := p.Capabilities(ctx)
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Empty(t, caps.Languages, "noop must not advertise languages")
		assert.Empty(t, caps.Operations, "noop must not advertise operations")
	})

	t.Run("Build", func(t *testing.T) {
		res, err := p.Build(ctx, cgprovider.BuildRequest{Commit: "c", SourceTreeHash: "h"})
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Empty(t, res.GraphRef, "noop must not fabricate a graph_ref")
		assert.Empty(t, res.SourceTreeRef)
	})

	t.Run("Explore", func(t *testing.T) {
		res, err := p.Explore(ctx, "any", cgprovider.ExploreRequest{Query: "q"})
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Empty(t, res.Hits, "noop must not fabricate hits")
		assert.Empty(t, res.Commit, "noop must not stamp a fake commit")
	})

	t.Run("Search", func(t *testing.T) {
		hits, err := p.Search(ctx, "any", cgprovider.CodeSearchRequest{Query: "q"})
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Nil(t, hits)
	})

	t.Run("Files", func(t *testing.T) {
		ft, err := p.Files(ctx, "any", cgprovider.FilesRequest{})
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Empty(t, ft.Files)
	})

	t.Run("Node", func(t *testing.T) {
		n, err := p.Node(ctx, "any", cgprovider.NodeRequest{Symbol: "S"})
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Empty(t, n.Loc.Symbol, "noop must not fabricate a resolved symbol")
	})

	t.Run("Callers", func(t *testing.T) {
		edges, err := p.Callers(ctx, "any", cgprovider.NodeRequest{Symbol: "S"})
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Nil(t, edges)
	})

	t.Run("Callees", func(t *testing.T) {
		edges, err := p.Callees(ctx, "any", cgprovider.NodeRequest{Symbol: "S"})
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Nil(t, edges)
	})

	t.Run("Impact", func(t *testing.T) {
		hits, err := p.Impact(ctx, "any", cgprovider.ImpactRequest{Symbol: "S"})
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Nil(t, hits)
	})

	t.Run("Status", func(t *testing.T) {
		st, err := p.Status(ctx, "any")
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
		assert.Empty(t, st.Commit, "noop must not fabricate a graph status")
		assert.False(t, st.Stale, "noop must not fabricate a stale flag")
	})

	t.Run("Delete", func(t *testing.T) {
		err := p.Delete(ctx, "any")
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
	})

	t.Run("Health", func(t *testing.T) {
		err := p.Health(ctx)
		require.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
	})
}

// TestNoopProvider_DistinctFromAuthorizedEmpty asserts the capability_unavailable
// sentinel is a distinct state from a successful-but-empty result: a caller can
// tell "provider down" (capability_unavailable) from "provider up, no matches"
// (nil, nil). This is the §15 red line — a regression that returned (nil, nil)
// from the unconfigured provider would mask the unconfigured state.
func TestNoopProvider_DistinctFromAuthorizedEmpty(t *testing.T) {
	p := NewNoopProvider()
	_, err := p.Search(context.Background(), "any", cgprovider.CodeSearchRequest{})
	require.Error(t, err)
	assert.True(t, errors.Is(err, cgprovider.ErrCapabilityUnavailable),
		"noop Search must return capability_unavailable, not a nil error that would mask the unconfigured state")
}

// TestNoopProvider_ImplementsPort is a compile-time contract check that the
// NoopProvider satisfies the provider.CodeGraphProvider port — a regression that
// broke the signature would surface at build time, not runtime.
func TestNoopProvider_ImplementsPort(t *testing.T) {
	var _ cgprovider.CodeGraphProvider = (*NoopProvider)(nil)
}
