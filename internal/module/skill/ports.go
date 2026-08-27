// Package skill implements the Skill package governance application service
// (design-docs/19 §2 / §3.1 / §4 / §7 — Phase 5-2, YS-161). It composes the
// three sub-domains (package / compatibility / validate) into the import →
// validate → persist + export pipeline and owns the storage port over
// skill_packages.
//
// Layering (modular monolith): this service declares the storage port
// (Repository); the pgx implementation lives in internal/infra/postgres
// (sub-task D wires it). The service stays pgx-free — same layering as
// module/binding/service over its sink.
//
// Hard invariants (§4.4 / §7 — the script-execution-count = 0 gate):
//   - Import, preview, index, validate are the four paths that must NEVER
//     execute a script. Parse consumes the archive in-memory with size caps;
//     Validate recomputes hashes from a content provider, never exec'ing;
//     Export re-packages from the manifest, never honoring an exec bit.
//   - Mora does not execute Skills. validation_status=passed means saveable /
//     deliverable, NOT executable.
//   - Lossless roundtrip: unknown legal frontmatter is preserved verbatim in
//     original_frontmatter; content_hash is the import→export anchor (§9 gate).
//   - No secret values: signature / provenance_ref carry shape only, never a
//     key or credential (§1.2 不外传 Secret).
package skill

import (
	"context"
	"errors"
	"io"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// Sentinel errors. The handler (sub-task D) maps these to the API envelope.
var (
	// ErrPackageNotFound — no skill_packages row for the asset version (404,
	// no existence leak — same convention as binding.ErrBindingNotFound).
	ErrPackageNotFound = errors.New("skill: package not found")
	// ErrInvalidPackage — structurally invalid import input (400).
	ErrInvalidPackage = errors.New("skill: invalid package")
	// ErrArchiveTooLarge — the archive exceeded a size cap (400). Re-surfaced
	// from package.Parse so the service maps it without importing the sub-pkg.
	ErrArchiveTooLarge = errors.New("skill: archive exceeds size cap")
	// ErrRoundtripMismatch — the export content_hash did not match the import
	// content_hash (§9 gate). 409 / 500 depending on caller; the service
	// treats it as a delivery-blocking failure.
	ErrRoundtripMismatch = errors.New("skill: roundtrip content_hash mismatch")
	// ErrProposalRejected — the agent lacks write on the workspace to submit a
	// skill proposal (§6.3 skill_propose). A deny surfaces as the same not-found
	// shape the delivery path uses (§8.2 no-leak): the caller cannot tell a
	// missing workspace from a write-denied one.
	ErrProposalRejected = errors.New("skill: proposal not permitted")
	// ErrInvalidProposal — a malformed proposal input (missing name / body).
	ErrInvalidProposal = errors.New("skill: invalid proposal")
)

// ScannerVersion is the version of the skill scanner (parse + validate +
// compatibility). Bumped when the scan algorithm changes so stored reports
// are reconcilable against the scanner that produced them (§3.1 scanner_version).
const ScannerVersion = "mora-skill-scanner/1.0"

// ImportOptions parameterizes an import.
type ImportOptions struct {
	AssetVersionID uuid.UUID // the knowledge_asset_versions row this package mounts on (1:1)
	StorageKey     string    // MinIO immutable archive original locator
	// DeclaredFormatID overrides the frontmatter-declared format_id when the
	// caller knows the profile (e.g. an opaque upload with no frontmatter).
	// Empty → infer from SKILL.md frontmatter, else opaque.
	DeclaredFormatID string
}

// ImportResult is the outcome of an import: the persisted SkillPackage (with
// manifest / reports / status) plus the derived content_hash for the §9
// roundtrip assertion. The service persists the package in one tx.
type ImportResult struct {
	Package     domain.SkillPackage
	ContentHash string
}

// ArchiveOpener opens a streaming reader over the immutable archive original
// (the MinIO-backed object). It is the storage-agnostic seam the import path
// uses; the concrete MinIO adapter is wired by the infra layer (sub-task D).
// Tests supply an in-memory opener.
//
// SECURITY: the opener returns the archive AS STORED. Mora never synthesizes
// or re-packs an executable bit at this seam (§4.4).
type ArchiveOpener interface {
	OpenArchive(storageKey string) (io.ReadCloser, error)
}

// Import imports + parses + classifies + validates + persists a skill package
// in one service-level call (§2 / §4). It is the composition root of the
// three sub-domains:
//  1. package.Parse — stream the archive, build the manifest + content_hash,
//     preserve frontmatter verbatim.
//  2. compatibility.Determine — classify the profile, build the
//     compatibility_report.
//  3. validate.Run — run the 8 static checks, build the validation_report +
//     roll up validation_status.
//
// The package is persisted via the Repository in one write. The returned
// ContentHash is the §9 roundtrip anchor the caller can assert on export.
type Import interface {
	Import(ctx context.Context, opts ImportOptions, opener ArchiveOpener) (ImportResult, error)
}

// Revalidate re-runs the static validator on an already-stored package (the
// :validate route). It re-reads the archive via the opener to recompute
// hashes (§4.2 check 5) and persists the updated validation_report /
// validation_status. A nil opener skips hash recomputation (info finding).
type Revalidate interface {
	Revalidate(ctx context.Context, assetVersionID uuid.UUID, opener ArchiveOpener) (domain.SkillPackage, error)
}

// Export losslessly re-derives the archive from the stored manifest +
// original frontmatter and asserts the content_hash equals the import
// content_hash (§9 往返门禁). The returned bytes are the canonical
// re-packaging; the :export route streams them.
type Export interface {
	Export(ctx context.Context, assetVersionID uuid.UUID, opener ArchiveOpener) (ExportOutput, error)
}

// ExportOutput is the exported archive + the recomputed content_hash.
type ExportOutput struct {
	Archive     []byte
	ContentHash string
}

// Repository is the storage port over skill_packages (§3.1). It mounts 1:1 on
// knowledge_asset_versions; the PK is the asset_version_id. The pgx
// implementation lives in internal/infra/postgres (sub-task D).
type Repository interface {
	// Get returns the skill package mounted on an asset version.
	Get(ctx context.Context, assetVersionID uuid.UUID) (domain.SkillPackage, error)
	// Save upserts a skill package (1:1 on asset_version_id). The import path
	// calls it once with the full package (manifest + reports + status).
	Save(ctx context.Context, pkg domain.SkillPackage) error
	// UpdateValidationReport updates validation_status + validation_report +
	// scanner_version for a stored package (the revalidate path).
	UpdateValidationReport(ctx context.Context, assetVersionID uuid.UUID, status domain.SkillValidationStatus, report domain.ValidationReport, scannerVersion string) error
}

// ProposalSink is the candidate-submission port (§6.3 skill_propose). It
// creates a CANDIDATE knowledge_asset + version (governance_status='candidate',
// build_status='ready') + a pending review_request, in one tx, WITHOUT
// publishing or binding the skill. The proposal enters the review/candidate
// flow; a human approves it via the governance REST surface (§6.1), not here.
//
// The sink owns the transaction (asset + version + review_request in one
// commit). It does NOT run the static validator or store a skill_packages row
// — validation is the management-side import path (§6.1), gated on `assign`.
// The agent's proposal is the human-reviewable candidate; a manager promotes
// it (import + validate + publish) when they accept it. No script execution
// occurs on the proposal path (§4.4 — the draft bytes are stored verbatim,
// never materialized with an exec bit).
type ProposalSink interface {
	// Submit creates the candidate asset + version + pending review_request and
	// returns the references the caller surfaces as the proposal tracking id.
	// workspaceID scopes the asset (AC-4). submittedBy attributes the
	// proposal to the acting agent. draftArchive is the minimal tar.gz the
	// caller built from the draft SKILL.md content (stored verbatim in MinIO).
	Submit(ctx context.Context, in ProposalInput) (ProposalResult, error)
}

// ProposalInput is the agent-submitted skill candidate (§6.3 skill_propose).
// Name + DraftBody are required; Description / Version / SourceRef are
// optional metadata the human reviewer reads.
type ProposalInput struct {
	WorkspaceID  uuid.UUID
	Name         string
	Description  string
	Version      string
	DraftArchive []byte
	SourceRef    map[string]any
	SubmittedBy  domain.EventActor
}

// ProposalResult is the candidate reference returned to the agent: the asset +
// version ids + the review_request id (the human-review tracking handle) and
// the storage_key of the stored draft archive. Nothing here is published — the
// candidate awaits governance review.
type ProposalResult struct {
	AssetID          uuid.UUID
	AssetVersionID   uuid.UUID
	ReviewRequestID  uuid.UUID
	StorageKey       string
}
