package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/pkg/pagination"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeUserRepo records the query it received and returns canned data.
type fakeUserRepo struct {
	gotQ  service.UserQuery
	users []domain.User
	total int
	err   error
}

func (f *fakeUserRepo) List(ctx context.Context, q service.UserQuery) ([]domain.User, int, error) {
	f.gotQ = q
	return f.users, f.total, f.err
}

type fakeRoleRepo struct {
	roles []domain.Role
	err   error
}

func (f *fakeRoleRepo) List(ctx context.Context) ([]domain.Role, error) {
	return f.roles, f.err
}

func mustParseUUID(t *testing.T, s string) domain.UUID {
	t.Helper()
	return uuid.MustParse(s)
}

// withAuth builds a gin engine where every request is authenticated as st,
// then mounts the handler at the given method/path.
func withAuth(st AuthState, method, path string, h gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(ctxAuth, st); c.Next() })
	r.Handle(method, path, h)
	return r
}

func TestUserHandler_List_RBACScopedAndPaged(t *testing.T) {
	viewer := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	repo := &fakeUserRepo{
		users: []domain.User{
			{ID: viewer, Email: "me@x.com", Name: "Me", Status: "active"},
			{ID: mustParseUUID(t, "22222222-2222-2222-2222-222222222222"), Email: "peer@x.com", Name: "Peer", Status: "active"},
		},
		total: 2,
	}
	h := NewUserHandler(repo)
	r := withAuth(AuthState{UserID: viewer, IsAdmin: false}, http.MethodGet, "/users", h.List)

	req := httptest.NewRequest(http.MethodGet, "/users?page=2&page_size=5&search=ali", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var env struct {
		Code int `json:"code"`
		Data struct {
			Items    []domain.User `json:"items"`
			Total    int           `json:"total"`
			Page     int           `json:"page"`
			PageSize int           `json:"page_size"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, 0, env.Code)
	assert.Equal(t, 2, env.Data.Total)
	assert.Len(t, env.Data.Items, 2)
	assert.Equal(t, 2, env.Data.Page)
	assert.Equal(t, 5, env.Data.PageSize)

	// RBAC scoping + filters propagated to the repository.
	assert.Equal(t, viewer, repo.gotQ.ViewerID)
	assert.False(t, repo.gotQ.IsAdmin)
	assert.Equal(t, "ali", repo.gotQ.Search)
	assert.Equal(t, 2, repo.gotQ.Page)
	assert.Equal(t, 5, repo.gotQ.PageSize)
	// password_hash must never be serialized
	assert.Equal(t, "", env.Data.Items[0].PasswordHash)
}

// TestUserHandler_List_NullAvatarURLSerialized locks in the DEFECT-04 fix:
// a user whose avatar_url is NULL (nil *string) must still serialize to 200
// with avatar_url omitted, and a set avatar must round-trip as a string.
func TestUserHandler_List_NullAvatarURLSerialized(t *testing.T) {
	viewer := mustParseUUID(t, "11111111-1111-1111-1111-111111111111")
	peer := mustParseUUID(t, "22222222-2222-2222-2222-222222222222")
	avatar := "https://cdn.local/a.png"
	repo := &fakeUserRepo{
		users: []domain.User{
			{ID: viewer, Email: "me@x.com", Name: "Me", Status: "active"}, // avatar_url NULL
			{ID: peer, Email: "peer@x.com", Name: "Peer", AvatarURL: &avatar, Status: "active"},
		},
		total: 2,
	}
	h := NewUserHandler(repo)
	r := withAuth(AuthState{UserID: viewer, IsAdmin: true}, http.MethodGet, "/users", h.List)

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var env struct {
		Code int `json:"code"`
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, 0, env.Code)
	require.Len(t, env.Data.Items, 2)
	// NULL avatar_url is omitted (not a 500, not a JSON null error)
	_, hasNull := env.Data.Items[0]["avatar_url"]
	assert.False(t, hasNull, "NULL avatar_url must be omitted, not cause a 500")
	// set avatar_url round-trips as a string
	assert.Equal(t, avatar, env.Data.Items[1]["avatar_url"])
}

func TestUserHandler_List_AdminFlagPropagated(t *testing.T) {
	admin := mustParseUUID(t, "99999999-9999-9999-9999-999999999999")
	repo := &fakeUserRepo{users: []domain.User{}, total: 0}
	h := NewUserHandler(repo)
	r := withAuth(AuthState{UserID: admin, IsAdmin: true}, http.MethodGet, "/users", h.List)

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.True(t, repo.gotQ.IsAdmin, "admin flag must reach the repo so it bypasses scoping")
	assert.Equal(t, admin, repo.gotQ.ViewerID)
	// default pagination applied
	assert.Equal(t, pagination.DefaultPage, repo.gotQ.Page)
	assert.Equal(t, pagination.DefaultPageSize, repo.gotQ.PageSize)
}

func TestRoleHandler_List(t *testing.T) {
	repo := &fakeRoleRepo{
		roles: []domain.Role{
			{ID: mustParseUUID(t, "aaaaaaaa-0000-0000-0000-000000000001"), Name: "viewer", Scope: domain.ScopeDirectory, Permissions: []domain.Action{domain.ActionRead}, IsSystem: true},
			{ID: mustParseUUID(t, "aaaaaaaa-0000-0000-0000-000000000002"), Name: "editor", Scope: domain.ScopeDirectory, Permissions: []domain.Action{domain.ActionRead, domain.ActionWrite}, IsSystem: true},
		},
	}
	h := NewRoleHandler(repo)
	r := withAuth(AuthState{UserID: mustParseUUID(t, "11111111-1111-1111-1111-111111111111")}, http.MethodGet, "/roles", h.List)

	req := httptest.NewRequest(http.MethodGet, "/roles", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var env struct {
		Code int `json:"code"`
		Data struct {
			Items []domain.Role `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	assert.Equal(t, 0, env.Code)
	require.Len(t, env.Data.Items, 2)
	assert.Equal(t, "viewer", env.Data.Items[0].Name)
	assert.Equal(t, []domain.Action{domain.ActionRead}, env.Data.Items[0].Permissions)
}
