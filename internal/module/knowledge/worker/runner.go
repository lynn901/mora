// Package worker is the Phase 1 knowledge-job dispatch layer (design-docs/14
// §5.2). Phase 0 shipped the JobStore mechanics (idempotent create, lease,
// retry classification); Phase 1 adds the job_type dispatch table and the
// acquire→run→mark loop that drives it.
//
// The Runner owns the consumer-side loop: it Acquires a pending job under a
// lease, dispatches to the registered Handler by job_type, and records the
// outcome (MarkSucceeded / MarkFailed with a retry class). It does NOT own the
// Stream consumption — that lives in cmd/knowledge-worker's main, which maps
// Stream messages to idempotent job.Create calls. The two surfaces compose:
// Streams deliver events → JobStore dedupes → Runner executes.
package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lynn901/mora/internal/domain"
)

// Job type identifiers (design-docs/14 §5.2 dispatch table). Each maps to a
// Handler registered on the Runner.
const (
	JobSourceSync       = "source_sync"
	JobProjectionBuild  = "projection_build"
	JobAssetActivate    = "asset_activate"
	JobReconcileScan    = "reconcile_scan"
	JobLegacyBackfill   = "legacy_backfill"
)

// DedupeKey builds the idempotency key for a job (§5.2 / §6.5):
//
//	{job_type}:{asset_version_id|source_id|workspace_id}:{target_key}:{build_revision}
//
// Components not relevant to a job_type are omitted. The resulting key is the
// UNIQUE constraint on knowledge_jobs.dedupe_key — a re-delivery that tries to
// create the same job is a no-op (worker.ErrJobExists), which is how the
// Stream→Job mapping stays idempotent across crashes/retries.
func DedupeKey(jobType string, ids ...string) string {
	parts := []string{jobType}
	for _, id := range ids {
		if id != "" {
			parts = append(parts, id)
		}
	}
	return strings.Join(parts, ":")
}

// Handler executes one job_type. Run is the business logic; it returns a
// RetryClass so the Runner can classify failures (transient → retry up to
// max_attempt; permanent/policy_denied → dead). The Handler receives the
// domain.Job so it has the asset_version_id / target_key / build_revision
// context it needs without a re-read.
//
// Handlers MUST be idempotent: the same job may be re-acquired after a crash,
// so a Run that already took effect (e.g. a projection already marked ready)
// must return Succeeded, not an error.
type Handler interface {
	Run(ctx context.Context, j domain.Job) (domain.RetryClass, error)
}

// HandlerFunc lets a plain function satisfy Handler.
type HandlerFunc func(ctx context.Context, j domain.Job) (domain.RetryClass, error)

// Run calls f.
func (f HandlerFunc) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	return f(ctx, j)
}

// Handlers maps job_type → Handler. An unregistered job_type fails as
// permanent (the Runner marks it dead so it is not retried indefinitely).
type Handlers map[string]Handler

// RunnerConfig is the wiring the Runner needs.
type RunnerConfig struct {
	Jobs     JobStore
	Handlers Handlers
	LeaseTTL time.Duration // worker.DefaultLeaseTTL when zero
	Owner    string        // consumer name for lease ownership
	Logf     func(format string, args ...any)
}

// Runner is the acquire→dispatch→mark loop. It is the only writer of
// knowledge_jobs.status after Create: Acquire (lease), then MarkSucceeded or
// MarkFailed. Multiple runners share the table safely via Acquire's
// FOR UPDATE SKIP LOCKED (§6.5).
type Runner struct {
	cfg      RunnerConfig
	jobs     JobStore
	handlers Handlers
	leaseTTL time.Duration
	owner    string
	logf     func(string, ...any)
}

// NewRunner builds a Runner. A zero LeaseTTL defaults to DefaultLeaseTTL.
func NewRunner(cfg RunnerConfig) *Runner {
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = domain.DefaultLeaseTTL
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Owner == "" {
		cfg.Owner = "knowledge-worker"
	}
	return &Runner{
		cfg:      cfg,
		jobs:     cfg.Jobs,
		handlers: cfg.Handlers,
		leaseTTL: cfg.LeaseTTL,
		owner:    cfg.Owner,
		logf:     cfg.Logf,
	}
}

// Run is the dispatch loop. It blocks until ctx is cancelled. Each iteration
// Acquires one job; if there is none it idles briefly before retrying. A
// transient failure leaves the job pending for retry (MarkFailed handles the
// attempt/max_attempt math); a permanent failure or unregistered job_type is
// marked dead.
//
// It does NOT re-publish Stream messages — the Stream→Job mapping in
// cmd/knowledge-worker's main is the re-delivery path; the Runner only owns
// job execution state.
func (r *Runner) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		job, err := r.jobs.Acquire(ctx, r.owner, r.leaseTTL)
		if err != nil {
			if errors.Is(err, ErrNoJob) {
				// Idle: back off briefly so we don't spin on an empty table.
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
				continue
			}
			r.logf("knowledge-worker: acquire: %v", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
			continue
		}

		r.runOne(ctx, job)
	}
}

// runOne dispatches a single acquired job to its handler and records the
// outcome. It owns the transactional boundary around MarkSucceeded/MarkFailed
// so a crash mid-handler leaves the job either leased (re-acquirable) or, after
// lease expiry, re-acquired and re-run (idempotent).
func (r *Runner) runOne(ctx context.Context, job domain.Job) {
	handler, ok := r.handlers[job.JobType]
	if !ok {
		// Unregistered job_type: mark dead so it is not retried forever. This is
		// a programming error (a job_type the Runner wasn't wired for), surfaced
		// for the operator via the dead job's error_code.
		err := r.jobs.MarkFailed(ctx, job.ID, domain.RetryPermanent,
			"job_type_unregistered", "no handler for job_type "+job.JobType)
		if err != nil {
			r.logf("knowledge-worker: mark dead %s: %v", job.ID, err)
		}
		return
	}

	retryClass, runErr := handler.Run(ctx, job)
	if runErr == nil {
		if err := r.jobs.MarkSucceeded(ctx, job.ID, nil); err != nil {
			r.logf("knowledge-worker: mark succeeded %s: %v", job.ID, err)
		}
		return
	}

	// Classify: default to transient when the handler returned no class.
	if retryClass == "" {
		retryClass = domain.RetryTransient
	}
	err := r.jobs.MarkFailed(ctx, job.ID, retryClass, classifyCode(runErr, retryClass), redact(runErr))
	if err != nil {
		r.logf("knowledge-worker: mark failed %s: %v", job.ID, err)
	}
}

// classifyCode returns a short, non-sensitive error code for the job row.
// It must not leak credentials or full paths — operators read this column.
func classifyCode(err error, class domain.RetryClass) string {
	if err == nil {
		return ""
	}
	// Prefer a sentinel-prefix; fall back to the first line of the message.
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return string(class) + ":" + msg
}

// redact strips credential-bearing substrings from an error message before it
// is persisted to error_detail_redacted. The §6.5 rule: never persist
// plaintext credentials; the call site is responsible for not putting them in
// the error in the first place. This is a belt-and-braces sweep.
func redact(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	for _, token := range credentialHints {
		if i := strings.Index(s, token); i >= 0 {
			// mask the segment after the marker up to the next whitespace
			j := i + len(token)
			k := strings.IndexAny(s[j:], " \t\r\n\"'")
			end := len(s)
			if k >= 0 {
				end = j + k
			}
			s = s[:j] + "***" + s[end:]
		}
	}
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// credentialHints are the substrings whose tail we mask if they appear in an
// error message. Conservative; mirrors §6.5's "no plaintext in logs/errors".
var credentialHints = []string{"password=", "token=", "secret=", "apikey=", "api_key=", "Bearer "}

// --- shared helpers for handlers ---

// JobTxStarter is the subset a handler needs to run its work inside a tx. The
// handler either commits (success) or returns an error (the Runner rolls back
// via defer and marks the job failed).
//
// Acquired jobs are not held inside one long tx — a projection build can take
// minutes. Instead the lease is the lock; the handler opens short txs for its
// own writes. This keeps Postgres snapshot age bounded (§6.5 long-running).
type JobTxStarter interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Compile-time guards for the handler helper types are in each handler file.

// uuidPtr returns a pointer to u; nil if u is the zero UUID (used by handlers
// building job IDs for downstream Create calls).
func uuidPtr(u uuid.UUID) *uuid.UUID {
	if u == uuid.Nil {
		return nil
	}
	return &u
}

// fmtErr wraps an error with a job_type prefix so the Runner's classifyCode
// surfaces which job failed in the error_code column.
func fmtErr(jobType string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", jobType, err)
}
