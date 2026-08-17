package postgres

// memory_link.go implements evidence.EvidenceLinkRepo over the
// 018_phase4_agent_memory.memory_evidence_links table (design-docs/18 §2.3).
// quote_locator is jsonb holding a non-executable reference (offset/range/hash)
// that never carries the original text (12 §4.4).

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// MemoryEvidenceLinkRepo adapts memory_evidence_links.
type MemoryEvidenceLinkRepo struct{ db *DB }

// NewMemoryEvidenceLinkRepo builds an EvidenceLinkRepo.
func NewMemoryEvidenceLinkRepo(db *DB) evidence.EvidenceLinkRepo {
	return &MemoryEvidenceLinkRepo{db: db}
}

// Insert creates a memory_unit ↔ evidence link. A duplicate (unit,evidence)
// pair violates the PK and surfaces as a unique-violation error to the caller.
func (r *MemoryEvidenceLinkRepo) Insert(ctx context.Context, l domain.MemoryEvidenceLink) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO memory_evidence_links (memory_unit_id, evidence_id, quote_locator, support_type)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (memory_unit_id, evidence_id) DO NOTHING`,
		l.MemoryUnitID, l.EvidenceID, jsonBytes(l.QuoteLocator), string(l.SupportType))
	return err
}

// ListForUnit returns all evidence links for a memory unit.
func (r *MemoryEvidenceLinkRepo) ListForUnit(ctx context.Context, memoryUnitID uuid.UUID) ([]domain.MemoryEvidenceLink, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT memory_unit_id, evidence_id, quote_locator, support_type, created_at
		FROM memory_evidence_links WHERE memory_unit_id = $1
		ORDER BY created_at`, memoryUnitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectLinks(rows)
}

// ListForEvidence returns all units linked to an evidence (for ACL expansion
// and deletion propagation — finding which units to mark evidence_missing).
func (r *MemoryEvidenceLinkRepo) ListForEvidence(ctx context.Context, evidenceID uuid.UUID) ([]domain.MemoryEvidenceLink, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT memory_unit_id, evidence_id, quote_locator, support_type, created_at
		FROM memory_evidence_links WHERE evidence_id = $1
		ORDER BY created_at`, evidenceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectLinks(rows)
}

func collectLinks(rows pgx.Rows) ([]domain.MemoryEvidenceLink, error) {
	var out []domain.MemoryEvidenceLink
	for rows.Next() {
		var l domain.MemoryEvidenceLink
		var quote []byte
		var support string
		if err := rows.Scan(&l.MemoryUnitID, &l.EvidenceID, &quote, &support, &l.CreatedAt); err != nil {
			return nil, err
		}
		l.QuoteLocator = jsonMap(quote)
		l.SupportType = domain.SupportType(support)
		out = append(out, l)
	}
	return out, rows.Err()
}

var _ evidence.EvidenceLinkRepo = (*MemoryEvidenceLinkRepo)(nil)
