package domain

import (
	"time"

	"github.com/google/uuid"
)

type UUID = uuid.UUID

func NewUUID() UUID { return uuid.New() }

type User struct {
	ID           UUID      `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	AvatarURL    *string   `json:"avatar_url,omitempty"`
	Status       string    `json:"status"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type Group struct {
	ID          UUID      `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type ServiceAccount struct {
	ID          UUID      `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Workspace struct {
	ID          UUID      `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description,omitempty"`
	OwnerID     UUID      `json:"owner_id"`
	Settings    any       `json:"settings"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Directory struct {
	ID          UUID      `json:"id"`
	WorkspaceID UUID      `json:"workspace_id"`
	ParentID    *UUID     `json:"parent_id,omitempty"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	SortOrder   int       `json:"sort_order"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Children    []*Directory `json:"children,omitempty"`
}

type DocumentStatus string

const (
	StatusDraft     DocumentStatus = "draft"
	StatusPublished DocumentStatus = "published"
	StatusArchived  DocumentStatus = "archived"
	StatusDeleted   DocumentStatus = "deleted"
)

type IndexStatus string

const (
	IndexPending    IndexStatus = "pending"
	IndexProcessing IndexStatus = "processing"
	IndexIndexed    IndexStatus = "indexed"
	IndexFailed     IndexStatus = "failed"
)

type DocumentFormat string

const (
	FormatBlocks   DocumentFormat = "blocks"
	FormatMarkdown DocumentFormat = "markdown"
)

type Document struct {
	ID           UUID            `json:"id"`
	WorkspaceID  UUID            `json:"workspace_id"`
	DirectoryID  *UUID           `json:"directory_id,omitempty"`
	Title        string          `json:"title"`
	Content      []Block         `json:"content"`
	ContentText  string          `json:"-"`
	Format       DocumentFormat  `json:"format"`
	Status       DocumentStatus  `json:"status"`
	IndexStatus  IndexStatus     `json:"index_status"`
	VersionNo    int             `json:"version_no"`
	Tags         []UUID          `json:"tags,omitempty"`
	CreatedBy    UUID            `json:"created_by"`
	UpdatedBy    *UUID           `json:"updated_by,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type DocumentVersion struct {
	ID          UUID      `json:"id"`
	DocumentID  UUID      `json:"document_id"`
	VersionNo   int       `json:"version_no"`
	Content     []Block   `json:"content"`
	ContentText string    `json:"-"`
	DiffSummary string    `json:"diff_summary,omitempty"`
	AuthorID    UUID      `json:"author_id"`
	CreatedAt   time.Time `json:"created_at"`
}

type Tag struct {
	ID          UUID      `json:"id"`
	WorkspaceID UUID      `json:"workspace_id"`
	Name        string    `json:"name"`
	Color       string    `json:"color,omitempty"`
	ParentID    *UUID     `json:"parent_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type Attachment struct {
	ID          UUID      `json:"id"`
	DocumentID  UUID      `json:"document_id"`
	Name        string    `json:"name"`
	MimeType    string    `json:"mime_type"`
	SizeBytes   int64     `json:"size_bytes"`
	StorageKey  string    `json:"storage_key"`
	StorageType string    `json:"storage_type"`
	UploadedBy  UUID      `json:"uploaded_by"`
	CreatedAt   time.Time `json:"created_at"`
}
