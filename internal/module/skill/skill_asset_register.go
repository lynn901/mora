package skill

// skill_asset_register.go defines the import-path transactional write seam
// that creates the knowledge_assets (asset_type=skill) + knowledge_asset_versions
// rows a skill package mounts on (design-docs/19 §6.1 POST import, §3.1 1:1
// mount). The skill service owns parse/validate/persist via Repository; this
// port owns the asset+version row creation + the MinIO archive put, so the
// service stays infra-free (same layering as asset.Registry over its sink).
//
// The port is separate from asset.Registry (document-specific): a skill has no
// native_document_id, no dual-write into documents, and no legacy_migration
// review — its governance lifecycle is the skill_packages validation_status +
// the standard review inbox. Bundling skill registration onto asset.Registry
// would leak skill concerns into the document write path.

import (
	"context"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// SkillAssetRegistrar creates the knowledge_assets + knowledge_asset_versions
// rows for a skill import in one transaction, and stores the immutable archive
// original in object storage. It returns the asset_version_id the skill
// package mounts on (1:1, §3.1) and the storage_key the ArchiveOpener reads.
//
// Idempotency: a re-import of the same archive (same content_hash) under the
// same asset name is handled by the skill package's content_hash anchor; the
// registrar creates a fresh asset+version per call (a skill import is a
// management action, not a high-volume write).
type SkillAssetRegistrar interface {
	// RegisterSkillAsset creates the asset + version rows and stores the
	// archive. workspaceID scopes the asset (AC-4 workspace isolation).
	// name/version/description come from the SKILL.md frontmatter (or the
	// caller's override); createdBy attributes the import. archiveBytes is the
	// raw tar.gz the caller uploaded — stored verbatim, never exec'd (§4.4).
	// The returned storage_key is the MinIO object key; assetVersionID is the
	// 1:1 mount point for skill_packages.
	RegisterSkillAsset(ctx context.Context, in RegisterSkillInput) (RegisterSkillResult, error)
}

// RegisterSkillInput is the input to a skill import's asset registration.
type RegisterSkillInput struct {
	WorkspaceID uuid.UUID
	Name        string            // asset name (from SKILL.md frontmatter or caller override)
	Description string            // asset description (optional)
	Version     string            // semantic version from SKILL.md frontmatter
	Archive     []byte            // the raw tar.gz archive (stored verbatim in MinIO)
	CreatedBy   domain.EventActor // the importing principal (audit attribution)
}

// RegisterSkillResult is the outcome of a skill asset registration: the ids
// the caller references + the storage_key the ArchiveOpener reads the archive
// from. ContentHash is the archive's sha256 (computed at storage time so the
// import assertion can compare without re-reading).
type RegisterSkillResult struct {
	AssetID       uuid.UUID
	AssetVersionID uuid.UUID
	StorageKey    string
}
