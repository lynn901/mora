package authz

import (
	"context"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// AsLocator adapts a ResourceLocator (returning authz.Location) to the
// rbac.Engine's Locator contract (returning workspaceID + []LocatorNode).
// It is the seam the engine's SetLocator consumes; wiring a CompositeLocator
// (or DocLocator) through AsLocator lets the engine delegate target
// resolution without importing authz (avoiding an import cycle, since authz
// already imports rbac for its Repository type).
//
// The adapted Locate returns the underlying locator's error to the engine
// when resolution fails, so non-existent / non-visible targets surface as a
// decision error rather than leaking existence.
func AsLocator(loc ResourceLocator) rbac.Locator {
	return &adapter{loc: loc}
}

// adapter implements rbac.Locator by delegating to a ResourceLocator.
type adapter struct {
	loc ResourceLocator
}

func (a *adapter) Locate(ctx context.Context, t domain.TargetType, id uuid.UUID) (uuid.UUID, []rbac.LocatorNode, error) {
	l, err := a.loc.Locate(ctx, TargetType(t), id)
	if err != nil {
		return uuid.Nil, nil, err
	}
	nodes := make([]rbac.LocatorNode, len(l.Chain))
	for i, n := range l.Chain {
		nodes[i] = rbac.LocatorNode{Type: domain.TargetType(n.Type), ID: n.ID}
	}
	return l.WorkspaceID, nodes, nil
}

var _ rbac.Locator = (*adapter)(nil)
