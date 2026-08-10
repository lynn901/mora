package postgres

// knowledge_job.go implements worker.JobStore against the knowledge_jobs table
// (design-docs/12 §6.5, 13 §5.4). Phase 0 scope: idempotent create, lease
// acquire/renew/release with FOR UPDATE SKIP LOCKED, retry classification, and
// dead marking. No job_type processing logic (Phase 1).

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/worker"
)

// JobRepo is the postgres implementation of worker.JobStore.
type JobRepo struct {
	pool *pgxpool.Pool
}

// NewJobRepo builds a worker.JobStore over a pgxpool.Pool.
func NewJobRepo(pool *pgxpool.Pool) *JobRepo { return &JobRepo{pool: pool} }

// Compile-time check.
var _ worker.JobStore = (*JobRepo)(nil)

// Create inserts a knowledge_jobs row keyed by dedupe_key. A duplicate
// dedupe_key returns worker.ErrJobExists (idempotency, §6.5). It runs inside tx
// so the Job is committed atomically with the consumer's ACK-side work — but it
// also accepts the pool itself (begins its own tx) when called standalone.
func (r *JobRepo) Create(ctx context.Context, tx pgx.Tx, j domain.Job) (domain.Job, error) {
	if j.DedupeKey == "" {
		return domain.Job{}, errors.New("job: dedupe_key is required")
	}
	if j.JobType == "" {
		return domain.Job{}, errors.New("job: job_type is required")
	}
	if j.MaxAttempt == 0 {
		j.MaxAttempt = 5
	}
	if j.Status == "" {
		j.Status = domain.JobPending
	}
	exec := r.execer(tx)
	var id uuid.UUID
	err := exec.QueryRow(ctx, `
		INSERT INTO knowledge_jobs
		  (source_event_id, job_type, asset_id, asset_version_id, source_id,
		   target_key, build_revision, dedupe_key, status, max_attempt, progress)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, created_at, updated_at`,
		nilIfZero(j.SourceEventID), j.JobType,
		nilIfZero(j.AssetID), nilIfZero(j.AssetVersionID), nilIfZero(j.SourceID),
		j.TargetKey, j.BuildRevision, j.DedupeKey, string(j.Status), j.MaxAttempt,
		j.Progress,
	).Scan(&id, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.Job{}, worker.ErrJobExists
		}
		return domain.Job{}, err
	}
	j.ID = id
	return j, nil
}

// Acquire claims the next pending or lease-expired job and extends its lease.
// FOR UPDATE SKIP LOCKED so concurrent workers don't contend (§6.5).
func (r *JobRepo) Acquire(ctx context.Context, owner string, ttl time.Duration) (domain.Job, error) {
	if owner == "" {
		return domain.Job{}, errors.New("job: acquire requires an owner")
	}
	until := time.Now().UTC().Add(ttl)
	row := r.pool.QueryRow(ctx, `
		UPDATE knowledge_jobs
		SET status = 'running', lease_owner = $1, lease_until = $2,
		    attempt = attempt + 1, updated_at = now()
		WHERE id = (
			SELECT id FROM knowledge_jobs
			WHERE status = 'pending'
			   OR (status = 'running' AND lease_until IS NOT NULL AND lease_until < now())
			ORDER BY created_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+jobColumns, owner, until)
	j, err := scanJob(row.Scan)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Job{}, worker.ErrNoJob
		}
		return domain.Job{}, err
	}
	return j, nil
}

// Renew extends the lease only if the caller still owns it. If the lease was
// reclaimed (expired + re-acquired by another worker), returns
// worker.ErrLeaseNotHeld so the caller stops work.
func (r *JobRepo) Renew(ctx context.Context, id uuid.UUID, owner string, ttl time.Duration) (time.Time, error) {
	until := time.Now().UTC().Add(ttl)
	var newUntil time.Time
	err := r.pool.QueryRow(ctx, `
		UPDATE knowledge_jobs
		SET lease_until = $3, updated_at = now()
		WHERE id = $1 AND lease_owner = $2
		RETURNING lease_until`, id, owner, until).Scan(&newUntil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, worker.ErrLeaseNotHeld
		}
		return time.Time{}, err
	}
	return newUntil, nil
}

// Release frees the lease (sets lease_owner/lease_until NULL) only if the caller
// owns it, returning status to pending so another worker can pick it up.
func (r *JobRepo) Release(ctx context.Context, id uuid.UUID, owner string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE knowledge_jobs
		SET lease_owner = NULL, lease_until = NULL, status = 'pending', updated_at = now()
		WHERE id = $1 AND lease_owner = $2`, id, owner)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return worker.ErrLeaseNotHeld
	}
	return nil
}

// MarkSucceeded sets status=succeeded and clears the lease.
func (r *JobRepo) MarkSucceeded(ctx context.Context, id uuid.UUID, progress map[string]any) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE knowledge_jobs
		SET status = 'succeeded', progress = COALESCE($2, progress),
		    lease_owner = NULL, lease_until = NULL, updated_at = now()
		WHERE id = $1`, id, progress)
	if err != nil {
		return err
	}
	return rowsAffected(tag, "job: mark succeeded")
}

// MarkFailed records a failure with a retry class (§6.5):
//   - transient + attempt < max  → status=pending (retry after backoff)
//   - transient at max_attempt   → status=dead
//   - permanent / policy_denied  → status=dead (no retry)
//
// The retry-vs-dead decision is expressed in SQL (CASE on attempt < max_attempt)
// so the read-then-decide happens atomically in one UPDATE — no race between a
// concurrent renew and this failure path.
func (r *JobRepo) MarkFailed(ctx context.Context, id uuid.UUID, class domain.RetryClass, code, redactedDetail string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE knowledge_jobs
		SET status = CASE
		      WHEN $2 = 'transient' AND attempt < max_attempt THEN 'pending'
		      ELSE 'dead'
		    END,
		    error_code = $3,
		    error_detail_redacted = $4,
		    lease_owner = NULL, lease_until = NULL,
		    updated_at = now()
		WHERE id = $1`, id, string(class), code, redactedDetail)
	if err != nil {
		return err
	}
	return rowsAffected(tag, "job: mark failed")
}

// Get loads a job by id.
func (r *JobRepo) Get(ctx context.Context, id uuid.UUID) (domain.Job, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM knowledge_jobs WHERE id = $1`, id)
	return scanJob(row.Scan)
}

// --- helpers ---

// execer returns tx if non-nil, else the pool. Both expose Exec/QueryRow.
func (r *JobRepo) execer(tx pgx.Tx) quadder {
	if tx != nil {
		return tx
	}
	return r.pool
}

type quadder interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func rowsAffected(tag pgconn.CommandTag, what string) error {
	if tag.RowsAffected() == 0 {
		return errors.New(what + ": no rows affected")
	}
	return nil
}

// nilIfZero returns a pointer to id, or nil for the zero UUID, so nullable
// UUID columns stay NULL for unset references.
func nilIfZero(id *uuid.UUID) any {
	if id == nil || *id == uuid.Nil {
		return nil
	}
	return *id
}

// jobColumns is the canonical SELECT column list for knowledge_jobs, kept in
// sync with scanJob's field order.
const jobColumns = `id, source_event_id, job_type, asset_id, asset_version_id, source_id,
	target_key, build_revision, dedupe_key, status, attempt, max_attempt,
	lease_owner, lease_until, progress, error_code, error_detail_redacted,
	created_at, updated_at`

func scanJob(scan func(dest ...any) error) (domain.Job, error) {
	var j domain.Job
	var (
		sourceEventID  *uuid.UUID
		assetID        *uuid.UUID
		assetVersionID *uuid.UUID
		sourceID       *uuid.UUID
		leaseOwner     *string
		leaseUntil     *time.Time
		progress       []byte
		errCode        *string
		errDetail      *string
		status         string
	)
	if err := scan(
		&j.ID, &sourceEventID, &j.JobType, &assetID, &assetVersionID, &sourceID,
		&j.TargetKey, &j.BuildRevision, &j.DedupeKey, &status, &j.Attempt, &j.MaxAttempt,
		&leaseOwner, &leaseUntil, &progress, &errCode, &errDetail,
		&j.CreatedAt, &j.UpdatedAt,
	); err != nil {
		return domain.Job{}, err
	}
	j.SourceEventID = sourceEventID
	j.AssetID = assetID
	j.AssetVersionID = assetVersionID
	j.SourceID = sourceID
	j.Status = domain.JobStatus(status)
	if leaseOwner != nil {
		j.LeaseOwner = *leaseOwner
	}
	j.LeaseUntil = leaseUntil
	if errCode != nil {
		j.ErrorCode = *errCode
	}
	if errDetail != nil {
		j.ErrorDetail = *errDetail
	}
	// progress is JSONB; leave as raw for Phase 0 (consumer decodes per job_type).
	_ = progress
	return j, nil
}
