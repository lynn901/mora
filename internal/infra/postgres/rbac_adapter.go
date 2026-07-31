package postgres

import (
	"context"

	"github.com/google/uuid"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/platform/rbac"
)

// RBACAdapter combines PermissionRepo, DirectoryRepo and DocumentRepo to
// satisfy the rbac.Engine's Repository interface (GrantsFor + DocumentLocation
// from perms, DirectoryAncestors from dirs, DocumentsInDirectorySubtree from
// docs).
type RBACAdapter struct {
	perms *PermissionRepo
	dirs  *DirectoryRepo
	docs  *DocumentRepo
}

func NewRBACAdapter(perms *PermissionRepo, dirs *DirectoryRepo, docs *DocumentRepo) *RBACAdapter {
	return &RBACAdapter{perms: perms, dirs: dirs, docs: docs}
}

func (a *RBACAdapter) GrantsFor(ctx context.Context, subjectID uuid.UUID, groupIDs []uuid.UUID, workspaceID uuid.UUID) ([]domain.Grant, error) {
	return a.perms.GrantsFor(ctx, subjectID, groupIDs, workspaceID)
}

func (a *RBACAdapter) DirectoryAncestors(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error) {
	return a.dirs.Ancestors(ctx, directoryID)
}

func (a *RBACAdapter) DocumentLocation(ctx context.Context, documentID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	return a.perms.DocumentLocation(ctx, documentID)
}

func (a *RBACAdapter) DocumentsInDirectorySubtree(ctx context.Context, directoryID uuid.UUID) ([]uuid.UUID, error) {
	return a.docs.DocumentIDsForTarget(ctx, domain.TargetDirectory, directoryID)
}

// compile-time interface check
var _ rbac.Repository = (*RBACAdapter)(nil)
