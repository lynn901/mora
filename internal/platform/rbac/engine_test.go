package rbac

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRepo is an in-memory Repository for engine unit tests.
type fakeRepo struct {
	grants    []domain.Grant
	ancestors map[uuid.UUID][]uuid.UUID
	docLoc    map[uuid.UUID][2]uuid.UUID // docID -> {workspace, directory}
}

func (f *fakeRepo) GrantsFor(_ context.Context, subject uuid.UUID, groups []uuid.UUID, ws uuid.UUID) ([]domain.Grant, error) {
	var out []domain.Grant
	for _, g := range f.grants {
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

func (f *fakeRepo) DirectoryAncestors(_ context.Context, dirID uuid.UUID) ([]uuid.UUID, error) {
	return f.ancestors[dirID], nil
}

func (f *fakeRepo) DocumentLocation(_ context.Context, docID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	loc := f.docLoc[docID]
	return loc[0], loc[1], nil
}

func (f *fakeRepo) DocumentsInDirectorySubtree(_ context.Context, dirID uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for docID, loc := range f.docLoc {
		if loc[1] == dirID {
			out = append(out, docID)
		}
	}
	return out, nil
}

func newFake() *fakeRepo {
	return &fakeRepo{
		ancestors: map[uuid.UUID][]uuid.UUID{},
		docLoc:    map[uuid.UUID][2]uuid.UUID{},
	}
}

// TestDecide_DefaultDeny: no grants → denied.
func TestDecide_DefaultDeny(t *testing.T) {
	d := decide(nil, []node{{typ: domain.TargetDocument, id: uuid.New()}}, domain.ActionRead)
	assert.False(t, d.Allowed)
	assert.Equal(t, "default deny", d.Reason)
}

// TestDecide_ExplicitAllow.
func TestDecide_ExplicitAllow(t *testing.T) {
	doc := uuid.New()
	grants := []domain.Grant{{
		SubjectType: domain.SubjectUser, SubjectID: uuid.New(),
		Actions:    []domain.Action{domain.ActionRead},
		TargetType: domain.TargetDocument, TargetID: doc, Effect: domain.EffectAllow,
	}}
	d := decide(grants, []node{{typ: domain.TargetDocument, id: doc}}, domain.ActionRead)
	assert.True(t, d.Allowed)
}

// TestDecide_ExplicitDenyBeatsAllow: deny at same level wins.
func TestDecide_ExplicitDenyBeatsAllow(t *testing.T) {
	doc := uuid.New()
	subj := uuid.New()
	grants := []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
			TargetType: domain.TargetDocument, TargetID: doc, Effect: domain.EffectAllow},
		{SubjectType: domain.SubjectGroup, SubjectID: uuid.New(), Actions: []domain.Action{domain.ActionRead},
			TargetType: domain.TargetDocument, TargetID: doc, Effect: domain.EffectDeny},
	}
	d := decide(grants, []node{{typ: domain.TargetDocument, id: doc}}, domain.ActionRead)
	assert.False(t, d.Allowed)
	assert.Equal(t, "explicit deny", d.Reason)
}

// TestDecide_InheritedAllow: allow at parent directory inherits to child doc.
func TestDecide_InheritedAllow(t *testing.T) {
	ws := uuid.New()
	dir := uuid.New()
	doc := uuid.New()
	subj := uuid.New()
	grants := []domain.Grant{{
		SubjectType: domain.SubjectUser, SubjectID: subj,
		Actions:    []domain.Action{domain.ActionRead},
		TargetType: domain.TargetDirectory, TargetID: dir, Effect: domain.EffectAllow,
	}}
	// chain: doc (no grant) -> dir (allow) -> ws
	chain := []node{
		{typ: domain.TargetDocument, id: doc},
		{typ: domain.TargetDirectory, id: dir},
		{typ: domain.TargetWorkspace, id: ws},
	}
	d := decide(grants, chain, domain.ActionRead)
	assert.True(t, d.Allowed)
}

// TestDecide_ChildDenyOverridesParentAllow: deny at doc level beats allow at dir.
func TestDecide_ChildDenyOverridesParentAllow(t *testing.T) {
	ws := uuid.New()
	dir := uuid.New()
	doc := uuid.New()
	subj := uuid.New()
	grants := []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
			TargetType: domain.TargetDirectory, TargetID: dir, Effect: domain.EffectAllow},
		{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
			TargetType: domain.TargetDocument, TargetID: doc, Effect: domain.EffectDeny},
	}
	chain := []node{
		{typ: domain.TargetDocument, id: doc},
		{typ: domain.TargetDirectory, id: dir},
		{typ: domain.TargetWorkspace, id: ws},
	}
	d := decide(grants, chain, domain.ActionRead)
	assert.False(t, d.Allowed)
	assert.Equal(t, "explicit deny", d.Reason)
}

// TestDecide_AdminImpliesAll: admin action grants read+write+admin.
func TestDecide_AdminImpliesAll(t *testing.T) {
	doc := uuid.New()
	subj := uuid.New()
	grants := []domain.Grant{{
		SubjectType: domain.SubjectUser, SubjectID: subj,
		Actions:    []domain.Action{domain.ActionAdmin},
		TargetType: domain.TargetDocument, TargetID: doc, Effect: domain.EffectAllow,
	}}
	chain := []node{{typ: domain.TargetDocument, id: doc}}
	assert.True(t, decide(grants, chain, domain.ActionRead).Allowed)
	assert.True(t, decide(grants, chain, domain.ActionWrite).Allowed)
	assert.True(t, decide(grants, chain, domain.ActionAdmin).Allowed)
}

// TestDecide_WriteImpliesRead.
func TestDecide_WriteImpliesRead(t *testing.T) {
	doc := uuid.New()
	grants := []domain.Grant{{
		SubjectType: domain.SubjectUser, SubjectID: uuid.New(),
		Actions:    []domain.Action{domain.ActionWrite},
		TargetType: domain.TargetDocument, TargetID: doc, Effect: domain.EffectAllow,
	}}
	chain := []node{{typ: domain.TargetDocument, id: doc}}
	assert.True(t, decide(grants, chain, domain.ActionRead).Allowed)
	assert.False(t, decide(grants, chain, domain.ActionAdmin).Allowed)
}

// TestEngine_Check_FullChain exercises the Engine.Check end-to-end with a
// directory tree: workspace -> dir -> subdir -> doc.
func TestEngine_Check_FullChain(t *testing.T) {
	ws := uuid.New()
	root := uuid.New()
	sub := uuid.New()
	doc := uuid.New()
	subj := uuid.New()
	group := uuid.New()

	f := newFake()
	// root directory ancestors: [ws-as-workspace-root-id]; we model ancestors
	// as the directory chain root-first. For DirectoryAncestors(sub) we return
	// [root, sub]; the engine uses ws id for workspace scoping.
	f.ancestors[sub] = []uuid.UUID{root, sub}
	f.ancestors[root] = []uuid.UUID{root}
	f.docLoc[doc] = [2]uuid.UUID{ws, sub}

	// Grant read on root directory (subtree) via group membership.
	f.grants = []domain.Grant{{
		SubjectType: domain.SubjectGroup, SubjectID: group,
		Actions:    []domain.Action{domain.ActionRead},
		TargetType: domain.TargetDirectory, TargetID: root, Effect: domain.EffectAllow,
	}}

	eng := NewEngine(f)
	// Engine.locate for TargetDirectory uses ancestors[0] as workspace — for
	// directory checks we pass the directory directly; here test document.
	d, err := eng.Check(context.Background(), subj, []uuid.UUID{group}, domain.TargetDocument, doc, domain.ActionRead)
	require.NoError(t, err)
	assert.True(t, d.Allowed, "group read on root dir should inherit to doc in sub")

	// Write not granted.
	d, err = eng.Check(context.Background(), subj, []uuid.UUID{group}, domain.TargetDocument, doc, domain.ActionWrite)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
}

// TestEngine_Check_DenyAtSubdirBlocksDoc.
func TestEngine_Check_DenyAtSubdirBlocksDoc(t *testing.T) {
	ws := uuid.New()
	root := uuid.New()
	sub := uuid.New()
	doc := uuid.New()
	subj := uuid.New()

	f := newFake()
	f.ancestors[sub] = []uuid.UUID{root, sub}
	f.ancestors[root] = []uuid.UUID{root}
	f.docLoc[doc] = [2]uuid.UUID{ws, sub}

	f.grants = []domain.Grant{
		{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
			TargetType: domain.TargetDirectory, TargetID: root, Effect: domain.EffectAllow},
		{SubjectType: domain.SubjectUser, SubjectID: subj, Actions: []domain.Action{domain.ActionRead},
			TargetType: domain.TargetDirectory, TargetID: sub, Effect: domain.EffectDeny},
	}

	eng := NewEngine(f)
	d, err := eng.Check(context.Background(), subj, nil, domain.TargetDocument, doc, domain.ActionRead)
	require.NoError(t, err)
	assert.False(t, d.Allowed, "deny on sub should block doc even with allow on root")
}
