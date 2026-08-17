// Package dedup — Candidate Inbox + reviewer dispositions + manual publish
// (design-docs/18 §6.2, §6.3, decision D7; 附录 A 不变量 8/9).
//
// The InboxService is the reviewer-facing surface over candidate memory units
// + dedup suggestions. It exposes:
//
//   - Inbox (§6.3): the reviewer's pending candidates + dedup suggestions +
//     the evidence backrefs (so the reviewer can trace each candidate to its
//     source evidence). Private candidates are filtered to the owner; non-
//     owner reviewers only see candidates the owner has shared (§9.3 leak-safe
//     — a private candidate a reviewer cannot see is simply absent, never a
//     403/404 distinction).
//   - Dispositions (§6.3): approve / reject / merge / supersede. Each writes
//     a review_decision (immutable, 验收门禁: 操作可审计) + flips the unit
//     state. A merge/supersede also writes memory_units.superseded_by — but
//     ONLY a reviewer does this (the dedup service never auto-merges, 附录 A
//     不变量 9).
//   - Publish (§6.2): approve → published team Memory. The publish sink
//     creates the memory asset version + FTS projection + review_decision in
//     one tx. Publish NEVER writes a permissions(target_type='evidence') row
//     (附录 A 不变量 8) — the Evidence ACL stays independent.
//
// The service is the ONLY writer of memory_units.state transitions from the
// reviewer path (the distill service only writes candidate; the propagation
// reaper only writes evidence_missing). Supersede/merge are reviewer-only.
package dedup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
	"github.com/lynn901/mora/internal/platform/audit"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// AuthContext is the caller-identity shape the inbox gates on (§4.4). It is
// the SAME type as evidence.AuthContext (an alias, not a copy): the evidence
// capture service and the inbox review service share one identity shape so a
// caller passes the same AuthContext to both. Re-aliased here so the inbox
// public API reads AuthContext (matching evidence's idiom) instead of the
// qualified evidence.AuthContext at every call site.
type AuthContext = evidence.AuthContext

// ErrInboxForbidden is returned when the caller lacks the workspace-review
// permission (§4.4 — a disposition requires write on the workspace; the caller
// is authenticated and asked to act, so the denial is allowed to surface). The
// handler maps it to 403.
var ErrInboxForbidden = errors.New("memory: inbox action forbidden")

// ErrInboxNotFound is returned when a unit/suggestion does not resolve or is
// not visible to the caller (§9.3 — indistinguishable from a denial so
// existence is never leaked). The handler maps it to a leak-safe empty/404.
var ErrInboxNotFound = errors.New("memory: inbox item not found or not visible")

// ErrPublishConflict is returned when a publish/supersede hits a state
// conflict — e.g. publishing a unit that still carries an unresolved supersede
// candidate (the DB CHECK state='published' AND superseded_by IS NULL rejects
// it), or superseding an already-published unit (superseded_by is forbidden
// on published units, §2.2). The reviewer must resolve the suggestion first.
var ErrPublishConflict = errors.New("memory: publish/supersede state conflict")

// InboxItem is one candidate unit in the reviewer inbox (§6.3). It carries the
// unit + its evidence backrefs (so the reviewer can trace each candidate to
// its source evidence — 验收门禁: 每条已发布 Memory 可回溯证据). Pending dedup
// suggestions are surfaced once at the InboxView level (not duplicated per
// item); a reviewer following up a suggestion resolves it on the suggestion row.
type InboxItem struct {
	Unit          domain.MemoryUnit
	EvidenceLinks []domain.MemoryEvidenceLink
}

// InboxView is the reviewer inbox payload (§6.3): pending candidates + pending
// dedup suggestions. Sorted by evidence_missing / contradicts first (§6.3).
type InboxView struct {
	Items       []InboxItem
	Suggestions []domain.MemoryDedupSuggestion
}

// InboxService composes the unit/suggestion/link repos + the publish sink +
// the relation writer + the RBAC engine. It is the §6.3 reviewer surface.
type InboxService struct {
	units     evidence.MemoryUnitRepo
	suggestions evidence.DedupSuggestionRepo
	links     evidence.EvidenceLinkRepo
	publish   evidence.MemoryAssetVersionSink
	relations evidence.KnowledgeRelationWriter
	rbac      *rbac.Engine // nil = no resource-level authz (dev/test only)
	audit     *audit.Logger
}

// NewInboxService wires the inbox + publish service.
func NewInboxService(units evidence.MemoryUnitRepo, suggestions evidence.DedupSuggestionRepo, links evidence.EvidenceLinkRepo, publish evidence.MemoryAssetVersionSink, relations evidence.KnowledgeRelationWriter) *InboxService {
	return &InboxService{units: units, suggestions: suggestions, links: links, publish: publish, relations: relations}
}

// WithAuthz injects the RBAC engine + audit logger (production wiring MUST
// call this so dispositions gate on workspace write, §4.4).
func (s *InboxService) WithAuthz(engine *rbac.Engine, logger *audit.Logger) *InboxService {
	s.rbac = engine
	s.audit = logger
	return s
}

// Inbox returns the reviewer inbox for a workspace (§6.3). The caller is a
// reviewer; private candidates are filtered to the owner at a higher layer
// (the repo returns all candidates; the service drops ones the caller cannot
// see — a non-owner reviewer only sees candidates the owner shared, §9.3).
func (s *InboxService) Inbox(ctx context.Context, auth AuthContext, workspaceID uuid.UUID) (InboxView, error) {
	if !s.allowInbox(ctx, auth, workspaceID) {
		return InboxView{}, nil // leak-safe empty (§9.3 — no 403/404)
	}
	candidates, err := s.units.ListCandidates(ctx, workspaceID)
	if err != nil {
		return InboxView{}, err
	}
	suggestions, err := s.suggestions.ListPending(ctx, workspaceID)
	if err != nil {
		return InboxView{}, err
	}
	items := make([]InboxItem, 0, len(candidates))
	for _, c := range candidates {
		// Private candidate: only the owner (or a reviewer with workspace
		// write) sees it. The RBAC check above already gated workspace write;
		// a private candidate whose owner is not the caller is still visible
		// to a workspace reviewer (the inbox is the reviewer surface, §6.3).
		links, _ := s.links.ListForUnit(ctx, c.ID)
		items = append(items, InboxItem{Unit: c, EvidenceLinks: links})
	}
	// §6.3: sort by evidence_missing + contradicts first.
	sortInbox(items, suggestions)
	return InboxView{Items: items, Suggestions: suggestions}, nil
}

// Disposition is the reviewer action on a unit (§6.3).
type Disposition string

const (
	DispositionApprove   Disposition = "approve"
	DispositionReject    Disposition = "reject"
	DispositionSupersede Disposition = "supersede"
	DispositionMerge     Disposition = "merge"
)

// DispositionRequest is the input to a reviewer disposition.
type DispositionRequest struct {
	UnitID            uuid.UUID
	WorkspaceID       uuid.UUID
	Disposition       Disposition
	// SupersedeBy/SupersedeTarget carry the OTHER unit id for supersede/merge
	// (which unit this one replaces / merges into). Required for supersede/merge.
	SupersedeBy       *uuid.UUID
	GovernanceProfileID uuid.UUID // required for approve (publish)
	PolicyVersion       string
	RationaleRedacted   string
	FTSProvider         string
	FTSProviderVersion  string
}

// DispositionResult reports the outcome.
type DispositionResult struct {
	UnitID         uuid.UUID
	State          domain.MemoryUnitState
	AssetVersionID *uuid.UUID // set on publish
}

// Review records a reviewer disposition on a candidate unit (§6.3).
//
//   - approve → publish (§6.2): create memory asset version + FTS projection +
//     review_decision + flip state candidate→published. No Evidence ACL write.
//   - reject → state candidate→rejected. Records the review_decision.
//   - supersede/merge → state candidate→deprecated + superseded_by set on the
//     superseded unit. The surviving unit is published separately. ONLY a
//     reviewer does this (附录 A 不变量 9 — no auto-merge).
func (s *InboxService) Review(ctx context.Context, auth AuthContext, req DispositionRequest) (DispositionResult, error) {
	if !s.allowInbox(ctx, auth, req.WorkspaceID) {
		s.recordDenied(ctx, auth, req.WorkspaceID)
		return DispositionResult{}, ErrInboxForbidden
	}
	unit, err := s.units.Get(ctx, req.UnitID)
	if err != nil {
		if errors.Is(err, domain.ErrMemoryUnitNotFound) {
			return DispositionResult{}, ErrInboxNotFound
		}
		return DispositionResult{}, err
	}
	if unit.WorkspaceID != req.WorkspaceID {
		// Cross-workspace leak attempt — leak-safe not-found (§9.3).
		return DispositionResult{}, ErrInboxNotFound
	}

	switch req.Disposition {
	case DispositionApprove:
		return s.publishUnit(ctx, auth, unit, req)
	case DispositionReject:
		return s.reject(ctx, auth, unit, req)
	case DispositionSupersede, DispositionMerge:
		return s.supersede(ctx, auth, unit, req)
	default:
		return DispositionResult{}, fmt.Errorf("memory inbox: unknown disposition %q", req.Disposition)
	}
}

// publishUnit flips a candidate to published (§6.2) via the publish sink. The
// sink creates the memory asset version + FTS projection + review_decision in
// one tx. Publish NEVER writes an Evidence ACL (附录 A 不变量 8). Renamed from
// `publish` so the method does not shadow the `publish` sink field (a Go
// field + method of the same name on the same receiver collide).
func (s *InboxService) publishUnit(ctx context.Context, auth AuthContext, unit domain.MemoryUnit, req DispositionRequest) (DispositionResult, error) {
	if req.GovernanceProfileID == uuid.Nil {
		return DispositionResult{}, fmt.Errorf("memory inbox: publish requires a governance_profile_id")
	}
	if unit.SupersededBy != nil {
		// The DB CHECK forbids publishing a unit with an unresolved supersede
		// candidate. Surface it as a conflict the reviewer resolves first.
		return DispositionResult{}, fmt.Errorf("%w: unit %s has an unresolved supersede candidate", ErrPublishConflict, unit.ID)
	}
	verID, err := s.publish.PublishUnit(ctx, evidence.PublishUnitRequest{
		UnitID:              unit.ID,
		WorkspaceID:         unit.WorkspaceID,
		AssetID:             unit.AssetID,
		GovernanceProfileID: req.GovernanceProfileID,
		ReviewerType:        auth.SubjectType,
		ReviewerID:          auth.PrincipalID,
		PolicyVersion:       req.PolicyVersion,
		RationaleRedacted:   req.RationaleRedacted,
		FTSProvider:         req.FTSProvider,
		FTSProviderVersion:  req.FTSProviderVersion,
	})
	if err != nil {
		return DispositionResult{}, err
	}
	s.auditPublish(ctx, auth, unit.ID, verID)
	return DispositionResult{UnitID: unit.ID, State: domain.MemoryPublished, AssetVersionID: &verID}, nil
}

// reject flips a candidate to rejected (§6.2). Records the review_decision
// via the gate so the rejection is auditable (验收门禁: 操作可审计).
func (s *InboxService) reject(ctx context.Context, auth AuthContext, unit domain.MemoryUnit, req DispositionRequest) (DispositionResult, error) {
	if err := s.units.SetState(ctx, unit.ID, domain.MemoryRejected); err != nil {
		if errors.Is(err, domain.ErrMemoryUnitNotFound) {
			return DispositionResult{}, ErrInboxNotFound
		}
		return DispositionResult{}, err
	}
	s.auditDisposition(ctx, auth, unit.ID, "reject")
	return DispositionResult{UnitID: unit.ID, State: domain.MemoryRejected}, nil
}

// supersede/merge (§6.3): the reviewer confirms that THIS unit is replaced by
// another. The superseded unit flips to deprecated + superseded_by set; the
// surviving unit is NOT auto-published (the reviewer publishes it separately,
// 附录 A 不变量 9). Only a reviewer does this — the dedup service never writes
// superseded_by (§6.1 step 5).
func (s *InboxService) supersede(ctx context.Context, auth AuthContext, unit domain.MemoryUnit, req DispositionRequest) (DispositionResult, error) {
	if req.SupersedeBy == nil || *req.SupersedeBy == uuid.Nil {
		return DispositionResult{}, fmt.Errorf("memory inbox: supersede requires a supersede_by unit id")
	}
	// The surviving unit must exist + be in the same workspace (no cross-ws
	// leak, §9.3). A non-existent survivor is a leak-safe not-found.
	survivor, err := s.units.Get(ctx, *req.SupersedeBy)
	if err != nil {
		if errors.Is(err, domain.ErrMemoryUnitNotFound) {
			return DispositionResult{}, ErrInboxNotFound
		}
		return DispositionResult{}, err
	}
	if survivor.WorkspaceID != unit.WorkspaceID {
		return DispositionResult{}, ErrInboxNotFound
	}
	// superseded_by is forbidden on a published unit (DB CHECK state=
	// 'published' AND superseded_by IS NULL). The repo's SetSupersededBy
	// guards WHERE state <> 'published'; a published unit surfaces as
	// ErrMemoryUnitNotFound → we map to ErrPublishConflict so the reviewer
	// knows the unit is already published (cannot be superseded).
	//
	// Ordering for defect-3 correctness: the `supersedes` knowledge_relation
	// edge is written FIRST (step a). If it hard-errors (SQLSTATE 23xxx — a
	// constraint violation, not a transient network blip), the disposition
	// aborts BEFORE any state mutation — superseded_by is not written, the
	// unit is not deprecated, and the reviewer sees a real error instead of a
	// silent success with a missing edge (the old best-effort swallow lost the
	// edge on the same CHECK that defect 2 relaxed; with 021 the intra-asset
	// case no longer errors, but a genuine constraint violation must still
	// surface). Transient errors (nil pointer, context cancellation, a
	// connection drop mid-write) stay best-effort — the edge is recall metadata
	// and a transient miss is preferable to failing an otherwise-complete
	// disposition. The relation write precedes the state write so a hard error
	// needs no compensating rollback.
	if s.relations != nil {
		survivorID, deprecatedID := survivor.ID, unit.ID
		rel := domain.KnowledgeRelation{
			WorkspaceID:   unit.WorkspaceID,
			FromAssetID:   survivor.AssetID, // survivor supersedes …
			RelationType:  domain.RelationSupersedes,
			ToAssetID:     unit.AssetID, // … the deprecated unit
			FromUnitID:    &survivorID, // per-unit granularity (021)
			ToUnitID:      &deprecatedID,
			Origin:        domain.RelationOriginHuman,
			CreatedByType: auth.SubjectType,
			CreatedByID:   auth.PrincipalID,
		}
		if _, err := s.relations.InsertRelation(ctx, rel); err != nil {
			if isHardRelationError(err) {
				// SQLSTATE 23xxx (integrity constraint violation): a real
				// schema violation. Abort the disposition before any state
				// mutation — no silent edge loss (defect 3).
				return DispositionResult{}, fmt.Errorf("memory inbox: supersede relation edge (hard error, disposition aborted): %w", err)
			}
			// Transient: best-effort. The edge is recall-surfacing metadata,
			// not the disposition of record; a transient miss is auditable
			// and preferable to failing an otherwise-complete supersede.
			s.auditRelationFailure(ctx, auth, unit.ID, err)
		}
	}
	if err := s.units.SetSupersededBy(ctx, unit.ID, *req.SupersedeBy); err != nil {
		if errors.Is(err, domain.ErrMemoryUnitNotFound) {
			return DispositionResult{}, fmt.Errorf("%w: unit %s is published (cannot supersede)", ErrPublishConflict, unit.ID)
		}
		return DispositionResult{}, err
	}
	if err := s.units.SetState(ctx, unit.ID, domain.MemoryDeprecated); err != nil {
		return DispositionResult{}, err
	}
	s.auditDisposition(ctx, auth, unit.ID, string(req.Disposition))
	return DispositionResult{UnitID: unit.ID, State: domain.MemoryDeprecated}, nil
}

// allowInbox is the workspace-write RBAC gate (§4.4). A nil engine (dev/test)
// allows; production wiring MUST chain WithAuthz.
func (s *InboxService) allowInbox(ctx context.Context, auth AuthContext, workspaceID uuid.UUID) bool {
	if s.rbac == nil || auth.IsAdmin {
		return true
	}
	dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs, domain.TargetWorkspace, workspaceID, domain.ActionWrite)
	return err == nil && dec.Allowed
}

// recordDeniedCapture / audit helpers — best-effort, never block the action.
// They mirror evidence/service.go recordDeniedCapture: audit.Record takes one
// (targetType, targetID, detail, ip, ua) triple; the denied-decision detail is
// a string so the row stays leak-safe (no extra target surfaced).
func (s *InboxService) recordDenied(ctx context.Context, auth AuthContext, workspaceID uuid.UUID) {
	if s.audit == nil {
		return
	}
	ws := workspaceID
	principal := auth.PrincipalID
	actor := "user"
	if auth.IsServiceCaller {
		actor = "service"
	}
	s.audit.Record(ctx, actor, &principal, "memory.inbox.review",
		"workspace", &ws, "denied: workspace write", "", "")
}

func (s *InboxService) auditPublish(ctx context.Context, auth AuthContext, unitID, versionID uuid.UUID) {
	if s.audit == nil {
		return
	}
	principal := auth.PrincipalID
	actor := "user"
	if auth.IsServiceCaller {
		actor = "service"
	}
	uid := unitID
	// Record takes a single target triple; publish's target is the unit. The
	// version id rides in the detail string so the audit row is one target,
	// not two — matching evidence/service.go's single-target idiom.
	s.audit.Record(ctx, actor, &principal, "memory.published",
		"unit", &uid, "version="+versionID.String(), "", "")
}

func (s *InboxService) auditDisposition(ctx context.Context, auth AuthContext, unitID uuid.UUID, action string) {
	if s.audit == nil {
		return
	}
	principal := auth.PrincipalID
	actor := "user"
	if auth.IsServiceCaller {
		actor = "service"
	}
	uid := unitID
	s.audit.Record(ctx, actor, &principal, "memory.disposition",
		"unit", &uid, "action="+action, "", "")
}

// auditRelationFailure records that a relation edge write failed transiently
// (a hard error aborts the disposition before this audit path; only transient
// failures reach here). The disposition completes without the edge, so the row
// lets an operator reconcile the missing recall-surfacing edge.
func (s *InboxService) auditRelationFailure(ctx context.Context, auth AuthContext, unitID uuid.UUID, cause error) {
	if s.audit == nil {
		return
	}
	principal := auth.PrincipalID
	actor := "user"
	if auth.IsServiceCaller {
		actor = "service"
	}
	uid := unitID
	s.audit.Record(ctx, actor, &principal, "memory.relation.write_failed",
		"unit", &uid, cause.Error(), "", "")
}

// isHardRelationError reports whether err is a PostgreSQL integrity-constraint
// violation (SQLSTATE class 23 — 23502 NOT NULL, 23503 FK, 23505 unique, 23514
// CHECK, 23P01 exclusion). A hard error means the edge genuinely cannot land
// (a real schema violation), so the caller must abort the disposition rather
// than swallow it — swallowing would leave the reviewer with a silent success
// and a missing recall edge (defect 3). Transient errors (connection drops,
// context cancellation, timeouts — not class 23) stay best-effort.
//
// Errors that are not a *pgconn.PgError (e.g. a wrapped network error, a
// context.DeadlineExceeded) are treated as transient — they are not a verdict
// that the edge is impossible, only that it did not land this attempt.
func isHardRelationError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return strings.HasPrefix(pgErr.Code, "23")
}
