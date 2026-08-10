package authz

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// miniRepo is a minimal rbac.Repository for wiring tests: it serves
// GrantsFor + DirectoryAncestors + DocumentLocation +
// DocumentsInDirectorySubtree from in-memory maps. It mirrors the rbac
// package's fakeRepo but stays local to avoid importing rbac's test files.
type miniRepo struct {
	grants    []domain.Grant
	ancestors map[uuid.UUID][]uuid.UUID
	docLoc    map[uuid.UUID][2]uuid.UUID // docID -> {workspace, directory}
}

func (m *miniRepo) GrantsFor(_ context.Context, subject uuid.UUID, groups []uuid.UUID, _ uuid.UUID) ([]domain.Grant, error) {
	var out []domain.Grant
	for _, g := range m.grants {
		if g.SubjectType == domain.SubjectUser && g.SubjectID == subject {
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
func (m *miniRepo) DirectoryAncestors(_ context.Context, dirID uuid.UUID) ([]uuid.UUID, error) {
	return m.ancestors[dirID], nil
}
func (m *miniRepo) DocumentLocation(_ context.Context, docID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	loc := m.docLoc[docID]
	return loc[0], loc[1], nil
}
func (m *miniRepo) DocumentsInDirectorySubtree(_ context.Context, dirID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for docID, loc := range m.docLoc {
		if loc[1] == dirID {
			out = append(out, docID)
		}
	}
	return out, nil
}

var _ rbac.Repository = (*miniRepo)(nil)

// TestDocLocator_DelegationAgreesWithBuiltIn wires a real DocLocator through
// AsLocator into a real rbac.Engine and checks the decision matches the
// engine's built-in doc-family path for the same grants. Guards §3.5: the
// delegation must not change Check's input/output contract.
func TestDocLocator_DelegationAgreesWithBuiltIn(t *testing.T) {
	ws := uuid.New()
	root := uuid.New()
	sub := uuid.New()
	doc := uuid.New()
	group := uuid.New()

	repo := &miniRepo{
		grants: []domain.Grant{{
			SubjectType: domain.SubjectGroup, SubjectID: group,
			Actions:    []domain.Action{domain.ActionRead},
			TargetType: domain.TargetDirectory, TargetID: root, Effect: domain.EffectAllow,
		}},
		ancestors: map[uuid.UUID][]uuid.UUID{
			sub:  {root, sub},
			root: {root},
		},
		docLoc: map[uuid.UUID][2]uuid.UUID{doc: {ws, sub}},
	}

	bare := rbac.NewEngine(repo) // built-in path

	deleg := rbac.NewEngine(repo)
	deleg.SetLocator(AsLocator(NewDocLocator(repo)))

	ctx := context.Background()
	// read via group grant on root directory → both must allow.
	d1, err := bare.Check(ctx, sub, []uuid.UUID{group}, domain.TargetDocument, doc, domain.ActionRead)
	require.NoError(t, err)
	d2, err := deleg.Check(ctx, sub, []uuid.UUID{group}, domain.TargetDocument, doc, domain.ActionRead)
	require.NoError(t, err)
	assert.True(t, d1.Allowed, "built-in path must allow group read")
	assert.Equal(t, d1.Allowed, d2.Allowed)
	assert.Equal(t, d1.Reason, d2.Reason)

	// workspace-scoped check: both must agree.
	d3, err := bare.Check(ctx, sub, []uuid.UUID{group}, domain.TargetWorkspace, ws, domain.ActionRead)
	require.NoError(t, err)
	d4, err := deleg.Check(ctx, sub, []uuid.UUID{group}, domain.TargetWorkspace, ws, domain.ActionRead)
	require.NoError(t, err)
	assert.Equal(t, d3.Allowed, d4.Allowed)
}

// TestCompositeLocator_RoutesByType ensures CompositeLocator dispatches to the
// registered child and returns ErrTargetNotFound for unregistered types.
func TestCompositeLocator_RoutesByType(t *testing.T) {
	ws := uuid.New()
	doc := uuid.New()
	repo := &miniRepo{
		ancestors: map[uuid.UUID][]uuid.UUID{},
		docLoc:    map[uuid.UUID][2]uuid.UUID{doc: {ws, uuid.Nil}},
	}
	comp := NewCompositeLocator(struct {
		Type TargetType
		Loc  ResourceLocator
	}{Type: domain.TargetDocument, Loc: NewDocLocator(repo)})

	loc, err := comp.Locate(context.Background(), domain.TargetDocument, doc)
	require.NoError(t, err)
	assert.Equal(t, ws, loc.WorkspaceID)
	require.Len(t, loc.Chain, 2, "doc chain = [doc, workspace]")
	assert.Equal(t, domain.TargetDocument, loc.Chain[0].Type)
	assert.Equal(t, doc, loc.Chain[0].ID)
	assert.Equal(t, domain.TargetWorkspace, loc.Chain[1].Type)

	_, err = comp.Locate(context.Background(), domain.TargetWorkspace, ws)
	assert.ErrorIs(t, err, ErrTargetNotFound, "unregistered type must surface as not-found")
}
