// Package service implements the Wiki maintenance application service
// (design-docs/16 §3/§4/§5/§8). It owns Wiki Space CRUD, maintenance-run
// orchestration, the managed/locked/manual differentiated write strategy
// (§4.4), per-page CAS activation (§4.5), RBAC hard-filtering (§8.2), and the
// audit trail (§8.3). It does NOT run the provider model itself — the
// knowledge-worker consumes the wiki_events outbox event, calls the provider,
// and writes the resulting PagePatches back as wiki_page_proposals via this
// service. The service only enqueues the run + outbox event inside the user's
// transaction (§6.2 transactional consistency).
//
// Security invariants enforced here (mirroring the source service):
//   - Existence never leaks (§8.2): a read denial AND a genuinely missing
//     Wiki Space both surface as ErrWikiSpaceNotFound, so the handler emits
//     404 + 40400 indistinguishable from not-found. A write/governance
//     denial surfaces as ErrWikiForbidden → 403 + 40300.
//   - Locked-page protection (§4.4 / 门禁 "locked 页自动覆盖为 0"): a locked
//     page never receives a coverage candidate — the service filters the
//     provider's input to summaries-only, the schema gate rejects update/create
//     actions for locked pages, and the CAS layer hard-refuses
//     is_bypass=false on a locked page (three-way guard, decision D3).
//   - Prompt-injection scope (§8.1): the source_versions set passed to the
//     provider is fixed before the call; the provider cannot widen its read.
package service

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
)

// Sentinel errors (mirrors the source-service no-leak contract). Repositories
// and the service return these so the handler maps them to the §11.4 envelope
// without leaking existence (§8.2).
var (
	// ErrWikiSpaceNotFound: a missing/unreadable Wiki Space → 404 + 40400. A
	// read denial ALSO maps here so existence never leaks (§8.2).
	ErrWikiSpaceNotFound = errors.New("wiki: space not found")
	// ErrWikiPageNotFound: a missing/unreadable page → 404 + 40400.
	ErrWikiPageNotFound = errors.New("wiki: page not found")
	// ErrWikiRunNotFound: a missing/unreadable maintenance run → 404 + 40400.
	ErrWikiRunNotFound = errors.New("wiki: run not found")
	// ErrWikiProposalNotFound: a missing/unreadable proposal → 404 + 40400.
	ErrWikiProposalNotFound = errors.New("wiki: proposal not found")
	// ErrWikiConflict: a name/etag/idempotency conflict → 409 + 40900.
	ErrWikiConflict = errors.New("wiki: conflict")
	// ErrWikiIdempotentRetry: same idempotency_key replayed — not an error,
	// the service returns the existing run.
	ErrWikiIdempotentRetry = errors.New("wiki: idempotent retry")
	// ErrWikiForbidden: a write/governance RBAC denial → 403 + 40300.
	ErrWikiForbidden = errors.New("wiki: forbidden")
	// ErrWikiSchemaViolation: a PagePatch failed the JSON Schema gate
	// (§4.2) — the run is marked failed, no candidate landed.
	ErrWikiSchemaViolation = errors.New("wiki: page patch failed schema gate")
	// ErrWikiLockedPageCovered: a coverage candidate (is_bypass=false) was
	// attempted on a locked page (§4.4 three-way guard, CAS layer). The CAS
	// hard-refuses and the attempt is audited as wiki.lock.
	ErrWikiLockedPageCovered = errors.New("wiki: locked page cannot be covered")
)

// AutomationState enumerates the page write strategies (§4.4).
type AutomationState string

const (
	AutomationManaged AutomationState = "managed"
	AutomationLocked   AutomationState = "locked"
	AutomationManual   AutomationState = "manual"
)

// PageKind enumerates the page categories (§2.2 wiki_pages.page_kind).
type PageKind string

// TriggerType enumerates the maintenance-run triggers (§2.4).
type TriggerType string

const (
	TriggerIngest    TriggerType = "ingest"
	TriggerQueryFile TriggerType = "query_file"
	TriggerLint      TriggerType = "lint"
	TriggerManual    TriggerType = "manual"
)

// AuthContext carries the caller identity needed for RBAC + audit (mirrors
// source/service.AuthContext). IsAdmin short-circuits the Check. An agent
// acting on behalf of a user records the user as the RBAC subject.
type AuthContext struct {
	SubjectType     domain.SubjectType
	PrincipalID     uuid.UUID
	GroupIDs        []uuid.UUID
	IsAdmin         bool
	IsServiceCaller bool
}

// WikiSpace is the in-memory projection of a wiki_spaces row (§2.1).
type WikiSpace struct {
	ID                 uuid.UUID
	WorkspaceID        uuid.UUID
	Name               string
	SchemaAssetID      uuid.UUID
	SchemaVersionID    uuid.UUID
	IndexAssetID       *uuid.UUID
	LogAssetID         *uuid.UUID
	GovernanceProfileID uuid.UUID
	MaintenancePolicy  map[string]any
	Status             string
	CreatedByType      domain.SubjectType
	CreatedByID        uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// WikiPage is the in-memory projection of a wiki_pages row (§2.2).
type WikiPage struct {
	WikiSpaceID      uuid.UUID
	DocumentAssetID  uuid.UUID
	PageKey          string
	PageKind         string
	AutomationState  AutomationState
	LastMaintainedAt *time.Time
	StaleReason      string
	StaleSince       *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// MaintenanceRun is the in-memory projection of a wiki_maintenance_runs row
// (§2.4).
type MaintenanceRun struct {
	ID                  uuid.UUID
	WikiSpaceID         uuid.UUID
	TriggerType         TriggerType
	SchemaVersionID     uuid.UUID
	InputSetHash        string
	ModelRevision       string
	PromptRevision      string
	RequestedByType     domain.SubjectType
	RequestedByID       uuid.UUID
	AnswerRef           map[string]any
	Status              string
	ProposalManifest    map[string]any
	IdempotencyKey      string
	StartedAt           *time.Time
	FinishedAt          *time.Time
	ErrorCode           string
	ErrorDetailRedacted string
	CreatedAt           time.Time
}

// PageProposal is the in-memory projection of a wiki_page_proposals row (§2.4).
type PageProposal struct {
	ID                 uuid.UUID
	RunID              uuid.UUID
	WikiSpaceID        uuid.UUID
	PageKey            string
	PageAssetID        *uuid.UUID
	ExpectedVersionID  *uuid.UUID
	ProposedVersionID  *uuid.UUID
	Action             string
	IsBypass           bool
	ContentHash        string
	RelationSuggestions []map[string]any
	Status             string
	ReviewRequestID    *uuid.UUID
	AppliedAt          *time.Time
	ErrorDetailRedacted string
	CreatedAt          time.Time
}

// CreateWikiSpaceInput is the create-Space payload (§7.1 POST /wiki-spaces).
type CreateWikiSpaceInput struct {
	WorkspaceID         uuid.UUID
	Name                string
	SchemaAssetID       uuid.UUID
	SchemaVersionID     uuid.UUID
	GovernanceProfileID uuid.UUID
	MaintenancePolicy   map[string]any
	CreatedByType       domain.SubjectType
	CreatedByID         uuid.UUID
}

// TriggerRunInput is the trigger-maintenance-run payload (§7.1 POST
// /maintenance-runs).
type TriggerRunInput struct {
	WikiSpaceID     uuid.UUID
	Trigger         TriggerType
	PageKey         string
	AnswerRef       map[string]any
	CheckKinds      []string
	RequestedByType domain.SubjectType
	RequestedByID   uuid.UUID
	// IdempotencyKey lets the caller replay the same trigger safely (§4.2).
	IdempotencyKey string
}

// ApplyProposalInput carries the per-proposal review decision (§7.1 POST
// /proposals/:id).
type ApplyProposalInput struct {
	ProposalID uuid.UUID
	Decision   string // approve|reject
	Rationale  string
	Auth       AuthContext
}

// WikiRepo is the persistence port over the wiki_* tables. It is the only
// write path; the authz locator reads via the same lookups so existence does
// not leak. All methods take a pgx.Tx so the run + proposals + outbox event
// commit atomically (§6.2).
type WikiRepo interface {
	// --- wiki_spaces ---
	CreateSpace(ctx context.Context, tx pgx.Tx, sp *WikiSpace) error
	GetSpace(ctx context.Context, id uuid.UUID) (*WikiSpace, error)
	ListSpaces(ctx context.Context, workspaceID uuid.UUID, page, pageSize int) ([]*WikiSpace, int, error)
	// --- wiki_maintenance_runs ---
	CreateRun(ctx context.Context, tx pgx.Tx, run *MaintenanceRun) error
	GetRun(ctx context.Context, id uuid.UUID) (*MaintenanceRun, error)
	ListRuns(ctx context.Context, spaceID uuid.UUID, status string, page, pageSize int) ([]*MaintenanceRun, int, error)
	UpdateRunStatus(ctx context.Context, id uuid.UUID, status, errorCode, errorDetail string) error
	// --- wiki_page_proposals ---
	CreateProposals(ctx context.Context, tx pgx.Tx, proposals []*PageProposal) error
	GetProposal(ctx context.Context, id uuid.UUID) (*PageProposal, error)
	ListProposals(ctx context.Context, spaceID uuid.UUID, pageKey, status string) ([]*PageProposal, error)
	UpdateProposalStatus(ctx context.Context, id uuid.UUID, status string, proposedVersionID *uuid.UUID, reviewRequestID *uuid.UUID) error
	// --- wiki_pages ---
	ListPages(ctx context.Context, spaceID uuid.UUID) ([]*WikiPage, error)
	// ApplyProposalCAS runs the §4.5 per-page CAS: the proposal must be
	// 'approved', is_bypass=false, and the asset's current_version_id must
	// match expected_version_id. On success flips current_version_id +
	// latest_requested_version_no, marks the proposal 'applied', clears the
	// page's stale_reason, and returns the page's automation_state so the
	// caller can audit locked-page coverage attempts.
	ApplyProposalCAS(ctx context.Context, tx pgx.Tx, proposalID uuid.UUID) (automation AutomationState, activated bool, err error)
}

// SpaceSink is the transactional double-write port for Wiki Space + Run
// creation (§6.2): the wiki_spaces / wiki_maintenance_runs row and its wiki_events
// outbox event commit in ONE transaction so the event is never lost relative
// to the state change. Mirrors source/service.SyncRunSink.
type SpaceSink interface {
	// CreateSpaceWithEvent inserts the space + records the outbox event in one tx.
	CreateSpaceWithEvent(ctx context.Context, sp *WikiSpace, ev domain.KnowledgeEvent) error
	// CreateRunWithEvent inserts the run + records the outbox event in one tx.
	CreateRunWithEvent(ctx context.Context, run *MaintenanceRun, ev domain.KnowledgeEvent) error
}

// MaintenanceProvider is the provider port (§4.1). The service holds it to
// forward run execution to the worker-driven path; the actual call happens in
// the knowledge-worker handler, which has the provider wired with the model
// adapter. The service declares the port so its unit tests can substitute a
// fake without importing the concrete provider package.
type MaintenanceProvider interface {
	// ExecuteRun is the worker entry: given a queued run, compute the
	// affected pages + authorized source versions, call the provider, validate
	// the PagePatches, and land them as proposals (§4.3). Returns the proposal
	// ids written. The service delegates to this from the worker handler.
	ExecuteRun(ctx context.Context, runID uuid.UUID) ([]uuid.UUID, error)
}
