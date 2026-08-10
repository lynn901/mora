// Package worker is the knowledge-jobs consumer-side store (design-docs/12
// §6.5, 13 §5.4). Phase 0 ships the store mechanics only: idempotent create,
// lease acquire/renew/release, retry classification, and dead marking. Concrete
// job_type processing logic arrives in Phase 1.
//
// The Job store complements the Outbox (§5.3): the Outbox guarantees the
// producer's event is not lost; the Job store guarantees consumption is
// idempotent (dedupe_key UNIQUE), lease-safe (FOR UPDATE SKIP LOCKED), and
// observable (status/attempt/error columns). An event can fan out into many
// projection Jobs sharing one dedupe_key shape.
package worker

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
)

// ErrJobExists is returned by Create when the dedupe_key already has a row.
// Producers treat this as success (idempotent fan-out).
var ErrJobExists = errors.New("worker: job already exists (dedupe_key)")

// ErrNoJob is returned by Acquire when no pending/claimable job is available.
var ErrNoJob = errors.New("worker: no claimable job")

// ErrLeaseNotHeld is returned by Renew/Release when the caller does not own
// the job's current lease (another worker claimed it, or it expired).
var ErrLeaseNotHeld = errors.New("worker: lease not held")

// JobStore is the consumer-side persistence surface for knowledge_jobs (§6.5).
// All methods are safe for concurrent workers: Acquire uses
// FOR UPDATE SKIP LOCKED so two workers never claim the same job.
type JobStore interface {
	// Create inserts a job keyed by dedupe_key. If a row with the same
	// dedupe_key already exists, it is a no-op (returns ErrJobExists) —
	// idempotency (§6.5).
	Create(ctx context.Context, tx pgx.Tx, j domain.Job) (domain.Job, error)

	// Acquire claims the next pending or lease-expired job for owner, extending
	// lease_until by ttl. Returns ErrNoJob when nothing is claimable. Uses
	// FOR UPDATE SKIP LOCKED (§6.5).
	Acquire(ctx context.Context, owner string, ttl time.Duration) (domain.Job, error)

	// Renew extends the lease on a job the owner currently holds.
	Renew(ctx context.Context, id uuid.UUID, owner string, ttl time.Duration) (time.Time, error)

	// Release frees the lease on a job (e.g. on graceful shutdown) without
	// changing status.
	Release(ctx context.Context, id uuid.UUID, owner string) error

	// MarkSucceeded sets status=succeeded and clears the lease.
	MarkSucceeded(ctx context.Context, id uuid.UUID, progress map[string]any) error

	// MarkFailed records a failure with a retry class. transient + attempt <
	// max_attempt → status=pending (retry); transient at max → dead;
	// permanent/policy_denied → dead (policy_denied also writes audit at caller).
	MarkFailed(ctx context.Context, id uuid.UUID, class domain.RetryClass, code, redactedDetail string) error

	// Get loads a job by id.
	Get(ctx context.Context, id uuid.UUID) (domain.Job, error)
}
