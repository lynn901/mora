package authz

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// Service is the authorization decision point (design-docs/12 §5.3, 13 §3.4).
// It composes the decision pipeline:
//
//  1. lifecycle gate     — asset/version status permits the action?
//     1b. pinned-version gate — a pinned binding's version revoked? block,
//     no auto-fallback to latest (§8.2 用例 5 / §11.4).
//  2. RBAC/ACL           — principal's read/write/admin/use grant on the
//     target chain (legacy rbac.Engine for doc-family;
//     workspace-level intersection for assets).
//  3. Agent use           — if principal is an agent, agent_bindings allow?
//     (intersection with RBAC — binding only narrows).
//  4. Binding deny        — explicit deny wins over all allow.
//  5. task scope          — Phase 0 placeholder (§19 #12 undecided).
//  6. Provider capability — issued separately by IssueDecision (§4).
//
// Invariant (附录 A #4): a binding can only NARROW an agent's reachable set,
// never grant the acting principal a capability it lacks. So step 3 takes the
// intersection of RBAC allow (for the acting principal's RBAC identity) with
// binding allow. Step 4 deny overrides.
//
// 'use' does NOT inherit from 'read' (§4.1): hasAction(use) must match 'use'
// or 'admin' explicitly. That rule is enforced HERE (not in rbac.Engine's
// hasAction), so the legacy engine and engine_test.go are untouched.
type Service struct {
	locator   ResourceLocator
	rbac      *rbac.Engine
	binding   BindingRepo
	agents    AgentRepo
	assets    AssetRepo
	versions  AssetVersionRepo
	revisions RevisionRepo
	decisions DecisionRepo
}

// NewService wires an authz.Service. rbac is the legacy engine (a sub-strategy
// for read/write/admin on doc/directory/workspace); locator resolves targets;
// the repos feed the lifecycle + agent-use + revision steps. versions reads
// knowledge_asset_versions for the pinned-version-revocation gate (§8.2 用例 5):
// a nil versions repo disables that gate (only acceptable where no agent can
// hold a pinned binding — production wiring always passes a real repo).
func NewService(locator ResourceLocator, engine *rbac.Engine, binding BindingRepo, agents AgentRepo, assets AssetRepo, versions AssetVersionRepo, revisions RevisionRepo, decisions DecisionRepo) *Service {
	return &Service{
		locator: locator, rbac: engine, binding: binding,
		agents: agents, assets: assets, versions: versions,
		revisions: revisions, decisions: decisions,
	}
}

// ErrNotFound is returned when a target cannot be resolved. Callers MUST
// surface this indistinguishably from a permission denial so existence of
// non-visible resources never leaks (§8.2 用例 1/10).
var ErrNotFound = ErrTargetNotFound

// Authorize is the linearization point (12 §5.6): it reads the current
// workspace authz revision and decides in one snapshot. For asset/agent
// targets it runs the full pipeline; for doc-family targets it delegates to
// the legacy rbac.Engine.Check (behavior unchanged).
func (s *Service) Authorize(ctx context.Context, req AuthzRequest) (AuthzContext, error) {
	rev, err := s.revisions.Current(ctx, req.WorkspaceID)
	if err != nil {
		return AuthzContext{}, err
	}
	out := AuthzContext{
		WorkspaceID:   req.WorkspaceID,
		AuthzRevision: rev,
		PrincipalType: req.PrincipalType,
		PrincipalID:   req.PrincipalID,
		ActingUserID:  req.ActingUserID,
		AgentID:       req.AgentID,
	}

	// Doc-family targets: delegate to legacy engine (read/write/admin only).
	// 'use' on a document is not a Phase 0 concern — assets are the use target.
	if isDocFamily(req.TargetType) {
		dec, err := s.rbacCheck(ctx, req)
		if err != nil {
			return out, err
		}
		out.Allowed = dec.Allowed
		out.Reason = dec.Reason
		return out, nil
	}

	// Resolve target location (existence check — no leak on miss).
	loc, err := s.locator.Locate(ctx, req.TargetType, req.TargetID)
	if err != nil {
		// Non-existent / non-visible target: deny, indistinguishable from not-found.
		out.Allowed = false
		out.Reason = "target not found"
		return out, ErrNotFound
	}
	if loc.WorkspaceID != req.WorkspaceID {
		// Cross-workspace reference (§8.2 用例 10): deny, no leak.
		out.Allowed = false
		out.Reason = "cross-workspace reference denied"
		return out, ErrNotFound
	}

	// 1. lifecycle gate (§8.2 用例 5: pinned version revocation blocks).
	if !s.lifecycleGate(ctx, req, loc) {
		out.Allowed = false
		out.Reason = "lifecycle gate: status does not permit action"
		return out, ErrNotFound
	}

	// 1b. Pinned-version-revocation gate (§8.2 用例 5 / §11.4): if an agent
	// principal holds a pinned binding on this asset whose pinned version is
	// revoked (no longer ready+published), block use — no auto-fallback to the
	// latest published version. Surfaced as ErrNotFound so a revoked pinned
	// version's (former) existence never leaks. Runs after the asset lifecycle
	// gate and before RBAC/binding narrowing: a revoked pinned version blocks
	// regardless of RBAC allow, matching the "阻断" invariant.
	if !s.pinnedVersionGate(ctx, req) {
		out.Allowed = false
		out.Reason = "lifecycle gate: pinned version revoked"
		return out, ErrNotFound
	}

	// 2. RBAC/ACL on the target chain. For an asset, RBAC is evaluated at
	// workspace scope: the acting principal must have the action on the
	// workspace (or a more specific node the chain includes).
	rbacAllow, rbacReason, err := s.rbacForTarget(ctx, req, loc)
	if err != nil {
		return out, err
	}

	// 3 & 4. Agent use + binding deny (only when principal is an agent).
	// Phase 5 §5.3: evalBindings also resolves the winning allow binding's
	// delivery_mode (tool/summary/inline) so the decision carries the delivery
	// contract for the MCP/internal delivery layer.
	bindingAllow := true // non-agent principals: no binding narrowing.
	bindingDeny := false
	delivery := domain.BindingDeliveryTool // default for non-agent / no-binding
	if req.PrincipalType == domain.SubjectAgent && req.AgentID != nil {
		delivery, bindingAllow, bindingDeny, err = s.evalBindings(ctx, *req.AgentID, req.WorkspaceID, req.TargetType, req.TargetID)
		if err != nil {
			return out, err
		}
	}

	// Decision: deny wins over everything; else require RBAC AND binding allow.
	// Any denial on an asset/agent target surfaces as ErrNotFound so existence
	// never leaks to the caller (§8.2 用例 1/2/3/4/6/9/10); the precise reason
	// is kept in out.Reason for audit/observability.
	if bindingDeny {
		out.Allowed = false
		out.Reason = "binding explicit deny"
		return out, ErrNotFound
	}
	if !rbacAllow {
		out.Allowed = false
		out.Reason = rbacReason
		return out, ErrNotFound
	}
	if !bindingAllow {
		out.Allowed = false
		out.Reason = "no binding allows this asset for the agent"
		return out, ErrNotFound
	}
	out.Allowed = true
	out.Reason = "authorized"
	out.DeliveryMode = delivery
	return out, nil
}

// VisibleAssets filters a set of asset IDs down to those the principal may use
// (§3.4, 存在性不泄露). Phase 0: returns the subset passing Authorize for
// ActionUse. Callers that pass an empty candidate set get an empty result.
func (s *Service) VisibleAssets(ctx context.Context, scope ListScope, candidates []uuid.UUID) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(candidates))
	for _, id := range candidates {
		req := AuthzRequest{
			WorkspaceID:   scope.WorkspaceID,
			PrincipalType: scope.PrincipalType,
			PrincipalID:   scope.PrincipalID,
			GroupIDs:      scope.GroupIDs,
			ActingUserID:  scope.ActingUserID,
			AgentID:       scope.AgentID,
			TargetType:    domain.TargetAsset,
			TargetID:      id,
			Action:        domain.ActionUse,
		}
		dec, err := s.Authorize(ctx, req)
		if err != nil {
			// Non-existent / non-visible: skip (no existence leak).
			continue
		}
		if dec.Allowed {
			out = append(out, id)
		}
	}
	return out, nil
}

// IssueDecision records an authorization_decision and returns a signed
// short-lived capability for a Provider to validate (§4). Phase 0 stub:
// records the decision row and returns the DecisionID; the signed token is
// filled by DelegatedManager (see delegated.go) — this method is the seam.
func (s *Service) IssueDecision(ctx context.Context, req AuthzRequest, audience string) (DecisionCapability, error) {
	dec, err := s.Authorize(ctx, req)
	if err != nil {
		return DecisionCapability{}, err
	}
	if !dec.Allowed {
		return DecisionCapability{}, errors.New("authz: cannot issue capability for denied decision")
	}
	rec := DecisionRecord{
		WorkspaceID:   req.WorkspaceID,
		AuthzRevision: dec.AuthzRevision,
		PrincipalType: req.PrincipalType,
		PrincipalID:   req.PrincipalID,
		ActingUserID:  req.ActingUserID,
		AgentID:       req.AgentID,
		Action:        req.Action,
		Audience:      audience,
		// ScopeHash / NonceHash / ExpiresAt set by the delegated/decision manager
		// that wraps this record — kept minimal here so the seam is explicit.
	}
	id, err := s.decisions.Record(ctx, rec)
	if err != nil {
		return DecisionCapability{}, err
	}
	return DecisionCapability{DecisionID: id, AuthzRevision: dec.AuthzRevision}, nil
}

// --- pipeline helpers ---

// rbacCheck delegates a doc-family check to the legacy engine. The engine's
// Check/VisibleDocuments contract is unchanged (regression red line).
func (s *Service) rbacCheck(ctx context.Context, req AuthzRequest) (rbac.Decision, error) {
	subject, groups := s.rbacSubject(ctx, req)
	return s.rbac.Check(ctx, subject, groups, domain.TargetType(req.TargetType), req.TargetID, req.Action)
}

// rbacSubject resolves the RBAC subject identity for a request, returning the
// subject and its group memberships:
//   - agent acting on behalf of a user  → the user + the user's groups (their
//     RBAC intersects the binding)
//   - agent self (service account)      → the service account id (§8.2 用例 3);
//     a service account holds no group memberships → nil groups
//   - user / service_account            → themselves + their groups
//
// GroupIDs is plumbed through so group-inherited grants (subject_type=group)
// are visible on the Service path — handlers already pass the same groups to
// rbac.Engine.Check via AuthState (PR2 gap #2). For an agent acting on behalf
// of a user we forward req.GroupIDs (the acting user's groups); for agent self
// the groups stay nil.
func (s *Service) rbacSubject(ctx context.Context, req AuthzRequest) (uuid.UUID, []uuid.UUID) {
	if req.PrincipalType == domain.SubjectAgent {
		if req.ActingUserID != nil {
			return *req.ActingUserID, req.GroupIDs
		}
		// agent self: surface the agent's service account as the RBAC subject.
		// Looked up lazily; on miss the engine returns default-deny (no leak).
		if req.AgentID != nil {
			if a, err := s.agents.Get(ctx, *req.AgentID); err == nil && a.ServiceAccountID != nil {
				return *a.ServiceAccountID, nil
			}
		}
		return uuid.Nil, nil
	}
	return req.PrincipalID, req.GroupIDs
}

// rbacForTarget evaluates RBAC over the resolved target chain. For asset/agent
// targets it checks each chain node using the legacy engine's Check.
//
// 'use' must not inherit from 'read' (§4.1): the legacy engine's hasAction
// only matches 'use' against 'use' or 'admin' (never 'read'), so asking the
// engine directly for ActionUse is sufficient and correct. The intersection
// with binding allow happens in the caller (Authorize).
func (s *Service) rbacForTarget(ctx context.Context, req AuthzRequest, loc Location) (bool, string, error) {
	subject, groups := s.rbacSubject(ctx, req)
	for _, n := range loc.Chain {
		dec, err := s.rbac.Check(ctx, subject, groups, domain.TargetType(n.Type), n.ID, req.Action)
		if err != nil {
			return false, "", err
		}
		if dec.Allowed {
			return true, "rbac allow", nil
		}
	}
	return false, "rbac default deny", nil
}

// evalBindings returns (deliveryMode, allowCovered, explicitDeny, err) for
// an agent's bindings against the target (Phase 5 §5.3). A binding covers the
// target when its scope matches (asset / workspace / asset_type). Explicit
// deny anywhere → deny (deny beats allow, §8.2 用例 4). Otherwise the agent
// needs at least one allow covering the target.
//
// delivery_mode resolution (§5.3): bindings are read priority DESC (the
// repo's ORDER BY). The winning allow is the highest-priority allow that
// covers the target — its DeliveryMode is the delivery contract for the
// MCP/internal layer (tool/summary/inline). A deny covering the target at
// equal-or-higher priority would already have returned (deny wins); among
// allow bindings the first allow encountered in priority order wins. If no
// allow covers the target, delivery defaults to tool (the decision is denied
// anyway, so the value is not delivered).
func (s *Service) evalBindings(ctx context.Context, agentID, workspaceID uuid.UUID, t TargetType, targetID uuid.UUID) (domain.BindingDeliveryMode, bool, bool, error) {
	bindings, err := s.binding.ActiveForAgent(ctx, agentID, workspaceID)
	if err != nil {
		return domain.BindingDeliveryTool, false, false, err
	}
	allowCovered := false
	delivery := domain.BindingDeliveryTool
	for _, b := range bindings {
		if !bindingCoversTarget(b, t, targetID) {
			continue
		}
		if b.Effect == domain.BindingDeny {
			return domain.BindingDeliveryTool, false, true, nil // explicit deny beats allow (§8.2 用例 4)
		}
		if b.Effect == domain.BindingAllow {
			allowCovered = true
			// First allow in priority-DESC order wins (bindings are ordered).
			if delivery == domain.BindingDeliveryTool && b.DeliveryMode != "" {
				delivery = b.DeliveryMode
			}
		}
	}
	return delivery, allowCovered, false, nil
}

// lifecycleGate checks the target's status permits the action (§8.2 用例 5).
// For assets: archived/rejected/deprecated blocks use (except sync/deprecate).
func (s *Service) lifecycleGate(ctx context.Context, req AuthzRequest, loc Location) bool {
	// Chain[0] is the most-specific node (the target itself).
	if len(loc.Chain) == 0 {
		return true
	}
	if req.TargetType == domain.TargetAsset {
		a, err := s.assets.Get(ctx, req.TargetID)
		if err != nil {
			return false
		}
		switch a.Status {
		case domain.AssetPublished, domain.AssetReviewing, domain.AssetDraft:
			return true
		case domain.AssetDeprecated:
			return req.Action == domain.ActionSync // allow sync to re-publish
		default: // archived, rejected
			return false
		}
	}
	return true
}

// pinnedVersionGate enforces the pinned-version-revocation invariant
// (§8.2 用例 5 / §11.4: 固定版本不存在/被撤权 → 阻断，不自动回退最新版).
//
// It applies only to an agent principal acting on an asset via a binding
// whose VersionPolicy is 'pinned' and whose scope covers that asset. For
// each such binding the gate loads the pinned version's build/governance
// state: a usable version (build_status='ready' AND governance_status=
// 'published', the same invariant current_version_id enforces) lets the
// request proceed; a missing row or any revoked state (deprecated /
// superseded / rejected / failed / in-flight) blocks use — no fallback to
// the latest published version.
//
// Non-asset targets and non-agent principals have no pinned binding, so the
// gate is a no-op (returns true). A missing versions repo (nil) also returns
// true; production wiring must pass a real AssetVersionRepo so the gate is
// active wherever pinned bindings are reachable.
//
// The gate only narrows (a binding can never grant a capability the
// principal lacks — 不变量 A #4): an agent with no pinned binding, or one
// whose pinned version is still usable, falls through to the RBAC +
// binding-narrowing steps unchanged.
func (s *Service) pinnedVersionGate(ctx context.Context, req AuthzRequest) bool {
	// Only agents can hold bindings; only assets are pin-able
	// (§4.3: pinned requires scope_kind=asset).
	if s.versions == nil || req.TargetType != domain.TargetAsset ||
		req.PrincipalType != domain.SubjectAgent || req.AgentID == nil {
		return true
	}
	bindings, err := s.binding.ActiveForAgent(ctx, *req.AgentID, req.WorkspaceID)
	if err != nil {
		// Fail closed: a binding read failure blocks rather than risks
		// authorizing a revoked pinned version. Surfaced as ErrNotFound
		// (no existence leak) by the caller.
		return false
	}
	for _, b := range bindings {
		if b.VersionPolicy != domain.BindingPinned || b.PinnedVersionID == nil {
			continue
		}
		if !bindingCoversTarget(b, req.TargetType, req.TargetID) {
			continue
		}
		// The binding pins a version on THIS asset. If that version is no
		// longer usable, block — the agent may NOT silently use the latest
		// published version instead (§11.4).
		v, err := s.versions.Get(ctx, *b.PinnedVersionID)
		if err != nil || !v.IsUsable() {
			return false
		}
	}
	return true
}

// isDocFamily reports whether a target type is handled by the legacy rbac path.
func isDocFamily(t TargetType) bool {
	switch domain.TargetType(t) {
	case domain.TargetWorkspace, domain.TargetDirectory, domain.TargetDocument:
		return true
	}
	return false
}

// bindingCoversTarget reports whether a binding's scope matches the target.
func bindingCoversTarget(b domain.AgentBinding, t TargetType, targetID uuid.UUID) bool {
	if domain.TargetType(t) == domain.TargetAsset {
		switch b.ScopeKind {
		case domain.BindingScopeWorkspace:
			return true
		case domain.BindingScopeAsset:
			return b.AssetID != nil && *b.AssetID == targetID
		case domain.BindingScopeAssetType:
			// asset_type scope covers assets of that type; Phase 0 allows the
			// binding's declared type to match generically (caller resolves).
			return true
		}
	}
	if domain.TargetType(t) == domain.TargetAgent {
		// Bindings target assets, not agents; an agent target is not covered.
		return b.ScopeKind == domain.BindingScopeWorkspace
	}
	return b.ScopeKind == domain.BindingScopeWorkspace
}
