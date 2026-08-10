//go:build integration

// Integration tests for the knowledge_jobs JobRepo (design-docs/12 §6.5,
// 13 §5.4). Skipped unless DATABASE_URL is set (run with:
// DATABASE_URL=... go test -tags=integration ./internal/infra/postgres/...).
//
// These verify the SQL contract unit tests can't: the dedupe_key UNIQUE guard,
// FOR UPDATE SKIP LOCKED lease acquire, lease renew/release ownership, and the
// retry-class → pending/dead decision in MarkFailed.
package postgres

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/worker"
)

func jobTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func resetJobs(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), "TRUNCATE knowledge_jobs CASCADE")
	require.NoError(t, err)
}

// TestJobRepo_CreateIdempotent: a first Create inserts; a second Create with
// the same dedupe_key returns ErrJobExists (idempotency, §6.5).
func TestJobRepo_CreateIdempotent(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	dedupe := "test:" + uuid.New().String()
	j := domain.Job{JobType: "knowledge.projection", DedupeKey: dedupe, MaxAttempt: 3}

	j1, err := repo.Create(ctx, nil, j)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, j1.ID)
	assert.Equal(t, domain.JobPending, j1.Status)
	assert.Equal(t, 3, j1.MaxAttempt)

	// second create with the same dedupe_key → ErrJobExists.
	_, err = repo.Create(ctx, nil, domain.Job{JobType: "knowledge.projection", DedupeKey: dedupe, MaxAttempt: 3})
	assert.ErrorIs(t, err, worker.ErrJobExists, "duplicate dedupe_key must be idempotent")
}

// TestJobRepo_RequiresDedupeAndType: Create rejects a job missing the dedupe
// key or job_type — the idempotency key is the contract, not optional.
func TestJobRepo_RequiresDedupeAndType(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	_, err := repo.Create(ctx, nil, domain.Job{JobType: "x"})
	require.Error(t, err, "missing dedupe_key must error")

	_, err = repo.Create(ctx, nil, domain.Job{DedupeKey: "x"})
	require.Error(t, err, "missing job_type must error")
}

// TestJobRepo_AcquireLeasesAndBumpsAttempt: Acquire claims a pending job,
// flips it to running, and increments attempt. A second Acquire finds nothing.
func TestJobRepo_AcquireLeasesAndBumpsAttempt(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	j, err := repo.Create(ctx, nil, domain.Job{JobType: "knowledge.projection", DedupeKey: "k1", MaxAttempt: 3})
	require.NoError(t, err)

	got, err := repo.Acquire(ctx, "worker-1", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, j.ID, got.ID)
	assert.Equal(t, domain.JobRunning, got.Status)
	assert.Equal(t, 1, got.Attempt, "acquire must increment attempt")
	assert.Equal(t, "worker-1", got.LeaseOwner)
	require.NotNil(t, got.LeaseUntil)

	// nothing else to acquire.
	_, err = repo.Acquire(ctx, "worker-2", time.Minute)
	assert.ErrorIs(t, err, worker.ErrNoJob)
}

// TestJobRepo_AcquireReclaimsExpiredLease: a running job whose lease_until has
// passed is reclaimable by another worker (crash recovery, §6.5).
func TestJobRepo_AcquireReclaimsExpiredLease(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	j, err := repo.Create(ctx, nil, domain.Job{JobType: "knowledge.projection", DedupeKey: "k2", MaxAttempt: 3})
	require.NoError(t, err)

	// first worker acquires with a very short lease.
	_, err = repo.Acquire(ctx, "crashy-worker", 50*time.Millisecond)
	require.NoError(t, err)
	// expire the lease (wait past ttl).
	time.Sleep(80 * time.Millisecond)

	// second worker reclaims.
	got, err := repo.Acquire(ctx, "worker-2", time.Minute)
	require.NoError(t, err)
	assert.Equal(t, j.ID, got.ID)
	assert.Equal(t, "worker-2", got.LeaseOwner, "expired lease must be reclaimable by another worker")
	assert.Equal(t, 2, got.Attempt, "reclaim must increment attempt again")
}

// TestJobRepo_RenewOnlyByOwner: the lease holder can renew; a non-owner cannot.
func TestJobRepo_RenewOnlyByOwner(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	j, err := repo.Create(ctx, nil, domain.Job{JobType: "knowledge.projection", DedupeKey: "k3", MaxAttempt: 3})
	require.NoError(t, err)
	_, err = repo.Acquire(ctx, "owner-1", time.Minute)
	require.NoError(t, err)

	// non-owner renew fails.
	_, err = repo.Renew(ctx, j.ID, "owner-2", time.Minute)
	assert.ErrorIs(t, err, worker.ErrLeaseNotHeld, "non-owner must not renew")

	// owner renew succeeds and extends.
	newUntil, err := repo.Renew(ctx, j.ID, "owner-1", 2*time.Minute)
	require.NoError(t, err)
	assert.True(t, newUntil.After(time.Now().Add(time.Minute)), "renew must extend the lease")
}

// TestJobRepo_ReleaseReturnsToPending: Release frees the lease and puts the
// job back to pending so it is immediately re-acquirable.
func TestJobRepo_ReleaseReturnsToPending(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	j, err := repo.Create(ctx, nil, domain.Job{JobType: "knowledge.projection", DedupeKey: "k4", MaxAttempt: 3})
	require.NoError(t, err)
	_, err = repo.Acquire(ctx, "owner", time.Minute)
	require.NoError(t, err)

	require.NoError(t, repo.Release(ctx, j.ID, "owner"))

	got, err := repo.Get(ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.JobPending, got.Status)
	assert.Empty(t, got.LeaseOwner)
}

// TestJobRepo_MarkFailedTransientRetriesUntilDead: a transient failure stays
// pending while attempt < max, then goes dead at max_attempt.
func TestJobRepo_MarkFailedTransientRetriesUntilDead(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	// max_attempt=2 → one retry allowed, then dead.
	j, err := repo.Create(ctx, nil, domain.Job{JobType: "knowledge.projection", DedupeKey: "k5", MaxAttempt: 2})
	require.NoError(t, err)

	// acquire → fail (attempt becomes 1, < max → pending).
	_, err = repo.Acquire(ctx, "w", time.Minute)
	require.NoError(t, err)
	require.NoError(t, repo.MarkFailed(ctx, j.ID, domain.RetryTransient, "boom", "redacted detail"))
	got, err := repo.Get(ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.JobPending, got.Status, "transient below max must retry")
	assert.Equal(t, "boom", got.ErrorCode)
	assert.Equal(t, "redacted detail", got.ErrorDetail)

	// second acquire → fail again (attempt becomes 2, == max → dead).
	_, err = repo.Acquire(ctx, "w", time.Minute)
	require.NoError(t, err)
	require.NoError(t, repo.MarkFailed(ctx, j.ID, domain.RetryTransient, "boom2", "still redacted"))
	got, err = repo.Get(ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.JobDead, got.Status, "transient at max_attempt must go dead")
}

// TestJobRepo_MarkFailedPermanentGoesDeadImmediately: permanent + policy_denied
// never retry — status=dead right away, even with attempts remaining.
func TestJobRepo_MarkFailedPermanentGoesDeadImmediately(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	for _, class := range []domain.RetryClass{domain.RetryPermanent, domain.RetryPolicyDenied} {
		require.NoError(t, resetJobsQuiet(t, pool))
		j, err := repo.Create(ctx, nil, domain.Job{JobType: "knowledge.projection", DedupeKey: "perm-" + uuid.New().String(), MaxAttempt: 5})
		require.NoError(t, err)
		_, err = repo.Acquire(ctx, "w", time.Minute)
		require.NoError(t, err)
		require.NoError(t, repo.MarkFailed(ctx, j.ID, class, "perm", "redacted"))
		got, err := repo.Get(ctx, j.ID)
		require.NoError(t, err)
		assert.Equal(t, domain.JobDead, got.Status, "%s must go dead immediately", class)
	}
}

// TestJobRepo_MarkSucceeded: MarkSucceeded sets status=succeeded and clears the
// lease so the job is terminal.
func TestJobRepo_MarkSucceeded(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	j, err := repo.Create(ctx, nil, domain.Job{JobType: "knowledge.projection", DedupeKey: "k6", MaxAttempt: 3})
	require.NoError(t, err)
	_, err = repo.Acquire(ctx, "w", time.Minute)
	require.NoError(t, err)

	require.NoError(t, repo.MarkSucceeded(ctx, j.ID, map[string]any{"chunks": 42}))
	got, err := repo.Get(ctx, j.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.JobSucceeded, got.Status)
	assert.Empty(t, got.LeaseOwner)
}

// TestJobRepo_CreateInTxRollsBack: Create inside a rolled-back transaction
// leaves no row — the job is committed only with the caller's ACK-side work.
func TestJobRepo_CreateInTxRollsBack(t *testing.T) {
	pool := jobTestPool(t)
	repo := NewJobRepo(pool)
	resetJobs(t, pool)
	ctx := context.Background()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	require.NoError(t, err)
	_, err = repo.Create(ctx, tx, domain.Job{JobType: "knowledge.projection", DedupeKey: "tx-rollback", MaxAttempt: 3})
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(ctx))

	// no row present after rollback.
	var n int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_jobs WHERE dedupe_key=$1`, "tx-rollback").Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "rolled-back Create must leave no row")
}

func resetJobsQuiet(t *testing.T, pool *pgxpool.Pool) error {
	t.Helper()
	_, err := pool.Exec(context.Background(), "TRUNCATE knowledge_jobs CASCADE")
	return err
}
