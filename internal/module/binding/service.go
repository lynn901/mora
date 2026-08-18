package binding

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/audit"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// Service is the Agent 配装（Binding）management application service
// (design-docs/19 §5, Phase 5-2 / YS-162). It composes the batch sink, the
// revoker, the read repository, and an optional pinned-version checker + RBAC
// engine + audit logger. The sink owns the transaction (binding write +
// outbox event + workspace revision bump in one tx); the service stays
// pgx-free — same layering as source/service over SyncRunSink.
//
// RBAC: management operations (batch upsert / revoke) require the `assign`
// action on the workspace (§6.1 marks them 管理型). The engine is nil in
// tests only; production wiring MUST chain WithAuthz.
type Service struct {
	bindings Repository
	batch    BatchUpsert
	revoke   RevokeRevoker
	pinned   PinnedVersionChecker
	rbac     *rbac.Engine // nil = no resource-level authz (dev/test only)
	audit    *audit.Logger
}

// NewService wires the binding management service. pinned may be nil (disables
// the blocked-pinned-version alert at batch time — acceptable in tests). rbac
// is nil by design: production wiring MUST chain WithAuthz so every management
// method enforces the `assign` action on the workspace.
func NewService(bindings Repository, batch BatchUpsert, revoke RevokeRevoker, pinned PinnedVersionChecker) *Service {
	return &Service{bindings: bindings, batch: batch, revoke: revoke, pinned: pinned}
}

// WithAuthz injects the RBAC engine + audit logger and returns the service for
// chaining (same pattern as source/service.WithAuthz). Once set, every
// management method calls rbac.Engine.Check before touching a binding.
func (s *Service) WithAuthz(engine *rbac.Engine, logger *audit.Logger) *Service {
	s.rbac = engine
	s.audit = logger
	return s
}

// AuthContext carries the caller identity for RBAC + audit (mirrors
// source/service.AuthContext). IsAdmin short-circuits the Check; a
// service_account caller resolves to itself with no admin bypass.
type AuthContext struct {
	SubjectType domain.SubjectType
	PrincipalID uuid.UUID
	GroupIDs    []uuid.UUID
	IsAdmin     bool
}

// BatchUpsertBindings applies a batch of create/update binding operations for
// one agent in one transaction (§5.2). For each item with
// VersionPolicy=pinned it checks the pinned version's usability; a
// non-usable pinned version is NOT rejected — the binding is still written
// (durable, audited) and flagged PinnedVersionBlocked so the caller surfaces
// the §5.1 alert ("阻断+告警": the authz pinnedVersionGate blocks use at
// decision time, this service alerts now, neither falls back to latest).
//
// Idempotency-Key (§5.2 / §11.1): empty → generated. A duplicate key for a
// different payload returns ErrIdempotencyConflict → 409; the same payload
// returns the original batch.
//
// Authorization: the caller must hold the `assign` action on the workspace
// (§6.1 管理型). A denial returns ErrBindingNotFound (no existence leak of
// the agent's binding set) and records a denied audit row.
func (s *Service) BatchUpsertBindings(ctx context.Context, auth AuthContext, agentID, workspaceID uuid.UUID, idempotencyKey string, inputs []BindingInput) (BatchResult, error) {
	if err := s.authorize(ctx, auth, domain.TargetWorkspace, workspaceID, domain.ActionAssign, true); err != nil {
		return BatchResult{}, err
	}
	if idempotencyKey == "" {
		idempotencyKey = uuid.NewString()
	}
	// Validate every item up front so a batch reports the offending item
	// rather than surfacing one opaque DB constraint violation. pinned requires
	// scope=asset + a version id; asset scope requires an asset id; asset_type
	// scope requires an asset type. (Mirrors the Phase 0 CHECK constraints.)
	for _, in := range inputs {
		if err := validateInput(in); err != nil {
			return BatchResult{}, err
		}
	}

	// Pre-flight pinned-version alerting (§5.1 告警). A non-usable pinned
	// version does NOT block the batch — the binding is written in a blocked
	// state and the authz pinnedVersionGate will阻断 use at decision time.
	// Collecting the blocked indexes here lets the sink flag each result.
	blocked := make(map[int]bool, len(inputs))
	if s.pinned != nil {
		for i, in := range inputs {
			if in.VersionPolicy == domain.BindingPinned && in.PinnedVersionID != nil {
				usable, err := s.pinned.IsUsable(ctx, *in.PinnedVersionID)
				if err != nil || !usable {
					blocked[i] = true
				}
			}
		}
	}

	actor := domain.EventActor{Type: auth.SubjectType, ID: auth.PrincipalID}
	if auth.SubjectType == "" {
		actor.Type = domain.SubjectUser
	}
	res, err := s.batch.BatchUpsert(ctx, agentID, workspaceID, idempotencyKey, inputs, actor)
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			return BatchResult{}, ErrIdempotencyConflict
		}
		if errors.Is(err, ErrIdempotentRetry) {
			// Same payload, original batch already exists — re-fetch by key
			// and return the original (§5.2 idempotent retry).
			orig, gerr := s.bindings.GetByIdempotencyKey(ctx, idempotencyKey)
			if gerr != nil || len(orig) == 0 {
				return BatchResult{}, ErrIdempotencyConflict
			}
			out := BatchResult{IdempotentHit: true}
			for _, b := range orig {
				out.Results = append(out.Results, BindingResult{Binding: b})
			}
			return out, nil
		}
		return BatchResult{}, err
	}

	// Stamp the blocked flags the sink could not know (it stays pgx-bound and
	// does not call the version checker; the service owns the alert decision).
	for i := range res.Results {
		if blocked[i] {
			res.Results[i].PinnedVersionBlocked = true
			res.Alerted = append(res.Alerted, i)
		}
	}

	// Best-effort audit of the management action (§5 审计: agent.binding_changed
	// outbox event is the durable record; this audit row attributes the action
	// to the caller at the HTTP layer's granularity).
	if s.audit != nil {
		pid := auth.PrincipalID
		aid := agentID
		s.audit.Record(ctx, string(auth.SubjectType), &pid,
			"agent.binding_changed",
			"agent", &aid,
			map[string]any{
				"workspace_id":    workspaceID.String(),
				"items":           len(inputs),
				"alerted":         len(res.Alerted),
				"idempotency_key": idempotencyKey,
				"new_revision":    res.NewRevision,
			}, "", "")
	}
	return res, nil
}

// RevokeBinding revokes a single binding + bumps the workspace authz revision
// in the same transaction (§5.4: revoke → revision+1 → cache invalidates →
// next request denies). Authorization: `assign` on the workspace. A missing
// binding returns ErrBindingNotFound (no leak); a binding in another
// workspace is denied as not-found (cross-workspace guard).
func (s *Service) RevokeBinding(ctx context.Context, auth AuthContext, bindingID, agentID, workspaceID uuid.UUID) (int64, error) {
	if err := s.authorize(ctx, auth, domain.TargetWorkspace, workspaceID, domain.ActionAssign, true); err != nil {
		return 0, err
	}
	// Existence + cross-workspace guard: load first, surface not-found on miss
	// or cross-workspace (no existence leak).
	cur, err := s.bindings.Get(ctx, bindingID)
	if err != nil {
		return 0, ErrBindingNotFound
	}
	if cur.WorkspaceID != workspaceID || cur.AgentID != agentID {
		return 0, ErrBindingNotFound
	}
	actor := domain.EventActor{Type: auth.SubjectType, ID: auth.PrincipalID}
	if actor.Type == "" {
		actor.Type = domain.SubjectUser
	}
	newRev, err := s.revoke.Revoke(ctx, bindingID, agentID, workspaceID, actor)
	if err != nil {
		return 0, err
	}
	if s.audit != nil {
		pid := auth.PrincipalID
		bid := bindingID
		s.audit.Record(ctx, string(auth.SubjectType), &pid,
			"agent.binding_changed",
			"agent", &agentID,
			map[string]any{
				"workspace_id": workspaceID.String(),
				"binding_id":   bid.String(),
				"action":       "revoke",
				"new_revision": newRev,
			}, "", "")
	}
	return newRev, nil
}

// ListBindings returns the active bindings for an agent (§6.1 list). The
// caller must hold `assign` on the workspace; a denial returns
// ErrBindingNotFound (no leak of the binding set's existence).
func (s *Service) ListBindings(ctx context.Context, auth AuthContext, agentID, workspaceID uuid.UUID, after *uuid.UUID, limit int) ([]domain.AgentBinding, error) {
	if err := s.authorize(ctx, auth, domain.TargetWorkspace, workspaceID, domain.ActionAssign, true); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return s.bindings.List(ctx, agentID, workspaceID, after, limit)
}

// GetBinding returns a single binding by id (§6.1 PATCH carry-forward). The
// caller must hold `assign` on the workspace; a missing or cross-workspace
// binding returns ErrBindingNotFound (no leak). Used by the PATCH handler to
// load the current binding's carry-forward fields before issuing the update
// (the update path is revoke-old + create-new, so the new binding must repeat
// the full shape — only delivery_mode/priority are overridden).
func (s *Service) GetBinding(ctx context.Context, auth AuthContext, bindingID, agentID, workspaceID uuid.UUID) (domain.AgentBinding, error) {
	if err := s.authorize(ctx, auth, domain.TargetWorkspace, workspaceID, domain.ActionAssign, true); err != nil {
		return domain.AgentBinding{}, err
	}
	cur, err := s.bindings.Get(ctx, bindingID)
	if err != nil {
		return domain.AgentBinding{}, ErrBindingNotFound
	}
	// Cross-workspace / cross-agent guard (no existence leak): a binding in
	// another workspace or for another agent is surfaced as not-found.
	if cur.WorkspaceID != workspaceID || cur.AgentID != agentID {
		return domain.AgentBinding{}, ErrBindingNotFound
	}
	return cur, nil
}

// authorize runs an rbac.Engine.Check for the workspace target. A denial
// returns ErrBindingNotFound on both read and write management paths — the
// existence of an agent's binding set never leaks (§8.2 / §1.2). An admin
// short-circuits; a nil engine (tests only) also allows.
func (s *Service) authorize(ctx context.Context, auth AuthContext, t domain.TargetType, id uuid.UUID, action domain.Action, leak bool) error {
	if s.rbac == nil || auth.IsAdmin {
		return nil
	}
	dec, err := s.rbac.Check(ctx, auth.PrincipalID, auth.GroupIDs, t, id, action)
	if err != nil {
		if leak {
			s.recordDeniedAudit(ctx, auth, action, t, id)
		}
		return ErrBindingNotFound
	}
	if !dec.Allowed {
		if leak {
			s.recordDeniedAudit(ctx, auth, action, t, id)
		}
		return ErrBindingNotFound
	}
	return nil
}

func (s *Service) recordDeniedAudit(ctx context.Context, auth AuthContext, action domain.Action, t domain.TargetType, id uuid.UUID) {
	if s.audit == nil {
		return
	}
	pid := auth.PrincipalID
	tid := id
	s.audit.Record(ctx, string(auth.SubjectType), &pid,
		"denied."+string(action),
		string(t), &tid,
		map[string]any{"reason": "rbac deny", "subject_type": string(auth.SubjectType)},
		"", "")
}

// validateInput checks the structural invariants the DB CHECK constraints
// also enforce, so a batch reports the offending item with a precise error
// instead of one opaque constraint violation. (§4.3 / 013 CHECK constraints.)
func validateInput(in BindingInput) error {
	switch in.ScopeKind {
	case domain.BindingScopeAsset:
		if in.AssetID == nil || *in.AssetID == uuid.Nil {
			return ErrInvalidBinding
		}
	case domain.BindingScopeAssetType:
		if in.AssetType == nil || *in.AssetType == "" {
			return ErrInvalidBinding
		}
	case domain.BindingScopeWorkspace:
		// no extra field
	default:
		return ErrInvalidBinding
	}
	if in.Effect != domain.BindingAllow && in.Effect != domain.BindingDeny {
		return ErrInvalidBinding
	}
	if in.VersionPolicy == domain.BindingPinned {
		if in.ScopeKind != domain.BindingScopeAsset || in.PinnedVersionID == nil || *in.PinnedVersionID == uuid.Nil {
			return ErrInvalidBinding
		}
	}
	switch in.DeliveryMode {
	case domain.BindingDeliveryTool, domain.BindingDeliverySummary, domain.BindingDeliveryInline:
	default:
		return ErrInvalidBinding
	}
	return nil
}

// nowUTC is a seam for tests; production uses time.Now().UTC(). Kept as a
// method (not a package-level var) so a future field can swap it without
// touching call sites.
func (s *Service) nowUTC() time.Time { return time.Now().UTC() }
