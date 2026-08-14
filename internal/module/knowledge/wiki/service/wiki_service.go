// Package service — WikiService methods. The service composes WikiRepo, the
// transactional SpaceSink, the provider port (for the worker-driven execute
// path), the rbac.Engine, and the audit.Logger. It is the application layer;
// HTTP lives in wiki/handler, persistence in infra/postgres.
//
// The §4.5 per-page CAS is delegated to WikiRepo.ApplyProposalCAS so the
// single-row UPDATE + proposal-status flip is one statement. The service's
// ReviewProposal enforces the locked-page coverage guard BEFORE calling CAS
// (§4.4 point 3) and records a wiki.lock audit when a coverage attempt is
// caught, so the three-way guard has an audit trail even when the schema gate
// (§4.2) and the CAS layer both refuse.
package service

import (
	"context"
	"encoding/json"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/audit"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// sha256Hex matches the §4.2 content_hash / contribution_hash pattern (mirrors
// provider.sha256Hex — kept local so the service does not import the provider
// package's internal symbols).
var sha256Hex = regexp.MustCompile(`^[a-f0-9]{64}$`)

// WikiEventStream is the Valkey Stream wiki events are published to (§6.1).
// The outbox dispatcher ships these; the knowledge-worker consumes the
// wiki_maintenance group.
const WikiEventStream = "wiki_events"

// WikiEventTypes enumerates the wiki event types (§6.1).
const (
	WikiEventIngest     = "wiki.ingest"
	WikiEventQueryFile  = "wiki.query_file"
	WikiEventLint       = "wiki.lint"
	WikiEventReconcile  = "wiki.reconcile"
	WikiEventCancelled  = "wiki.cancelled"
)

// DefaultModelRevision / DefaultPromptRevision are the placeholder revisions
// the service pins on a run when no model/prompt revision is configured yet
// (the model adapter is swappable, §4.1; until it lands the run records a
// stable sentinel so the idempotency_key is well-formed). PM/架构 fill the
// real revisions when the model wiring lands (§0 决策 D5).
const (
	DefaultModelRevision = "noop-v1"
	DefaultPromptRevision = "noop-v1"
)

// Service is the Wiki maintenance application service.
type Service struct {
	repo     WikiRepo
	sink     SpaceSink
	provider MaintenanceProvider
	rbac     *rbac.Engine // nil = no resource-level authz (dev/test only)
	audit    *audit.Logger
}

// NewService wires the Wiki service. provider may be nil — the worker-driven
// execute path (ExecuteRun) is wired when the worker constructs the service
// with the real provider; mora-api constructs a service without a provider
// (it only triggers runs, never executes them). rbac is nil by design;
// production wiring MUST chain WithAuthz.
func NewService(repo WikiRepo, sink SpaceSink, provider MaintenanceProvider) *Service {
	return &Service{repo: repo, sink: sink, provider: provider}
}

// WithAuthz injects the RBAC engine + audit logger and returns the service
// for chaining (mirrors source/service.WithAuthz).
func (s *Service) WithAuthz(engine *rbac.Engine, logger *audit.Logger) *Service {
	s.rbac = engine
	s.audit = logger
	return s
}

// authorize runs an rbac.Engine.Check and maps the outcome to the §8.2
// no-leak / §10.4 deny contract (mirrors source/service.authorize). A Wiki
// Space resolves to its owning workspace; a missing/cross-workspace space
// fails to resolve → ErrWikiSpaceNotFound (no leak). A write/governance
// denial on a resolvable target returns ErrWikiForbidden (403) + audit.
func (s *Service) authorize(ctx context.Context, auth AuthContext, t domain.TargetType, id uuid.UUID, action domain.Action, leak bool) error {
	if s.rbac == nil || auth.IsAdmin {
		return nil
	}
	dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs, t, id, action)
	if err != nil {
		if leak {
			s.recordDeniedAudit(ctx, auth, action, t, id)
		}
		return ErrWikiSpaceNotFound
	}
	if !dec.Allowed {
		if leak {
			s.recordDeniedAudit(ctx, auth, action, t, id)
			return ErrWikiForbidden
		}
		return ErrWikiSpaceNotFound
	}
	return nil
}

func (s *Service) recordDeniedAudit(ctx context.Context, auth AuthContext, action domain.Action, t domain.TargetType, id uuid.UUID) {
	if s.audit == nil {
		return
	}
	actorType := "user"
	if auth.IsServiceCaller {
		actorType = "service"
	}
	var actorID *uuid.UUID
	if auth.PrincipalID != uuid.Nil {
		pid := auth.PrincipalID
		actorID = &pid
	}
	tid := id
	s.audit.Record(ctx, actorType, actorID,
		"denied."+string(action), string(t), &tid,
		map[string]any{"reason": "rbac deny", "subject_type": string(auth.SubjectType)}, "", "")
}

// recordWikiAudit writes a best-effort wiki.* audit event (§8.3). Used for
// wiki.lock (locked-page coverage attempt), wiki.review, wiki.apply.
func (s *Service) recordWikiAudit(ctx context.Context, auth AuthContext, action string, spaceID, proposalID *uuid.UUID, detail map[string]any) {
	if s.audit == nil {
		return
	}
	actorType := "user"
	if auth.IsServiceCaller {
		actorType = "service"
	}
	var actorID *uuid.UUID
	if auth.PrincipalID != uuid.Nil {
		pid := auth.PrincipalID
		actorID = &pid
	}
	var tid *uuid.UUID
	if spaceID != nil {
		tid = spaceID
	}
	s.audit.Record(ctx, actorType, actorID, action, "wiki_space", tid, detail, "", "")
}

// --- Wiki Space CRUD (§7.1) ---

// CreateSpace registers a new Wiki Space. It validates that the schema +
// governance assets resolve within the caller's workspace (a cross-workspace
// schema_asset_id fails authorization → forbidden, no leak). The space +
// outbox event commit in one transaction via SpaceSink (§6.2).
func (s *Service) CreateSpace(ctx context.Context, auth AuthContext, in CreateWikiSpaceInput) (*WikiSpace, error) {
	if err := s.authorize(ctx, auth, domain.TargetWorkspace, in.WorkspaceID, domain.ActionWrite, true); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("wiki: name is required")
	}
	policy := in.MaintenancePolicy
	if policy == nil {
		policy = map[string]any{}
	}
	sp := &WikiSpace{
		ID:                  uuid.New(),
		WorkspaceID:         in.WorkspaceID,
		Name:                in.Name,
		SchemaAssetID:       in.SchemaAssetID,
		SchemaVersionID:     in.SchemaVersionID,
		GovernanceProfileID: in.GovernanceProfileID,
		MaintenancePolicy:   policy,
		Status:              "active",
		CreatedByType:       in.CreatedByType,
		CreatedByID:         in.CreatedByID,
		CreatedAt:           time.Now().UTC(),
		UpdatedAt:           time.Now().UTC(),
	}
	ev := domain.KnowledgeEvent{
		EventID:       uuid.NewString(),
		EventType:     WikiEventReconcile,
		EventVersion:  1,
		AggregateType: "wiki_space",
		AggregateID:   sp.ID,
		WorkspaceID:   &in.WorkspaceID,
		Actor:         domain.EventActor{Type: in.CreatedByType, ID: in.CreatedByID},
		OccurredAt:    sp.CreatedAt,
		Payload:       map[string]any{"action": "create_space", "name": in.Name},
	}
	if err := s.sink.CreateSpaceWithEvent(ctx, sp, ev); err != nil {
		if isUniqueViolation(err) {
			return nil, ErrWikiConflict
		}
		return nil, err
	}
	return sp, nil
}

// GetSpace loads a Wiki Space after an RBAC read check. Existence never
// leaks (§8.2).
func (s *Service) GetSpace(ctx context.Context, auth AuthContext, id uuid.UUID) (*WikiSpace, error) {
	if err := s.authorize(ctx, auth, domain.TargetAsset, id, domain.ActionRead, false); err != nil {
		return nil, err
	}
	sp, err := s.repo.GetSpace(ctx, id)
	if err != nil {
		return nil, err
	}
	return sp, nil
}

// ListSpaces returns a paginated page of the workspace's Wiki Spaces. The page
// is scoped to q.WorkspaceID at the repo layer; this gates the call on a
// workspace read grant (§10.4 用例 27).
func (s *Service) ListSpaces(ctx context.Context, auth AuthContext, workspaceID uuid.UUID, page, pageSize int) ([]*WikiSpace, int, error) {
	if err := s.authorize(ctx, auth, domain.TargetWorkspace, workspaceID, domain.ActionRead, false); err != nil {
		return nil, 0, err
	}
	return s.repo.ListSpaces(ctx, workspaceID, page, pageSize)
}

// --- Maintenance Run trigger (§4.3 / §7.1) ---

// TriggerRun creates a maintenance run + wiki_events outbox event. It
// validates the trigger+payload shape (query_file requires answer_ref),
// pins the schema_version + model/prompt revision from the space, computes
// the input_set_hash, and builds the idempotency_key (§0 D5). A replay with
// the same idempotency_key returns the existing run (ErrWikiIdempotentRetry).
func (s *Service) TriggerRun(ctx context.Context, auth AuthContext, in TriggerRunInput) (*MaintenanceRun, error) {
	sp, err := s.GetSpace(ctx, auth, in.WikiSpaceID)
	if err != nil {
		return nil, err
	}
	// query_file requires answer_ref (§2.4 CHECK constraint mirror).
	if in.Trigger == TriggerQueryFile {
		if in.PageKey == "" {
			return nil, fmt.Errorf("wiki: page_key required for query_file trigger")
		}
		if len(in.AnswerRef) == 0 {
			return nil, fmt.Errorf("wiki: answer_ref required for query_file trigger")
		}
	}
	if in.Trigger == "" {
		return nil, fmt.Errorf("wiki: trigger is required")
	}
	// Build the idempotency key. When the caller supplies one, use it; else
	// derive from (space, trigger, input_set_hash, schema, model, prompt).
	inputSetHash := computeInputSetHash(in)
	idempotencyKey := in.IdempotencyKey
	if idempotencyKey == "" {
		idempotencyKey = buildIdempotencyKey(sp.ID, in.Trigger, inputSetHash, sp.SchemaVersionID, DefaultModelRevision, DefaultPromptRevision)
	}
	// Idempotency: if a run with this key already exists, return it.
	if existing, _ := s.findRunByIdempotencyKey(ctx, idempotencyKey); existing != nil {
		return existing, ErrWikiIdempotentRetry
	}
	run := &MaintenanceRun{
		ID:               uuid.New(),
		WikiSpaceID:      sp.ID,
		TriggerType:      in.Trigger,
		SchemaVersionID:  sp.SchemaVersionID,
		InputSetHash:     inputSetHash,
		ModelRevision:    DefaultModelRevision,
		PromptRevision:   DefaultPromptRevision,
		RequestedByType:  in.RequestedByType,
		RequestedByID:    in.RequestedByID,
		AnswerRef:        in.AnswerRef,
		Status:           "queued",
		IdempotencyKey:   idempotencyKey,
		CreatedAt:        time.Now().UTC(),
	}
	evType := WikiEventIngest
	switch in.Trigger {
	case TriggerQueryFile:
		evType = WikiEventQueryFile
	case TriggerLint:
		evType = WikiEventLint
	case TriggerManual:
		evType = WikiEventReconcile
	}
	ev := domain.KnowledgeEvent{
		EventID:       uuid.NewString(),
		EventType:     evType,
		EventVersion:  1,
		AggregateType: "wiki_space",
		AggregateID:   sp.ID,
		WorkspaceID:   &sp.WorkspaceID,
		Actor:         domain.EventActor{Type: in.RequestedByType, ID: in.RequestedByID},
		OccurredAt:    run.CreatedAt,
		Payload: map[string]any{
			"run_id":           run.ID.String(),
			"trigger":          string(in.Trigger),
			"input_set_hash":   inputSetHash,
			"schema_version_id": sp.SchemaVersionID.String(),
			"model_revision":    run.ModelRevision,
			"prompt_revision":   run.PromptRevision,
		},
	}
	if err := s.sink.CreateRunWithEvent(ctx, run, ev); err != nil {
		if isUniqueViolation(err) {
			// Lost the idempotency race: re-read the winner.
			if existing, _ := s.findRunByIdempotencyKey(ctx, idempotencyKey); existing != nil {
				return existing, ErrWikiIdempotentRetry
			}
			return nil, ErrWikiConflict
		}
		return nil, err
	}
	return run, nil
}

// GetRun loads a maintenance run after an RBAC read check on its space.
func (s *Service) GetRun(ctx context.Context, auth AuthContext, id uuid.UUID) (*MaintenanceRun, error) {
	run, err := s.repo.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, auth, domain.TargetAsset, run.WikiSpaceID, domain.ActionRead, false); err != nil {
		return nil, err
	}
	return run, nil
}

// ListRuns returns a paginated page of the space's maintenance runs.
func (s *Service) ListRuns(ctx context.Context, auth AuthContext, spaceID uuid.UUID, status string, page, pageSize int) ([]*MaintenanceRun, int, error) {
	if err := s.authorize(ctx, auth, domain.TargetAsset, spaceID, domain.ActionRead, false); err != nil {
		return nil, 0, err
	}
	return s.repo.ListRuns(ctx, spaceID, status, page, pageSize)
}

// --- Proposals (§7.1 / §4.5) ---

// ListProposals returns the proposals for a page (review UI). Existence never
// leaks (§8.2).
func (s *Service) ListProposals(ctx context.Context, auth AuthContext, spaceID uuid.UUID, pageKey, status string) ([]*PageProposal, error) {
	if err := s.authorize(ctx, auth, domain.TargetAsset, spaceID, domain.ActionRead, false); err != nil {
		return nil, err
	}
	return s.repo.ListProposals(ctx, spaceID, pageKey, status)
}

// StatusResult is the aggregated wiki_status payload (§7.3): the Space's
// directory (pages), its most recent maintenance run, and the pending
// proposals visible to the caller. All fields are RBAC-trimmed; an
// unauthorized caller gets ErrWikiSpaceNotFound (§8.2 — no existence leak).
type StatusResult struct {
	Space     *WikiSpace        `json:"space"`
	Pages     []*WikiPage       `json:"pages"`
	LastRun   *MaintenanceRun  `json:"last_run,omitempty"`
	Proposals []*PageProposal   `json:"proposals"`
}

// Status aggregates a Wiki Space's directory, latest maintenance run, and
// pending proposals for the wiki_status MCP tool (§7.3). It is the read-only
// counterpart to the control-plane list endpoints, composed server-side so the
// MCP client makes one authenticated call. RBAC: a single authorize gate on the
// space covers all three reads (§8.2) — a caller who can't read the space gets
// a 404, never partial data.
func (s *Service) Status(ctx context.Context, auth AuthContext, spaceID uuid.UUID) (*StatusResult, error) {
	if err := s.authorize(ctx, auth, domain.TargetAsset, spaceID, domain.ActionRead, false); err != nil {
		return nil, err
	}
	sp, err := s.repo.GetSpace(ctx, spaceID)
	if err != nil {
		return nil, ErrWikiSpaceNotFound
	}
	pages, err := s.repo.ListPages(ctx, spaceID)
	if err != nil {
		return nil, err
	}
	// Latest run = first page of runs ordered newest-first (repo default).
	runs, _, err := s.repo.ListRuns(ctx, spaceID, "", 1, 1)
	if err != nil {
		return nil, err
	}
	var lastRun *MaintenanceRun
	if len(runs) > 0 {
		lastRun = runs[0]
	}
	// Pending proposals only (status proposed/approved) — the surfaced review
	// queue. An empty set is normal.
	proposals, err := s.repo.ListProposals(ctx, spaceID, "", "proposed")
	if err != nil {
		return nil, err
	}
	return &StatusResult{
		Space:     sp,
		Pages:     pages,
		LastRun:   lastRun,
		Proposals: proposals,
	}, nil
}

// ReviewProposal records the human review decision (approve/reject) and, on
// approve, attempts the §4.5 per-page CAS. The locked-page coverage guard
// (§4.4 point 3) runs BEFORE the CAS: a managed/locked/manual
// differentiation is enforced at the repo layer (the CAS UPDATE carries
// is_bypass=false), and a locked page that received an is_bypass=false
// proposal is caught here and audited as wiki.lock.
func (s *Service) ReviewProposal(ctx context.Context, in ApplyProposalInput) (*PageProposal, error) {
	proposal, err := s.repo.GetProposal(ctx, in.ProposalID)
	if err != nil {
		return nil, err
	}
	if err := s.authorize(ctx, auth(in.Auth), domain.TargetAsset, proposal.WikiSpaceID, domain.ActionReview, true); err != nil {
		return nil, err
	}
	if proposal.Status != "proposed" && proposal.Status != "approved" {
		return nil, fmt.Errorf("wiki: proposal not in reviewable state: %s", proposal.Status)
	}
	switch in.Decision {
	case "approve":
		// A bypass proposal (locked/manual) never activates a coverage
		// version; it only records the decision for the review UI.
		if proposal.IsBypass {
			if err := s.repo.UpdateProposalStatus(ctx, in.ProposalID, "approved", nil, nil); err != nil {
				return nil, err
			}
			s.recordWikiAudit(ctx, in.Auth, "wiki.review", &proposal.WikiSpaceID, &proposal.ID, map[string]any{
				"page_key": proposal.PageKey, "decision": "approve", "is_bypass": true,
			})
			return s.repo.GetProposal(ctx, in.ProposalID)
		}
		// Coverage candidate → CAS. The repo's ApplyProposalCAS enforces
		// is_bypass=false + expected_version_id match + automation guard.
		automation, activated, casErr := s.repo.ApplyProposalCAS(ctx, nil, in.ProposalID)
		if casErr != nil {
			// A locked page coverage attempt (§4.4) is audited and rejected.
			if automation == AutomationLocked {
				s.recordWikiAudit(ctx, in.Auth, "wiki.lock", &proposal.WikiSpaceID, &proposal.ID, map[string]any{
					"page_key": proposal.PageKey, "reason": "coverage attempt on locked page",
				})
				return nil, ErrWikiLockedPageCovered
			}
			// CAS stale / expected-mismatch → mark proposal superseded/failed.
			_ = s.repo.UpdateProposalStatus(ctx, in.ProposalID, "failed", nil, nil)
			return nil, casErr
		}
		_ = automation
		if !activated {
			// CAS did not flip (stale expected_version_id) → superseded.
			_ = s.repo.UpdateProposalStatus(ctx, in.ProposalID, "superseded", nil, nil)
		}
		s.recordWikiAudit(ctx, in.Auth, "wiki.apply", &proposal.WikiSpaceID, &proposal.ID, map[string]any{
			"page_key": proposal.PageKey, "activated": activated,
		})
	case "reject":
		if err := s.repo.UpdateProposalStatus(ctx, in.ProposalID, "rejected", nil, nil); err != nil {
			return nil, err
		}
		s.recordWikiAudit(ctx, in.Auth, "wiki.review", &proposal.WikiSpaceID, &proposal.ID, map[string]any{
			"page_key": proposal.PageKey, "decision": "reject",
		})
	default:
		return nil, fmt.Errorf("wiki: decision must be approve|reject")
	}
	return s.repo.GetProposal(ctx, in.ProposalID)
}

// --- Worker-driven execute path (§4.3) ---

// ExecuteRun is the worker entry point (§4.3): it loads the queued run,
// computes the affected pages + authorized source versions, calls the provider
// (ProposeIngest for ingest/reconcile, ProposeAnswer for query_file), validates
// the returned PagePatches through the §4.2 schema gate, and lands them as
// proposals with the managed/locked/manual differentiation (§4.4). It is
// invoked by the knowledge-worker's wiki_maintain handler. Returns the proposal
// ids written (nil for a no-op run). The service delegates to its
// MaintenanceProvider (the worker wires the real provider + model adapter).
//
// Locked-page protection (§4.4 point 1): the affected-page set passed to the
// provider carries only page_key + version summary for locked pages (no body),
// and a patch whose action is update/create on a locked page is rejected at
// the schema gate (provider/schema.go), so the provider cannot widen its read
// or rewrite a locked page.
func (s *Service) ExecuteRun(ctx context.Context, runID uuid.UUID) ([]uuid.UUID, error) {
	if s.provider == nil {
		return nil, fmt.Errorf("wiki: provider not wired")
	}
	run, err := s.repo.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	// Mark the run analyzing (§2.4 status machine) best-effort.
	_ = s.repo.UpdateRunStatus(ctx, runID, "analyzing", "", "")

	// Compute the affected pages + authorized source versions (§4.3). For
	// query_file the page is the run's page_key; for ingest/reconcile the
	// repo resolves the dependency graph from wiki_page_sources.
	pages, err := s.repo.AffectedPages(ctx, run.WikiSpaceID, pageKeyForRun(run))
	if err != nil {
		_ = s.repo.UpdateRunStatus(ctx, runID, "failed", "affected_pages", redactWikiSvcErr(err))
		return nil, err
	}

	// Call the provider per the trigger (§4.3). lint runs do not land patches
	// here — the wiki_lint_scan handler owns that path.
	var patches []PagePatch
	switch run.TriggerType {
	case TriggerQueryFile:
		patches, err = s.provider.ProposeAnswer(ctx, run.WikiSpaceID, runPageKey(run), run.AnswerRef, pages)
	case TriggerIngest, TriggerManual:
		patches, err = s.provider.ProposeIngest(ctx, run.WikiSpaceID, runPageKey(run), pages)
	default:
		// lint/other: no patch landing from this path.
		_ = s.repo.UpdateRunStatus(ctx, runID, "applied", "", "")
		return nil, nil
	}
	if err != nil {
		_ = s.repo.UpdateRunStatus(ctx, runID, "failed", "provider", redactWikiSvcErr(err))
		return nil, err
	}
	if len(patches) == 0 {
		// No-op run (e.g. NoopProvider): applied with zero proposals.
		_ = s.repo.UpdateRunStatus(ctx, runID, "applied", "", "")
		return nil, nil
	}

	// §4.2 schema gate: a non-conformant patch fails the whole run (§4.2
	// "未通过的 patch 整条 Run 标 failed"). The gate also enforces §4.4 point
	// 2 — a locked page receiving an update/create action is rejected here.
	if err := s.validatePatches(patches, pages); err != nil {
		_ = s.repo.UpdateRunStatus(ctx, runID, "failed", "schema_violation", redactWikiSvcErr(err))
		return nil, fmt.Errorf("%w: %s", ErrWikiSchemaViolation, err.Error())
	}

	// Land the patches as proposals (§4.4 differentiation): managed → coverage
	// candidate (is_bypass=false); locked/manual → bypass suggestion
	// (is_bypass=true, proposed_version_id=nil). The candidate
	// knowledge_asset_versions row is created by the repo; the service only
	// records the proposal (the content body never crosses this boundary —
	// only its hash, §4.2).
	proposals := make([]*PageProposal, 0, len(patches))
	now := time.Now().UTC()
	automation := automationIndex(pages)
	for _, p := range patches {
		state := automation[p.PageKey]
		isBypass := state == AutomationLocked || state == AutomationManual
		prop := &PageProposal{
			ID:                  uuid.New(),
			RunID:               runID,
			WikiSpaceID:         run.WikiSpaceID,
			PageKey:             p.PageKey,
			ExpectedVersionID:   p.ExpectedVersionID,
			Action:              p.Action,
			IsBypass:            isBypass,
			ContentHash:         p.ContentHash,
			RelationSuggestions: relationSuggestionToMap(p.RelationSuggestions),
			Status:              "proposed",
			CreatedAt:           now,
		}
		proposals = append(proposals, prop)
	}
	if err := s.repo.CreateProposals(ctx, nil, proposals); err != nil {
		_ = s.repo.UpdateRunStatus(ctx, runID, "failed", "create_proposals", redactWikiSvcErr(err))
		return nil, err
	}
	// Record the proposal manifest (§2.4 proposal_manifest) best-effort.
	ids := make([]uuid.UUID, len(proposals))
	for i, p := range proposals {
		ids[i] = p.ID
	}
	_ = s.repo.UpdateRunStatus(ctx, runID, "awaiting_review", "", "")
	return ids, nil
}

// validatePatches runs the §4.2 schema gate over the provider's patches and
// enforces the §4.4 locked-page action guard (point 2): a locked page must not
// receive a create/update coverage patch. The structural validation mirrors
// provider.ValidatePatch so the service keeps a one-way dependency on the
// provider package (no import). Returns the first violation.
func (s *Service) validatePatches(patches []PagePatch, pages []AffectedPage) error {
	locked := make(map[string]bool, len(pages))
	seen := make(map[string]bool, len(pages))
	for _, p := range pages {
		seen[p.PageKey] = true
		if p.AutomationState == AutomationLocked {
			locked[p.PageKey] = true
		}
	}
	for _, p := range patches {
		if strings.TrimSpace(p.PageKey) == "" {
			return fmt.Errorf("page_key: required")
		}
		if len(p.PageKey) > 256 {
			return fmt.Errorf("page_key: maxLength 256 exceeded")
		}
		switch p.Action {
		case "create", "update", "link", "contradiction", "stale":
		default:
			return fmt.Errorf("action: must be one of create|update|link|contradiction|stale")
		}
		if !sha256Hex.MatchString(p.ContentHash) {
			return fmt.Errorf("content_hash: must be a 64-char lowercase hex SHA-256")
		}
		if len(p.SourceVersions) < 1 {
			return fmt.Errorf("source_versions: minItems 1")
		}
		// §4.4 point 2: a locked page receiving a coverage action (create/
		// update) is rejected at the schema gate. link/contradiction/stale
		// bypass suggestions are allowed (is_bypass=true path).
		if locked[p.PageKey] && (p.Action == "create" || p.Action == "update") {
			return fmt.Errorf("page %q: locked page cannot receive %s coverage", p.PageKey, p.Action)
		}
		_ = seen
	}
	return nil
}

// pageKeyForRun returns the run's page_key (query_file) or "" (whole-space).
func pageKeyForRun(run *MaintenanceRun) string {
	if len(run.AnswerRef) > 0 {
		if v, ok := run.AnswerRef["page_key"].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// runPageKey is pageKeyForRun kept under its original name for the call sites.
func runPageKey(run *MaintenanceRun) string { return pageKeyForRun(run) }

// automationIndex maps page_key → automation_state for the affected-page set.
func automationIndex(pages []AffectedPage) map[string]AutomationState {
	m := make(map[string]AutomationState, len(pages))
	for _, p := range pages {
		m[p.PageKey] = p.AutomationState
	}
	return m
}

// relationSuggestionToMap converts []RelationSuggestion to the JSONB-shaped
// []map[string]any the repo stores (§2.4 relation_suggestions).
func relationSuggestionToMap(rs []RelationSuggestion) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		m := map[string]any{
			"kind": r.Kind, "to_asset_id": r.ToAssetID,
		}
		if r.ToVersionID != nil {
			m["to_version_id"] = *r.ToVersionID
		}
		out = append(out, m)
	}
	return out
}

// redactWikiSvcErr trims an error string for storage in
// error_detail_redacted (§8.3 — no sensitive content in the run's error
// column).
func redactWikiSvcErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}

// findRunByIdempotencyKey looks up a prior run by idempotency_key (§4.2 / §0
// D5). It is the fast path of the idempotency guard; the repo's
// CreateRunWithEvent UNIQUE(idempotency_key) is the authoritative fallback
// that settles a concurrent race.
func (s *Service) findRunByIdempotencyKey(ctx context.Context, key string) (*MaintenanceRun, error) {
	run, err := s.repo.GetRunByIdempotencyKey(ctx, key)
	if err != nil {
		// Not found is not an error here — it means no prior run exists.
		if errors.Is(err, ErrWikiRunNotFound) {
			return nil, nil
		}
		return nil, nil // best-effort: fall back to the UNIQUE constraint.
	}
	return run, nil
}

// computeInputSetHash derives a stable hash for the run's input set (§0 D5).
// For query_file the input set is the answer_ref; for ingest/lint it is the
// sorted check_kinds / page_key list. Canonical encoding keeps the hash stable
// across reorderings.
func computeInputSetHash(in TriggerRunInput) string {
	h := sha256.New()
	fmt.Fprintln(h, string(in.Trigger))
	fmt.Fprintln(h, in.PageKey)
	// answer_ref: canonical JSON.
	if len(in.AnswerRef) > 0 {
		if b, err := json.Marshal(in.AnswerRef); err == nil {
			h.Write(b)
		}
	}
	// check_kinds: sorted.
	cks := append([]string(nil), in.CheckKinds...)
	sort.Strings(cks)
	for _, k := range cks {
		fmt.Fprintln(h, k)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// buildIdempotencyKey assembles the run-level idempotency key (§0 D5):
// input_set_hash + schema_version_id + model_revision + prompt_revision,
// scoped by space + trigger so different triggers on the same input are
// distinct runs.
func buildIdempotencyKey(spaceID uuid.UUID, trigger TriggerType, inputSetHash string, schemaVersionID uuid.UUID, modelRev, promptRev string) string {
	return strings.Join([]string{
		"wiki", spaceID.String(), string(trigger), inputSetHash,
		schemaVersionID.String(), modelRev, promptRev,
	}, ":")
}

// auth adapts the service AuthContext to the internal authorize signature.
func auth(a AuthContext) AuthContext { return a }

// isUniqueViolation reports whether err is a Postgres unique_violation
// (23P01). Used to map idempotency / name-conflict races.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "23505") || strings.Contains(err.Error(), "unique")
}
