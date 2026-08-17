package postgres

// memory_evidence.go implements evidence.EvidenceRepo over the
// 018_phase4_agent_memory.memory_evidence table (design-docs/18 §2.1, §4).
//
// Storage split (D4): the caller (evidence.Service) runs the redaction gate
// (§4.1) and decides inline vs. object storage (§4.2). This repo persists
// whichever of EncryptedContent/StorageKey is set on the row — it does NOT
// touch the DEK/KEK or MinIO; that is the evidence.Service's job (composing
// evidence.KEK + evidence.Crypto + evidence.ObjectStore). The CHECK constraint
// on memory_evidence enforces the either/or at the DB layer as a backstop.
//
// All SQL is parameterized — no string-concatenated user input (07-security §10).
// Reads are leak-safe: a deleted/missing row returns ErrEvidenceNotFound so the
// caller (EvidenceLocator) can surface it as not-found, indistinguishable from
// a permission denial (§9.3 存在性不泄露).

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// MemoryEvidenceRepo adapts memory_evidence for the evidence.Service.
type MemoryEvidenceRepo struct{ db *DB }

// NewMemoryEvidenceRepo builds an EvidenceRepo over the 018 memory_evidence table.
func NewMemoryEvidenceRepo(db *DB) evidence.EvidenceRepo { return &MemoryEvidenceRepo{db: db} }

// Insert persists a new evidence row. The row's EncryptedContent or StorageKey
// is stored as given (the caller has already run the storage split). Returns
// the generated id.
func (r *MemoryEvidenceRepo) Insert(ctx context.Context, e domain.MemoryEvidence) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO memory_evidence
		  (workspace_id, owner_type, owner_id, source_kind, source_ref,
		   source_asset_id, source_asset_version_id, visibility, captured_authz_revision,
		   content_hash, encrypted_content, storage_key, key_version,
		   redacted_excerpt, classification, retention_policy_id, state, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		RETURNING id`,
		e.WorkspaceID, string(e.OwnerType), e.OwnerID, string(e.SourceKind), e.SourceRef,
		uuidPtr(e.SourceAssetID), uuidPtr(e.SourceAssetVersionID),
		string(e.Visibility), e.CapturedAuthzRevision,
		e.ContentHash, byteSlicePtr(e.EncryptedContent), strPtr(e.StorageKey), intPtr(e.KeyVersion),
		e.RedactedExcerpt, classStrPtr(e.Classification),
		uuidPtr(e.RetentionPolicyID), string(e.State), timePtr(e.ExpiresAt),
	).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, domain.ErrEvidenceNotFound
		}
		return uuid.Nil, err
	}
	return id, nil
}

// Get loads an evidence row by id, excluding soft-deleted rows. A missing or
// deleted row returns ErrEvidenceNotFound so existence never leaks (§9.3).
func (r *MemoryEvidenceRepo) Get(ctx context.Context, id uuid.UUID) (domain.MemoryEvidence, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, owner_type, owner_id, source_kind, source_ref,
		       source_asset_id, source_asset_version_id, visibility, captured_authz_revision,
		       content_hash, encrypted_content, storage_key, key_version,
		       redacted_excerpt, classification, retention_policy_id, state,
		       created_at, expires_at, pending_purged_at, purged_at, deleted_at
		FROM memory_evidence
		WHERE id = $1 AND deleted_at IS NULL`, id)
	e, err := scanEvidence(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.MemoryEvidence{}, domain.ErrEvidenceNotFound
		}
		return domain.MemoryEvidence{}, err
	}
	return e, nil
}

// ListByOwner returns non-deleted evidence owned by (workspace, owner).
func (r *MemoryEvidenceRepo) ListByOwner(ctx context.Context, workspaceID uuid.UUID, ownerType domain.OwnerType, ownerID uuid.UUID) ([]domain.MemoryEvidence, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, owner_type, owner_id, source_kind, source_ref,
		       source_asset_id, source_asset_version_id, visibility, captured_authz_revision,
		       content_hash, encrypted_content, storage_key, key_version,
		       redacted_excerpt, classification, retention_policy_id, state,
		       created_at, expires_at, pending_purged_at, purged_at, deleted_at
		FROM memory_evidence
		WHERE workspace_id = $1 AND owner_type = $2 AND owner_id = $3 AND deleted_at IS NULL
		ORDER BY created_at DESC`, workspaceID, string(ownerType), ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEvidence(rows)
}

// ListBySourceAsset returns non-deleted evidence referencing an asset. Used by
// the deletion-propagation path (D3): source_asset delete → mark evidence_missing.
func (r *MemoryEvidenceRepo) ListBySourceAsset(ctx context.Context, sourceAssetID uuid.UUID) ([]domain.MemoryEvidence, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, owner_type, owner_id, source_kind, source_ref,
		       source_asset_id, source_asset_version_id, visibility, captured_authz_revision,
		       content_hash, encrypted_content, storage_key, key_version,
		       redacted_excerpt, classification, retention_policy_id, state,
		       created_at, expires_at, pending_purged_at, purged_at, deleted_at
		FROM memory_evidence
		WHERE source_asset_id = $1 AND deleted_at IS NULL`, sourceAssetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEvidence(rows)
}

// MarkPendingPurge flips an active evidence to pending_purge (D3 expiry) and
// stamps pending_purged_at = now() — the start of the purge_after grace window
// the reaper counts from (019 migration / §9.2). A pending_purge/purged row is a
// no-op. Returns ErrEvidenceNotFound if the row is missing/deleted so the
// reaper can't infer existence of purged rows.
func (r *MemoryEvidenceRepo) MarkPendingPurge(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE memory_evidence
		SET state = 'pending_purge', pending_purged_at = now()
		WHERE id = $1 AND state = 'active' AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Either missing/deleted or already not active — both are "nothing to
		// do"; the reaper treats this as already-purged/expired, not an error.
		return nil
	}
	return nil
}

// Purge erases encrypted_content + storage_key + key_version, sets state=purged
// + purged_at, retaining id/content_hash/redacted_excerpt/audit (12 §8.4). A
// purged row keeps its hash so the deletion proof remains verifiable.
func (r *MemoryEvidenceRepo) Purge(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE memory_evidence
		SET state = 'purged', purged_at = now(),
		    encrypted_content = NULL, storage_key = NULL, key_version = NULL
		WHERE id = $1 AND state IN ('active','pending_purge') AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrEvidenceNotFound
	}
	return nil
}

// ClearContent nulls encrypted_content + storage_key + key_version without
// changing state. Used when the content bytes are moved off PG but the row
// should stay active (e.g. a migration tool), and as the field-level helper
// behind Purge.
func (r *MemoryEvidenceRepo) ClearContent(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE memory_evidence
		SET encrypted_content = NULL, storage_key = NULL, key_version = NULL
		WHERE id = $1 AND deleted_at IS NULL`, id)
	return err
}

// scanEvidence is the shared scan for the 22-column evidence SELECT. It uses a
// scanFunc indirection so the same code serves QueryRow.Scan and rows.Scan.
type scanFunc func(dest ...any) error

func scanEvidence(scan scanFunc) (domain.MemoryEvidence, error) {
	var e domain.MemoryEvidence
	var (
		ownerType, sourceKind, visibility, state        string
		class                                           *string
		srcAsset, srcAssetVer, retentionID              *uuid.UUID
		encrypted                                       []byte
		storageKey                                      *string
		keyVersion                                      *int
		expiresAt, pendingPurgedAt, purgedAt, deletedAt *time.Time
	)
	err := scan(
		&e.ID, &e.WorkspaceID, &ownerType, &e.OwnerID, &sourceKind, &e.SourceRef,
		&srcAsset, &srcAssetVer, &visibility, &e.CapturedAuthzRevision,
		&e.ContentHash, &encrypted, &storageKey, &keyVersion,
		&e.RedactedExcerpt, &class, &retentionID, &state,
		&e.CreatedAt, &expiresAt, &pendingPurgedAt, &purgedAt, &deletedAt,
	)
	if err != nil {
		return domain.MemoryEvidence{}, err
	}
	e.OwnerType = domain.OwnerType(ownerType)
	e.SourceKind = domain.EvidenceSourceKind(sourceKind)
	e.Visibility = domain.EvidenceVisibility(visibility)
	e.State = domain.EvidenceState(state)
	e.SourceAssetID = srcAsset
	e.SourceAssetVersionID = srcAssetVer
	e.EncryptedContent = encrypted
	if storageKey != nil {
		e.StorageKey = *storageKey
	}
	e.KeyVersion = keyVersion
	if class != nil {
		e.Classification = domain.EvidenceClassification(*class)
	}
	e.RetentionPolicyID = retentionID
	e.ExpiresAt = expiresAt
	e.PendingPurgedAt = pendingPurgedAt
	e.PurgedAt = purgedAt
	e.DeletedAt = deletedAt
	return e, nil
}

// collectEvidence iterates rows, scanning each into a MemoryEvidence.
func collectEvidence(rows pgx.Rows) ([]domain.MemoryEvidence, error) {
	var out []domain.MemoryEvidence
	for rows.Next() {
		e, err := scanEvidence(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// --- memory_evidence helpers ---

// uuidPtr returns a *uuid.UUID for a *domain.UUID (nil-safe for nullable FK
// columns). domain.UUID is a uuid.UUID alias, so this is a pointer cast.
func uuidPtr(u *uuid.UUID) any {
	if u == nil {
		return nil
	}
	return *u
}

func byteSlicePtr(b []byte) any {
	if b == nil {
		return nil
	}
	return b
}

func strPtr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func intPtr(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

func timePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return *t
}

func classStrPtr(c domain.EvidenceClassification) any {
	if c == "" {
		return nil
	}
	return string(c)
}

// intervalToDuration converts a pgtype.Interval to a time.Duration. PG
// intervals of "N days" land in Microseconds (pgx v5); months-only intervals
// cannot be losslessly converted to a Duration and fall back to 0 (the
// retention-policy seed uses day-based intervals, §2.4, so this is exact).
func intervalToDuration(iv pgtype.Interval) time.Duration {
	if iv.Valid {
		return time.Duration(iv.Microseconds) * time.Microsecond
	}
	return 0
}

// durationToInterval is the inverse for writes (RetentionPolicyRepo.Insert).
func durationToInterval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: int64(d / time.Microsecond), Valid: true}
}

// jsonMap is a helper for nullable jsonb columns scanned as raw bytes then
// unmarshaled into a map[string]any (same pattern as knowledge_source.go).
func jsonMap(b []byte) map[string]any {
	if len(b) == 0 {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}
	return m
}

// jsonBytes marshals a map for an INSERT; returns nil for empty so the column
// receives the table DEFAULT '{}' rather than an explicit null.
func jsonBytes(m map[string]any) any {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}

var _ evidence.EvidenceRepo = (*MemoryEvidenceRepo)(nil)
