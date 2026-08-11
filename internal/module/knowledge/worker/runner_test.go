// Package worker — runner tests (no DB). These verify the dispatch +
// retry-classification logic that is the Runner's core: acquire→run→mark with
// the §5.2 retry classes (transient retries until max_attempt; permanent /
// policy_denied → dead). The JobStore is a fake so the test is hermetic.
package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lynn901/mora/internal/domain"
)

// fakeJobStore is an in-memory worker.JobStore for runner tests. It records
// every call so the test can assert the dispatch outcome without a DB.
type fakeJobStore struct {
	mu      sync.Mutex
	jobs    map[uuid.UUID]domain.Job
	owner   string
	ttl     time.Duration
	succeeded []uuid.UUID
	failed    map[uuid.UUID]fakeFail
}

type fakeFail struct {
	class  domain.RetryClass
	code   string
	detail string
}

func newFakeStore(jobs ...domain.Job) *fakeJobStore {
	m := make(map[uuid.UUID]domain.Job, len(jobs))
	for _, j := range jobs {
		if j.Status == "" {
			j.Status = domain.JobPending
		}
		m[j.ID] = j
	}
	return &fakeJobStore{jobs: m, failed: map[uuid.UUID]fakeFail{}}
}

func (s *fakeJobStore) Create(ctx context.Context, tx pgx.Tx, j domain.Job) (domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[j.ID]; ok {
		return domain.Job{}, ErrJobExists
	}
	s.jobs[j.ID] = j
	return j, nil
}

func (s *fakeJobStore) Acquire(ctx context.Context, owner string, ttl time.Duration) (domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.Status == domain.JobPending {
			j.Status = domain.JobRunning
			j.LeaseOwner = owner
			t := time.Now().UTC().Add(ttl)
			j.LeaseUntil = &t
			s.jobs[j.ID] = j
			s.owner = owner
			s.ttl = ttl
			return j, nil
		}
	}
	return domain.Job{}, ErrNoJob
}

func (s *fakeJobStore) Renew(ctx context.Context, id uuid.UUID, owner string, ttl time.Duration) (time.Time, error) {
	return time.Now().Add(ttl), nil
}

func (s *fakeJobStore) Release(ctx context.Context, id uuid.UUID, owner string) error { return nil }

func (s *fakeJobStore) MarkSucceeded(ctx context.Context, id uuid.UUID, progress map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	j.Status = domain.JobSucceeded
	s.jobs[id] = j
	s.succeeded = append(s.succeeded, id)
	return nil
}

func (s *fakeJobStore) MarkFailed(ctx context.Context, id uuid.UUID, class domain.RetryClass, code, redactedDetail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[id]
	switch {
	case class == domain.RetryTransient && j.Attempt+1 < j.MaxAttempt:
		j.Status = domain.JobPending // retry
	default:
		j.Status = domain.JobDead
	}
	j.Attempt++
	j.ErrorCode = code
	j.ErrorDetail = redactedDetail
	s.jobs[id] = j
	s.failed[id] = fakeFail{class: class, code: code, detail: redactedDetail}
	return nil
}

func (s *fakeJobStore) Get(ctx context.Context, id uuid.UUID) (domain.Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return domain.Job{}, errors.New("not found")
	}
	return j, nil
}

// Compile-time check: the fake satisfies JobStore.
var _ JobStore = (*fakeJobStore)(nil)

// runOnce runs the runner until every job reaches a terminal status (succeeded
// or dead) then cancels. The real Runner.Run blocks until ctx cancel; the test
// cancels once the store has no pending jobs left.
func runOnce(t *testing.T, r *Runner, store *fakeJobStore) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			time.Sleep(5 * time.Millisecond)
			store.mu.Lock()
			allTerminal := true
			for _, j := range store.jobs {
				if j.Status != domain.JobSucceeded && j.Status != domain.JobDead {
					allTerminal = false
					break
				}
			}
			store.mu.Unlock()
			if allTerminal {
				cancel()
				return
			}
		}
	}()
	_ = r.Run(ctx)
}

// TestRunner_MarksSucceededOnHandlerSuccess: a handler returning (nil, nil) →
// the job is marked succeeded.
func TestRunner_MarksSucceededOnHandlerSuccess(t *testing.T) {
	jid := uuid.New()
	store := newFakeStore(domain.Job{ID: jid, JobType: JobAssetActivate, MaxAttempt: 5})
	r := NewRunner(RunnerConfig{
		Jobs:     store,
		Handlers: Handlers{JobAssetActivate: HandlerFunc(func(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
			return "", nil // success
		})},
		LeaseTTL: time.Second,
		Owner:    "test",
	})
	runOnce(t, r, store)

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.succeeded) != 1 || store.succeeded[0] != jid {
		t.Fatalf("expected 1 succeeded, got %+v", store.succeeded)
	}
	if store.jobs[jid].Status != domain.JobSucceeded {
		t.Fatalf("job status = %s, want succeeded", store.jobs[jid].Status)
	}
}

// TestRunner_TransientFailureRetriesUntilMaxThenDead: a handler that always
// fails transiently should leave the job pending until attempt reaches
// max_attempt, then dead.
func TestRunner_TransientFailureRetriesUntilMaxThenDead(t *testing.T) {
	jid := uuid.New()
	store := newFakeStore(domain.Job{ID: jid, JobType: JobProjectionBuild, MaxAttempt: 3})
	// Handler always fails transiently.
	failing := HandlerFunc(func(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
		return domain.RetryTransient, errors.New("boom")
	})
	r := NewRunner(RunnerConfig{
		Jobs: store, Handlers: Handlers{JobProjectionBuild: failing},
		LeaseTTL: time.Second, Owner: "test",
	})
	// Run the runner in a loop: the job re-enters pending after each transient
	// failure, so Acquire picks it again until MaxAttempt is reached.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		for {
			store.mu.Lock()
			st := store.jobs[jid].Status
			store.mu.Unlock()
			if st == domain.JobDead {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	_ = r.Run(ctx)

	store.mu.Lock()
	defer store.mu.Unlock()
	j := store.jobs[jid]
	if j.Status != domain.JobDead {
		t.Fatalf("status = %s, want dead after max_attempt retries", j.Status)
	}
	if j.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3", j.Attempt)
	}
}

// TestRunner_PermanentFailureMarksDeadImmediately: a permanent failure must not
// retry — the job is dead on the first attempt (§6.5).
func TestRunner_PermanentFailureMarksDeadImmediately(t *testing.T) {
	jid := uuid.New()
	store := newFakeStore(domain.Job{ID: jid, JobType: JobAssetActivate, MaxAttempt: 5})
	r := NewRunner(RunnerConfig{
		Jobs: store,
		Handlers: Handlers{JobAssetActivate: HandlerFunc(func(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
			return domain.RetryPermanent, errors.New("nope")
		})},
		LeaseTTL: time.Second, Owner: "test",
	})
	runOnce(t, r, store)

	store.mu.Lock()
	defer store.mu.Unlock()
	j := store.jobs[jid]
	if j.Status != domain.JobDead {
		t.Fatalf("status = %s, want dead (permanent must not retry)", j.Status)
	}
	if j.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 (no retry on permanent)", j.Attempt)
	}
}

// TestRunner_UnregisteredJobTypeMarkedDead: a job whose job_type has no handler
// is marked permanent/dead so it is not retried forever.
func TestRunner_UnregisteredJobTypeMarkedDead(t *testing.T) {
	jid := uuid.New()
	store := newFakeStore(domain.Job{ID: jid, JobType: "unknown_type", MaxAttempt: 5})
	r := NewRunner(RunnerConfig{
		Jobs:     store,
		Handlers: Handlers{}, // no handlers registered
		LeaseTTL: time.Second, Owner: "test",
	})
	runOnce(t, r, store)

	store.mu.Lock()
	defer store.mu.Unlock()
	j := store.jobs[jid]
	if j.Status != domain.JobDead {
		t.Fatalf("status = %s, want dead for unregistered job_type", j.Status)
	}
	if !contains(j.ErrorCode, "job_type_unregistered") {
		t.Fatalf("error code = %q, want it to mention job_type_unregistered", j.ErrorCode)
	}
}

// TestDedupeKey_FormatsPerJobType: the dedupe key shape matches §5.2 for each
// job_type (the UNIQUE constraint on knowledge_jobs.dedupe_key relies on it).
func TestDedupeKey_FormatsPerJobType(t *testing.T) {
	cases := []struct {
		name   string
		job    string
		args   []string
		want   string
	}{
		{"source_sync", JobSourceSync, []string{"src-1", "latest"}, "source_sync:src-1:latest"},
		{"projection_build", JobProjectionBuild, []string{"ver-1", "fts", "rev-2"}, "projection_build:ver-1:fts:rev-2"},
		{"asset_activate", JobAssetActivate, []string{"ver-1"}, "asset_activate:ver-1"},
		{"reconcile_scan", JobReconcileScan, []string{"ws-1", "full"}, "reconcile_scan:ws-1:full"},
		{"empty_components_omitted", JobSourceSync, []string{"src-1", "", ""}, "source_sync:src-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DedupeKey(c.job, c.args...)
			if got != c.want {
				t.Fatalf("DedupeKey(%q, %v) = %q, want %q", c.job, c.args, got, c.want)
			}
		})
	}
}

// TestRedact_MasksCredentialHints: the §6.5 belt-and-braces sweep masks
// credential-bearing substrings in error messages before persistence.
func TestRedact_MasksCredentialHints(t *testing.T) {
	in := errors.New(`fetch failed: password=hunter2 and token=abc123 for Bearer xyz`)
	got := redact(in)
	if contains(got, "hunter2") || contains(got, "abc123") || contains(got, "xyz") {
		t.Fatalf("redact() leaked a credential: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && containsAny(s, sub)))
}

func containsAny(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
