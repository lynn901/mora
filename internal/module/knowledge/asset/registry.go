// Package asset defines the Phase 1 asset registry port
// (design-docs/14 §3.1–§3.2, §8 internal/module/knowledge/asset). The registry
// is the transactional write surface for registering a native document as a
// knowledge asset: idempotently inserting knowledge_assets +
// knowledge_asset_versions, CAS-activating current_version_id, and recording
// the legacy_migration system review decisions for backfilled versions.
//
// The port deliberately takes a pgx.Tx so the registry write commits in the
// SAME transaction as the document write (dual-write, §3.1) or the backfill
// batch (§3.2) — never a separate commit. Callers are DocWriteSink (dual-write)
// and the backfill/reconcile runner.
package asset

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
)

// Registration is the input to RegisterDocumentAsset: the document + version
// facts the registry needs to create or update an asset version. Content is
// NOT included — native documents keep their content in documents.content /
// document_versions.content; the asset version only holds a
// native_document_version_id reference (§3.3 "不复制正文").
type Registration struct {
	DocumentID    domain.UUID
	WorkspaceID   domain.UUID
	VersionID     domain.UUID
	VersionNo     int64
	Title         string
	CreatedByType domain.SubjectType
	CreatedByID   domain.UUID
	// GovernanceProfileID selects the system profile under which this version is
	// registered. The legacy_migration profile (§3.4) is used for backfill and
	// for documents dual-written before a workspace-specific profile is chosen.
	// A nil pointer means "use the workspace's legacy_migration profile" — the
	// implementation resolves it so callers (DocWriteSink) don't need to look it up.
	GovernanceProfileID *domain.UUID
	// MigrationServiceAccountID is the service account that approves backfill
	// review requests (§3.4). When non-nil, the registration also records an
	// approved review_request + review_decision for the version. When nil
	// (dual-write of a user-authored new doc), no review row is written — the
	// native document is published by default per §3.1, and review is only
	// recorded on explicit governance actions.
	MigrationServiceAccountID *domain.UUID
}

// Result is the outcome of RegisterDocumentAsset. Created=false means the
// asset/version already existed (dedupe_key hit) — the ids are returned so the
// caller can reference them in the outbox event payload regardless.
type Result struct {
	AssetID       domain.UUID
	VersionID     domain.UUID
	AssetVersionID domain.UUID
	Created       bool // true if a new asset_version row was inserted this call
}

// Registry is the transactional asset write port (§3.1/§3.2).
type Registry interface {
	// RegisterDocumentAsset idempotently registers a native document version as
	// a Document asset version inside tx. On the first version of a document it
	// inserts the knowledge_assets row; on subsequent versions it bumps
	// latest_requested_version_no. It then CAS-activates current_version_id to
	// the registered version (§3.2: backfill only sets the initial value; the
	// CAS WHERE current_version_id IS NULL guard preserves that). When
	// reg.MigrationServiceAccountID is set it also writes the legacy_migration
	// system review_request + review_decision (§3.4). Idempotent: re-registering
	// the same version (dedupe_key hit) returns the existing ids, Created=false.
	RegisterDocumentAsset(ctx context.Context, tx pgx.Tx, reg Registration) (Result, error)

	// LegacyMigrationProfileID returns the workspace's legacy_migration system
	// governance profile id, creating it if missing (§2.2/§3.4). Used by the
	// backfill runner and by dual-write callers to resolve the profile without
	// a separate lookup. Runs inside tx so the profile row commits with the
	// caller's batch.
	LegacyMigrationProfileID(ctx context.Context, tx pgx.Tx, workspaceID domain.UUID) (domain.UUID, error)
}
