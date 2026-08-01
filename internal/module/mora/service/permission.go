package service

// Package service (permission.go) orchestrates RBAC permission changes with
// cross-layer propagation: after a grant/revoke it publishes permission.change
// events for every affected document so the RAG worker recomputes chunk
// visible_to (design 05 §4.3.3, PRD §7 RBAC cross-layer consistency).
//
// Without this fan-out the Qdrant visible_to payload stays stale after a
// permission change — only the FTS/mora-search path (live RBAC SQL) converges;
// the Dense/MCP search path would never converge.

import (
	"context"

	"github.com/lynn901/mora/internal/domain"
)

// PermissionService wraps PermissionRepo with event propagation. Grant/Revoke
// persist the change, then publish permission.change events for every affected
// document so the RAG pipeline recomputes visible_to.
type PermissionService struct {
	perms  PermissionRepo
	docs   DocumentRepo
	events EventPublisher
}

func NewPermissionService(perms PermissionRepo, docs DocumentRepo, events EventPublisher) *PermissionService {
	return &PermissionService{perms: perms, docs: docs, events: events}
}

func (s *PermissionService) List(ctx context.Context, targetType domain.TargetType, targetID, subjectID *domain.UUID) ([]domain.Permission, error) {
	return s.perms.List(ctx, targetType, targetID, subjectID)
}

// Grant persists the permission and fans out permission.change events for the
// affected document scope so visible_to is recomputed.
func (s *PermissionService) Grant(ctx context.Context, p *domain.Permission) error {
	if err := s.perms.Grant(ctx, p); err != nil {
		return err
	}
	s.publishPermissionChange(ctx, p.TargetType, p.TargetID)
	return nil
}

// Revoke fetches the permission (to learn its target), deletes it, then fans
// out permission.change events for the affected document scope.
func (s *PermissionService) Revoke(ctx context.Context, id domain.UUID) error {
	p, err := s.perms.Get(ctx, id)
	if err != nil {
		return mapNotFound(err)
	}
	if err := s.perms.Revoke(ctx, id); err != nil {
		return err
	}
	s.publishPermissionChange(ctx, p.TargetType, p.TargetID)
	return nil
}

// publishPermissionChange enumerates the documents affected by a permission
// target and publishes a permission.change event per document. Publishing is
// best-effort: the permission is already persisted; a missed event leaves
// visible_to stale until the next re-index (eventually consistent).
func (s *PermissionService) publishPermissionChange(ctx context.Context, targetType domain.TargetType, targetID domain.UUID) {
	docIDs, err := s.docs.DocumentIDsForTarget(ctx, targetType, targetID)
	if err != nil {
		return
	}
	var wsID domain.UUID
	if targetType == domain.TargetWorkspace {
		wsID = targetID
	}
	for _, docID := range docIDs {
		if wsID == (domain.UUID{}) {
			if d, err := s.docs.Get(ctx, docID); err == nil {
				wsID = d.WorkspaceID
			}
		}
		_ = s.events.PublishDocumentEvent(ctx, DocumentEvent{
			Type:        EventPermissionChange,
			DocumentID:  docID,
			WorkspaceID: wsID,
		})
	}
}
