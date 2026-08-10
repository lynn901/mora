package postgres

// authz_repos.go implements the authz layer's persistence ports over the
// 013_knowledge_core tables (design-docs/13 §4.2–4.3, §5.1, §5.6):
//   - RevisionRepo  → workspace_authz_revisions (linearization point, §5.6)
//   - SessionRepo   → delegated_sessions (server-side revocable record, §5.1)
//
// AssetRepo / AgentRepo / BindingRepo / DecisionRepo are wired here too so the
// authz.Service has a single postgres-side adapter. All SQL is parameterized —
// no string-concatenated user input (07-security §10).
//
// Revocation linearization (§5.6: 撤权后下一次请求同步拒绝): SessionRepo.Revoke
// sets revoked_at=now() AND bumps workspace_authz_revisions.revision in ONE
// transaction. A concurrent VerifyDelegated that already read the old revision
// will, on its next Current() call, see the bumped value and refuse — so a
// yanked permission takes effect on the very next request.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/authz"
)

// --- RevisionRepo ---

// RevisionRepo reads workspace_authz_revisions (the linearization point, §5.6).
type revisionsRepo struct{ db *DB }

func NewRevisionsRepo(db *DB) authz.RevisionRepo { return &revisionsRepo{db: db} }

// Current returns the workspace's authz revision. A missing row (workspace
// created before 013 was applied) is treated as revision 0 so the linearization
// invariant holds without forcing a backfill — the first Revoke/permission
// change inserts/upserts the row.
func (r *revisionsRepo) Current(ctx context.Context, workspaceID uuid.UUID) (int64, error) {
	var rev int64
	err := r.db.Pool.QueryRow(ctx,
		`SELECT revision FROM workspace_authz_revisions WHERE workspace_id = $1`,
		workspaceID).Scan(&rev)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return rev, nil
}

// --- SessionRepo ---

// sessionRepo persists delegated_sessions (§5.1). delegated_sessions is the
// authority VerifyDelegated consults after the JWT signature check.
type sessionRepo struct{ db *DB }

func NewSessionRepo(db *DB) authz.SessionRepo { return &sessionRepo{db: db} }

// Insert writes a delegated_sessions row. allowed_actions is stored as a JSONB
// array of action strings (Phase 0 vocab is small + lossless).
func (r *sessionRepo) Insert(ctx context.Context, s authz.DelegatedSession) error {
	actions := actionsToAny(s.AllowedActions)
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO delegated_sessions
		  (id, token_id, agent_id, acting_user_id, workspace_id,
		   allowed_actions, issued_authz_revision, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		s.ID, nilIfZero(s.TokenID), nilIfZero(s.AgentID), nilIfZero(s.ActingUserID),
		s.WorkspaceID, actions, s.IssuedAuthzRevision, s.ExpiresAt)
	return err
}

// Get loads a delegated_sessions row by id (for VerifyDelegated). A missing row
// returns an error the manager wraps as "session not found" — indistinguishable
// from a forged JTI to the caller.
func (r *sessionRepo) Get(ctx context.Context, id uuid.UUID) (authz.DelegatedSession, error) {
	var (
		s              authz.DelegatedSession
		tokenID        *uuid.UUID
		agentID        *uuid.UUID
		actingUserID   *uuid.UUID
		allowedActions []byte
		revokedAt      *time.Time
	)
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, token_id, agent_id, acting_user_id, workspace_id,
		       allowed_actions, issued_authz_revision, expires_at, revoked_at
		FROM delegated_sessions WHERE id = $1`, id).Scan(
		&s.ID, &tokenID, &agentID, &actingUserID, &s.WorkspaceID,
		&allowedActions, &s.IssuedAuthzRevision, &s.ExpiresAt, &revokedAt)
	if err != nil {
		return authz.DelegatedSession{}, err
	}
	s.TokenID = tokenID
	s.AgentID = agentID
	s.ActingUserID = actingUserID
	s.RevokedAt = revokedAt
	// allowed_actions is JSONB; decode into domain.Action slice. A NULL/empty
	// array yields a zero-length slice (no capabilities) — fail-closed.
	s.AllowedActions = scanActions(allowedActions)
	return s, nil
}

// Revoke sets revoked_at=now() AND bumps the workspace authz revision in the
// SAME transaction (§5.6). The revision bump is what makes every other
// delegated session issued under the prior revision stale
// (VerifyDelegated → ErrDelegatedStaleRevision), giving the
// "撤权后下一次请求同步拒绝" guarantee. Returns the new revision.
//
// If the session was already revoked, this is a no-op on the row but still
// bumps the revision — callers that need idempotent revoke semantics should
// check the returned revision against their expectation. The bump is cheap and
// safe (a second revoke for the same capability removal is still a linearization
// event for any sessions issued between the two revokes).
func (r *sessionRepo) Revoke(ctx context.Context, id, workspaceID uuid.UUID) (int64, error) {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck — documented pgx pattern

	if _, err := tx.Exec(ctx,
		`UPDATE delegated_sessions SET revoked_at = now() WHERE id = $1`,
		id); err != nil {
		return 0, err
	}

	// Upsert + bump in one statement: if the row exists, revision = revision+1;
	// if it does not (pre-013 workspace), INSERT … ON CONFLICT seeds it at 1.
	// The single statement inside this tx is the linearization point.
	var newRev int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO workspace_authz_revisions (workspace_id, revision, updated_at)
		VALUES ($1, 1, now())
		ON CONFLICT (workspace_id)
		DO UPDATE SET revision = workspace_authz_revisions.revision + 1,
		              updated_at = now()
		RETURNING revision`, workspaceID).Scan(&newRev); err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return newRev, nil
}

// --- AssetRepo / AgentRepo / BindingRepo (authz.Service ports) ---

// assetRepo adapts knowledge_assets reads for authz.Service (lifecycle gate +
// locator). It returns assetInfo; a missing asset returns an error the locator
// maps to ErrTargetNotFound so existence never leaks (§8.2).
type assetRepo struct{ db *DB }

func NewAuthzAssetRepo(db *DB) authz.AssetRepo { return &assetRepo{db: db} }

func (r *assetRepo) Get(ctx context.Context, assetID uuid.UUID) (authz.AssetInfo, error) {
	var a authz.AssetInfo
	var currentVersionID *uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		SELECT workspace_id, status, owner_type, owner_id, current_version_id
		FROM knowledge_assets WHERE id = $1`, assetID).Scan(
		&a.WorkspaceID, &a.Status, &a.OwnerType, &a.OwnerID, &currentVersionID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.AssetInfo{}, errNotFound
		}
		return authz.AssetInfo{}, err
	}
	a.CurrentVersionID = currentVersionID
	return a, nil
}

// assetVersionRepo adapts knowledge_asset_versions reads for authz.Service's
// pinned-version-revocation gate (§8.2 用例 5 / §11.4). It returns only the
// two status axes the gate decides on (build_status, governance_status); a
// missing row returns errNotFound so the service maps it to a deny (existence
// never leaks). All SQL is parameterized — no string-concatenated input
// (07-security §10).
type assetVersionRepo struct{ db *DB }

func NewAuthzAssetVersionRepo(db *DB) authz.AssetVersionRepo {
	return &assetVersionRepo{db: db}
}

func (r *assetVersionRepo) Get(ctx context.Context, versionID uuid.UUID) (authz.AssetVersionInfo, error) {
	var v authz.AssetVersionInfo
	err := r.db.Pool.QueryRow(ctx, `
		SELECT build_status, governance_status
		FROM knowledge_asset_versions WHERE id = $1`, versionID).Scan(
		&v.BuildStatus, &v.GovernanceStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.AssetVersionInfo{}, errNotFound
		}
		return authz.AssetVersionInfo{}, err
	}
	return v, nil
}

// agentRepo adapts agents reads for authz.Service (rbac subject resolution +
// locator).
type agentRepo struct{ db *DB }

func NewAuthzAgentRepo(db *DB) authz.AgentRepo { return &agentRepo{db: db} }

func (r *agentRepo) Get(ctx context.Context, agentID uuid.UUID) (authz.AgentInfo, error) {
	var a authz.AgentInfo
	var serviceAccountID *uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		SELECT workspace_id, status, service_account_id
		FROM agents WHERE id = $1`, agentID).Scan(
		&a.WorkspaceID, &a.Status, &serviceAccountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return authz.AgentInfo{}, errNotFound
		}
		return authz.AgentInfo{}, err
	}
	a.ServiceAccountID = serviceAccountID
	return a, nil
}

// bindingRepo adapts agent_bindings reads for authz.Service (binding narrowing).
// Only active (revoked_at IS NULL) bindings are returned.
type bindingRepo struct{ db *DB }

func NewAuthzBindingRepo(db *DB) authz.BindingRepo { return &bindingRepo{db: db} }

func (r *bindingRepo) ActiveForAgent(ctx context.Context, agentID, workspaceID uuid.UUID) ([]domain.AgentBinding, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, agent_id, workspace_id, scope_kind, asset_id, asset_type,
		       effect, version_policy, pinned_version_id, delivery_mode,
		       priority, created_by, created_at, revoked_at
		FROM agent_bindings
		WHERE agent_id = $1 AND workspace_id = $2 AND revoked_at IS NULL
		ORDER BY priority DESC, created_at`, agentID, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.AgentBinding
	for rows.Next() {
		var b domain.AgentBinding
		var (
			scopeKind       string
			effect          string
			versionPolicy   string
			deliveryMode    string
			assetType       *string
			createdBy       *uuid.UUID
			revokedAt       *time.Time
		)
		if err := rows.Scan(
			&b.ID, &b.AgentID, &b.WorkspaceID, &scopeKind, &b.AssetID, &assetType,
			&effect, &versionPolicy, &b.PinnedVersionID, &deliveryMode,
			&b.Priority, &createdBy, &b.CreatedAt, &revokedAt); err != nil {
			return nil, err
		}
		b.ScopeKind = domain.BindingScopeKind(scopeKind)
		b.Effect = domain.BindingEffect(effect)
		b.VersionPolicy = domain.BindingVersionPolicy(versionPolicy)
		b.DeliveryMode = domain.BindingDeliveryMode(deliveryMode)
		if assetType != nil {
			at := domain.AssetType(*assetType)
			b.AssetType = &at
		}
		b.CreatedBy = createdBy
		b.RevokedAt = revokedAt
		out = append(out, b)
	}
	return out, rows.Err()
}

// --- DecisionRepo ---

// decisionRepo records authorization_decisions (§5.6, Provider capability
// audit). Phase 0 records the row; the signed token lives in DelegatedManager.
type decisionRepo struct{ db *DB }

func NewDecisionRepo(db *DB) authz.DecisionRepo { return &decisionRepo{db: db} }

func (r *decisionRepo) Record(ctx context.Context, d authz.DecisionRecord) (uuid.UUID, error) {
	if d.ExpiresAt.IsZero() {
		// Default to the delegated TTL so a recorded-but-unconsumed decision
		// can't outlive a capability it was meant to back.
		d.ExpiresAt = time.Now().UTC().Add(30 * time.Second)
	}
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO authorization_decisions
		  (workspace_id, authz_revision, principal_type, principal_id,
		   acting_user_id, agent_id, action, scope_hash, audience, nonce_hash,
		   expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		d.WorkspaceID, d.AuthzRevision, d.PrincipalType, d.PrincipalID,
		nilIfZero(d.ActingUserID), nilIfZero(d.AgentID),
		string(d.Action), d.ScopeHash, d.Audience, d.NonceHash, d.ExpiresAt).Scan(&id)
	return id, err
}

// --- helpers ---

// actionsToAny converts a domain.Action slice into the string slice pgx will
// encode as a JSONB array. nil → empty slice so the column default is never
// relied on (explicit fail-closed).
func actionsToAny(actions []domain.Action) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, string(a))
	}
	return out
}

// scanActions decodes a JSONB allowed_actions blob into a domain.Action slice.
// A NULL or non-array blob yields an empty slice — fail-closed (no capability
// claimed from malformed storage).
func scanActions(blob []byte) []domain.Action {
	if len(blob) == 0 || string(blob) == "null" {
		return nil
	}
	// pgx returns JSONB as a []byte holding the JSON text. Decode the array.
	var strs []string
	if err := json.Unmarshal(blob, &strs); err != nil {
		return nil
	}
	out := make([]domain.Action, 0, len(strs))
	for _, s := range strs {
		out = append(out, domain.Action(s))
	}
	return out
}

// Compile-time checks.
var (
	_ authz.RevisionRepo    = (*revisionsRepo)(nil)
	_ authz.SessionRepo     = (*sessionRepo)(nil)
	_ authz.AssetRepo       = (*assetRepo)(nil)
	_ authz.AssetVersionRepo = (*assetVersionRepo)(nil)
	_ authz.AgentRepo       = (*agentRepo)(nil)
	_ authz.BindingRepo     = (*bindingRepo)(nil)
	_ authz.DecisionRepo    = (*decisionRepo)(nil)
)
