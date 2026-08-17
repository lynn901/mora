// memory_extract_loader.go implements distill.EvidenceLoader over the 018
// memory_evidence table (design-docs/18 §5.3). It loads an Evidence row,
// decrypts the inline fragment (via the KEK + Crypto ports) or reads the
// large object (via the ObjectStore), and returns the REDACTED excerpt + the
// source-kind metadata the ExtractionProvider needs. It never returns the raw
// ciphertext and never touches DB/storage handles the Provider could misuse
// (§9.1: Provider receives only the redacted snapshot + schema).
//
// A missing/purged Evidence returns ErrEvidenceNotFound so the distill service
// surfaces it as ErrEvidenceMissing (§9.2 — the extract Job goes dead).
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/objstore"
	"github.com/lynn901/mora/internal/module/memory/distill"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// MemoryExtractLoader is the distill.EvidenceLoader adapter. It composes the
// EvidenceRepo (raw row read) with the KEK + Crypto + ObjectStore ports to
// recover the redacted plaintext for the Provider.
type MemoryExtractLoader struct {
	db      *DB
	kek     evidence.KEK
	crypto  evidence.Crypto
	objects evidence.ObjectStore
	// (see distill.MemoryAssetResolver at the service port) the knowledge_assets(asset_type='memory') id
	// for a workspace. The loader calls it so the ExtractService's writeCandidate
	// has the asset to attach the candidate unit to. Phase 4 first version: a
	// simple per-workspace lookup (the asset is created on first capture).
	assets MemoryAssetResolver
}

// (see distill.MemoryAssetResolver at the service port) the memory asset id for a workspace.
type MemoryAssetResolver interface {
	GetOrCreateMemoryAsset(ctx context.Context, workspaceID uuid.UUID, ownerType domain.OwnerType, ownerID uuid.UUID) (uuid.UUID, error)
}

// NewMemoryExtractLoader wires the distill loader. kek/crypto/objects may be
// nil only in dev (the inline decrypt path fails closed on first use).
func NewMemoryExtractLoader(db *DB, kek evidence.KEK, crypto evidence.Crypto, objects evidence.ObjectStore, assets MemoryAssetResolver) *MemoryExtractLoader {
	return &MemoryExtractLoader{db: db, kek: kek, crypto: crypto, objects: objects, assets: assets}
}

// Compile-time check: MemoryExtractLoader satisfies distill.EvidenceLoader.
var _ distill.EvidenceLoader = (*MemoryExtractLoader)(nil)

// LoadForExtract loads an Evidence row and recovers its redacted plaintext.
// Inline rows (encrypted_content) are AEAD-decrypted; object rows are read
// from the ObjectStore. A purged row (content erased, §9.2) returns the
// redacted_excerpt as the snapshot — the Provider extracts from the excerpt
// only, which is the leak-safe fallback (§4.3).
func (l *MemoryExtractLoader) LoadForExtract(ctx context.Context, evidenceID uuid.UUID) (distill.LoadedEvidence, error) {
	e, err := l.loadRow(ctx, evidenceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return distill.LoadedEvidence{}, domain.ErrEvidenceNotFound
		}
		return distill.LoadedEvidence{}, err
	}

	var redacted string
	switch {
	case e.State == domain.EvidencePurged:
		// Content erased (§9.2); fall back to the redacted excerpt (§4.3).
		redacted = e.RedactedExcerpt
	case len(e.EncryptedContent) > 0:
		plain, err := l.decryptInline(ctx, e.EncryptedContent, e.KeyVersion)
		if err != nil {
			// Fail closed: a decrypt failure surfaces as missing — the Provider
			// does not get partial content (§9.1).
			return distill.LoadedEvidence{}, fmt.Errorf("memory: evidence decrypt: %w", err)
		}
		redacted = string(plain)
	case e.StorageKey != "":
		if l.objects == nil {
			return distill.LoadedEvidence{}, objstore.ErrEvidenceStoreNotConfigured
		}
		obj, err := l.objects.Read(ctx, e.StorageKey)
		if err != nil {
			return distill.LoadedEvidence{}, fmt.Errorf("memory: evidence object read: %w", err)
		}
		redacted = string(obj)
	default:
		// No content + not purged: use the excerpt.
		redacted = e.RedactedExcerpt
	}

	// Resolve the memory asset for this workspace so the candidate unit can
	// attach to it. A resolver failure is transient (asset row insert race).
	var memoryAssetID uuid.UUID
	if l.assets != nil {
		memoryAssetID, err = l.assets.GetOrCreateMemoryAsset(ctx, e.WorkspaceID, e.OwnerType, e.OwnerID)
		if err != nil {
			return distill.LoadedEvidence{}, fmt.Errorf("memory: resolve memory asset: %w", err)
		}
	}

	return distill.LoadedEvidence{
		ID:              e.ID,
		WorkspaceID:     e.WorkspaceID,
		OwnerType:        e.OwnerType,
		OwnerID:          e.OwnerID,
		SourceKind:       e.SourceKind,
		SourceRef:         e.SourceRef,
		SourceAssetID:    e.SourceAssetID,
		MemoryAssetID:    memoryAssetID,
		RedactedExcerpt: redacted,
	}, nil
}

// loadRow reads the raw evidence row (reuses the MemoryEvidenceRepo scan so the
// column set + leak-safe not-found mapping stay consistent).
func (l *MemoryExtractLoader) loadRow(ctx context.Context, id uuid.UUID) (domain.MemoryEvidence, error) {
	row := l.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, owner_type, owner_id, source_kind, source_ref,
		       source_asset_id, source_asset_version_id, visibility, captured_authz_revision,
		       content_hash, encrypted_content, storage_key, key_version,
		       redacted_excerpt, classification, retention_policy_id, state,
		       created_at, expires_at, purged_at, deleted_at
		FROM memory_evidence
		WHERE id = $1 AND deleted_at IS NULL`, id)
	return scanEvidence(row.Scan)
}

// decryptInline reverses the §4.2 length-prefixed envelope emitted by the
// capture service:
//
//	[1: nonceLen][nonceLen: content nonce]
//	[2: wrappedLen][wrappedLen: wrapped DEK]
//	[rest: content ciphertext]
//
// The length prefixes make the DEK envelope + content ciphertext boundaries
// unambiguous (both are variable-length GCM blobs). A malformed pack fails
// closed — the Provider never receives partial content (§9.1).
func (l *MemoryExtractLoader) decryptInline(ctx context.Context, packed []byte, keyVersion *int) ([]byte, error) {
	if l.kek == nil || l.crypto == nil {
		return nil, evidence.ErrCryptoNotConfigured
	}
	if keyVersion == nil {
		return nil, errors.New("memory: inline evidence missing key_version")
	}
	if len(packed) < 1 {
		return nil, errors.New("memory: inline evidence too short (nonce len)")
	}
	off := 0
	nonceLen := int(packed[off])
	off++
	if len(packed) < off+nonceLen {
		return nil, errors.New("memory: inline evidence truncated at nonce")
	}
	nonce := packed[off : off+nonceLen]
	off += nonceLen
	if len(packed) < off+2 {
		return nil, errors.New("memory: inline evidence truncated at wrapped-len")
	}
	wrappedLen := int(packed[off])<<8 | int(packed[off+1])
	off += 2
	if len(packed) < off+wrappedLen {
		return nil, errors.New("memory: inline evidence truncated at wrapped DEK")
	}
	wrapped := packed[off : off+wrappedLen]
	off += wrappedLen
	ciphertext := packed[off:]
	if len(ciphertext) == 0 {
		return nil, errors.New("memory: inline evidence missing content ciphertext")
	}

	dek, err := l.kek.Unwrap(ctx, wrapped, *keyVersion)
	if err != nil {
		return nil, fmt.Errorf("memory: unwrap DEK: %w", err)
	}
	plain, err := l.crypto.Decrypt(ctx, dek, ciphertext, nonce)
	if err != nil {
		return nil, fmt.Errorf("memory: AEAD decrypt: %w", err)
	}
	// Zero the DEK (best-effort).
	for i := range dek {
		dek[i] = 0
	}
	return plain, nil
}
