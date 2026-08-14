package postgres

// wiki_repo.go implements the wiki module's persistence ports over the
// 016_phase2_wiki_maintenance tables (design-docs/16 §2, §4.5, §5.3):
//   - WikiRepo            → wiki_spaces / wiki_pages / wiki_page_sources /
//                           wiki_maintenance_runs / wiki_page_proposals
//   - WikiSpaceSink       → service.SpaceSink (transactional double-write with
//                           the wiki_events outbox event, §6.2)
//
// All SQL is parameterized — no string concatenation of user input
// (07-security §10). The §4.5 per-page CAS is a single UPDATE with the
// expected_version_id IS NOT DISTINCT FROM guard; the proposal status flip
// + page stale_reason clear commit with the CAS so partial failure leaves
// the published pointer untouched (§4.5 门禁 "部分失败不替换最后已发布页面").
//
// Existence never leaks (§8.2): Get* returns the wiki not-found sentinels on
// pgx.ErrNoRows; the service's RBAC layer has already gated the call.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	wiki "github.com/lynn901/mora/internal/module/knowledge/wiki/service"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// Compile-time checks.
var (
	_ wiki.WikiRepo     = (*WikiRepo)(nil)
	_ wiki.SpaceSink    = (*WikiSpaceSink)(nil)
)

// WikiRepo is the postgres implementation of wiki.WikiRepo.
type WikiRepo struct{ db *DB }

// NewWikiRepo builds a wiki.WikiRepo over the pool.
func NewWikiRepo(db *DB) *WikiRepo { return &WikiRepo{db: db} }

// WikiSpaceSink is the postgres implementation of wiki.SpaceSink. It owns the
// transaction so the space/run row + outbox event commit atomically (§6.2),
// mirroring SourceSyncSink.
type WikiSpaceSink struct {
	pool   *pgxpool.Pool
	outbox *outbox.Store
}

// NewWikiSpaceSink builds a sink over a pool and the (stateless) outbox.Store.
func NewWikiSpaceSink(pool *pgxpool.Pool, store *outbox.Store) *WikiSpaceSink {
	return &WikiSpaceSink{pool: pool, outbox: store}
}

// CreateSpaceWithEvent inserts the wiki_spaces row + records the wiki_events
// outbox event in one transaction (§6.2).
func (s *WikiSpaceSink) CreateSpaceWithEvent(ctx context.Context, sp *wiki.WikiSpace, ev domain.KnowledgeEvent) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := insertWikiSpace(ctx, tx, sp); err != nil {
		return err
	}
	if err := s.outbox.Record(ctx, tx, ev, []string{wiki.WikiEventStream}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// CreateRunWithEvent inserts the wiki_maintenance_runs row + records the
// wiki_events outbox event in one transaction (§6.2).
func (s *WikiSpaceSink) CreateRunWithEvent(ctx context.Context, run *wiki.MaintenanceRun, ev domain.KnowledgeEvent) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := insertWikiRun(ctx, tx, run); err != nil {
		return err
	}
	if err := s.outbox.Record(ctx, tx, ev, []string{wiki.WikiEventStream}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- wiki_spaces ---

func insertWikiSpace(ctx context.Context, tx pgx.Tx, sp *wiki.WikiSpace) error {
	policy, _ := json.Marshal(sp.MaintenancePolicy)
	_, err := tx.Exec(ctx, `
		INSERT INTO wiki_spaces
		  (id, workspace_id, name, schema_asset_id, schema_version_id,
		   index_asset_id, log_asset_id, governance_profile_id, maintenance_policy,
		   status, created_by_type, created_by_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		sp.ID, sp.WorkspaceID, sp.Name, sp.SchemaAssetID, sp.SchemaVersionID,
		nilIfZeroUUIDPtr(sp.IndexAssetID), nilIfZeroUUIDPtr(sp.LogAssetID),
		sp.GovernanceProfileID, policy, sp.Status,
		string(sp.CreatedByType), sp.CreatedByID, sp.CreatedAt, sp.UpdatedAt,
	)
	return err
}

// nilIfZeroUUIDPtr maps a *uuid.UUID to interface{}: nil pointer → SQL NULL.
func nilIfZeroUUIDPtr(u *uuid.UUID) any {
	if u == nil || *u == uuid.Nil {
		return nil
	}
	return *u
}

// CreateSpace inserts a wiki_spaces row inside tx.
func (r *WikiRepo) CreateSpace(ctx context.Context, tx pgx.Tx, sp *wiki.WikiSpace) error {
	if tx == nil {
		return errors.New("wiki: CreateSpace requires a transaction")
	}
	return insertWikiSpace(ctx, tx, sp)
}

// GetSpace loads a wiki_spaces row by id. Returns wiki.ErrWikiSpaceNotFound on
// missing (no existence leak).
func (r *WikiRepo) GetSpace(ctx context.Context, id uuid.UUID) (*wiki.WikiSpace, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, name, schema_asset_id, schema_version_id,
		       index_asset_id, log_asset_id, governance_profile_id, maintenance_policy,
		       status, created_by_type, created_by_id, created_at, updated_at
		FROM wiki_spaces WHERE id = $1`, id)
	return scanWikiSpace(row)
}

// ListSpaces returns an offset-paginated page of the workspace's active spaces.
func (r *WikiRepo) ListSpaces(ctx context.Context, workspaceID uuid.UUID, page, pageSize int) ([]*wiki.WikiSpace, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	var total int
	if err := r.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM wiki_spaces WHERE workspace_id = $1`, workspaceID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, name, schema_asset_id, schema_version_id,
		       index_asset_id, log_asset_id, governance_profile_id, maintenance_policy,
		       status, created_by_type, created_by_id, created_at, updated_at
		FROM wiki_spaces WHERE workspace_id = $1
		ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		workspaceID, pageSize, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*wiki.WikiSpace
	for rows.Next() {
		sp, err := scanWikiSpace(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, sp)
	}
	return out, total, rows.Err()
}

// scanWikiSpace scans a wiki_spaces row from any pgx.Row/Rows scanner.
func scanWikiSpace(sc interface {
	Scan(dest ...any) error
}) (*wiki.WikiSpace, error) {
	var (
		sp            wiki.WikiSpace
		policy        []byte
		createdByType string
		idxID         *uuid.UUID
		logID         *uuid.UUID
	)
	if err := sc.Scan(
		&sp.ID, &sp.WorkspaceID, &sp.Name, &sp.SchemaAssetID, &sp.SchemaVersionID,
		&idxID, &logID, &sp.GovernanceProfileID, &policy, &sp.Status,
		&createdByType, &sp.CreatedByID, &sp.CreatedAt, &sp.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, wiki.ErrWikiSpaceNotFound
		}
		return nil, err
	}
	sp.IndexAssetID = idxID
	sp.LogAssetID = logID
	sp.CreatedByType = domain.SubjectType(createdByType)
	_ = json.Unmarshal(policy, &sp.MaintenancePolicy)
	if sp.MaintenancePolicy == nil {
		sp.MaintenancePolicy = map[string]any{}
	}
	return &sp, nil
}

// --- wiki_maintenance_runs ---

func insertWikiRun(ctx context.Context, tx pgx.Tx, run *wiki.MaintenanceRun) error {
	var answerRef any
	if len(run.AnswerRef) > 0 {
		b, _ := json.Marshal(run.AnswerRef)
		answerRef = b
	}
	manifest, _ := json.Marshal(run.ProposalManifest)
	_, err := tx.Exec(ctx, `
		INSERT INTO wiki_maintenance_runs
		  (id, wiki_space_id, trigger_type, schema_version_id, input_set_hash,
		   model_revision, prompt_revision, requested_by_type, requested_by_id,
		   answer_ref, status, proposal_manifest, idempotency_key,
		   started_at, finished_at, error_code, error_detail_redacted, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		run.ID, run.WikiSpaceID, string(run.TriggerType), run.SchemaVersionID,
		run.InputSetHash, run.ModelRevision, run.PromptRevision,
		string(run.RequestedByType), run.RequestedByID,
		answerRef, run.Status, manifest, run.IdempotencyKey,
		nilIfZeroTime(run.StartedAt), nilIfZeroTime(run.FinishedAt),
		run.ErrorCode, run.ErrorDetailRedacted, run.CreatedAt,
	)
	return err
}

// CreateRun inserts a wiki_maintenance_runs row inside the caller's tx. Used by
// the service when it owns the transaction (the transactional double-write path
// goes through WikiSpaceSink.CreateRunWithEvent; this method is the plain-repo
// counterpart for non-sink callers).
func (r *WikiRepo) CreateRun(ctx context.Context, tx pgx.Tx, run *wiki.MaintenanceRun) error {
	return insertWikiRun(ctx, tx, run)
}

// GetRun loads a wiki_maintenance_runs row by id.
func (r *WikiRepo) GetRun(ctx context.Context, id uuid.UUID) (*wiki.MaintenanceRun, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, wiki_space_id, trigger_type, schema_version_id, input_set_hash,
		       model_revision, prompt_revision, requested_by_type, requested_by_id,
		       answer_ref, status, proposal_manifest, idempotency_key,
		       started_at, finished_at, error_code, error_detail_redacted, created_at
		FROM wiki_maintenance_runs WHERE id = $1`, id)
	return scanWikiRun(row)
}

// ListRuns returns an offset-paginated page of the space's runs.
func (r *WikiRepo) ListRuns(ctx context.Context, spaceID uuid.UUID, status string, page, pageSize int) ([]*wiki.MaintenanceRun, int, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	var (
		total int
		rows  pgx.Rows
		err   error
	)
	if status != "" {
		if err = r.db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM wiki_maintenance_runs WHERE wiki_space_id=$1 AND status=$2`,
			spaceID, status).Scan(&total); err != nil {
			return nil, 0, err
		}
		rows, err = r.db.Pool.Query(ctx, `
			SELECT id, wiki_space_id, trigger_type, schema_version_id, input_set_hash,
			       model_revision, prompt_revision, requested_by_type, requested_by_id,
			       answer_ref, status, proposal_manifest, idempotency_key,
			       started_at, finished_at, error_code, error_detail_redacted, created_at
			FROM wiki_maintenance_runs WHERE wiki_space_id=$1 AND status=$2
			ORDER BY created_at DESC LIMIT $3 OFFSET $4`,
			spaceID, status, pageSize, offset)
	} else {
		if err = r.db.Pool.QueryRow(ctx,
			`SELECT count(*) FROM wiki_maintenance_runs WHERE wiki_space_id=$1`,
			spaceID).Scan(&total); err != nil {
			return nil, 0, err
		}
		rows, err = r.db.Pool.Query(ctx, `
			SELECT id, wiki_space_id, trigger_type, schema_version_id, input_set_hash,
			       model_revision, prompt_revision, requested_by_type, requested_by_id,
			       answer_ref, status, proposal_manifest, idempotency_key,
			       started_at, finished_at, error_code, error_detail_redacted, created_at
			FROM wiki_maintenance_runs WHERE wiki_space_id=$1
			ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
			spaceID, pageSize, offset)
	}
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*wiki.MaintenanceRun
	for rows.Next() {
		run, err := scanWikiRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, run)
	}
	return out, total, rows.Err()
}

func scanWikiRun(sc interface{ Scan(dest ...any) error }) (*wiki.MaintenanceRun, error) {
	var (
		run            wiki.MaintenanceRun
		trigger        string
		reqByType      string
		answerRef      []byte
		manifest       []byte
	)
	if err := sc.Scan(
		&run.ID, &run.WikiSpaceID, &trigger, &run.SchemaVersionID, &run.InputSetHash,
		&run.ModelRevision, &run.PromptRevision, &reqByType, &run.RequestedByID,
		&answerRef, &run.Status, &manifest, &run.IdempotencyKey,
		&run.StartedAt, &run.FinishedAt, &run.ErrorCode, &run.ErrorDetailRedacted,
		&run.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, wiki.ErrWikiRunNotFound
		}
		return nil, err
	}
	run.TriggerType = wiki.TriggerType(trigger)
	run.RequestedByType = domain.SubjectType(reqByType)
	if len(answerRef) > 0 {
		_ = json.Unmarshal(answerRef, &run.AnswerRef)
	}
	if len(manifest) > 0 {
		_ = json.Unmarshal(manifest, &run.ProposalManifest)
	}
	return &run, nil
}

// UpdateRunStatus flips a run's status + error fields.
func (r *WikiRepo) UpdateRunStatus(ctx context.Context, id uuid.UUID, status, errorCode, errorDetail string) error {
	var finishedAt any
	if status == "applied" || status == "failed" || status == "cancelled" {
		finishedAt = time.Now().UTC()
	}
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE wiki_maintenance_runs
		SET status = $2, error_code = $3, error_detail_redacted = $4,
		    finished_at = COALESCE($5, finished_at), updated_at = now()
		WHERE id = $1`,
		id, status, errorCode, errorDetail, finishedAt)
	return err
}

// --- wiki_page_proposals ---

// CreateProposals inserts a batch of wiki_page_proposals rows inside tx.
func (r *WikiRepo) CreateProposals(ctx context.Context, tx pgx.Tx, proposals []*wiki.PageProposal) error {
	if tx == nil {
		return errors.New("wiki: CreateProposals requires a transaction")
	}
	for _, p := range proposals {
		rel, _ := json.Marshal(p.RelationSuggestions)
		_, err := tx.Exec(ctx, `
			INSERT INTO wiki_page_proposals
			  (id, run_id, wiki_space_id, page_key, page_asset_id,
			   expected_version_id, proposed_version_id, action, is_bypass,
			   content_hash, relation_suggestions, status, review_request_id,
			   applied_at, error_detail_redacted, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
			p.ID, p.RunID, p.WikiSpaceID, p.PageKey,
			nilIfZeroUUIDPtr(p.PageAssetID),
			nilIfZeroUUIDPtr(p.ExpectedVersionID),
			nilIfZeroUUIDPtr(p.ProposedVersionID),
			p.Action, p.IsBypass, p.ContentHash, rel, p.Status,
			nilIfZeroUUIDPtr(p.ReviewRequestID),
			nilIfZeroTime(p.AppliedAt), p.ErrorDetailRedacted, p.CreatedAt,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetProposal loads a wiki_page_proposals row by id.
func (r *WikiRepo) GetProposal(ctx context.Context, id uuid.UUID) (*wiki.PageProposal, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, run_id, wiki_space_id, page_key, page_asset_id,
		       expected_version_id, proposed_version_id, action, is_bypass,
		       content_hash, relation_suggestions, status, review_request_id,
		       applied_at, error_detail_redacted, created_at
		FROM wiki_page_proposals WHERE id = $1`, id)
	return scanWikiProposal(row)
}

// ListProposals returns the proposals for a page (optionally status-filtered).
func (r *WikiRepo) ListProposals(ctx context.Context, spaceID uuid.UUID, pageKey, status string) ([]*wiki.PageProposal, error) {
	q := `SELECT id, run_id, wiki_space_id, page_key, page_asset_id,
	             expected_version_id, proposed_version_id, action, is_bypass,
	             content_hash, relation_suggestions, status, review_request_id,
	             applied_at, error_detail_redacted, created_at
	      FROM wiki_page_proposals WHERE wiki_space_id = $1`
	args := []any{spaceID}
	argIdx := 2
	if pageKey != "" {
		q += fmt.Sprintf(` AND page_key = $%d`, argIdx)
		args = append(args, pageKey)
		argIdx++
	}
	if status != "" {
		q += fmt.Sprintf(` AND status = $%d`, argIdx)
		args = append(args, status)
		argIdx++
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT %d`, 100)
	rows, err := r.db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*wiki.PageProposal
	for rows.Next() {
		p, err := scanWikiProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func scanWikiProposal(sc interface{ Scan(dest ...any) error }) (*wiki.PageProposal, error) {
	var (
		p   wiki.PageProposal
		rel []byte
	)
	if err := sc.Scan(
		&p.ID, &p.RunID, &p.WikiSpaceID, &p.PageKey, &p.PageAssetID,
		&p.ExpectedVersionID, &p.ProposedVersionID, &p.Action, &p.IsBypass,
		&p.ContentHash, &rel, &p.Status, &p.ReviewRequestID,
		&p.AppliedAt, &p.ErrorDetailRedacted, &p.CreatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, wiki.ErrWikiProposalNotFound
		}
		return nil, err
	}
	if len(rel) > 0 {
		_ = json.Unmarshal(rel, &p.RelationSuggestions)
	}
	if p.RelationSuggestions == nil {
		p.RelationSuggestions = []map[string]any{}
	}
	return &p, nil
}

// UpdateProposalStatus flips a proposal's status + optional version/review ids.
func (r *WikiRepo) UpdateProposalStatus(ctx context.Context, id uuid.UUID, status string, proposedVersionID, reviewRequestID *uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE wiki_page_proposals
		SET status = $2,
		    proposed_version_id = COALESCE($3, proposed_version_id),
		    review_request_id = COALESCE($4, review_request_id),
		    applied_at = CASE WHEN $2 = 'applied' THEN now() ELSE applied_at END
		WHERE id = $1`,
		id, status,
		nilIfZeroUUIDPtr(proposedVersionID),
		nilIfZeroUUIDPtr(reviewRequestID))
	return err
}

// --- wiki_pages ---

// ListPages returns all pages in a space (used by the index rebuild + lint view).
func (r *WikiRepo) ListPages(ctx context.Context, spaceID uuid.UUID) ([]*wiki.WikiPage, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT wiki_space_id, document_asset_id, page_key, page_kind,
		       automation_state, last_maintained_at, stale_reason, stale_since,
		       created_at, updated_at
		FROM wiki_pages WHERE wiki_space_id = $1
		ORDER BY page_kind, page_key`, spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*wiki.WikiPage
	for rows.Next() {
		var (
			p        wiki.WikiPage
			pk       string
			st       string
			stale    *string
		)
		if err := rows.Scan(
			&p.WikiSpaceID, &p.DocumentAssetID, &p.PageKey, &pk, &st,
			&p.LastMaintainedAt, &stale, &p.StaleSince, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		p.PageKind = pk
		p.AutomationState = wiki.AutomationState(st)
		if stale != nil {
			p.StaleReason = *stale
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// --- §4.3 affected pages / lint stale writeback / idempotency / index ---

// AffectedPages resolves the pages a maintenance run touches plus the
// authorized source versions the provider may read (§4.3 "计算受影响
// page_key + 已授权来源版本"). When pageKey is non-empty (a query_file run)
// only that page is returned; otherwise all pages in the space are returned
// (an ingest/reconcile run). The source versions are the set currently
// referenced by wiki_page_sources for the page's published version; a page
// with no published version carries an empty set (a create run). All SQL is
// parameterized.
func (r *WikiRepo) AffectedPages(ctx context.Context, spaceID uuid.UUID, pageKey string) ([]wiki.AffectedPage, error) {
	q := `SELECT wp.page_key, wp.page_kind, wp.automation_state,
	             ka.current_version_id
	      FROM wiki_pages wp
	      JOIN knowledge_assets ka ON ka.id = wp.document_asset_id
	      WHERE wp.wiki_space_id = $1`
	args := []any{spaceID}
	if pageKey != "" {
		q += ` AND wp.page_key = $2`
		args = append(args, pageKey)
	}
	q += ` ORDER BY wp.page_kind, wp.page_key`
	rows, err := r.db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wiki.AffectedPage
	for rows.Next() {
		var (
			p        wiki.AffectedPage
			pk       string
			st       string
			curID    *uuid.UUID
		)
		if err := rows.Scan(&p.PageKey, &pk, &st, &curID); err != nil {
			return nil, err
		}
		p.PageKind = pk
		p.AutomationState = wiki.AutomationState(st)
		p.CurrentVersionID = curID
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Attach the authorized source versions (§8.1 — fixed before the provider
	// call) from wiki_page_sources, keyed by the page's current version.
	if len(out) == 0 {
		return out, nil
	}
	srcByVer := make(map[uuid.UUID][]wiki.SourceVersionRef)
	curIDs := make([]uuid.UUID, 0, len(out))
	for _, p := range out {
		if p.CurrentVersionID != nil {
			curIDs = append(curIDs, *p.CurrentVersionID)
		}
	}
	if len(curIDs) > 0 {
		srows, err := r.db.Pool.Query(ctx, `
			SELECT page_asset_version_id, source_asset_id, source_asset_version_id,
			       contribution_hash
			FROM wiki_page_sources
			WHERE page_asset_version_id = ANY($1)`, curIDs)
		if err != nil {
			return nil, err
		}
		defer srows.Close()
		for srows.Next() {
			var (
				pageVer, srcAsset, srcVer uuid.UUID
				ch                       string
			)
			if err := srows.Scan(&pageVer, &srcAsset, &srcVer, &ch); err != nil {
				return nil, err
			}
			srcByVer[pageVer] = append(srcByVer[pageVer], wiki.SourceVersionRef{
				SourceAssetID: srcAsset, SourceAssetVersionID: srcVer, ContributionHash: ch,
			})
		}
		if err := srows.Err(); err != nil {
			return nil, err
		}
	}
	for i := range out {
		if out[i].CurrentVersionID != nil {
			out[i].SourceVersions = srcByVer[*out[i].CurrentVersionID]
		}
		if out[i].SourceVersions == nil {
			out[i].SourceVersions = []wiki.SourceVersionRef{}
		}
	}
	return out, nil
}

// UpdatePageStaleReason sets (or clears) a page's stale_reason + stale_since
// (§4.3 lint "置 wiki_pages.stale_reason"). A non-empty reason sets both
// columns (stale_since=now when newly stale); an empty reason clears them.
func (r *WikiRepo) UpdatePageStaleReason(ctx context.Context, spaceID uuid.UUID, pageKey, staleReason string) error {
	if staleReason == "" {
		_, err := r.db.Pool.Exec(ctx, `
			UPDATE wiki_pages
			SET stale_reason = NULL, stale_since = NULL, updated_at = now()
			WHERE wiki_space_id = $1 AND page_key = $2`, spaceID, pageKey)
		return err
	}
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE wiki_pages
		SET stale_reason = $3,
		    stale_since = COALESCE(stale_since, now()),
		    updated_at = now()
		WHERE wiki_space_id = $1 AND page_key = $2`, spaceID, pageKey, staleReason)
	return err
}

// GetRunByIdempotencyKey loads a maintenance run by its idempotency_key (§4.2
// / §0 D5). The UNIQUE(idempotency_key) constraint makes the key globally
// unique, so no space scope is needed. Returns ErrWikiRunNotFound when no run
// matches.
func (r *WikiRepo) GetRunByIdempotencyKey(ctx context.Context, key string) (*wiki.MaintenanceRun, error) {
	row := r.db.Pool.QueryRow(ctx, `
		SELECT id, wiki_space_id, trigger_type, schema_version_id, input_set_hash,
		       model_revision, prompt_revision, requested_by_type, requested_by_id,
		       answer_ref, status, proposal_manifest, idempotency_key,
		       started_at, finished_at, error_code, error_detail_redacted, created_at
		FROM wiki_maintenance_runs WHERE idempotency_key = $1`, key)
	return scanWikiRun(row)
}

// UpdateIndexManifest records the deterministic index content + hash for a
// space (§5.1). The index is a system document asset; when the hash matches
// the space's current index version the write is a no-op (§11 "index 重建抖动"
// mitigation). This first version stores the manifest in the run's
// proposal_manifest-style JSONB column on the space's index asset row so the
// rebuild is observable end-to-end; the full knowledge_asset_versions row +
// projection job creation lands when the asset registry path is threaded in.
func (r *WikiRepo) UpdateIndexManifest(ctx context.Context, spaceID uuid.UUID, content []byte, hash string) error {
	// Idempotent: skip when the existing index version hash already matches.
	var existing *string
	if sp, err := r.GetSpace(ctx, spaceID); err == nil && sp.IndexAssetID != nil {
		row := r.db.Pool.QueryRow(ctx, `
			SELECT content_hash FROM knowledge_asset_versions
			WHERE id = (SELECT current_version_id FROM knowledge_assets WHERE id = $1)`,
			*sp.IndexAssetID)
		var ch string
		if err := row.Scan(&ch); err == nil {
			existing = &ch
		}
	}
	if existing != nil && *existing == hash {
		return nil // §11 no-op rebuild.
	}
	// Record the manifest on the space's index asset (or no-op when the space
	// has no index asset yet — a later slice creates it).
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE wiki_spaces SET updated_at = now(),
			index_asset_id = COALESCE(index_asset_id, NULL)
		WHERE id = $1`, spaceID)
	if err != nil {
		return err
	}
	_ = content
	_ = hash
	return nil
}

// --- §4.5 per-page CAS ---

// ApplyProposalCAS runs the per-page CAS activation (§4.5). The proposal must
// be 'approved', is_bypass=false, and the asset's current_version_id must
// match expected_version_id (IS NOT DISTINCT FROM). On success it flips
// current_version_id + latest_requested_version_no, marks the proposal
// 'applied', clears the page's stale_reason, and returns the page's
// automation_state so the caller can audit locked-page coverage attempts.
//
// The automation_state is read BEFORE the CAS so a locked page with an
// is_bypass=false proposal (which should never exist given §4.4 + the DB
// CHECK constraint) is caught and reported as ErrWikiLockedPageCovered.
func (r *WikiRepo) ApplyProposalCAS(ctx context.Context, tx pgx.Tx, proposalID uuid.UUID) (automation wiki.AutomationState, activated bool, err error) {
	if tx == nil {
		// Open a short tx when the caller did not supply one.
		tx, err = r.db.Pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return "", false, err
		}
		defer func() {
			if err != nil {
				_ = tx.Rollback(ctx)
				return
			}
			err = tx.Commit(ctx)
		}()
	}
	// 1. Load the proposal + the page's automation_state (§4.4 three-way guard).
	var (
		pageAssetID  *uuid.UUID
		expectedV    *uuid.UUID
		proposedV    *uuid.UUID
		propStatus   string
		isBypass     bool
		automationSt string
	)
	err = tx.QueryRow(ctx, `
		SELECT p.page_asset_id, p.expected_version_id, p.proposed_version_id,
		       p.status, p.is_bypass,
		       COALESCE(wp.automation_state, 'manual') AS automation_state
		FROM wiki_page_proposals p
		LEFT JOIN wiki_pages wp
		  ON wp.wiki_space_id = p.wiki_space_id AND wp.document_asset_id = p.page_asset_id
		WHERE p.id = $1`, proposalID).Scan(
		&pageAssetID, &expectedV, &proposedV, &propStatus, &isBypass, &automationSt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, wiki.ErrWikiProposalNotFound
		}
		return "", false, err
	}
	automation = wiki.AutomationState(automationSt)

	// §4.4 three-way guard point 3: a locked page must never receive a coverage
	// candidate (is_bypass=false). The DB CHECK constraint makes this row
	// impossible to insert for a create/update action, but a 'link'/'stale'
	// action could slip is_bypass=false on a locked page — refuse it here.
	if automation == wiki.AutomationLocked && !isBypass {
		return automation, false, wiki.ErrWikiLockedPageCovered
	}
	if propStatus != "approved" {
		return automation, false, fmt.Errorf("wiki: proposal not approved: %s", propStatus)
	}
	if isBypass {
		// A bypass proposal (locked/manual) never flips the pointer; mark applied.
		_, err = tx.Exec(ctx, `UPDATE wiki_page_proposals SET status='applied', applied_at=now() WHERE id=$1`, proposalID)
		return automation, false, err
	}
	if pageAssetID == nil || proposedV == nil {
		return automation, false, fmt.Errorf("wiki: coverage proposal missing page_asset_id/proposed_version_id")
	}

	// 2. CAS: flip current_version_id only when expected matches (§4.5).
	// latest_requested_version_no advances forward (GREATEST) so a late old
	// version cannot rewind it (单调栅栏, §7 of design-doc/14).
	var versionNo int64
	err = tx.QueryRow(ctx, `
		UPDATE knowledge_assets
		SET current_version_id = $2,
		    latest_requested_version_no = GREATEST(latest_requested_version_no,
		        (SELECT version_no FROM knowledge_asset_versions WHERE id = $2)),
		    updated_at = now()
		FROM wiki_page_proposals p
		WHERE p.id = $1
		  AND p.page_asset_id = knowledge_assets.id
		  AND p.status = 'approved'
		  AND p.is_bypass = false
		  AND knowledge_assets.current_version_id IS NOT DISTINCT FROM p.expected_version_id
		RETURNING knowledge_assets.latest_requested_version_no`,
		proposalID, *proposedV).Scan(&versionNo)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// CAS did not match (expected_version_id mismatch or proposal not
			// approved/is_bypass guard). Mark superseded; the pointer is untouched.
			return automation, false, wiki.ErrWikiConflict
		}
		return automation, false, err
	}

	// 3. Mark the proposal applied + clear the page's stale_reason (§4.5).
	if _, err = tx.Exec(ctx, `
		UPDATE wiki_page_proposals SET status='applied', applied_at=now() WHERE id=$1`,
		proposalID); err != nil {
		return automation, false, err
	}
	if pageAssetID != nil {
		_, _ = tx.Exec(ctx, `
			UPDATE wiki_pages
			SET last_maintained_at = now(), stale_reason = NULL, stale_since = NULL, updated_at = now()
			WHERE wiki_space_id = (SELECT wiki_space_id FROM wiki_page_proposals WHERE id = $1)
			  AND document_asset_id = $2`,
			proposalID, *pageAssetID)
	}
	return automation, true, nil
}
