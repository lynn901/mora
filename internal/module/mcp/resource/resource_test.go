package resource

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/module/mcp/auth"
	"github.com/lynn901/mora/internal/module/mcp/moraclient"
	domainerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/platform/rbac"
)

const (
	rwsEng  = "ws-r-eng-0001"
	rdocAPI = "doc-r-api-0001"
	rid     = "user-r-1"
)

func newResRegistry(t *testing.T) (*Registry, *auth.AuthContext) {
	t.Helper()
	mock := newMockWith(t, rwsEng, rdocAPI, rid)
	reg := NewRegistry(mock)
	ac := &auth.AuthContext{
		TokenID: "tok-r", IdentityType: rbac.IdentityUser, IdentityID: rid,
		IdentityName: "R", Scope: rbac.ScopeReadWrite,
	}
	return reg, ac
}

func TestParseURI(t *testing.T) {
	cases := []struct {
		uri   string
		kind  string
		id    string
		valid bool
	}{
		{"mora://workspaces", "workspaces", "", true},
		{"mora://workspaces/ws-1/tree", "tree", "ws-1", true},
		{"mora://workspaces/ws-1/tags", "tags", "ws-1", true},
		{"mora://documents/doc-1/meta", "meta", "doc-1", true},
		{"mora://documents/doc-1/versions", "versions", "doc-1", true},
		{"mora://unknown", "", "", false},
		{"http://nope", "", "", false},
		{"mora://workspaces/ws-1", "", "", false},
		{"mora://documents/doc-1", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.uri, func(t *testing.T) {
			p, err := parseURI(c.uri)
			if !c.valid {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.kind, p.kind)
			assert.Equal(t, c.id, p.id)
		})
	}
}

func TestListResources(t *testing.T) {
	reg, ac := newResRegistry(t)
	defs, err := reg.List(auth.WithAuthContext(context.Background(), ac))
	require.NoError(t, err)
	uris := make([]string, len(defs))
	for i, d := range defs {
		uris[i] = d.URI
	}
	assert.Contains(t, uris, "mora://workspaces")
	assert.Contains(t, uris, "mora://workspaces/"+rwsEng+"/tree")
	assert.Contains(t, uris, "mora://workspaces/"+rwsEng+"/tags")
}

func TestReadWorkspaces(t *testing.T) {
	reg, ac := newResRegistry(t)
	res, err := reg.Read(auth.WithAuthContext(context.Background(), ac), "mora://workspaces")
	require.NoError(t, err)
	require.Len(t, res.Contents, 1)
	assert.Contains(t, res.Contents[0].Text, rwsEng)
}

func TestReadDocumentMeta(t *testing.T) {
	reg, ac := newResRegistry(t)
	res, err := reg.Read(auth.WithAuthContext(context.Background(), ac), "mora://documents/"+rdocAPI+"/meta")
	require.NoError(t, err)
	require.Len(t, res.Contents, 1)
	assert.Contains(t, res.Contents[0].Text, rdocAPI)
}

func TestReadVersions(t *testing.T) {
	reg, ac := newResRegistry(t)
	res, err := reg.Read(auth.WithAuthContext(context.Background(), ac), "mora://documents/"+rdocAPI+"/versions")
	require.NoError(t, err)
	require.Len(t, res.Contents, 1)
	assert.Contains(t, res.Contents[0].Text, "version_no")
}

func TestReadTree(t *testing.T) {
	reg, ac := newResRegistry(t)
	res, err := reg.Read(auth.WithAuthContext(context.Background(), ac), "mora://workspaces/"+rwsEng+"/tree")
	require.NoError(t, err)
	require.Len(t, res.Contents, 1)
}

func TestReadNoPermissionReturnsNotFound(t *testing.T) {
	reg, _ := newResRegistry(t)
	// An identity with no grants.
	other := &auth.AuthContext{
		TokenID: "tok-x", IdentityType: rbac.IdentityUser, IdentityID: "user-nobody",
		Scope: rbac.ScopeReadOnly,
	}
	_, err := reg.Read(auth.WithAuthContext(context.Background(), other), "mora://documents/"+rdocAPI+"/meta")
	assert.ErrorIs(t, err, domainerr.ErrNotFound, "no-permission read must surface as not-found (server collapses to empty)")
}

func TestReadUnknownURI(t *testing.T) {
	reg, ac := newResRegistry(t)
	_, err := reg.Read(auth.WithAuthContext(context.Background(), ac), "mora://bogus/thing")
	assert.ErrorIs(t, err, domainerr.ErrNotFound)
}

// newMockWith builds a mock with one workspace + one document readable/writable
// by the given identity.
func newMockWith(t *testing.T, wsID, docID, identityID string) *moraclient.Mock {
	t.Helper()
	m := moraclient.NewMock()
	m.AddWorkspace(moraclient.Workspace{ID: wsID, Name: "工程", Slug: "eng", OwnerID: identityID})
	m.AddDirectory(moraclient.DirectoryNode{ID: "dir-r-1", Name: "Docs", Path: "", SortOrder: 1}, wsID)
	m.AddDocument(moraclient.DocumentMeta{
		ID: docID, WorkspaceID: wsID, DirectoryID: "dir-r-1", Title: "API 规范",
		Status: "published", IndexStatus: "indexed", VersionNo: 3, Tags: []string{"api"},
		CreatedBy: identityID, UpdatedAt: "2026-07-29T08:00:00Z",
	}, "# API 规范\n\n分页说明。\n", "markdown",
		[]moraclient.VersionSummary{{VersionNo: 3, AuthorID: identityID, CreatedAt: "2026-07-29T08:00:00Z"}})
	m.AddTags(wsID, []moraclient.Tag{{ID: "tag-r-1", Name: "api"}})
	m.GrantWrite(identityID, wsID)
	return m
}
