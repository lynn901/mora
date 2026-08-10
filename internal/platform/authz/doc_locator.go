package authz

import (
	"context"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// DocLocator resolves workspace/directory/document targets using the existing
// rbac.Repository (DirectoryAncestors / DocumentLocation). It replicates the
// rbac.Engine.locate + targetChain logic verbatim so that delegating the
// engine's internal locate to this locator changes neither input nor output
// (regression red line: Check/VisibleDocuments behavior unchanged).
//
// It is read-only and side-effect free. A document/directory that does not
// exist surfaces via the repository's own error (the engine preserves its
// existing not-found semantics rather than remapping to ErrTargetNotFound,
// to keep behavior identical to pre-delegation).
type DocLocator struct {
	repo rbac.Repository
}

// NewDocLocator builds a DocLocator over the existing rbac repository.
func NewDocLocator(repo rbac.Repository) *DocLocator { return &DocLocator{repo: repo} }

// Locate resolves the workspace and ancestor chain for a doc-family target.
func (l *DocLocator) Locate(ctx context.Context, t TargetType, id uuid.UUID) (Location, error) {
	workspaceID, directoryID, err := l.locate(ctx, t, id)
	if err != nil {
		return Location{}, err
	}
	chain, err := l.targetChain(ctx, t, id, workspaceID, directoryID)
	if err != nil {
		return Location{}, err
	}
	return Location{WorkspaceID: workspaceID, Chain: chain}, nil
}

// locate resolves the workspace and directory for a target of any doc-family
// type. Mirrors rbac.Engine.locate exactly.
func (l *DocLocator) locate(ctx context.Context, t domain.TargetType, id uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	switch t {
	case domain.TargetWorkspace:
		return id, uuid.Nil, nil
	case domain.TargetDirectory:
		anc, err := l.repo.DirectoryAncestors(ctx, id)
		if err != nil || len(anc) == 0 {
			return uuid.Nil, uuid.Nil, err
		}
		return anc[0], id, nil
	case domain.TargetDocument:
		return l.repo.DocumentLocation(ctx, id)
	}
	return uuid.Nil, uuid.Nil, nil
}

// targetChain builds the evaluation order (most-specific first). Mirrors
// rbac.Engine.targetChain exactly.
func (l *DocLocator) targetChain(ctx context.Context, t domain.TargetType, id, workspaceID, directoryID uuid.UUID) ([]Node, error) {
	var nodes []Node
	switch t {
	case domain.TargetDocument:
		nodes = append(nodes, Node{Type: domain.TargetDocument, ID: id})
		if directoryID != uuid.Nil {
			anc, err := l.repo.DirectoryAncestors(ctx, directoryID)
			if err != nil {
				return nil, err
			}
			for i := len(anc) - 1; i >= 0; i-- {
				nodes = append(nodes, Node{Type: domain.TargetDirectory, ID: anc[i]})
			}
		}
		nodes = append(nodes, Node{Type: domain.TargetWorkspace, ID: workspaceID})
	case domain.TargetDirectory:
		anc, err := l.repo.DirectoryAncestors(ctx, id)
		if err != nil {
			return nil, err
		}
		for i := len(anc) - 1; i >= 0; i-- {
			nodes = append(nodes, Node{Type: domain.TargetDirectory, ID: anc[i]})
		}
		nodes = append(nodes, Node{Type: domain.TargetWorkspace, ID: workspaceID})
	case domain.TargetWorkspace:
		nodes = append(nodes, Node{Type: domain.TargetWorkspace, ID: workspaceID})
	}
	return nodes, nil
}

// Compile-time check: DocLocator satisfies ResourceLocator.
var _ ResourceLocator = (*DocLocator)(nil)
