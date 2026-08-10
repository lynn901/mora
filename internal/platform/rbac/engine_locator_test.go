package rbac

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEngine_DelegatesToLocator proves the delegated path (SetLocator) produces
// the same decision as the built-in doc-family path. This guards the §3.5
// red line: behavior of Check is unchanged whether or not a Locator is wired.
func TestEngine_DelegatesToLocator(t *testing.T) {
	ws := uuid.New()
	root := uuid.New()
	sub := uuid.New()
	doc := uuid.New()
	group := uuid.New()

	// fakeRepo doubles as the rbac.Repository the DocLocator reads from and
	// the engine grants source — same in-memory store, same answers.
	f := newFake()
	f.ancestors[sub] = []uuid.UUID{root, sub}
	f.ancestors[root] = []uuid.UUID{root}
	f.docLoc[doc] = [2]uuid.UUID{ws, sub}
	f.grants = []domain.Grant{{
		SubjectType: domain.SubjectGroup, SubjectID: group,
		Actions:    []domain.Action{domain.ActionRead},
		TargetType: domain.TargetDirectory, TargetID: root, Effect: domain.EffectAllow,
	}}

	bare := NewEngine(f) // built-in path, no locator

	// stubLocator implements the engine's Locator contract using the same
	// locate/targetChain semantics as the built-in path. It stays in-package
	// so this test does not import platform/authz (which imports rbac and
	// would form an import cycle within the test binary).
	stub := &stubLocator{repo: f}

	deleg := NewEngine(f)
	deleg.SetLocator(stub)

	// read: both engines must agree, and must allow.
	d1, err := bare.Check(context.Background(), sub, []uuid.UUID{group}, domain.TargetDocument, doc, domain.ActionRead)
	require.NoError(t, err)
	d2, err := deleg.Check(context.Background(), sub, []uuid.UUID{group}, domain.TargetDocument, doc, domain.ActionRead)
	require.NoError(t, err)
	assert.True(t, d1.Allowed)
	assert.Equal(t, d1.Allowed, d2.Allowed, "delegated and built-in path must agree")
	assert.Equal(t, d1.Reason, d2.Reason, "reasons must match too")

	// write: both engines must agree, and must deny.
	d1, err = bare.Check(context.Background(), sub, []uuid.UUID{group}, domain.TargetDocument, doc, domain.ActionWrite)
	require.NoError(t, err)
	d2, err = deleg.Check(context.Background(), sub, []uuid.UUID{group}, domain.TargetDocument, doc, domain.ActionWrite)
	require.NoError(t, err)
	assert.False(t, d1.Allowed)
	assert.Equal(t, d1.Allowed, d2.Allowed, "delegated deny must match built-in")
}

// stubLocator implements rbac.Locator by reusing the fake repo with the same
// locate/targetChain semantics as the engine's built-in path. It exists only
// to keep this test in-package; production wiring uses platform/authz.AsLocator
// to adapt a DocLocator/CompositeLocator to rbac.Locator.
type stubLocator struct {
	repo *fakeRepo
}

func (s *stubLocator) Locate(ctx context.Context, t domain.TargetType, id uuid.UUID) (uuid.UUID, []LocatorNode, error) {
	switch t {
	case domain.TargetWorkspace:
		return id, []LocatorNode{{Type: domain.TargetWorkspace, ID: id}}, nil
	case domain.TargetDirectory:
		anc := s.repo.ancestors[id]
		if len(anc) == 0 {
			return uuid.Nil, nil, nil
		}
		ws := anc[0]
		nodes := []LocatorNode{{Type: domain.TargetDirectory, ID: id}}
		for i := len(anc) - 1; i >= 0; i-- {
			nodes = append(nodes, LocatorNode{Type: domain.TargetDirectory, ID: anc[i]})
		}
		nodes = append(nodes, LocatorNode{Type: domain.TargetWorkspace, ID: ws})
		return ws, nodes, nil
	case domain.TargetDocument:
		loc := s.repo.docLoc[id]
		ws, dir := loc[0], loc[1]
		nodes := []LocatorNode{{Type: domain.TargetDocument, ID: id}}
		if dir != uuid.Nil {
			anc := s.repo.ancestors[dir]
			for i := len(anc) - 1; i >= 0; i-- {
				nodes = append(nodes, LocatorNode{Type: domain.TargetDirectory, ID: anc[i]})
			}
		}
		nodes = append(nodes, LocatorNode{Type: domain.TargetWorkspace, ID: ws})
		return ws, nodes, nil
	}
	return uuid.Nil, nil, nil
}

var _ Locator = (*stubLocator)(nil)
