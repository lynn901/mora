// Package worker — codegraph provider adapter. Mirrors wiki_provider_adapter:
// the build handler / query service own a local port that does not import the
// concrete infra/codegraph package (one-way dependency, design-docs/17 §3.3).
// The worker, which already imports both, bridges the real
// provider.CodeGraphProvider to this port. A nil inner provider yields the
// NoopProvider's fail-closed contract (capability_unavailable, §3.3).
package worker

import (
	"context"

	"github.com/google/uuid"

	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// CodeGraphProviderPort is the local worker-side port the build handler depends
// on, so it does not import the concrete provider package. It is a subset of
// provider.CodeGraphProvider covering the build path (Build + Health +
// Capabilities). The worker bridges the real provider here.
type CodeGraphProviderPort interface {
	Build(ctx context.Context, req cgprovider.BuildRequest) (cgprovider.BuildResult, error)
	Health(ctx context.Context) error
}

// CodeGraphProviderAdapter bridges a provider.CodeGraphProvider to the worker's
// local CodeGraphProviderPort. It owns no state; the Capability is resolved per
// build from the asset version's owning workspace. A nil inner provider returns
// capability_unavailable so the build handler fails closed (§3.3).
type CodeGraphProviderAdapter struct {
	Inner cgprovider.CodeGraphProvider
}

// Compile-time check.
var _ CodeGraphProviderPort = (*CodeGraphProviderAdapter)(nil)

// Build delegates to the inner provider. A nil inner fails closed.
func (a *CodeGraphProviderAdapter) Build(ctx context.Context, req cgprovider.BuildRequest) (cgprovider.BuildResult, error) {
	if a.Inner == nil {
		return cgprovider.BuildResult{}, cgprovider.ErrCapabilityUnavailable
	}
	return a.Inner.Build(ctx, req)
}

// Health delegates to the inner provider; nil inner = unavailable.
func (a *CodeGraphProviderAdapter) Health(ctx context.Context) error {
	if a.Inner == nil {
		return cgprovider.ErrCapabilityUnavailable
	}
	return a.Inner.Health(ctx)
}

// capability builds the §10.1 Capability envelope for a build. WorkspaceID is
// the codebase asset's owning workspace; budget fields use generous defaults
// (the sidecar trims from the acting principal's RBAC in production). The
// DecisionID + AuthzRevision are carried through from the version's governance
// snapshot when available.
func capability(workspaceID uuid.UUID, decisionID uuid.UUID, authzRev int64) cgprovider.Capability {
	return cgprovider.Capability{
		WorkspaceID:    workspaceID,
		AuthzRevision:  authzRev,
		DecisionID:     decisionID,
		MaxReadBytes:   64 << 20, // 64 MiB
		MaxReadFiles:   50000,
		MaxResults:     1000,
	}
}
