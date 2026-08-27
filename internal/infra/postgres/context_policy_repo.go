package postgres

// context_policy_repo.go implements ctxmod.PolicyRepo over
// context_authority_policies (migration 024, design-docs/19 §2.1 / §5.3).
//
// The repo is the ONLY writer; the Broker only reads. Upsert is versioned:
// supersede the prior is_current row (is_current=false, superseded_at=now)
// and insert a new policy_version+1 is_current row in one transaction. The
// is_current exclusion is enforced by the table's EXCLUDE constraint +
// partial unique index (migration 024), so a concurrent second writer that
// races is rejected by the DB, not by application logic.
//
// All SQL is parameterized (07-security §10). No user input is interpolated.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lynn901/mora/internal/domain"
	ctxmod "github.com/lynn901/mora/internal/module/knowledge/context"
)

// ContextPolicyRepo is the postgres implementation of ctxmod.PolicyRepo.
type ContextPolicyRepo struct{ db *DB }

// NewContextPolicyRepo builds a ctxmod.PolicyRepo over the mora database.
func NewContextPolicyRepo(db *DB) *ContextPolicyRepo {
	return &ContextPolicyRepo{db: db}
}

// Compile-time check.
var _ ctxmod.PolicyRepo = (*ContextPolicyRepo)(nil)

// LoadCurrent returns the is_current policy for (workspace, intent) (§5.3).
// A missing row yields ctxmod.ErrPolicyNotFound so the caller falls back to
// the built-in defaults.
func (r *ContextPolicyRepo) LoadCurrent(ctx context.Context, workspaceID uuid.UUID, intent ctxmod.Intent) (ctxmod.AuthorityPolicyRecord, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, intent, policy_version, is_current,
		       config, created_at, superseded_at, created_by_id
		  FROM context_authority_policies
		 WHERE workspace_id = $1 AND intent = $2 AND is_current = TRUE
		 LIMIT 1`, workspaceID, string(intent))
	rec, err := scanAuthorityPolicy(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ctxmod.AuthorityPolicyRecord{}, ctxmod.ErrPolicyNotFound
		}
		return ctxmod.AuthorityPolicyRecord{}, err
	}
	return rec, nil
}

// ListByWorkspace returns the is_current policies for all intents in a
// workspace (GET /knowledge/policies, §11.1). Empty slice when none exist.
func (r *ContextPolicyRepo) ListByWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]ctxmod.AuthorityPolicyRecord, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, intent, policy_version, is_current,
		       config, created_at, superseded_at, created_by_id
		  FROM context_authority_policies
		 WHERE workspace_id = $1 AND is_current = TRUE
		 ORDER BY intent`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ctxmod.AuthorityPolicyRecord
	for rows.Next() {
		rec, err := scanAuthorityPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// CurrentVersion returns the highest policy_version for (workspace, intent);
// 0 when no row exists. Used as a cache key (§0 D10 / §5.3).
func (r *ContextPolicyRepo) CurrentVersion(ctx context.Context, workspaceID uuid.UUID, intent ctxmod.Intent) (int, error) {
	var version int
	err := r.db.Pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(policy_version), 0)
		  FROM context_authority_policies
		 WHERE workspace_id = $1 AND intent = $2`, workspaceID, string(intent)).Scan(&version)
	if err != nil {
		return 0, err
	}
	return version, nil
}

// Upsert creates policy_version+1 and supersedes the prior current row (§5.3).
// If no prior row exists, policy_version=1. The is_current exclusion is enforced
// by the table constraint (migration 024). The whole operation is one tx so a
// failure leaves the prior current row intact.
func (r *ContextPolicyRepo) Upsert(ctx context.Context, rec ctxmod.AuthorityPolicyRecord) (ctxmod.AuthorityPolicyRecord, error) {
	tx, err := r.db.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ctxmod.AuthorityPolicyRecord{}, fmt.Errorf("context.policy: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) // safe: no-op on commit

	// Resolve the next version + the prior current row id atomically.
	var priorID *uuid.UUID
	var nextVersion int = 1
	err = tx.QueryRow(ctx, `
		SELECT id, COALESCE(MAX(policy_version), 0)
		  FROM context_authority_policies
		 WHERE workspace_id = $1 AND intent = $2 AND is_current = TRUE
		 GROUP BY id
		 LIMIT 1`, rec.WorkspaceID, string(rec.Intent)).Scan(&priorID, &nextVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ctxmod.AuthorityPolicyRecord{}, fmt.Errorf("context.policy: load prior: %w", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		nextVersion = 1
		priorID = nil
	} else {
		nextVersion = nextVersion + 1
	}

	// Supersede the prior current row (if any) in the SAME tx.
	if priorID != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE context_authority_policies
			   SET is_current = FALSE, superseded_at = now()
			 WHERE id = $1`, *priorID); err != nil {
			return ctxmod.AuthorityPolicyRecord{}, fmt.Errorf("context.policy: supersede prior: %w", err)
		}
	}

	cfgJSON, err := marshalPolicyConfig(rec.Config)
	if err != nil {
		return ctxmod.AuthorityPolicyRecord{}, fmt.Errorf("context.policy: marshal config: %w", err)
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO context_authority_policies
		       (workspace_id, intent, policy_version, is_current, config, created_by_id)
		VALUES ($1, $2, $3, TRUE, $4, $5)
		RETURNING id, created_at`,
		rec.WorkspaceID, string(rec.Intent), nextVersion, cfgJSON, rec.CreatedByID).Scan(&id, &rec.CreatedAt)
	if err != nil {
		return ctxmod.AuthorityPolicyRecord{}, fmt.Errorf("context.policy: insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return ctxmod.AuthorityPolicyRecord{}, fmt.Errorf("context.policy: commit: %w", err)
	}
	rec.ID = id
	rec.PolicyVersion = nextVersion
	rec.IsCurrent = true
	return rec, nil
}

// authorityPolicyScanner is the common scan surface for QueryRow + Rows.
type authorityPolicyScanner interface {
	Scan(dest ...any) error
}

func scanAuthorityPolicy(s authorityPolicyScanner) (ctxmod.AuthorityPolicyRecord, error) {
	var (
		rec           ctxmod.AuthorityPolicyRecord
		intentStr     string
		cfgBytes      []byte
		supersededAt  *time.Time
	)
	if err := s.Scan(
		&rec.ID, &rec.WorkspaceID, &intentStr, &rec.PolicyVersion, &rec.IsCurrent,
		&cfgBytes, &rec.CreatedAt, &supersededAt, &rec.CreatedByID,
	); err != nil {
		return ctxmod.AuthorityPolicyRecord{}, err
	}
	rec.Intent = ctxmod.Intent(intentStr)
	rec.SupersededAt = supersededAt
	cfg, err := unmarshalPolicyConfig(cfgBytes)
	if err != nil {
		return ctxmod.AuthorityPolicyRecord{}, fmt.Errorf("context.policy: unmarshal config: %w", err)
	}
	rec.Config = cfg
	return rec, nil
}

// marshalPolicyConfig serializes PolicyConfig to JSONB. Unknown keys (Raw) are
// merged so a round-trip preserves forward-compat fields.
func marshalPolicyConfig(c ctxmod.PolicyConfig) ([]byte, error) {
	base := map[string]any{
		"primary_basis":          assetTypeStrings(c.PrimaryBasis),
		"must_surface_conflicts": c.MustSurfaceConflicts,
		"weights":                weightStringKeys(c.Weights),
		"exclude_when":           c.ExcludeWhen,
	}
	for k, v := range c.Raw {
		if _, exists := base[k]; !exists {
			base[k] = v
		}
	}
	return json.Marshal(base)
}

// unmarshalPolicyConfig deserializes JSONB into PolicyConfig, preserving
// unknown keys in Raw for forward-compat.
func unmarshalPolicyConfig(b []byte) (ctxmod.PolicyConfig, error) {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return ctxmod.PolicyConfig{}, err
	}
	cfg := ctxmod.PolicyConfig{Raw: raw}
	if v, ok := raw["primary_basis"].([]any); ok {
		cfg.PrimaryBasis = parseAssetTypes(v)
	}
	if v, ok := raw["must_surface_conflicts"].([]any); ok {
		cfg.MustSurfaceConflicts = toStrings(v)
	}
	if v, ok := raw["weights"].(map[string]any); ok {
		cfg.Weights = parseWeights(v)
	}
	if v, ok := raw["exclude_when"].([]any); ok {
		cfg.ExcludeWhen = toStrings(v)
	}
	return cfg, nil
}

func assetTypeStrings(ts []domain.AssetType) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}

func weightStringKeys(w map[domain.AssetType]float64) map[string]float64 {
	out := make(map[string]float64, len(w))
	for k, v := range w {
		out[string(k)] = v
	}
	return out
}

func parseAssetTypes(v []any) []domain.AssetType {
	out := make([]domain.AssetType, 0, len(v))
	for _, e := range v {
		if s, ok := e.(string); ok {
			out = append(out, domain.AssetType(s))
		}
	}
	return out
}

func toStrings(v []any) []string {
	out := make([]string, 0, len(v))
	for _, e := range v {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func parseWeights(v map[string]any) map[domain.AssetType]float64 {
	out := make(map[domain.AssetType]float64, len(v))
	for k, val := range v {
		switch n := val.(type) {
		case float64:
			out[domain.AssetType(k)] = n
		case int:
			out[domain.AssetType(k)] = float64(n)
		}
	}
	return out
}
