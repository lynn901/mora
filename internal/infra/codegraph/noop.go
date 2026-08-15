// Package codegraph — NoopProvider. The deterministic fallback when no sidecar
// is configured (design-docs/17 §3.3). It never synthesizes results: every
// query returns ErrCapabilityUnavailable so the document/RAG/MCP surfaces
// degrade gracefully (§"未启用 CodeGraph 时文档能力继续工作") and a caller can
// never receive faked graph data. Health/Capabilities report unavailable too,
// so the Compose healthcheck + §7.2 contract tests observe the unconfigured
// state distinctly from a real provider fault.
package codegraph

import (
	"context"

	"github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// NoopProvider is the unconfigured-state CodeGraph provider. It is the default
// wiring in mora-api / knowledge-worker; swap it for the SidecarProvider when
// the sidecar profile is enabled (§9).
type NoopProvider struct{}

// NewNoopProvider builds the deterministic fallback provider.
func NewNoopProvider() *NoopProvider { return &NoopProvider{} }

// Capabilities reports no languages / no operations — the contract surface that
// code_* MCP tools consult to decide whether to expose an operation (§6.2).
func (NoopProvider) Capabilities(ctx context.Context) (provider.CodeGraphCapabilities, error) {
	if err := ctx.Err(); err != nil {
		return provider.CodeGraphCapabilities{}, err
	}
	return provider.CodeGraphCapabilities{}, provider.ErrCapabilityUnavailable
}

// Build fails closed: without a configured sidecar there is no graph to build.
func (NoopProvider) Build(ctx context.Context, _ provider.BuildRequest) (provider.BuildResult, error) {
	return provider.BuildResult{}, provider.ErrCapabilityUnavailable
}

// Explore fails closed — never fabricates hits.
func (NoopProvider) Explore(ctx context.Context, _ string, _ provider.ExploreRequest) (provider.ExploreResult, error) {
	return provider.ExploreResult{}, provider.ErrCapabilityUnavailable
}

// Search fails closed.
func (NoopProvider) Search(ctx context.Context, _ string, _ provider.CodeSearchRequest) ([]provider.CodeHit, error) {
	return nil, provider.ErrCapabilityUnavailable
}

// Files fails closed.
func (NoopProvider) Files(ctx context.Context, _ string, _ provider.FilesRequest) (provider.FileTree, error) {
	return provider.FileTree{}, provider.ErrCapabilityUnavailable
}

// Node fails closed.
func (NoopProvider) Node(ctx context.Context, _ string, _ provider.NodeRequest) (provider.CodeNode, error) {
	return provider.CodeNode{}, provider.ErrCapabilityUnavailable
}

// Callers fails closed.
func (NoopProvider) Callers(ctx context.Context, _ string, _ provider.NodeRequest) ([]provider.CodeEdge, error) {
	return nil, provider.ErrCapabilityUnavailable
}

// Callees fails closed.
func (NoopProvider) Callees(ctx context.Context, _ string, _ provider.NodeRequest) ([]provider.CodeEdge, error) {
	return nil, provider.ErrCapabilityUnavailable
}

// Impact fails closed.
func (NoopProvider) Impact(ctx context.Context, _ string, _ provider.ImpactRequest) ([]provider.CodeHit, error) {
	return nil, provider.ErrCapabilityUnavailable
}

// Status fails closed — a query against an unconfigured provider returns the
// capability_unavailable sentinel, not a fabricated graph status.
func (NoopProvider) Status(ctx context.Context, _ string) (provider.GraphStatus, error) {
	return provider.GraphStatus{}, provider.ErrCapabilityUnavailable
}

// Delete is a no-op when there is no provider to hold a graph.
func (NoopProvider) Delete(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return provider.ErrCapabilityUnavailable
}

// Health reports unavailable — the Compose healthcheck surfaces this as the
// sidecar not being wired (distinct from a real provider that is down).
func (NoopProvider) Health(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return provider.ErrCapabilityUnavailable
}

// Compile-time check.
var _ provider.CodeGraphProvider = (*NoopProvider)(nil)
