package service

import (
	"context"
	"time"

	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/pkg/pagination"
)

// Repositories aggregates the storage interfaces the Wiki module needs.
// Implementations live in internal/infra/postgres. Keeping them as interfaces
// allows unit-testing the service layer with fakes and swapping backends.

type WorkspaceRepo interface {
	List(ctx context.Context, userID domain.UUID) ([]domain.Workspace, error)
	Get(ctx context.Context, id domain.UUID) (*domain.Workspace, error)
	Create(ctx context.Context, ws *domain.Workspace) error
}

type DirectoryRepo interface {
	ListByWorkspace(ctx context.Context, workspaceID domain.UUID) ([]domain.Directory, error)
	Get(ctx context.Context, id domain.UUID) (*domain.Directory, error)
	Create(ctx context.Context, d *domain.Directory) error
	Update(ctx context.Context, d *domain.Directory) error
	Delete(ctx context.Context, id domain.UUID) error
	// Ancestors returns directory IDs root-first (for RBAC inheritance).
	Ancestors(ctx context.Context, dirID domain.UUID) ([]domain.UUID, error)
}

type DocumentRepo interface {
	List(ctx context.Context, q DocumentQuery) ([]domain.Document, int, error)
	Get(ctx context.Context, id domain.UUID) (*domain.Document, error)
	Create(ctx context.Context, d *domain.Document) error
	Update(ctx context.Context, d *domain.Document, prevVersion int) error
	SoftDelete(ctx context.Context, id domain.UUID, userID domain.UUID) error
}

type DocumentQuery struct {
	WorkspaceID  domain.UUID
	DirectoryID  *domain.UUID
	TagID        *domain.UUID
	Status       domain.DocumentStatus
	CreatedBy    *domain.UUID
	UpdatedAfter *time.Time
	VisibleDocs  []domain.UUID // RBAC filter: empty + VisibleAll=false → none visible
	VisibleAll   bool          // workspace-wide read grants visibility to all
	pagination.Params
}

type VersionRepo interface {
	List(ctx context.Context, documentID domain.UUID, p pagination.Params) ([]domain.DocumentVersion, int, error)
	Get(ctx context.Context, documentID domain.UUID, versionNo int) (*domain.DocumentVersion, error)
	Create(ctx context.Context, v *domain.DocumentVersion) error
	MaxVersionNo(ctx context.Context, documentID domain.UUID) (int, error)
}

type TagRepo interface {
	ListByWorkspace(ctx context.Context, workspaceID domain.UUID) ([]domain.Tag, error)
	Create(ctx context.Context, t *domain.Tag) error
	SetDocumentTags(ctx context.Context, documentID domain.UUID, tagIDs []domain.UUID) error
}

type CommentRepo interface {
	List(ctx context.Context, documentID domain.UUID, blockID *domain.UUID) ([]domain.Comment, error)
	Create(ctx context.Context, c *domain.Comment) error
	Resolve(ctx context.Context, id, resolvedBy domain.UUID) error
}

type AuditRepo interface {
	Append(ctx context.Context, log *domain.AuditLog) error
}

type PermissionRepo interface {
	List(ctx context.Context, targetType domain.TargetType, targetID, subjectID *domain.UUID) ([]domain.Permission, error)
	Grant(ctx context.Context, p *domain.Permission) error
	Revoke(ctx context.Context, id domain.UUID) error
	// GrantsFor resolves effective grants for a subject within a workspace.
	GrantsFor(ctx context.Context, subjectID domain.UUID, groupIDs []domain.UUID, workspaceID domain.UUID) ([]domain.Grant, error)
}

// UserQuery parameters for listing users visible to a viewer. RBAC scoping is
// enforced by the repository: a non-admin viewer only receives users who share
// at least one readable workspace (plus the viewer themselves), preventing
// unauthorized user enumeration. Admins receive all active users.
type UserQuery struct {
	ViewerID domain.UUID
	IsAdmin  bool
	Search   string // optional case-insensitive name/email substring filter
	pagination.Params
}

// UserRepo lists users within the caller's visible scope (04-api-contract §3.5).
type UserRepo interface {
	List(ctx context.Context, q UserQuery) ([]domain.User, int, error)
}

// RoleRepo lists roles — the relatively static dictionary that Permission.role_id
// references (04-api-contract §3.5). Cacheable by consumers.
type RoleRepo interface {
	List(ctx context.Context) ([]domain.Role, error)
}

// EventPublisher publishes document change events to the message queue
// (Valkey Streams), consumed by the RAG worker. Wiki never calls RAG directly.
type EventPublisher interface {
	PublishDocumentEvent(ctx context.Context, evt DocumentEvent) error
}

type DocumentEventType string

const (
	EventCreate           DocumentEventType = "create"
	EventUpdate           DocumentEventType = "update"
	EventDelete           DocumentEventType = "delete"
	EventPermissionChange DocumentEventType = "permission_change"
)

type DocumentEvent struct {
	EventID     string             `json:"event_id"`
	Type        DocumentEventType  `json:"event_type"`
	DocumentID  domain.UUID        `json:"document_id"`
	WorkspaceID domain.UUID        `json:"workspace_id"`
	VersionNo   int                `json:"version_no"`
	Timestamp   time.Time          `json:"timestamp"`
}
