package postgres

// memory_retention.go implements evidence.RetentionPolicyRepo over the
// 018_phase4_agent_memory.memory_retention_policies table (design-docs/18
// §2.4, decision D3). It also carries the PurgeDue query (active evidence
// whose expires_at has passed) because expiry is derived from policy +
// evidence.expires_at and belongs with the retention domain.
//
// Specific duration values are a PM governance decision (§19.6); the migration
// seeds a workspace-wide system default (365 days retain + 30 days purge_after)
// which this repo reads/writes but does not second-guess.

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// RetentionPolicyRepo adapts memory_retention_policies.
type RetentionPolicyRepo struct{ db *DB }

// NewRetentionPolicyRepo builds a RetentionPolicyRepo.
func NewRetentionPolicyRepo(db *DB) evidence.RetentionPolicyRepo {
	return &RetentionPolicyRepo{db: db}
}

// Insert persists a retention policy. The (workspace_id, memory_type) UNIQUE
// constraint means a type-specific row can replace the workspace default if
// memory_type matches — callers insert one row per (workspace, type-or-null).
func (r *RetentionPolicyRepo) Insert(ctx context.Context, p domain.RetentionPolicy) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO memory_retention_policies
		  (workspace_id, memory_type, retain_for, purge_after, is_system)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id`,
		p.WorkspaceID, memTypeStrPtr(p.MemoryType),
		durationToInterval(p.RetainFor), intervalPtr(p.PurgeAfter), p.IsSystem,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// Get loads a retention policy by id.
func (r *RetentionPolicyRepo) Get(ctx context.Context, id uuid.UUID) (domain.RetentionPolicy, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, memory_type, retain_for, purge_after, is_system,
		       created_at, updated_at
		FROM memory_retention_policies WHERE id = $1`, id)
	p, err := scanPolicy(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.RetentionPolicy{}, domain.ErrEvidenceNotFound
		}
		return domain.RetentionPolicy{}, err
	}
	return p, nil
}

// GetForType resolves the effective policy for a (workspace, memory_type):
// the type-specific row, else the workspace default (memory_type IS NULL).
// This is the read path for evidence.expires_at computation.
func (r *RetentionPolicyRepo) GetForType(ctx context.Context, workspaceID uuid.UUID, memoryType domain.MemoryType) (domain.RetentionPolicy, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, memory_type, retain_for, purge_after, is_system,
		       created_at, updated_at
		FROM memory_retention_policies
		WHERE workspace_id = $1 AND (memory_type = $2 OR memory_type IS NULL)
		ORDER BY memory_type NULLS LAST
		LIMIT 1`, workspaceID, string(memoryType))
	p, err := scanPolicy(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.RetentionPolicy{}, domain.ErrEvidenceNotFound
		}
		return domain.RetentionPolicy{}, err
	}
	return p, nil
}

// ListForWorkspace returns all policies for a workspace (admin view).
func (r *RetentionPolicyRepo) ListForWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]domain.RetentionPolicy, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, memory_type, retain_for, purge_after, is_system,
		       created_at, updated_at
		FROM memory_retention_policies WHERE workspace_id = $1
		ORDER BY is_system DESC, memory_type NULLS FIRST`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RetentionPolicy
	for rows.Next() {
		p, err := scanPolicy(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PurgeDue returns active evidence whose expires_at has passed, for the
// retention reaper (D3 → pending_purge). The reaper calls MarkPendingPurge on
// each, then Purge after the purge_after grace window. Limited to `limit` rows
// per tick so a backlog doesn't starve other workers.
func (r *RetentionPolicyRepo) PurgeDue(ctx context.Context, now time.Time, limit int) ([]domain.MemoryEvidence, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, owner_type, owner_id, source_kind, source_ref,
		       source_asset_id, source_asset_version_id, visibility, captured_authz_revision,
		       content_hash, encrypted_content, storage_key, key_version,
		       redacted_excerpt, classification, retention_policy_id, state,
		       created_at, expires_at, purged_at, deleted_at
		FROM memory_evidence
		WHERE state = 'active' AND expires_at IS NOT NULL AND expires_at <= $1
		ORDER BY expires_at
		LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectEvidence(rows)
}

func scanPolicy(scan scanFunc) (domain.RetentionPolicy, error) {
	var p domain.RetentionPolicy
	var (
		mtStr      *string
		retain      pgtype.Interval
		purgeAfter  pgtype.Interval
	)
	err := scan(
		&p.ID, &p.WorkspaceID, &mtStr, &retain, &purgeAfter, &p.IsSystem,
		&p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return domain.RetentionPolicy{}, err
	}
	if mtStr != nil {
		mt := domain.MemoryType(*mtStr)
		p.MemoryType = &mt
	}
	p.RetainFor = intervalToDuration(retain)
	if purgeAfter.Valid {
		d := intervalToDuration(purgeAfter)
		p.PurgeAfter = &d
	}
	return p, nil
}

// memTypeStrPtr returns the string for a *MemoryType, nil for the workspace default.
func memTypeStrPtr(mt *domain.MemoryType) any {
	if mt == nil {
		return nil
	}
	return string(*mt)
}

// intervalPtr converts a *time.Duration to a pgtype.Interval for writes.
func intervalPtr(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return durationToInterval(*d)
}

var _ evidence.RetentionPolicyRepo = (*RetentionPolicyRepo)(nil)
