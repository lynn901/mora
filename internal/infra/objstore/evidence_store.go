package objstore

// evidence_store.go adapts the S3-compatible objstore.Store to the Phase 4
// evidence.ObjectStore port (design-docs/18 §4.2). Large evidence payloads
// (>64KiB redacted) are written under the mora-evidence/<workspace>/<id>
// prefix in the same MinIO bucket the parser already uses; the DB row stores
// only the storage_key + content_hash + redacted_excerpt (D4).
//
// The prefix keeps evidence bytes in a separate namespace from document
// uploads (mora-evidence/ vs the document upload keys) so a per-namespace
// retention/purge sweep is cheap and doesn't touch document objects.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// ErrEvidenceStoreNotConfigured is returned when the evidence object store is
// not wired (nil Store). It is distinct from a leak-safe "not found" so callers
// can decide: a read of a large object with no store wired should surface as a
// denial, not as an existence leak.
var ErrEvidenceStoreNotConfigured = errors.New("evidence object store: not configured")

// EvidenceObjectStore adapts *Store to evidence.ObjectStore.
type EvidenceObjectStore struct{ Store *Store }

// NewEvidenceObjectStore wraps an objstore.Store for evidence large objects.
func NewEvidenceObjectStore(s *Store) *EvidenceObjectStore { return &EvidenceObjectStore{Store: s} }

// Put writes a large evidence payload and returns the canonical storage_key
// (mora-evidence/<workspace>/<evidence_id>). Callers persist this key on the
// memory_evidence row.
func (s *EvidenceObjectStore) Put(ctx context.Context, workspaceID, evidenceID uuid.UUID, data []byte) (string, error) {
	if s.Store == nil {
		return "", ErrEvidenceStoreNotConfigured
	}
	key := fmt.Sprintf("mora-evidence/%s/%s", workspaceID, evidenceID)
	return s.Store.Put(ctx, key, "application/octet-stream", data)
}

// Read fetches the bytes at a storage_key (the read path for authorized
// evidence expansion, gated by the §4.3 ACL chain above this adapter).
func (s *EvidenceObjectStore) Read(ctx context.Context, storageKey string) ([]byte, error) {
	if s.Store == nil {
		return nil, ErrEvidenceStoreNotConfigured
	}
	return s.Store.Read(ctx, storageKey)
}

// Delete removes the object (the purge path, D3 §9.2). Best-effort: a missing
// object is not an error so the purge reaper is idempotent.
func (s *EvidenceObjectStore) Delete(ctx context.Context, storageKey string) error {
	if s.Store == nil {
		return nil
	}
	return s.Store.Delete(ctx, storageKey)
}

// Compile-time check: EvidenceObjectStore satisfies evidence.ObjectStore.
var _ evidence.ObjectStore = (*EvidenceObjectStore)(nil)
