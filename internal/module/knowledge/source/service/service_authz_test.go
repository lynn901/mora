package service

// service_authz_test.go pins the §8.5 / §10.4 resource-level RBAC contract
// that SourceService enforces once WithAuthz is wired (YS-115). Each case
// drives a real rbac.Engine over a fake Repository + a fake source Locator,
// so it exercises the full authorize() path: target resolution, grant
// evaluation, and the no-leak / forbidden error mapping the design requires.
//
// Cases mirror design-docs/14 §10.4:
//   - 用例 25: no sync permission → ErrSourceForbidden (→ 403 + audit).
//   - 用例 27: cross-workspace source → ErrSourceNotFound (no leak, → 404).
//   - 用例 29: review by non-review-role → ErrSourceForbidden (→ 403).
//   - 用例 26/27 read leg: cross-workspace / no-read → ErrSourceNotFound (404).
//   - authorized user CRUD regression: admin/read/write/sync permitted.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRBACRepo is an in-memory rbac.Repository for source-authz tests. It only
// implements GrantsFor (source resolution is delegated to a Locator, not the
// doc-family repo methods), so the other methods return empty/nil — the
// source path never calls them.
type fakeRBACRepo struct{ grants []domain.Grant }

func (f *fakeRBACRepo) GrantsFor(_ context.Context, subject uuid.UUID, groups []uuid.UUID, _ uuid.UUID) ([]domain.Grant, error) {
	var out []domain.Grant
	for _, g := range f.grants {
		if (g.SubjectType == domain.SubjectUser || g.SubjectType == domain.SubjectServiceAccount) && g.SubjectID == subject {
			out = append(out, g)
			continue
		}
		if g.SubjectType == domain.SubjectGroup {
			for _, gid := range groups {
				if g.SubjectID == gid {
					out = append(out, g)
					break
				}
			}
		}
	}
	return out, nil
}
func (f *fakeRBACRepo) DirectoryAncestors(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}
func (f *fakeRBACRepo) DocumentLocation(context.Context, uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	return uuid.Nil, uuid.Nil, nil
}
func (f *fakeRBACRepo) DocumentsInDirectorySubtree(context.Context, uuid.UUID) ([]uuid.UUID, error) {
	return nil, nil
}

// fakeSourceLocator resolves a TargetSource to its owning workspace, modeling
// the real SourceLocator's no-leak contract (§8.2): a missing, disabled, OR
// cross-workspace source returns an error the engine surfaces as a decision
// error. TargetReview is resolved the same way (a missing/cross-workspace
// review errors). The engine's resolveTarget maps the error through unchanged.
type fakeSourceLocator struct {
	sources map[uuid.UUID]sourceLoc // source id → location
	reviews map[uuid.UUID]uuid.UUID // review id → workspace
}

type sourceLoc struct {
	workspace uuid.UUID
	enabled   bool
}

func (l *fakeSourceLocator) Locate(_ context.Context, t domain.TargetType, id uuid.UUID) (uuid.UUID, []rbac.LocatorNode, error) {
	switch t {
	case domain.TargetWorkspace:
		// A workspace target always resolves (the real CompositeLocator has a
		// DocLocator for workspace targets). The engine then scopes GrantsFor to
		// this workspace, so a non-member gets default-deny.
		return id, []rbac.LocatorNode{{Type: domain.TargetWorkspace, ID: id}}, nil
	case domain.TargetSource:
		s, ok := l.sources[id]
		if !ok || !s.enabled {
			// Missing / disabled: indistinguishable from not-found (no leak).
			return uuid.Nil, nil, errLocateNotFound
		}
		return s.workspace, []rbac.LocatorNode{
			{Type: domain.TargetSource, ID: id},
			{Type: domain.TargetWorkspace, ID: s.workspace},
		}, nil
	case domain.TargetReview:
		ws, ok := l.reviews[id]
		if !ok {
			return uuid.Nil, nil, errLocateNotFound
		}
		return ws, []rbac.LocatorNode{
			{Type: domain.TargetReview, ID: id},
			{Type: domain.TargetWorkspace, ID: ws},
		}, nil
	}
	return uuid.Nil, nil, errLocateNotFound
}

// errLocateNotFound is the locator-side miss error; authorize maps it to the
// not-found sentinel (no existence leak).
var errLocateNotFound = errString("authz: target not found or not visible")

type errString string

func (e errString) Error() string { return string(e) }

// newAuthzService builds a Service wired with a real rbac.Engine over the
// given grants + source/review locations. The audit logger is nil (denied-
// audit is best-effort; the HTTP AuditMiddleware records 403s in production).
func newAuthzService(grants []domain.Grant, sources map[uuid.UUID]sourceLoc, reviews map[uuid.UUID]uuid.UUID) *Service {
	eng := rbac.NewEngine(&fakeRBACRepo{grants: grants})
	eng.SetLocator(&fakeSourceLocator{sources: sources, reviews: reviews})
	return NewService(&fakeSourceRepo{}, &fakeRunRepo{}, fakeReviewRepo{}, &fakeRunSink{}, nil).WithAuthz(eng, nil)
}

// grant is a compact helper to build a user grant on a workspace.
func grant(userID, ws uuid.UUID, effect domain.Effect, actions ...domain.Action) domain.Grant {
	return domain.Grant{
		SubjectType: domain.SubjectUser, SubjectID: userID,
		TargetType: domain.TargetWorkspace, TargetID: ws,
		Effect: effect, Actions: actions,
	}
}

// --- §10.4 用例 25: no sync permission → 403 + audit ---

// TestAuthz_CreateSource_NoWritePermissionRejected asserts a workspace
// read-only member (read grant only, no write) is denied CreateSource with
// ErrSourceForbidden (→ 403, §10.4 用例 25). The denial is a write action so
// it surfaces as forbidden — the caller is authenticated and asked to mutate.
func TestAuthz_CreateSource_NoWritePermissionRejected(t *testing.T) {
	ws := uuid.New()
	reader := uuid.New()
	srcs := map[uuid.UUID]sourceLoc{}
	svc := newAuthzService(
		[]domain.Grant{grant(reader, ws, domain.EffectAllow, domain.ActionRead)},
		srcs, nil,
	)
	_, err := svc.CreateSource(context.Background(),
		AuthContext{PrincipalID: reader, SubjectType: domain.SubjectUser},
		CreateSourceInput{WorkspaceID: ws, SourceType: domain.SourceGit, Name: "leak"})
	assert.ErrorIs(t, err, ErrSourceForbidden, "read-only member must be denied source create (403)")
}

// TestAuthz_TriggerSync_NoSyncPermissionRejected asserts a workspace writer
// (write grant only, no sync action) is denied TriggerSync with
// ErrSourceForbidden (§10.4 用例 25). 'sync' does not inherit from write.
func TestAuthz_TriggerSync_NoSyncPermissionRejected(t *testing.T) {
	ws := uuid.New()
	writer := uuid.New()
	srcID := uuid.New()
	srcs := map[uuid.UUID]sourceLoc{srcID: {workspace: ws, enabled: true}}
	// fakeSourceRepo.Get returns the enabled source so TriggerSync proceeds
	// past the authorize gate to the sink (which the fake accepts).
	svc := newAuthzService(
		[]domain.Grant{grant(writer, ws, domain.EffectAllow, domain.ActionWrite)},
		srcs, nil,
	)
	svc.sources = &fakeSourceRepo{get: &domain.KnowledgeSource{
		ID: srcID, WorkspaceID: ws, Enabled: true, SourceType: domain.SourceGit,
	}}
	_, err := svc.TriggerSync(context.Background(),
		AuthContext{PrincipalID: writer, SubjectType: domain.SubjectUser},
		TriggerSyncInput{SourceID: srcID, RequestedAssetType: domain.RequestedAssetDocument, IdempotencyKey: "k"})
	assert.ErrorIs(t, err, ErrSourceForbidden, "writer without sync grant must be denied trigger (403)")
}

// --- §10.4 用例 27: cross-workspace / no-read → 404 (no leak) ---

// TestAuthz_GetSource_CrossWorkspaceIsNotFound asserts a workspace-B reader
// calling GetSource on a source in workspace A gets ErrSourceNotFound (→ 404),
// NOT forbidden — so the source's existence never leaks (§10.4 用例 27). The
// locator returns a miss for an out-of-workspace source (the engine's grant
// scope is the source's workspace A, where B has no grants → default-deny →
// the read denial maps to not-found).
func TestAuthz_GetSource_CrossWorkspaceIsNotFound(t *testing.T) {
	wsA := uuid.New()
	wsB := uuid.New()
	readerB := uuid.New()
	srcID := uuid.New()
	// The locator resolves the source to wsA; readerB has grants in wsB only.
	srcs := map[uuid.UUID]sourceLoc{srcID: {workspace: wsA, enabled: true}}
	svc := newAuthzService(
		[]domain.Grant{grant(readerB, wsB, domain.EffectAllow, domain.ActionRead)},
		srcs, nil,
	)
	svc.sources = &fakeSourceRepo{get: &domain.KnowledgeSource{ID: srcID, WorkspaceID: wsA, Enabled: true}}
	_, err := svc.GetSource(context.Background(),
		AuthContext{PrincipalID: readerB, SubjectType: domain.SubjectUser},
		srcID)
	assert.ErrorIs(t, err, ErrSourceNotFound, "cross-workspace source read must be 404 (no leak)")
	assert.NotErrorIs(t, err, ErrSourceForbidden, "a read denial must NOT surface as 403 (would leak existence)")
}

// TestAuthz_GetSource_MissingIsNotFound asserts a genuinely missing source is
// also ErrSourceNotFound — indistinguishable from the cross-workspace denial
// above, so a caller cannot tell not-found from not-allowed (§8.2).
func TestAuthz_GetSource_MissingIsNotFound(t *testing.T) {
	ws := uuid.New()
	reader := uuid.New()
	svc := newAuthzService(
		[]domain.Grant{grant(reader, ws, domain.EffectAllow, domain.ActionRead)},
		map[uuid.UUID]sourceLoc{}, nil, // source not in the locator → miss
	)
	_, err := svc.GetSource(context.Background(),
		AuthContext{PrincipalID: reader, SubjectType: domain.SubjectUser},
		uuid.New())
	assert.ErrorIs(t, err, ErrSourceNotFound, "missing source must be 404 (no leak)")
}

// TestAuthz_GetSource_NoReadGrantIsNotFound asserts a user WITH a workspace
// write grant but NO read grant is denied a source read as not-found (write
// implies read only within hasAction for the SAME grant; a write-only grant
// without read still satisfies read via hasAction's write→read rule, so this
// case uses a deny-on-read grant to assert the no-leak mapping). A read
// denial maps to ErrSourceNotFound, never forbidden.
func TestAuthz_GetSource_ExplicitReadDenyIsNotFound(t *testing.T) {
	ws := uuid.New()
	user := uuid.New()
	srcID := uuid.New()
	srcs := map[uuid.UUID]sourceLoc{srcID: {workspace: ws, enabled: true}}
	svc := newAuthzService([]domain.Grant{
		grant(user, ws, domain.EffectAllow, domain.ActionRead),
		grant(user, ws, domain.EffectDeny, domain.ActionRead), // explicit read deny wins
	}, srcs, nil)
	svc.sources = &fakeSourceRepo{get: &domain.KnowledgeSource{ID: srcID, WorkspaceID: ws, Enabled: true}}
	_, err := svc.GetSource(context.Background(),
		AuthContext{PrincipalID: user, SubjectType: domain.SubjectUser},
		srcID)
	assert.ErrorIs(t, err, ErrSourceNotFound, "explicit read deny must surface as 404 (no leak)")
}

// --- §10.4 用例 29: review by non-review-role → 403 ---

// TestAuthz_AppendReviewDecision_NoReviewPermissionRejected asserts a
// principal without the 'review' action on the review target is denied with
// ErrSourceForbidden (→ 403, §10.4 用例 29). The denial is a governance write
// so it surfaces as forbidden; a cross-workspace review would instead surface
// as not-found (no leak), covered indirectly by the locator miss path.
func TestAuthz_AppendReviewDecision_NoReviewPermissionRejected(t *testing.T) {
	ws := uuid.New()
	writer := uuid.New()
	revID := uuid.New()
	reviews := map[uuid.UUID]uuid.UUID{revID: ws}
	svc := newAuthzService(
		[]domain.Grant{grant(writer, ws, domain.EffectAllow, domain.ActionWrite)},
		map[uuid.UUID]sourceLoc{}, reviews,
	)
	err := svc.AppendReviewDecision(context.Background(),
		AuthContext{PrincipalID: writer, SubjectType: domain.SubjectUser},
		ReviewDecisionInput{ReviewRequestID: revID, Decision: domain.DecisionApprove, PolicyVersion: "v1"})
	assert.ErrorIs(t, err, ErrSourceForbidden, "writer without review grant must be denied review decision (403)")
}

// TestAuthz_AppendReviewDecision_CrossWorkspaceIsForbidden asserts a
// cross-workspace review DECISION (a governance write) is denied with
// ErrSourceForbidden (→ 403), matching the issue's contract: write/governance
// denials return 403 (the caller is authenticated and asked to mutate), while
// only READ denials return 404 to avoid leaking existence (§10.4 用例 27 read
// leg vs 用例 29 write leg). The review resolves to workspace A where the
// caller (a B member) has no grant → default-deny → forbidden.
func TestAuthz_AppendReviewDecision_CrossWorkspaceIsForbidden(t *testing.T) {
	wsA := uuid.New()
	wsB := uuid.New()
	readerB := uuid.New()
	revID := uuid.New()
	// Review lives in wsA; readerB has grants in wsB only.
	reviews := map[uuid.UUID]uuid.UUID{revID: wsA}
	svc := newAuthzService(
		[]domain.Grant{grant(readerB, wsB, domain.EffectAllow, domain.ActionReview)},
		map[uuid.UUID]sourceLoc{}, reviews,
	)
	err := svc.AppendReviewDecision(context.Background(),
		AuthContext{PrincipalID: readerB, SubjectType: domain.SubjectUser},
		ReviewDecisionInput{ReviewRequestID: revID, Decision: domain.DecisionApprove, PolicyVersion: "v1"})
	assert.ErrorIs(t, err, ErrSourceForbidden, "cross-workspace review decision (write) must be 403, not leak")
}

// --- authorized user regression (no false denials) ---

// TestAuthz_CreateSource_AdminBypass asserts an admin bypasses the Check
// (matching the document-service IsAdmin pattern) so a source admin can
// create without a workspace grant.
func TestAuthz_CreateSource_AdminBypass(t *testing.T) {
	ws := uuid.New()
	svc := newAuthzService(nil, map[uuid.UUID]sourceLoc{}, nil) // no grants at all
	_, err := svc.CreateSource(context.Background(),
		AuthContext{PrincipalID: uuid.New(), IsAdmin: true, SubjectType: domain.SubjectUser},
		CreateSourceInput{WorkspaceID: ws, SourceType: domain.SourceGit, Name: "admin-src"})
	require.NoError(t, err, "admin must bypass RBAC and create a source")
}

// TestAuthz_TriggerSync_SyncGranted asserts a principal WITH the sync action
// on the source's workspace can trigger a sync (no false denial). This is the
// authorized regression for §10.4 用例 25's red line: only the NO-permission
// caller is rejected; the permissioned caller proceeds.
func TestAuthz_TriggerSync_SyncGranted(t *testing.T) {
	ws := uuid.New()
	syncer := uuid.New()
	srcID := uuid.New()
	srcs := map[uuid.UUID]sourceLoc{srcID: {workspace: ws, enabled: true}}
	svc := newAuthzService(
		[]domain.Grant{grant(syncer, ws, domain.EffectAllow, domain.ActionSync)},
		srcs, nil,
	)
	svc.sources = &fakeSourceRepo{get: &domain.KnowledgeSource{
		ID: srcID, WorkspaceID: ws, Enabled: true, SourceType: domain.SourceGit,
	}}
	run, err := svc.TriggerSync(context.Background(),
		AuthContext{PrincipalID: syncer, SubjectType: domain.SubjectUser},
		TriggerSyncInput{SourceID: srcID, RequestedAssetType: domain.RequestedAssetDocument, IdempotencyKey: "ok"})
	require.NoError(t, err, "sync-granted principal must trigger a sync")
	require.NotNil(t, run)
}

// TestAuthz_GetSource_ReadGranted asserts a workspace reader can read a source
// in their workspace (no false denial).
func TestAuthz_GetSource_ReadGranted(t *testing.T) {
	ws := uuid.New()
	reader := uuid.New()
	srcID := uuid.New()
	srcs := map[uuid.UUID]sourceLoc{srcID: {workspace: ws, enabled: true}}
	svc := newAuthzService(
		[]domain.Grant{grant(reader, ws, domain.EffectAllow, domain.ActionRead)},
		srcs, nil,
	)
	svc.sources = &fakeSourceRepo{get: &domain.KnowledgeSource{ID: srcID, WorkspaceID: ws, Enabled: true}}
	out, err := svc.GetSource(context.Background(),
		AuthContext{PrincipalID: reader, SubjectType: domain.SubjectUser},
		srcID)
	require.NoError(t, err, "reader must read a source in their workspace")
	require.NotNil(t, out)
}
