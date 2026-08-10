package authz

// regression_test.go covers §8.3 回归用例 (design-docs/13 §8.3): the doc-family
// delegation (rbac.Engine.Check / VisibleDocuments routed through
// CompositeLocator + DocLocator + AsLocator) must NOT change the legacy engine's
// input/output contract. The decision outcomes for read/write/admin on
// workspace/directory/document must be byte-identical to the built-in path.
//
// The rbac package's own engine_test.go stays green untouched; these tests
// assert the DELEGATED path agrees with the BUILT-IN path across a matrix of
// grant shapes (the equivalence that lets wiring.go swap in the locator without
// behavior drift — the §1 回归红线).
//
// Fakes are shared with locator_test.go (miniRepo) and service_test.go.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildEngines wires two engines over the SAME repository: one with the
// built-in doc-family resolver, one delegating through DocLocator+AsLocator.
// Every Check/VisibleDocuments call must agree between them.
func buildEngines(repo rbac.Repository) (bare, delegated *rbac.Engine) {
	bare = rbac.NewEngine(repo)
	delegated = rbac.NewEngine(repo)
	delegated.SetLocator(AsLocator(NewDocLocator(repo)))
	return bare, delegated
}

// checkEq asserts bare and delegated produce the same Decision for the same
// (subject, groups, target, action).
func checkEq(t *testing.T, bare, delegated *rbac.Engine, ctx context.Context,
	subject uuid.UUID, groups []uuid.UUID, tt domain.TargetType, id uuid.UUID, action domain.Action) {
	t.Helper()
	d1, err1 := bare.Check(ctx, subject, groups, tt, id, action)
	d2, err2 := delegated.Check(ctx, subject, groups, tt, id, action)
	require.Equal(t, err1 == nil, err2 == nil, "error presence mismatch for %s %s: bare=%v delegated=%v", tt, action, err1, err2)
	assert.Equal(t, d1.Allowed, d2.Allowed, "Allowed mismatch for %s %s", tt, action)
	assert.Equal(t, d1.Reason, d2.Reason, "Reason mismatch for %s %s", tt, action)
}

// Test_Regression_DocFamilyCheckEquivalence runs the §8.3 regression: across a
// matrix of (action × target × grant-shape), the delegated DocLocator path
// agrees with the built-in rbac.Engine path exactly.
func Test_Regression_DocFamilyCheckEquivalence(t *testing.T) {
	ws := uuid.New()
	root := uuid.New()
	sub := uuid.New()
	doc := uuid.New()
	subj := uuid.New()
	group := uuid.New()

	repo := &miniRepo{
		grants: []domain.Grant{
			// user read on root dir (subtree) — inherited allow.
			{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
				TargetType: domain.TargetDirectory, TargetID: root, Effect: domain.EffectAllow},
			// group write on workspace.
			{SubjectType: domain.SubjectGroup, SubjectID: group, Actions: []domain.Action{domain.ActionWrite},
				TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
			// explicit deny: admin on sub directory (denies admin to everything under sub).
			{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionAdmin},
				TargetType: domain.TargetDirectory, TargetID: sub, Effect: domain.EffectDeny},
		},
		ancestors: map[uuid.UUID][]uuid.UUID{
			sub:  {root, sub},
			root: {root},
		},
		docLoc: map[uuid.UUID][2]uuid.UUID{doc: {ws, sub}},
	}

	bare, delegated := buildEngines(repo)
	ctx := context.Background()
	groups := []uuid.UUID{group}

	// Matrix: actions × targets. Every cell must agree bare-vs-delegated.
	actions := []domain.Action{domain.ActionRead, domain.ActionWrite, domain.ActionAdmin}
	for _, action := range actions {
		checkEq(t, bare, delegated, ctx, subj, groups, domain.TargetDocument, doc, action)
		checkEq(t, bare, delegated, ctx, subj, groups, domain.TargetDirectory, sub, action)
		checkEq(t, bare, delegated, ctx, subj, groups, domain.TargetDirectory, root, action)
		checkEq(t, bare, delegated, ctx, subj, groups, domain.TargetWorkspace, ws, action)
	}
}

// Test_Regression_VisibleDocumentsEquivalence asserts the delegated path's
// VisibleDocuments matches the built-in path's visible set (§8.3: "现有
// rbac.Engine ... VisibleDocuments 行为不变"). Covers workspace-level allow +
// directory + document grants, with and without a workspace-wide deny.
func Test_Regression_VisibleDocumentsEquivalence(t *testing.T) {
	ws := uuid.New()
	root := uuid.New()
	sub := uuid.New()
	docA := uuid.New() // in sub
	docB := uuid.New() // in root
	subj := uuid.New()
	group := uuid.New()

	// read on root (subtree) → docA + docB visible; plus a doc-level deny on docA.
	repo := &miniRepo{
		grants: []domain.Grant{
			{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
				TargetType: domain.TargetDirectory, TargetID: root, Effect: domain.EffectAllow},
			{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
				TargetType: domain.TargetDocument, TargetID: docA, Effect: domain.EffectDeny},
		},
		ancestors: map[uuid.UUID][]uuid.UUID{
			sub:  {root, sub},
			root: {root},
		},
		docLoc: map[uuid.UUID][2]uuid.UUID{
			docA: {ws, sub},
			docB: {ws, root},
		},
	}

	bare, delegated := buildEngines(repo)
	ctx := context.Background()
	groups := []uuid.UUID{group}

	visBare, err := bare.VisibleDocuments(ctx, subj, groups, ws)
	require.NoError(t, err)
	visDelegated, err := delegated.VisibleDocuments(ctx, subj, groups, ws)
	require.NoError(t, err)

	// Normalize to a set of visible doc IDs (excluding the uuid.Nil workspace
	// sentinel that encodes "all workspace docs visible").
	bareSet, delegSet := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for id, v := range visBare {
		if id != uuid.Nil {
			bareSet[id] = v
		}
	}
	for id, v := range visDelegated {
		if id != uuid.Nil {
			delegSet[id] = v
		}
	}
	assert.Equal(t, bareSet, delegSet, "VisibleDocuments must be identical bare vs delegated")
	// docA is denied even though root allow covers it; docB is visible.
	require.False(t, bareSet[docA], "docA must be denied (doc-level deny beats subtree allow)")
	require.True(t, bareSet[docB], "docB must be visible (subtree allow)")
	assert.Equal(t, bareSet[docA], delegSet[docA])
	assert.Equal(t, bareSet[docB], delegSet[docB])
}

// Test_Regression_WorkspaceWideDenyEquivalence covers the workspace-wide deny
// short-circuit in VisibleDocuments ("nothing visible") under both paths.
func Test_Regression_WorkspaceWideDenyEquivalence(t *testing.T) {
	ws := uuid.New()
	root := uuid.New()
	doc := uuid.New()
	subj := uuid.New()

	repo := &miniRepo{
		grants: []domain.Grant{
			// workspace-level read deny → nothing visible.
			{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
				TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectDeny},
			// and a workspace-level allow that must NOT override the deny.
			{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
				TargetType: domain.TargetWorkspace, TargetID: ws, Effect: domain.EffectAllow},
		},
		ancestors: map[uuid.UUID][]uuid.UUID{root: {root}},
		docLoc:    map[uuid.UUID][2]uuid.UUID{doc: {ws, root}},
	}

	bare, delegated := buildEngines(repo)
	ctx := context.Background()

	visBare, err := bare.VisibleDocuments(ctx, subj, nil, ws)
	require.NoError(t, err)
	visDelegated, err := delegated.VisibleDocuments(ctx, subj, nil, ws)
	require.NoError(t, err)

	// Both must report an empty visible set (deny > allow, workspace-wide).
	assert.Equal(t, visBare, visDelegated, "workspace-wide deny must yield identical empty sets")
	assert.Empty(t, visBare)
	assert.Empty(t, visDelegated)
}

// Test_Regression_NoGrantDefaultDeny asserts a subject with NO grants at all
// is default-denied identically on both paths (the §8.3 "default deny" floor).
func Test_Regression_NoGrantDefaultDeny(t *testing.T) {
	ws := uuid.New()
	doc := uuid.New()
	root := uuid.New()
	subj := uuid.New()

	repo := &miniRepo{
		grants:    nil,
		ancestors: map[uuid.UUID][]uuid.UUID{root: {root}},
		docLoc:    map[uuid.UUID][2]uuid.UUID{doc: {ws, root}},
	}

	bare, delegated := buildEngines(repo)
	ctx := context.Background()

	for _, action := range []domain.Action{domain.ActionRead, domain.ActionWrite, domain.ActionAdmin} {
		checkEq(t, bare, delegated, ctx, subj, nil, domain.TargetDocument, doc, action)
		d, _ := bare.Check(ctx, subj, nil, domain.TargetDocument, doc, action)
		assert.False(t, d.Allowed, "no grant → default deny (%s)", action)
	}
}
