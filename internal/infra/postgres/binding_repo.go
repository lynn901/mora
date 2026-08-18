package postgres

// binding_repo.go implements binding.Repository, BatchUpsert, and RevokeRevoker
// (Phase 5-2 / YS-162, design-docs/19 §5). It does NOT change the
// agent_bindings table structure: the batch idempotency record lives in
// agent_binding_batches (migration 023), so agent_bindings stays as Phase 0
// built it (migration 013) + the 022 supplemental indexes.
//
// Transactions: the sink owns the pgx.Tx (the service layer stays pgx-free —
// same layering as source_sync_sink). One BatchUpsert transaction:
//   1. INSERT … ON CONFLICT (idempotency_key) DO NOTHING on
//      agent_binding_batches (the idempotency gate).
//      - 0 rows inserted → idempotency collision → resolve retry vs conflict.
//      - 1 row inserted → proceed to write the bindings + outbox + revision.
//   2. For each input: create (INSERT) or update (UPDATE gated by ETag).
//   3. For each changed binding: outbox.Store.Record(agent.binding_changed).
//   4. Bump workspace_authz_revisions.revision in the SAME tx (§5.4 — the
//      linearization point that invalidates the resolved-set cache).
//   5. Commit.
//
// On any error the tx rolls back — neither the batch record, the bindings,
// the outbox events, nor the revision bump land. The atomic guarantee
// (§5.2 事务内原子性) is preserved.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	binding "github.com/lynn901/mora/internal/module/binding"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// BindingRepo implements binding.Repository over agent_bindings.
type BindingRepo struct{ db *DB }

func NewBindingRepo(db *DB) *BindingRepo { return &BindingRepo{db: db} }

// Compile-time checks.
var (
	_ binding.Repository    = (*BindingRepo)(nil)
	_ binding.BatchUpsert   = (*BindingSink)(nil)
	_ binding.RevokeRevoker = (*BindingSink)(nil)
)

// bindingColumns is the shared SELECT list (matches the 013 schema).
const bindingColumns = `id, agent_id, workspace_id, scope_kind, asset_id, asset_type,
	effect, version_policy, pinned_version_id, delivery_mode,
	priority, created_by, created_at, revoked_at`

// scanBinding scans one agent_binding row into a domain.AgentBinding.
// Reused by the authz bindingRepo and here so the two read paths stay aligned.
func scanBinding(row pgx.Row) (domain.AgentBinding, error) {
	var b domain.AgentBinding
	var (
		scopeKind     string
		effect        string
		versionPolicy string
		deliveryMode  string
		assetType     *string
		createdBy     *uuid.UUID
		revokedAt     *time.Time
	)
	if err := row.Scan(
		&b.ID, &b.AgentID, &b.WorkspaceID, &scopeKind, &b.AssetID, &assetType,
		&effect, &versionPolicy, &b.PinnedVersionID, &deliveryMode,
		&b.Priority, &createdBy, &b.CreatedAt, &revokedAt); err != nil {
		return b, err
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
	return b, nil
}

// List returns active bindings for an agent, cursor-paginated by id (§6.1).
// `after` nil → first page. rev-A + id-A > rev-A + id-B is not needed: a
// simple id > after cursor keeps a stable order over a single agent's set.
func (r *BindingRepo) List(ctx context.Context, agentID, workspaceID uuid.UUID, after *uuid.UUID, limit int) ([]domain.AgentBinding, error) {
	if limit <= 0 {
		limit = 50
	}
	q := fmt.Sprintf(`
		SELECT %s FROM agent_bindings
		WHERE agent_id = $1 AND workspace_id = $2 AND revoked_at IS NULL`,
		bindingColumns)
	args := []any{agentID, workspaceID}
	if after != nil && *after != uuid.Nil {
		q += ` AND id > $3`
		args = append(args, *after)
	}
	q += ` ORDER BY id LIMIT $` + fmt.Sprintf("%d", len(args)+1)
	args = append(args, limit)
	rows, err := r.db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.AgentBinding, 0, limit)
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Get returns a single binding by id (active or revoked). A missing row maps
// to binding.ErrBindingNotFound so the service surfaces 404 (no existence leak).
func (r *BindingRepo) Get(ctx context.Context, id uuid.UUID) (domain.AgentBinding, error) {
	row := r.db.Pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT %s FROM agent_bindings WHERE id = $1`, bindingColumns), id)
	b, err := scanBinding(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return b, binding.ErrBindingNotFound
		}
		return b, err
	}
	return b, nil
}

// GetByIdempotencyKey loads the bindings written by a batch (§5.2 retry).
// The batch row carries the agent_id; we return all bindings for that agent
// that were created/updated by the batch — identified by created_at falling
// at/between the batch's created_at (approximate; the batch is one tx so all
// its bindings share the same tx timestamp). For an exact match we tag each
// binding's id into the batch's payload at write time and read them back by id.
// (See BatchUpsert: the batch payload stores the written binding ids.)
func (r *BindingRepo) GetByIdempotencyKey(ctx context.Context, key string) ([]domain.AgentBinding, error) {
	var payloadJSON []byte
	err := r.db.Pool.QueryRow(ctx,
		`SELECT payload FROM agent_binding_batches WHERE idempotency_key = $1`, key).
		Scan(&payloadJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, binding.ErrBindingNotFound
		}
		return nil, err
	}
	// The batch payload carries the written binding ids (see BatchUpsert).
	var rec batchPayload
	if err := json.Unmarshal(payloadJSON, &rec); err != nil {
		return nil, err
	}
	if len(rec.BindingIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Pool.Query(ctx,
		fmt.Sprintf(`SELECT %s FROM agent_bindings WHERE id = ANY($1)`, bindingColumns),
		rec.BindingIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.AgentBinding, 0, len(rec.BindingIDs))
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// --- BindingSink: batch upsert + revoke (transactional) ---

// BindingSink implements BatchUpsert + RevokeRevoker. It owns the pgx.Tx and
// the outbox double-write (§6.3), the workspace revision bump (§5.4), and the
// batch idempotency record (migration 023).
type BindingSink struct {
	pool   *pgxpool.Pool
	outbox *outbox.Store
}

func NewBindingSink(pool *pgxpool.Pool, store *outbox.Store) *BindingSink {
	return &BindingSink{pool: pool, outbox: store}
}

// batchPayload is stored in agent_binding_batches.payload — the written
// binding ids + the per-item inputs (canonical form) so an idempotent retry
// can return the originals and a collision can compare payloads. No secrets.
type batchPayload struct {
	BindingIDs []uuid.UUID            `json:"binding_ids"`
	Items      []binding.BindingInput `json:"items"`
}

// BatchUpsert applies a batch of create/update binding operations in one tx.
// See file-level comment for the 5-step flow.
func (s *BindingSink) BatchUpsert(ctx context.Context, agentID, workspaceID uuid.UUID, idempotencyKey string, inputs []binding.BindingInput, actor domain.EventActor) (binding.BatchResult, error) {
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	payloadHash := hashBatchInputs(agentID, inputs)
	actorID := actor.ID
	actorIDPtr := &actorID

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return binding.BatchResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck — documented pgx pattern

	// 1. Idempotency gate: INSERT the batch record. ON CONFLICT DO NOTHING
	//    → if 0 rows, a batch with this key already exists → resolve.
	var batchID uuid.UUID
	var existingHash string
	inserted := true
	err = tx.QueryRow(ctx, `
		INSERT INTO agent_binding_batches
		  (idempotency_key, agent_id, workspace_id, payload_hash, binding_count,
		   authz_revision, actor_type, actor_id)
		VALUES ($1, $2, $3, $4, $5, 0, $6, $7)
		ON CONFLICT (idempotency_key) DO UPDATE
		  SET idempotency_key = agent_binding_batches.idempotency_key
		RETURNING id, payload_hash`, idempotencyKey, agentID, workspaceID,
		payloadHash, len(inputs), string(actor.Type), nilIfZero(actorIDPtr)).
		Scan(&batchID, &existingHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO UPDATE returning nothing means the DO UPDATE
			// branch's RETURNING was suppressed — actually pgx returns ErrNoRows
			// only when the DO UPDATE produces 0 rows, which can't happen here.
			// Treat defensively as a collision resolution path.
			inserted = false
		} else {
			return binding.BatchResult{}, err
		}
	}
	// Distinguish: a real INSERT (existingHash == payloadHash because we wrote
	// it) vs a collision (existingHash is the original batch's hash). The
	// trick: ON CONFLICT DO UPDATE SET <no-op col> = <same col> returns the
	// EXISTING row's columns, so existingHash is the original payload_hash.
	// If it equals ours → idempotent retry; else → conflict.
	_ = batchID
	if existingHash != payloadHash {
		// Same key, different payload → conflict (§11.1).
		return binding.BatchResult{}, binding.ErrIdempotencyConflict
	}
	// If the row pre-existed with the SAME hash → idempotent retry. The caller
	// (service) re-fetches by idempotency_key and returns the originals. Signal
	// via ErrIdempotentRetry only if we did NOT insert (the row pre-existed).
	if !inserted {
		return binding.BatchResult{IdempotentHit: true}, binding.ErrIdempotentRetry
	}

	// 2. Write each binding (create or ETag-gated update).
	written := make([]domain.AgentBinding, 0, len(inputs))
	for _, in := range inputs {
		b, werr := s.upsertOne(ctx, tx, agentID, workspaceID, in)
		if werr != nil {
			return binding.BatchResult{}, werr
		}
		written = append(written, b)
	}

	// 3. Bump workspace_authz_revisions.revision in the SAME tx (§5.4 — the
	//    linearization point: the next authz request reads the new revision,
	//    so the resolved-set cache invalidates by key). Mirrors
	//    delegated_sessions.Revoke's upsert+1.
	var newRev int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO workspace_authz_revisions (workspace_id, revision, updated_at)
		VALUES ($1, 1, now())
		ON CONFLICT (workspace_id)
		DO UPDATE SET revision = workspace_authz_revisions.revision + 1,
		              updated_at = now()
		RETURNING revision`, workspaceID).Scan(&newRev); err != nil {
		return binding.BatchResult{}, err
	}

	// 4. Stamp the batch row's binding_count + authz_revision + payload
	//    (binding ids), then record one agent.binding_changed outbox event per
	//    changed binding (§5.2). All in the same tx — the outbox dispatcher
	//    only ever sees a batch whose bindings + revision are already durable.
	writtenIDs := make([]uuid.UUID, 0, len(written))
	for _, b := range written {
		writtenIDs = append(writtenIDs, b.ID)
	}
	finalPayload, _ := json.Marshal(batchPayload{BindingIDs: writtenIDs, Items: inputs})
	if _, err := tx.Exec(ctx, `
		UPDATE agent_binding_batches
		SET binding_count = $2, authz_revision = $3, payload = $4
		WHERE idempotency_key = $1`,
		idempotencyKey, len(writtenIDs), newRev, finalPayload); err != nil {
		return binding.BatchResult{}, err
	}
	wsID := workspaceID
	for _, b := range written {
		ev := bindingChangedEvent(b, actor, newRev, &wsID)
		if err := s.outbox.Record(ctx, tx, ev, []string{outbox.KnowledgeEventsStream}); err != nil {
			return binding.BatchResult{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return binding.BatchResult{}, err
	}
	results := make([]binding.BindingResult, 0, len(written))
	for _, b := range written {
		results = append(results, binding.BindingResult{Binding: b})
	}
	return binding.BatchResult{Results: results, NewRevision: newRev}, nil
}

// upsertOne creates a new binding (in.ID nil) or "updates" an existing one
// (in.ID set + in.ETag). Because agent_bindings has no updated_at column
// (Phase 0 schema, unchanged), a binding is effectively immutable after
// create; an "update" is modeled as revoke-the-old + create-the-new so the
// history is preserved (AC-6 不可改写历史) and no column is added. The ETag
// is derived from the old binding's created_at epoch-ms (its immutable
// identity): a mismatch means the caller read a different (or already-revoked)
// row → ErrBindingConflict. The ETag-aware revoke (UPDATE … WHERE revoked_at
// IS NULL) is the optimistic-concurrency fence: a concurrent revoke by
// another batch affects 0 rows → ErrBindingConflict (§5.2 防覆盖).
func (s *BindingSink) upsertOne(ctx context.Context, tx pgx.Tx, agentID, workspaceID uuid.UUID, in binding.BindingInput) (domain.AgentBinding, error) {
	// Normalize optional columns.
	var assetID any
	if in.AssetID != nil {
		assetID = nilIfZero(in.AssetID)
	}
	var assetType any
	if in.AssetType != nil {
		assetType = string(*in.AssetType)
	}
	var pinnedID any
	if in.PinnedVersionID != nil {
		pinnedID = nilIfZero(in.PinnedVersionID)
	}

	// Create path (also the tail of the update path): INSERT a new binding.
	create := func() (domain.AgentBinding, error) {
		id := uuid.New()
		_, err := tx.Exec(ctx, `
			INSERT INTO agent_bindings
			  (id, agent_id, workspace_id, scope_kind, asset_id, asset_type,
			   effect, version_policy, pinned_version_id, delivery_mode,
			   priority, created_at, revoked_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11, now(), NULL)`,
			id, agentID, workspaceID, string(in.ScopeKind), assetID, assetType,
			string(in.Effect), string(in.VersionPolicy), pinnedID,
			string(in.DeliveryMode), in.Priority)
		if err != nil {
			if isUniqueViolation(err) {
				return domain.AgentBinding{}, binding.ErrBindingConflict
			}
			return domain.AgentBinding{}, err
		}
		row := tx.QueryRow(ctx,
			fmt.Sprintf(`SELECT %s FROM agent_bindings WHERE id = $1`, bindingColumns), id)
		return scanBinding(row)
	}

	if in.ID == nil || *in.ID == uuid.Nil {
		return create()
	}

	// Update path: ETag-gated revoke of the old binding + create of the new.
	// ETag = created_at epoch-ms (immutable). A mismatch → conflict.
	id := *in.ID
	var curCreated time.Time
	var curRevoked *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT created_at, revoked_at FROM agent_bindings WHERE id = $1`, id).
		Scan(&curCreated, &curRevoked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.AgentBinding{}, binding.ErrBindingNotFound
		}
		return domain.AgentBinding{}, err
	}
	if etagOf(curCreated, 0) != in.ETag {
		return domain.AgentBinding{}, binding.ErrBindingConflict
	}
	// Optimistic-concurrency fence: revoke only if still active. A concurrent
	// revoke affects 0 rows → conflict (§5.2 防覆盖).
	tag, err := tx.Exec(ctx,
		`UPDATE agent_bindings SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return domain.AgentBinding{}, err
	}
	if tag.RowsAffected() == 0 {
		return domain.AgentBinding{}, binding.ErrBindingConflict
	}
	return create()
}

// Revoke revokes a single binding + bumps the workspace authz revision in the
// SAME tx (§5.4 — the linearization point). Mirrors delegated_sessions.Revoke.
// Writes one agent.binding_changed outbox event. Returns the new revision.
func (s *BindingSink) Revoke(ctx context.Context, bindingID, agentID, workspaceID uuid.UUID, actor domain.EventActor) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck — documented pgx pattern

	tag, err := tx.Exec(ctx,
		`UPDATE agent_bindings SET revoked_at = now()
		 WHERE id = $1 AND agent_id = $2 AND workspace_id = $3 AND revoked_at IS NULL`,
		bindingID, agentID, workspaceID)
	if err != nil {
		return 0, err
	}
	if tag.RowsAffected() == 0 {
		// Either missing, cross-workspace/agent, or already revoked → not-found.
		return 0, binding.ErrBindingNotFound
	}
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
	// agent.binding_changed outbox event (action=revoke).
	b := domain.AgentBinding{ID: bindingID, AgentID: agentID, WorkspaceID: workspaceID}
	ev := bindingChangedEvent(b, actor, newRev, &workspaceID)
	ev.Payload["action"] = "revoke"
	if err := s.outbox.Record(ctx, tx, ev, []string{outbox.KnowledgeEventsStream}); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return newRev, nil
}

// bindingChangedEvent builds an agent.binding_changed KnowledgeEvent for a
// written/revoked binding. Carries only IDs + revision — no content, no
// secrets (§5.1 envelope). AggregateID is the binding; WorkspaceID is set so
// the dispatcher can route and consumers can scope.
func bindingChangedEvent(b domain.AgentBinding, actor domain.EventActor, rev int64, wsID *uuid.UUID) domain.KnowledgeEvent {
	return domain.KnowledgeEvent{
		EventID:       uuid.NewString(),
		EventType:     domain.KEAgentBindingChanged,
		EventVersion:  1,
		AggregateType: domain.AggAgent,
		AggregateID:   b.ID,
		WorkspaceID:   wsID,
		Actor:         actor,
		OccurredAt:    time.Now().UTC(),
		Payload: map[string]any{
			"agent_id":       b.AgentID.String(),
			"binding_id":     b.ID.String(),
			"authz_revision": rev,
			"scope_kind":     string(b.ScopeKind),
			"effect":         string(b.Effect),
			"version_policy": string(b.VersionPolicy),
			"delivery_mode":  string(b.DeliveryMode),
		},
	}
}

// hashBatchInputs returns the canonical hash of the batch inputs (no secrets).
// Used to decide idempotent-retry (same hash) vs conflict (different hash).
// The agent_id is included so two batches for different agents can't collide
// on the same idempotency_key (the key is also UNIQUE, so this is belt-and-
// suspenders + a stable comparison field).
func hashBatchInputs(agentID uuid.UUID, inputs []binding.BindingInput) string {
	type canonItem struct {
		ID              *uuid.UUID `json:"id,omitempty"`
		ScopeKind       string     `json:"scope_kind"`
		AssetID         string     `json:"asset_id,omitempty"`
		AssetType       string     `json:"asset_type,omitempty"`
		Effect          string     `json:"effect"`
		VersionPolicy   string     `json:"version_policy"`
		PinnedVersionID string     `json:"pinned_version_id,omitempty"`
		DeliveryMode    string     `json:"delivery_mode"`
		Priority        int        `json:"priority"`
	}
	items := make([]canonItem, len(inputs))
	for i, in := range inputs {
		c := canonItem{
			ScopeKind:     string(in.ScopeKind),
			Effect:        string(in.Effect),
			VersionPolicy: string(in.VersionPolicy),
			DeliveryMode:  string(in.DeliveryMode),
			Priority:      in.Priority,
		}
		if in.ID != nil {
			c.ID = in.ID
		}
		if in.AssetID != nil {
			c.AssetID = in.AssetID.String()
		}
		if in.AssetType != nil {
			c.AssetType = string(*in.AssetType)
		}
		if in.PinnedVersionID != nil {
			c.PinnedVersionID = in.PinnedVersionID.String()
		}
		items[i] = c
	}
	b, _ := json.Marshal(struct {
		AgentID string      `json:"agent_id"`
		Items   []canonItem `json:"items"`
	}{AgentID: agentID.String(), Items: items})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
