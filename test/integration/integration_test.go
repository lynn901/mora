//go:build integration

// Package integration contains end-to-end tests against a live PostgreSQL
// instance. Skipped unless DATABASE_URL is set (run with:
//
//	DATABASE_URL=... go test -tags=integration ./test/integration/...
//
// ). These verify the ACs that span DB + RBAC + service layers.
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/pg"
	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/module/mora/event"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/pkg/pagination"
	"github.com/lynn901/mora/internal/platform/rbac"
)

func TestMain(m *testing.M) {
	if os.Getenv("DATABASE_URL") == "" {
		// skip whole package when no DB configured
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type Suite struct {
	suite.Suite
	pool  *pgxpool.Pool
	db    *postgres.DB
	perms *postgres.PermissionRepo
	dirs  *postgres.DirectoryRepo
	docs  *postgres.DocumentRepo
	vers  *postgres.VersionRepo
	ws    *postgres.WorkspaceRepo
	users *postgres.UserRepo
	roles *postgres.RoleRepo
}

func TestSuite(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	suite.Run(t, new(Suite))
}

func (s *Suite) SetupSuite() {
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(s.T(), err)
	s.pool = pool
	s.db = postgres.NewDB(pool)
	s.perms = postgres.NewPermissionRepo(s.db)
	s.dirs = postgres.NewDirectoryRepo(s.db)
	s.docs = postgres.NewDocumentRepo(s.db)
	s.vers = postgres.NewVersionRepo(s.db)
	s.ws = postgres.NewWorkspaceRepo(s.db)
	s.users = postgres.NewUserRepo(s.db)
	s.roles = postgres.NewRoleRepo(s.db)
}

func (s *Suite) TearDownSuite() { s.pool.Close() }

func (s *Suite) SetupTest() {
	// clean tables in dependency order; preserve system roles (migration 005 seed)
	ctx := context.Background()
	for _, t := range []string{"chunk_relations", "parse_tasks", "parse_configs",
		"comments", "document_tags", "document_versions", "documents",
		"directories", "tags", "permissions", "workspaces", "group_members", "groups", "users"} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	_, _ = s.pool.Exec(ctx, "DELETE FROM roles WHERE is_system = false")
}

// seedUser inserts a user and returns its ID.
func (s *Suite) seedUser(email, name string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	err := s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		email, name).Scan(&id)
	require.NoError(s.T(), err)
	return id
}

func (s *Suite) seedWorkspace(owner domain.UUID, slug string) *domain.Workspace {
	ws := &domain.Workspace{Name: "WS " + slug, Slug: slug, OwnerID: owner}
	require.NoError(s.T(), s.ws.Create(context.Background(), ws))
	return ws
}

// TestAC4_WorkspaceIsolationAndTree: multi-workspace isolation + infinite tree.
func (s *Suite) TestAC4_WorkspaceIsolationAndTree() {
	ctx := context.Background()
	owner := s.seedUser("owner4@x.com", "Owner")
	wsA := s.seedWorkspace(owner, "ws-a")
	wsB := s.seedWorkspace(owner, "ws-b")

	// tree in wsA: root -> child -> grandchild
	rootA := &domain.Directory{WorkspaceID: wsA.ID, Name: "root", Path: "root"}
	require.NoError(s.T(), s.dirs.Create(ctx, rootA))
	child := &domain.Directory{WorkspaceID: wsA.ID, ParentID: &rootA.ID, Name: "child", Path: "root.child"}
	require.NoError(s.T(), s.dirs.Create(ctx, child))
	grand := &domain.Directory{WorkspaceID: wsA.ID, ParentID: &child.ID, Name: "grand", Path: "root.child.grand"}
	require.NoError(s.T(), s.dirs.Create(ctx, grand))

	// a directory in wsB
	dirB := &domain.Directory{WorkspaceID: wsB.ID, Name: "rootB", Path: "rootb"}
	require.NoError(s.T(), s.dirs.Create(ctx, dirB))

	// wsA dirs must not include wsB's dir
	dirsA, err := s.dirs.ListByWorkspace(ctx, wsA.ID)
	require.NoError(s.T(), err)
	require.Len(s.T(), dirsA, 3)

	dirsB, err := s.dirs.ListByWorkspace(ctx, wsB.ID)
	require.NoError(s.T(), err)
	require.Len(s.T(), dirsB, 1)
	assert.Equal(s.T(), "rootB", dirsB[0].Name)

	// tree assembly (infinite nesting)
	tree := service.BuildTree(dirsA, nil)
	require.Len(s.T(), tree, 1)
	assert.Equal(s.T(), "root", tree[0].Name)
	require.Len(s.T(), tree[0].Children, 1)
	assert.Equal(s.T(), "grand", tree[0].Children[0].Children[0].Name)
}

// TestAC7_RBACInheritanceAndOverride: explicit deny > allow > inherited > deny.
func (s *Suite) TestAC7_RBACInheritanceAndOverride() {
	ctx := context.Background()
	owner := s.seedUser("owner7@x.com", "Owner")
	alice := s.seedUser("alice7@x.com", "Alice")
	ws := s.seedWorkspace(owner, "ws-rbac")

	root := &domain.Directory{WorkspaceID: ws.ID, Name: "root", Path: "root"}
	require.NoError(s.T(), s.dirs.Create(ctx, root))
	sub := &domain.Directory{WorkspaceID: ws.ID, ParentID: &root.ID, Name: "sub", Path: "root.sub"}
	require.NoError(s.T(), s.dirs.Create(ctx, sub))

	doc := &domain.Document{WorkspaceID: ws.ID, DirectoryID: &sub.ID, Title: "secret", CreatedBy: owner}
	require.NoError(s.T(), s.docs.Create(ctx, doc))

	// grant alice read on root (subtree) → should inherit to doc
	roleViewer := s.roleID("viewer")
	require.NoError(s.T(), s.perms.Grant(ctx, &domain.Permission{
		SubjectType: domain.SubjectUser, SubjectID: alice, RoleID: roleViewer,
		TargetType: domain.TargetDirectory, TargetID: root.ID, Effect: domain.EffectAllow,
	}))

	engine := rbac.NewEngine(postgres.NewRBACAdapter(s.perms, s.dirs, s.docs))
	dec, err := engine.Check(ctx, alice, nil, domain.TargetDocument, doc.ID, domain.ActionRead)
	require.NoError(s.T(), err)
	assert.True(s.T(), dec.Allowed, "alice should read doc via inherited root allow")

	// explicit deny on sub overrides inherited allow on root
	require.NoError(s.T(), s.perms.Grant(ctx, &domain.Permission{
		SubjectType: domain.SubjectUser, SubjectID: alice, RoleID: roleViewer,
		TargetType: domain.TargetDirectory, TargetID: sub.ID, Effect: domain.EffectDeny,
	}))
	dec, err = engine.Check(ctx, alice, nil, domain.TargetDocument, doc.ID, domain.ActionRead)
	require.NoError(s.T(), err)
	assert.False(s.T(), dec.Allowed, "deny on sub must override inherited allow")

	// bob has no grants → default deny
	bob := s.seedUser("bob7@x.com", "Bob")
	dec, err = engine.Check(ctx, bob, nil, domain.TargetDocument, doc.ID, domain.ActionRead)
	require.NoError(s.T(), err)
	assert.False(s.T(), dec.Allowed, "bob default deny")
}

// TestAC6_VersionDiffAndRollback: rollback produces a new version; diff works.
func (s *Suite) TestAC6_VersionDiffAndRollback() {
	ctx := context.Background()
	owner := s.seedUser("owner6@x.com", "Owner")
	ws := s.seedWorkspace(owner, "ws-ver")

	pub := event.NewNoopPublisher()
	engine := rbac.NewEngine(postgres.NewRBACAdapter(s.perms, s.dirs, s.docs))
	docSvc := service.NewDocumentService(s.docs, s.vers, engine, pub)

	auth := service.AuthContext{UserID: owner, IsAdmin: true}
	d := &domain.Document{WorkspaceID: ws.ID, Title: "V1", Content: []domain.Block{
		{ID: "b1", Type: domain.BlockParagraph, Content: []domain.Block{{Type: domain.BlockText, Text: "first"}}},
	}}
	out, err := docSvc.Create(ctx, auth, d)
	require.NoError(s.T(), err)
	require.Equal(s.T(), 1, out.VersionNo)

	// update → version 2
	out, err = docSvc.Update(ctx, auth, out.ID, 1, "V2", []domain.Block{
		{ID: "b1", Type: domain.BlockParagraph, Content: []domain.Block{{Type: domain.BlockText, Text: "second"}}},
	}, "")
	require.NoError(s.T(), err)
	require.Equal(s.T(), 2, out.VersionNo)

	// diff v1..v2
	diff, err := docSvc.DiffVersions(ctx, auth, out.ID, 1, 2)
	require.NoError(s.T(), err)
	assert.NotEmpty(s.T(), diff)

	// rollback to v1 → produces NEW version (3), history intact
	rolled, err := docSvc.Rollback(ctx, auth, out.ID, 1)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 3, rolled.VersionNo, "rollback must produce a new version")

	// v1 and v2 still exist
	max, err := s.vers.MaxVersionNo(ctx, out.ID)
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 3, max)
}

// TestAC8_SearchRBACFiltering: search only returns visible docs.
func (s *Suite) TestAC8_SearchRBACFiltering() {
	ctx := context.Background()
	owner := s.seedUser("owner8@x.com", "Owner")
	alice := s.seedUser("alice8@x.com", "Alice")
	ws := s.seedWorkspace(owner, "ws-search")

	visibleDoc := &domain.Document{WorkspaceID: ws.ID, Title: "Kubernetes 部署指南", CreatedBy: owner,
		ContentText: "Kubernetes 部署指南 kubectl"}
	hiddenDoc := &domain.Document{WorkspaceID: ws.ID, Title: "Kubernetes 密钥", CreatedBy: owner,
		ContentText: "Kubernetes 密钥 secret"}
	require.NoError(s.T(), s.docs.Create(ctx, visibleDoc))
	require.NoError(s.T(), s.docs.Create(ctx, hiddenDoc))

	// grant alice read on visibleDoc only
	roleViewer := s.roleID("viewer")
	require.NoError(s.T(), s.perms.Grant(ctx, &domain.Permission{
		SubjectType: domain.SubjectUser, SubjectID: alice, RoleID: roleViewer,
		TargetType: domain.TargetDocument, TargetID: visibleDoc.ID, Effect: domain.EffectAllow,
	}))

	engine := rbac.NewEngine(postgres.NewRBACAdapter(s.perms, s.dirs, s.docs))
	vis, err := engine.VisibleDocuments(ctx, alice, nil, ws.ID)
	require.NoError(s.T(), err)
	assert.Contains(s.T(), vis, visibleDoc.ID)
	assert.NotContains(s.T(), vis, hiddenDoc.ID, "hidden doc must not be visible")
}

// TestUsersAndRoles_Query: GET /users RBAC scoping + GET /roles dictionary.
// A non-admin viewer must only see users who share a readable workspace (plus
// themselves); a stranger with no grants sees only themselves. Roles return the
// system dictionary that Permission.role_id references.
func (s *Suite) TestUsersAndRoles_Query() {
	ctx := context.Background()
	owner := s.seedUser("owner-u@x.com", "Owner")
	alice := s.seedUser("alice-u@x.com", "Alice")
	stranger := s.seedUser("stranger-u@x.com", "Stranger")
	ws := s.seedWorkspace(owner, "ws-users")

	// alice can read ws (viewer role); stranger has no grants; owner owns ws.
	roleViewer := s.roleID("viewer")
	require.NoError(s.T(), s.perms.Grant(ctx, &domain.Permission{
		SubjectType: domain.SubjectUser, SubjectID: alice, RoleID: roleViewer,
		TargetType: domain.TargetWorkspace, TargetID: ws.ID, Effect: domain.EffectAllow,
	}))

	// admin sees everyone
	all, total, err := s.users.List(ctx, service.UserQuery{ViewerID: owner, IsAdmin: true, Params: pagination.Params{Page: 1, PageSize: 50}})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 3, total)
	assert.Len(s.T(), all, 3)

	// owner (non-admin) sees self + alice (co-reader of ws), not stranger
	ownerView, total, err := s.users.List(ctx, service.UserQuery{ViewerID: owner, Params: pagination.Params{Page: 1, PageSize: 50}})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 2, total)
	s.assertUserIDs(s.T(), ownerView, owner, alice)

	// alice (non-admin) sees self + owner (symmetric co-membership)
	aliceView, total, err := s.users.List(ctx, service.UserQuery{ViewerID: alice, Params: pagination.Params{Page: 1, PageSize: 50}})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 2, total)
	s.assertUserIDs(s.T(), aliceView, alice, owner)

	// stranger (non-admin, no grants) sees only themselves — no enumeration leak
	strangerView, total, err := s.users.List(ctx, service.UserQuery{ViewerID: stranger, Params: pagination.Params{Page: 1, PageSize: 50}})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 1, total, "stranger must not enumerate other users")
	s.assertUserIDs(s.T(), strangerView, stranger)

	// search filter narrows by name/email substring
	searched, total, err := s.users.List(ctx, service.UserQuery{ViewerID: owner, IsAdmin: true, Search: "alice", Params: pagination.Params{Page: 1, PageSize: 50}})
	require.NoError(s.T(), err)
	assert.Equal(s.T(), 1, total)
	require.Len(s.T(), searched, 1)
	assert.Equal(s.T(), "Alice", searched[0].Name)
	// password_hash never returned
	assert.Equal(s.T(), "", searched[0].PasswordHash)

	// roles dictionary: system roles present, viewer carries read
	roles, err := s.roles.List(ctx)
	require.NoError(s.T(), err)
	names := make(map[string]bool, len(roles))
	for _, ro := range roles {
		names[ro.Name] = true
		if ro.Name == "viewer" {
			assert.Contains(s.T(), ro.Permissions, domain.ActionRead)
		}
	}
	for _, n := range []string{"super_admin", "workspace_admin", "editor", "viewer"} {
		assert.True(s.T(), names[n], "system role %q should be present", n)
	}
}

func (s *Suite) TestParse_DocumentParsingMigrationAndStore() {
	// Verifies migration 011 landed (documents new columns + parse_tasks /
	// parse_configs / chunk_relations tables) and the ParseTaskStore round-trips
	// the parse state machine + progress timeline (design-docs/10 §4.2.2, §4.3).
	ctx := context.Background()
	t := s.T()

	// migration 011 columns exist on documents
	uid := s.seedUser("parse@mora.dev", "Parse User")
	ws := s.seedWorkspace(uid, "parse-ws")
	doc := &domain.Document{
		ID: domain.NewUUID(), WorkspaceID: ws.ID, Title: "parse doc",
		Format: domain.FormatBlocks, Status: domain.StatusDraft, IndexStatus: domain.IndexPending,
		VersionNo: 1, CreatedBy: uid, StorageKey: "mora/source/file.pdf",
		SourceFormat: "pdf", ParseStatus: domain.ParsePending,
	}
	require.NoError(t, s.docs.Create(ctx, doc))

	got, err := s.docs.Get(ctx, doc.ID)
	require.NoError(t, err)
	assert.Equal(t, "mora/source/file.pdf", got.StorageKey)
	assert.Equal(t, "pdf", got.SourceFormat)
	assert.Equal(t, domain.ParsePending, got.ParseStatus)

	// ParseTaskStore: upsert idempotent, status transitions, progress timeline
	store := pg.NewParseTaskStore(s.pool)
	task, err := store.UpsertParseTask(ctx, domain.ParseTask{
		DocumentID: doc.ID.String(), EventID: domain.NewUUID().String(),
		Status: domain.ParseTaskPending, MaxAttempt: 3,
		ParseOpts: map[string]any{"chunking_strategy": "adaptive_3tier"},
	})
	require.NoError(t, err)
	assert.Equal(t, domain.ParseTaskPending, task.Status)

	// idempotent re-upsert returns the same task (no duplicate)
	task2, err := store.UpsertParseTask(ctx, domain.ParseTask{
		DocumentID: doc.ID.String(), EventID: task.EventID, Status: domain.ParseTaskPending,
	})
	require.NoError(t, err)
	assert.Equal(t, task.ID, task2.ID)

	// progress timeline: append two stages
	require.NoError(t, store.AppendProgress(ctx, task.ID, domain.ProgressStage{Stage: "parsing", Status: "started", At: time.Now().UTC().Format(time.RFC3339)}))
	require.NoError(t, store.AppendProgress(ctx, task.ID, domain.ProgressStage{Stage: "parsing", Status: "done", At: time.Now().UTC().Format(time.RFC3339)}))

	// mark parsed + write content
	require.NoError(t, store.SetDocumentParseStatus(ctx, doc.ID.String(), domain.ParseParsing, task.ID))
	require.NoError(t, store.SetDocumentContent(ctx, doc.ID.String(), []byte(`[{"type":"paragraph","content":[{"type":"text","text":"parsed body"}]}]`), "parsed body", "ledongthuc/pdf", "pdf"))
	require.NoError(t, store.SetDocumentParseStatus(ctx, doc.ID.String(), domain.ParseParsed, task.ID))

	// read model carries both badges + the timeline
	info, err := store.GetParseProgress(ctx, doc.ID.String())
	require.NoError(t, err)
	assert.Equal(t, domain.ParseParsed, info.ParseStatus)
	assert.Len(t, info.Progress, 2)

	// ParseConfigStore: create + list a workspace template
	cfgStore := pg.NewParseConfigStore(s.pool)
	wsID := ws.ID.String()
	cfg, err := cfgStore.Create(ctx, pg.ParseConfig{
		WorkspaceID: &wsID, Name: "adaptive-default",
		Config: map[string]any{"chunking_strategy": "adaptive_3tier"}, IsDefault: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "adaptive-default", cfg.Name)
	assert.True(t, cfg.IsDefault)
	listed, err := cfgStore.List(ctx, ws.ID.String())
	require.NoError(t, err)
	assert.NotEmpty(t, listed)
}

func (s *Suite) assertUserIDs(t *testing.T, got []domain.User, want ...domain.UUID) {
	t.Helper()
	gotIDs := make(map[domain.UUID]bool, len(got))
	for _, u := range got {
		gotIDs[u.ID] = true
	}
	for _, w := range want {
		assert.True(t, gotIDs[w], "expected user %s in list", w)
	}
	assert.Len(t, got, len(want), "user list length mismatch")
}

// roleID fetches a system role id by name.
func (s *Suite) roleID(name string) domain.UUID {
	var id domain.UUID
	err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM roles WHERE name=$1`, name).Scan(&id)
	require.NoError(s.T(), err)
	return id
}

var _ = time.Now
