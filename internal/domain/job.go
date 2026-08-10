package domain

import "time"

// JobStatus is the lifecycle state of a knowledge_jobs row (design-docs/12
// §6.5, 13 §5.4). Phase 0 builds the store + lease mechanics; concrete
// job_type processing arrives in Phase 1.
type JobStatus string

const (
	JobPending   JobStatus = "pending"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobDead      JobStatus = "dead"
	JobCancelled JobStatus = "cancelled"
)

// RetryClass is the retry disposition the worker assigns when marking a job
// failed (§6.5). transient → back off and retry (until max_attempt); permanent
// and policy_denied → no retry (policy_denied also writes an audit record).
type RetryClass string

const (
	RetryTransient    RetryClass = "transient"
	RetryPermanent    RetryClass = "permanent"
	RetryPolicyDenied RetryClass = "policy_denied"
)

// Job is the in-memory value object for a knowledge_jobs row (§6.5). It is the
// consumer-side idempotency + lease record: the Outbox guarantees the producer
// event is not lost; the Job guarantees consumption is idempotent, lease-safe,
// and observable. Phase 0 ships the store mechanics only — no job_type logic.
type Job struct {
	ID             UUID
	SourceEventID  *UUID
	JobType        string
	AssetID        *UUID
	AssetVersionID *UUID
	SourceID       *UUID
	TargetKey      string
	BuildRevision  string
	DedupeKey      string
	Status         JobStatus
	Attempt        int
	MaxAttempt     int
	LeaseOwner     string
	LeaseUntil     *time.Time
	Progress       map[string]any
	ErrorCode      string
	ErrorDetail    string // already redacted at the call site
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// LeaseTTL is how long a worker holds a job before it can be re-claimed by
// another. Phase 0 default; configurable per call.
const DefaultLeaseTTL = 5 * time.Minute
