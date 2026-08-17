package postgres

// memory_dedup.go implements evidence.DedupSuggestionRepo over the
// 018_phase4_agent_memory.memory_dedup_suggestions table (design-docs/18 §2.6,
// decision D7). Suggestions never auto-merge — only a reviewer disposition
// (accepted/rejected) mutates a suggestion row, and the caller is responsible
// for writing memory_units.superseded_by / knowledge_relations(contradicts).

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// DedupSuggestionRepo adapts memory_dedup_suggestions.
type DedupSuggestionRepo struct{ db *DB }

// NewDedupSuggestionRepo builds a DedupSuggestionRepo.
func NewDedupSuggestionRepo(db *DB) evidence.DedupSuggestionRepo {
	return &DedupSuggestionRepo{db: db}
}

// Insert persists a dedup/conflict suggestion in the pending state (D7).
// The CHECK (unit_a_id <> unit_b_id) is enforced by the DB; a self-relation
// surfaces as a constraint violation to the caller.
func (r *DedupSuggestionRepo) Insert(ctx context.Context, s domain.MemoryDedupSuggestion) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO memory_dedup_suggestions
		  (workspace_id, unit_a_id, unit_b_id, suggestion_type, origin,
		   confidence, evidence_ref, state)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		s.WorkspaceID, s.UnitAID, s.UnitBID,
		string(s.SuggestionType), string(s.Origin),
		floatPtr(s.Confidence), jsonBytes(s.EvidenceRef), string(s.State),
	).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Get loads a dedup suggestion by id.
func (r *DedupSuggestionRepo) Get(ctx context.Context, id uuid.UUID) (domain.MemoryDedupSuggestion, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, unit_a_id, unit_b_id, suggestion_type, origin,
		       confidence, evidence_ref, state, resolved_by_type, resolved_by_id,
		       resolved_at, created_at
		FROM memory_dedup_suggestions WHERE id = $1`, id)
	s, err := scanSuggestion(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.MemoryDedupSuggestion{}, domain.ErrEvidenceNotFound
		}
		return domain.MemoryDedupSuggestion{}, err
	}
	return s, nil
}

// ListPending returns pending suggestions in a workspace (reviewer inbox, §6.3).
func (r *DedupSuggestionRepo) ListPending(ctx context.Context, workspaceID uuid.UUID) ([]domain.MemoryDedupSuggestion, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, unit_a_id, unit_b_id, suggestion_type, origin,
		       confidence, evidence_ref, state, resolved_by_type, resolved_by_id,
		       resolved_at, created_at
		FROM memory_dedup_suggestions
		WHERE workspace_id = $1 AND state = 'pending'
		ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.MemoryDedupSuggestion
	for rows.Next() {
		s, err := scanSuggestion(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Resolve records a reviewer disposition (accepted/rejected). The caller writes
// the downstream effect (superseded_by / knowledge_relations); this repo only
// stamps the suggestion row.
func (r *DedupSuggestionRepo) Resolve(ctx context.Context, id uuid.UUID, state domain.DedupSuggestionState, resolvedByType domain.OwnerType, resolvedByID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE memory_dedup_suggestions
		SET state = $2, resolved_by_type = $3, resolved_by_id = $4, resolved_at = now()
		WHERE id = $1 AND state = 'pending'`,
		id, string(state), string(resolvedByType), resolvedByID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrEvidenceNotFound
	}
	return nil
}

func scanSuggestion(scan scanFunc) (domain.MemoryDedupSuggestion, error) {
	var s domain.MemoryDedupSuggestion
	var (
		stype, origin, state string
		confidence           *float64
		eref                 []byte
		resolvedByType       *string
		resolvedByID         *uuid.UUID
	)
	err := scan(
		&s.ID, &s.WorkspaceID, &s.UnitAID, &s.UnitBID, &stype, &origin,
		&confidence, &eref, &state, &resolvedByType, &resolvedByID,
		&s.ResolvedAt, &s.CreatedAt,
	)
	if err != nil {
		return domain.MemoryDedupSuggestion{}, err
	}
	s.SuggestionType = domain.DedupSuggestionType(stype)
	s.Origin = domain.DedupSuggestionOrigin(origin)
	s.State = domain.DedupSuggestionState(state)
	s.Confidence = confidence
	s.EvidenceRef = jsonMap(eref)
	if resolvedByType != nil {
		ot := domain.OwnerType(*resolvedByType)
		s.ResolvedByType = &ot
	}
	s.ResolvedByID = resolvedByID
	return s, nil
}

var _ evidence.DedupSuggestionRepo = (*DedupSuggestionRepo)(nil)
