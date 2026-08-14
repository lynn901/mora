package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
)

// fakeWikiRepo is an in-memory WikiRepo for service tests. It is deliberately
// minimal — only the methods Status/TriggerRun touch are real; the rest
// satisfy the interface with no-ops so the service compiles + runs.
type fakeWikiRepo struct {
	space     *WikiSpace
	pages     []*WikiPage
	runs      []*MaintenanceRun
	proposals []*PageProposal
	// denyGetSpace makes GetSpace return ErrWikiSpaceNotFound (simulates a
	// missing or RBAC-invisible space).
	denyGetSpace bool
}

func (f *fakeWikiRepo) CreateSpace(context.Context, pgx.Tx, *WikiSpace) error { return nil }
func (f *fakeWikiRepo) GetSpace(_ context.Context, _ uuid.UUID) (*WikiSpace, error) {
	if f.denyGetSpace || f.space == nil {
		return nil, ErrWikiSpaceNotFound
	}
	return f.space, nil
}
func (f *fakeWikiRepo) ListSpaces(context.Context, uuid.UUID, int, int) ([]*WikiSpace, int, error) {
	return nil, 0, nil
}
func (f *fakeWikiRepo) CreateRun(context.Context, pgx.Tx, *MaintenanceRun) error { return nil }
func (f *fakeWikiRepo) GetRun(_ context.Context, _ uuid.UUID) (*MaintenanceRun, error) {
	if len(f.runs) > 0 {
		return f.runs[0], nil
	}
	return nil, ErrWikiRunNotFound
}
func (f *fakeWikiRepo) ListRuns(_ context.Context, _ uuid.UUID, _ string, page, pageSize int) ([]*MaintenanceRun, int, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	total := len(f.runs)
	start := (page - 1) * pageSize
	if start >= total {
		return nil, total, nil
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return f.runs[start:end], total, nil
}
func (f *fakeWikiRepo) UpdateRunStatus(context.Context, uuid.UUID, string, string, string) error {
	return nil
}
func (f *fakeWikiRepo) CreateProposals(context.Context, pgx.Tx, []*PageProposal) error { return nil }
func (f *fakeWikiRepo) GetProposal(_ context.Context, _ uuid.UUID) (*PageProposal, error) {
	return nil, ErrWikiProposalNotFound
}
func (f *fakeWikiRepo) ListProposals(_ context.Context, _ uuid.UUID, _, _ string) ([]*PageProposal, error) {
	return f.proposals, nil
}
func (f *fakeWikiRepo) UpdateProposalStatus(context.Context, uuid.UUID, string, *uuid.UUID, *uuid.UUID) error {
	return nil
}
func (f *fakeWikiRepo) ListPages(_ context.Context, _ uuid.UUID) ([]*WikiPage, error) {
	return f.pages, nil
}
func (f *fakeWikiRepo) AffectedPages(_ context.Context, _ uuid.UUID, _ string) ([]AffectedPage, error) {
	return nil, nil
}
func (f *fakeWikiRepo) UpdatePageStaleReason(context.Context, uuid.UUID, string, string) error {
	return nil
}
func (f *fakeWikiRepo) GetRunByIdempotencyKey(_ context.Context, _ string) (*MaintenanceRun, error) {
	return nil, ErrWikiRunNotFound
}
func (f *fakeWikiRepo) UpdateIndexManifest(context.Context, uuid.UUID, []byte, string) error {
	return nil
}
func (f *fakeWikiRepo) ApplyProposalCAS(context.Context, pgx.Tx, uuid.UUID) (AutomationState, bool, error) {
	return AutomationManaged, false, nil
}

// newStatusService builds a service with no RBAC engine (dev/test path: the
// authorize gate is a no-op pass-through when rbac is nil) over a fake repo.
func newStatusService(repo WikiRepo) *Service {
	return NewService(repo, nil, nil)
}

func TestStatus_AggregatesSpace(t *testing.T) {
	spaceID := uuid.New()
	repo := &fakeWikiRepo{
		space: &WikiSpace{ID: spaceID, Name: "工程 Wiki", Status: "active"},
		pages: []*WikiPage{
			{WikiSpaceID: spaceID, PageKey: "api", PageKind: "managed"},
			{WikiSpaceID: spaceID, PageKey: "onboarding", PageKind: "manual"},
		},
		runs: []*MaintenanceRun{
			{ID: uuid.New(), WikiSpaceID: spaceID, TriggerType: "ingest", Status: "applied"},
		},
		proposals: []*PageProposal{
			{ID: uuid.New(), WikiSpaceID: spaceID, PageKey: "api", Status: "proposed"},
		},
	}
	svc := newStatusService(repo)
	st, err := svc.Status(context.Background(), AuthContext{}, spaceID)
	require.NoError(t, err)
	require.NotNil(t, st)
	assert.Equal(t, "工程 Wiki", st.Space.Name)
	assert.Len(t, st.Pages, 2)
	assert.Equal(t, "applied", st.LastRun.Status)
	assert.Len(t, st.Proposals, 1)
}

// Status on a missing/hidden space returns ErrWikiSpaceNotFound (the sentinel
// the handler maps to 404 — §8.2 no existence leak).
func TestStatus_MissingSpaceNotFound(t *testing.T) {
	spaceID := uuid.New()
	repo := &fakeWikiRepo{denyGetSpace: true}
	svc := newStatusService(repo)
	_, err := svc.Status(context.Background(), AuthContext{}, spaceID)
	assert.ErrorIs(t, err, ErrWikiSpaceNotFound)
}

// Status with no runs / proposals returns a non-nil result with nil/empty
// fields (a healthy space that has never run maintenance).
func TestStatus_EmptyRunAndProposals(t *testing.T) {
	spaceID := uuid.New()
	repo := &fakeWikiRepo{
		space: &WikiSpace{ID: spaceID, Name: "工程 Wiki", Status: "active"},
		pages: []*WikiPage{{WikiSpaceID: spaceID, PageKey: "api", PageKind: "managed"}},
	}
	svc := newStatusService(repo)
	st, err := svc.Status(context.Background(), AuthContext{}, spaceID)
	require.NoError(t, err)
	require.NotNil(t, st)
	assert.Nil(t, st.LastRun)
	assert.Empty(t, st.Proposals)
}

// domain import retained for future AuthContext field extensions; pin it now
// so the import does not rot.
var _ = domain.SubjectUser
