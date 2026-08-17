package postgres

// memory_evidence_sink.go implements evidence.EvidenceSink — the
// transactional boundary for capture (design-docs/18 §5.3, §6.3).
//
// CreateEvidence owns the Begin/Commit so the memory_evidence row + the
// outbox event land in one tx (§6.3). For the object-store path (!inline) the
// large payload is written to MinIO BEFORE the tx commits, using a
// pre-generated evidence id so the object key (mora-evidence/<ws>/<id>) is
// stable; if the tx rolls back the object is an orphan handled by the purge
// reaper's orphan sweep (same compromise as the parser's attachment path — PG
// cannot be transactional with MinIO, and a sweep reconciles).
//
// All SQL is parameterized — no string-concatenated user input (07-security
// §10). The redactedBytes are the post-gate content; the raw snippet never
// reaches this sink.

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/objstore"
	"github.com/lynn901/mora/internal/module/memory/evidence"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// MemoryEvidenceSink is the postgres implementation of evidence.EvidenceSink.
type MemoryEvidenceSink struct {
	db      *DB
	outbox  *outbox.Store
	objects evidence.ObjectStore // nil = object-store path disabled (Put fails closed)
}

// NewMemoryEvidenceSink builds the capture sink over a pool, the outbox Store,
// and the evidence object store (nil acceptable when only inline captures
// are exercised; a nil store fails closed on a large-fragment Put).
func NewMemoryEvidenceSink(db *DB, store *outbox.Store, objects evidence.ObjectStore) *MemoryEvidenceSink {
	return &MemoryEvidenceSink{db: db, outbox: store, objects: objects}
}

// Compile-time check: MemoryEvidenceSink satisfies evidence.EvidenceSink.
var _ evidence.EvidenceSink = (*MemoryEvidenceSink)(nil)

// CreateEvidence persists the evidence row, writes the large object (when
// !inline), and records the `evidence.captured` outbox event — all in one tx.
func (s *MemoryEvidenceSink) CreateEvidence(ctx context.Context, e *domain.MemoryEvidence, redactedBytes []byte, inline bool, ev domain.KnowledgeEvent) (uuid.UUID, error) {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.State == "" {
		e.State = domain.EvidenceActive
	}

	// Object-store path: Put the large payload FIRST under the pre-generated
	// id, then persist storage_key. A Put failure aborts the capture entirely
	// (no orphan row). A later tx rollback leaves an orphan OBJECT (not row);
	// the purge reaper's orphan sweep reconciles.
	if !inline {
		if s.objects == nil {
			return uuid.Nil, objstore.ErrEvidenceStoreNotConfigured
		}
		key, err := s.objects.Put(ctx, e.WorkspaceID, e.ID, redactedBytes)
		if err != nil {
			return uuid.Nil, err
		}
		e.StorageKey = key
	}

	tx, err := s.db.Pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck — documented pgx pattern

	if _, err := tx.Exec(ctx, `
		INSERT INTO memory_evidence
		  (id, workspace_id, owner_type, owner_id, source_kind, source_ref,
		   source_asset_id, source_asset_version_id, visibility, captured_authz_revision,
		   content_hash, encrypted_content, storage_key, key_version,
		   redacted_excerpt, classification, retention_policy_id, state, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		e.ID, e.WorkspaceID, string(e.OwnerType), e.OwnerID, string(e.SourceKind), e.SourceRef,
		uuidPtr(e.SourceAssetID), uuidPtr(e.SourceAssetVersionID),
		string(e.Visibility), e.CapturedAuthzRevision,
		e.ContentHash, byteSlicePtr(e.EncryptedContent), strPtr(e.StorageKey), intPtr(e.KeyVersion),
		e.RedactedExcerpt, classStrPtr(e.Classification),
		uuidPtr(e.RetentionPolicyID), string(e.State), timePtr(e.ExpiresAt),
	); err != nil {
		return uuid.Nil, err
	}

	// Outbox event — same tx. destinations = [memory_events] (D5).
	if err := s.outbox.Record(ctx, tx, ev, []string{domain.MemoryEventsStream}); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return e.ID, nil
}

// guard against an accidental unused import if a future refactor drops the
// evidence store import; the var is compile-time only.
var _ = errors.Is
var _ = pgx.ErrNoRows