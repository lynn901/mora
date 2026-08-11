package authz

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSourceRepo is a minimal authz.SourceRepo for locator tests. It models
// the three states the SourceLocator cares about: present+enabled, present+
// disabled, and missing. A missing source returns an error so the locator
// maps it to ErrTargetNotFound (existence never leaks, §8.2).
type fakeSourceRepo struct {
	sources map[uuid.UUID]SourceInfo
	err     error // force a read error (e.g. DB down)
}

func (f *fakeSourceRepo) Get(_ context.Context, id uuid.UUID) (SourceInfo, error) {
	if f.err != nil {
		return SourceInfo{}, f.err
	}
	s, ok := f.sources[id]
	if !ok {
		return SourceInfo{}, errors.New("not found")
	}
	return s, nil
}

// fakeReviewRepo is a minimal authz.ReviewRepo for locator tests.
type fakeReviewRepo struct {
	reviews map[uuid.UUID]ReviewInfo
	err    error
}

func (f *fakeReviewRepo) Get(_ context.Context, id uuid.UUID) (ReviewInfo, error) {
	if f.err != nil {
		return ReviewInfo{}, f.err
	}
	r, ok := f.reviews[id]
	if !ok {
		return ReviewInfo{}, errors.New("not found")
	}
	return r, nil
}

// TestSourceLocator_Enabled verifies a present+enabled source resolves to the
// [source, workspace] chain with the correct workspace id.
func TestSourceLocator_Enabled(t *testing.T) {
	ws := uuid.New()
	src := uuid.New()
	loc := NewSourceLocator(&fakeSourceRepo{
		sources: map[uuid.UUID]SourceInfo{src: {WorkspaceID: ws, Enabled: true}},
	})
	out, err := loc.Locate(context.Background(), domain.TargetSource, src)
	require.NoError(t, err)
	assert.Equal(t, ws, out.WorkspaceID)
	require.Len(t, out.Chain, 2)
	assert.Equal(t, domain.TargetSource, out.Chain[0].Type)
	assert.Equal(t, src, out.Chain[0].ID)
	assert.Equal(t, domain.TargetWorkspace, out.Chain[1].Type)
	assert.Equal(t, ws, out.Chain[1].ID)
}

// TestSourceLocator_DisabledIsInvisible asserts the no-existence-leak
// invariant for a DISABLED source (§4.4 DELETE = soft-disable, §8.2): the
// locator must return ErrTargetNotFound — NOT a found-but-denied result — so
// a caller cannot infer that the source exists but is disabled.
func TestSourceLocator_DisabledIsInvisible(t *testing.T) {
	ws := uuid.New()
	src := uuid.New()
	loc := NewSourceLocator(&fakeSourceRepo{
		sources: map[uuid.UUID]SourceInfo{src: {WorkspaceID: ws, Enabled: false}},
	})
	_, err := loc.Locate(context.Background(), domain.TargetSource, src)
	assert.ErrorIs(t, err, ErrTargetNotFound, "disabled source must be indistinguishable from not-found")
}

// TestSourceLocator_MissingIsNotFound verifies a missing source maps to
// ErrTargetNotFound (the read error is swallowed; existence never leaks).
func TestSourceLocator_MissingIsNotFound(t *testing.T) {
	loc := NewSourceLocator(&fakeSourceRepo{sources: map[uuid.UUID]SourceInfo{}})
	_, err := loc.Locate(context.Background(), domain.TargetSource, uuid.New())
	assert.ErrorIs(t, err, ErrTargetNotFound)
}

// TestSourceLocator_ReadErrorIsNotFound verifies a DB-down read error is also
// mapped to ErrTargetNotFound — never a panic, never a leak.
func TestSourceLocator_ReadErrorIsNotFound(t *testing.T) {
	loc := NewSourceLocator(&fakeSourceRepo{err: errors.New("db down")})
	_, err := loc.Locate(context.Background(), domain.TargetSource, uuid.New())
	assert.ErrorIs(t, err, ErrTargetNotFound)
}

// TestSourceLocator_WrongType rejects a non-source target type up front.
func TestSourceLocator_WrongType(t *testing.T) {
	loc := NewSourceLocator(&fakeSourceRepo{})
	_, err := loc.Locate(context.Background(), domain.TargetDocument, uuid.New())
	assert.Error(t, err)
	assert.NotErrorIs(t, err, ErrTargetNotFound) // wrong-type is a programmer error, not not-found
}

// TestReviewLocator_Present verifies a present review resolves to
// [review, workspace].
func TestReviewLocator_Present(t *testing.T) {
	ws := uuid.New()
	rev := uuid.New()
	loc := NewReviewLocator(&fakeReviewRepo{
		reviews: map[uuid.UUID]ReviewInfo{rev: {WorkspaceID: ws}},
	})
	out, err := loc.Locate(context.Background(), domain.TargetReview, rev)
	require.NoError(t, err)
	assert.Equal(t, ws, out.WorkspaceID)
	require.Len(t, out.Chain, 2)
	assert.Equal(t, domain.TargetReview, out.Chain[0].Type)
	assert.Equal(t, rev, out.Chain[0].ID)
	assert.Equal(t, domain.TargetWorkspace, out.Chain[1].Type)
}

// TestReviewLocator_MissingIsNotFound verifies the no-leak invariant for a
// missing review (§8.2): ErrTargetNotFound, indistinguishable from a denial.
func TestReviewLocator_MissingIsNotFound(t *testing.T) {
	loc := NewReviewLocator(&fakeReviewRepo{reviews: map[uuid.UUID]ReviewInfo{}})
	_, err := loc.Locate(context.Background(), domain.TargetReview, uuid.New())
	assert.ErrorIs(t, err, ErrTargetNotFound)
}

// TestReviewLocator_ReadErrorIsNotFound verifies a DB error maps to
// ErrTargetNotFound (no leak, no panic).
func TestReviewLocator_ReadErrorIsNotFound(t *testing.T) {
	loc := NewReviewLocator(&fakeReviewRepo{err: errors.New("db down")})
	_, err := loc.Locate(context.Background(), domain.TargetReview, uuid.New())
	assert.ErrorIs(t, err, ErrTargetNotFound)
}
