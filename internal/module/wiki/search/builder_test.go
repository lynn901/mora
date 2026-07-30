package search

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/pkg/pagination"
)

func wsFilter() Filter {
	return Filter{
		Query:     "API 设计",
		WorkspaceID: uuid.New(),
		Params:    pagination.Params{Page: 1, PageSize: 10},
		FTSConfig: "chinese_zh",
	}
}

func TestBuild_BasicFTS(t *testing.T) {
	q := wsFilter().Build()
	assert.Contains(t, q.SQL, "plainto_tsquery")
	assert.Contains(t, q.SQL, "ts_rank_cd")
	assert.Contains(t, q.SQL, "ORDER BY score DESC")
	assert.Equal(t, "API 设计", q.Args[0])
}

func TestBuild_RBAC_EmptyVisibleForcesEmpty(t *testing.T) {
	f := wsFilter()
	f.VisibleAll = false
	f.VisibleDocs = nil
	q := f.Build()
	assert.Contains(t, q.SQL, "AND FALSE", "no visible docs must force empty result")
}

func TestBuild_RBAC_VisibleAllNoFilter(t *testing.T) {
	f := wsFilter()
	f.VisibleAll = true
	q := f.Build()
	assert.NotContains(t, q.SQL, "d.id IN")
	assert.NotContains(t, q.SQL, "AND FALSE")
}

func TestBuild_RBAC_ExplicitVisibleSet(t *testing.T) {
	f := wsFilter()
	f.VisibleDocs = []domain.UUID{uuid.New(), uuid.New()}
	q := f.Build()
	assert.Contains(t, q.SQL, "d.id IN (")
	// the two visible IDs should be in args (after query + workspace)
	assert.Len(t, q.Args, 4)
}

func TestBuild_DirectoryTagCreatorFilters(t *testing.T) {
	f := wsFilter()
	dir := uuid.New()
	tag := uuid.New()
	creator := uuid.New()
	f.DirectoryID = &dir
	f.Tag = &tag
	f.CreatedBy = &creator
	q := f.Build()
	assert.Contains(t, q.SQL, "d.directory_id = $")
	assert.Contains(t, q.SQL, "document_tags")
	assert.Contains(t, q.SQL, "d.created_by = $")
}

func TestBuild_TimeFilters(t *testing.T) {
	f := wsFilter()
	after := "2026-01-01T00:00:00Z"
	before := "2026-12-31T00:00:00Z"
	f.UpdatedAfter = &after
	f.UpdatedBefore = &before
	q := f.Build()
	assert.Contains(t, q.SQL, "d.updated_at >=")
	assert.Contains(t, q.SQL, "d.updated_at <=")
}

func TestBuild_SortUpdated(t *testing.T) {
	f := wsFilter()
	f.Sort = "updated"
	q := f.Build()
	assert.Contains(t, q.SQL, "ORDER BY d.updated_at DESC")
}

func TestBuild_Pagination(t *testing.T) {
	f := wsFilter()
	f.Params = pagination.Params{Page: 3, PageSize: 20}
	q := f.Build()
	assert.Contains(t, q.SQL, "LIMIT 20")
	assert.Contains(t, q.SQL, "OFFSET 40")
}

func TestBuild_FTSConfigInjectionGuard(t *testing.T) {
	f := wsFilter()
	f.FTSConfig = "simple'; DROP TABLE x;--"
	q := f.Build()
	// Unsafe config falls back to "simple"; no injection payload reaches SQL.
	assert.NotContains(t, q.SQL, "DROP TABLE")
	assert.Contains(t, q.SQL, "to_tsquery('simple', $1)")
}
