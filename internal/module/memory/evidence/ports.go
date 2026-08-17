// Package evidence defines the storage and ACL ports for memory evidence
// (design-docs/18 §4 Evidence 存储、脱敏与 ACL，决策 D2/D4).
//
// This package is the Phase 4 基座 (base): it owns the Evidence/Unit/link/
// retention-policy/feedback/dedup repository ports, the redaction gate, the
// envelope-encryption KEK port, and the large-object store port. The distill/
// dedup/recall services (sub-issues B/C/D) consume these ports; the REST/MCP
// handlers (sub-issue E) call the services. Implementations live in
// infra/postgres (memory_*.go) and infra/objstore.
package evidence

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
)

// EvidenceRepo is the persistence port over memory_evidence (§2.1).
// Writes honor the storage split (D4): small fragments carry ciphertext, large
// objects carry a MinIO key. Reads are leak-safe — a missing/unreadable row
// returns ErrEvidenceNotFound so existence is never leaked (§9.3).
type EvidenceRepo interface {
	// Insert persists a new evidence row. The caller has already run the
	// redaction gate (§4.1) and chosen inline vs. object storage (§4.2); the
	// repo stores whichever of EncryptedContent/StorageKey is set.
	Insert(ctx context.Context, e domain.MemoryEvidence) (uuid.UUID, error)
	// Get loads an evidence row by id. A deleted/missing row returns
	// ErrEvidenceNotFound (existence never leaks, §9.3).
	Get(ctx context.Context, id uuid.UUID) (domain.MemoryEvidence, error)
	// ListByOwner returns non-deleted evidence owned by (workspace, owner).
	ListByOwner(ctx context.Context, workspaceID uuid.UUID, ownerType domain.OwnerType, ownerID uuid.UUID) ([]domain.MemoryEvidence, error)
	// ListBySourceAsset returns non-deleted evidence referencing an asset
	// (for deletion propagation: source_asset delete → mark evidence_missing).
	ListBySourceAsset(ctx context.Context, sourceAssetID uuid.UUID) ([]domain.MemoryEvidence, error)
	// MarkPendingPurge flips an active evidence to pending_purge (D3 expiry).
	MarkPendingPurge(ctx context.Context, id uuid.UUID) error
	// Purge erases encrypted_content + storage_key, sets state=purged +
	// purged_at, retaining id/content_hash/redacted_excerpt/audit (12 §8.4).
	Purge(ctx context.Context, id uuid.UUID) error
	// ClearContent is the field-level erase used by Purge + the purge-after
	// reaper. It nulls encrypted_content + storage_key + key_version without
	// touching the hash/excerpt (the audit-only residue).
	ClearContent(ctx context.Context, id uuid.UUID) error
}

// MemoryUnitRepo is the persistence port over memory_units (§2.2).
type MemoryUnitRepo interface {
	Insert(ctx context.Context, u domain.MemoryUnit) (uuid.UUID, error)
	Get(ctx context.Context, id uuid.UUID) (domain.MemoryUnit, error)
	ListByAsset(ctx context.Context, assetID uuid.UUID) ([]domain.MemoryUnit, error)
	// ListCandidates returns candidate-state units in a workspace (the
	// Candidate Inbox view, §6.3). Private candidates are filtered to the owner
	// by the service layer above; this repo only carries the state filter.
	ListCandidates(ctx context.Context, workspaceID uuid.UUID) ([]domain.MemoryUnit, error)
	// SetState transitions a unit's state (§6.2). Published requires a
	// review_decision by the caller — the repo does not enforce review here.
	SetState(ctx context.Context, id uuid.UUID, state domain.MemoryUnitState) error
	// SetSupersededBy records a reviewer-confirmed merge/supersede (D7).
	SetSupersededBy(ctx context.Context, id, supersededBy uuid.UUID) error
	// MarkEvidenceMissing flags a unit whose backing evidence is gone (D3
	// propagation): the unit exits high-authority recall but stays readable.
	MarkEvidenceMissing(ctx context.Context, id uuid.UUID) error
}

// EvidenceLinkRepo is the persistence port over memory_evidence_links (§2.3).
type EvidenceLinkRepo interface {
	Insert(ctx context.Context, l domain.MemoryEvidenceLink) error
	ListForUnit(ctx context.Context, memoryUnitID uuid.UUID) ([]domain.MemoryEvidenceLink, error)
	ListForEvidence(ctx context.Context, evidenceID uuid.UUID) ([]domain.MemoryEvidenceLink, error)
	// CountAvailableEvidence counts a unit's links whose backing evidence is
	// still readable (state != 'purged' AND deleted_at IS NULL). The deletion
	// propagation path (§9.2) uses this to decide whether to mark a unit
	// evidence_missing: when the purged evidence was a unit's last independent
	// support, the count drops to 0 and the unit exits high-authority recall.
	// excludeEvidenceID lets the caller ask "how much is left if THIS one goes"
	// in one query without a read-then-write race on the row being purged.
	CountAvailableEvidence(ctx context.Context, memoryUnitID, excludeEvidenceID uuid.UUID) (int, error)
}

// RetentionPolicyRepo is the persistence port over memory_retention_policies
// (§2.4). Specific duration values are a PM governance decision (§19.6).
type RetentionPolicyRepo interface {
	Insert(ctx context.Context, p domain.RetentionPolicy) (uuid.UUID, error)
	Get(ctx context.Context, id uuid.UUID) (domain.RetentionPolicy, error)
	// GetForType resolves the effective policy for a (workspace, memory_type):
	// the type-specific row, else the workspace default (memory_type IS NULL).
	GetForType(ctx context.Context, workspaceID uuid.UUID, memoryType domain.MemoryType) (domain.RetentionPolicy, error)
	ListForWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]domain.RetentionPolicy, error)
	// PurgeDue returns active evidence whose expires_at has passed, for the
	// retention reaper (D3 → pending_purge). Belongs here because expiry is
	// derived from policy + evidence.expires_at.
	PurgeDue(ctx context.Context, now time.Time, limit int) ([]domain.MemoryEvidence, error)
	// PurgeReady returns pending_purge evidence whose purge_after grace window
	// has elapsed (pending_purged_at + policy.purge_after ≤ now), for the
	// reaper's second half (D3 → purged). Evidence without a retention_policy
	// (policy_id NULL) is still returned when pending_purged_at + a default
	// grace has passed, so an explicitly-deleted evidence with no policy row
	// is not stranded in pending_purge forever. Limited per tick.
	PurgeReady(ctx context.Context, now time.Time, defaultGrace time.Duration, limit int) ([]domain.MemoryEvidence, error)
}

// FeedbackRepo is the persistence port over memory_feedback (§2.5, D8).
type FeedbackRepo interface {
	Insert(ctx context.Context, f domain.MemoryFeedback) (uuid.UUID, error)
	ListForUnit(ctx context.Context, memoryUnitID uuid.UUID) ([]domain.MemoryFeedback, error)
}

// DedupSuggestionRepo is the persistence port over memory_dedup_suggestions
// (§2.6, D7). Suggestions never auto-merge — only a reviewer disposition mutates.
type DedupSuggestionRepo interface {
	Insert(ctx context.Context, s domain.MemoryDedupSuggestion) (uuid.UUID, error)
	Get(ctx context.Context, id uuid.UUID) (domain.MemoryDedupSuggestion, error)
	ListPending(ctx context.Context, workspaceID uuid.UUID) ([]domain.MemoryDedupSuggestion, error)
	// Resolve records a reviewer disposition (accepted/rejected). Writing
	// memory_units.superseded_by / knowledge_relations is the caller's job.
	Resolve(ctx context.Context, id uuid.UUID, state domain.DedupSuggestionState, resolvedByType domain.OwnerType, resolvedByID uuid.UUID) error
}

// KEK is the envelope key-encryption-key port (D4). It wraps a per-evidence
// data-encryption key (DEK): Wrap returns the wrapped DEK + the KEK version
// used; Unwrap recovers the DEK for a given version. The KEK plaintext never
// leaves the implementation (same credential_ref-only contract as
// knowledge_sources.credential_ref, §13.2 — no in-code/in-log secrets, 07 §10).
// Key rotation: a new version does NOT rewrite existing ciphertext; reads unwrap
// by the version the ciphertext carries, writes always use the current version.
type KEK interface {
	// Wrap encrypts a DEK under the current KEK version, returning the wrapped
	// bytes + the version to persist alongside the ciphertext.
	Wrap(ctx context.Context, dek []byte) (wrapped []byte, version int, err error)
	// Unwrap recovers the DEK wrapped under the given version.
	Unwrap(ctx context.Context, wrapped []byte, version int) ([]byte, error)
	// CurrentVersion returns the active KEK version (for new writes).
	CurrentVersion(ctx context.Context) (int, error)
}

// ObjectStore is the large-object port for evidence payloads > 64KiB (D4).
// It mirrors the mora parser's objstore.Store contract (Put/Read/Delete) but is
// scoped to the mora-evidence/<workspace>/<evidence_id> prefix.
type ObjectStore interface {
	Put(ctx context.Context, workspaceID, evidenceID uuid.UUID, data []byte) (storageKey string, err error)
	Read(ctx context.Context, storageKey string) ([]byte, error)
	Delete(ctx context.Context, storageKey string) error
}

// Crypto is the AEAD port over a DEK (AES-256-GCM, D4). The KEK wraps the DEK;
// this port does the content-level encrypt/decrypt. Splitting KEK (envelope)
// from Crypto (AEAD) lets key rotation stay independent of cipher choice.
type Crypto interface {
	// Encrypt produces the ciphertext + nonce under the given DEK.
	Encrypt(ctx context.Context, dek, plaintext []byte) (ciphertext []byte, nonce []byte, err error)
	// Decrypt reverses Encrypt.
	Decrypt(ctx context.Context, dek, ciphertext, nonce []byte) ([]byte, error)
}
