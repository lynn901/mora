//go:build integration

// Integration tests for DocWriteSink (design-docs/13 §6.3, PR2 item ⑤).
// Skipped unless DATABASE_URL is set (run with:
// DATABASE_URL=... go test -tags=integration ./internal/infra/postgres/...).
//
// These verify the SQL contract unit tests can't: that documents +
// document_versions + the outbox_events row commit in ONE transaction
// (atomicity), and that a failure (version CAS conflict) rolls back ALL three
// so no orphan outbox event ever lands.
package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/platform/outbox"
)

func sinkTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return jobTestPool(t)
}

// seedUserWorkspace inserts a user and a workspace owned by them, returning
// their IDs. documents FKs both, so the sink's INSERT needs real parents.
// Cleaned up by the returned cleanup.
func seedUserWorkspace(t *testing.T, pool *pgxpool.Pool) (userID, wsID uuid.UUID, cleanup func()) {
	t.Helper()
	ctx := context.Background()
	userID = uuid.New()
	wsID = uuid.New()
	email := "sink_" + userID.String()[:8] + "@mora.local"
	_, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, name, status) VALUES ($1,$2,'Sink Test','active')`, userID, email)
	require.NoError(t, err, "seed user")
	_, err = pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1,'Sink WS',$2,$3)`,
		wsID, "sink-"+wsID.String()[:8], userID)
	require.NoError(t, err, "seed workspace")
	return userID, wsID, func() {
		// documents cascade-deletes its versions; workspaces cascade-deletes
		// their documents. Order: workspace first, then user.
		_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, wsID)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id=$1`, userID)
	}
}

// TestDocWriteSink_CreateDocAtomic: WriteDoc(create) lands the document, the
// version snapshot, AND the outbox event in one tx — all three are readable
// after commit, and the outbox event carries only IDs (no content).
func TestDocWriteSink_CreateDocAtomic(t *testing.T) {
	pool := sinkTestPool(t)
	sink := NewDocWriteSink(pool, outbox.NewStore())
	ctx := context.Background()

	author, ws, cleanup := seedUserWorkspace(t, pool)
	t.Cleanup(cleanup)

	doc := &domain.Document{
		ID:          uuid.New(),
		WorkspaceID: ws,
		Title:       "sink-create",
		Content:     []domain.Block{{Type: "p", Text: "hello"}},
		Format:      "markdown",
	}
	doc.CreatedBy = author
	doc.UpdatedBy = &author
	doc.Status = domain.StatusDraft
	doc.IndexStatus = domain.IndexPending
	doc.ParseStatus = domain.ParseParsed

	ver := &domain.DocumentVersion{
		DocumentID: doc.ID, VersionNo: 1, Content: doc.Content,
		ContentText: "hello", AuthorID: author, DiffSummary: "initial",
	}
	ev := domain.KnowledgeEvent{
		EventType: domain.KEAssetCreated, AggregateType: domain.AggKnowledgeAsset,
		AggregateID: doc.ID, WorkspaceID: &ws,
	}

	out, err := sink.WriteDoc(ctx, doc, ver, 0, true, ev)
	require.NoError(t, err)
	assert.Equal(t, 1, out.VersionNo)

	// document row present.
	var title string
	require.NoError(t, pool.QueryRow(ctx, `SELECT title FROM documents WHERE id=$1`, doc.ID).Scan(&title))
	assert.Equal(t, "sink-create", title)

	// version row present.
	var vno int
	require.NoError(t, pool.QueryRow(ctx, `SELECT version_no FROM document_versions WHERE document_id=$1`, doc.ID).Scan(&vno))
	assert.Equal(t, 1, vno)

	// outbox row present, unpublished, destined for knowledge_events, payload
	// carries IDs but NOT content (§5.1).
	var (
		aggType string
		evType  string
		payload []byte
		pub     *any
	)
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT aggregate_type, event_type, payload, published_at FROM outbox_events WHERE aggregate_id=$1`,
		doc.ID).Scan(&aggType, &evType, &payload, &pub))
	assert.Equal(t, domain.AggKnowledgeAsset, aggType)
	assert.Equal(t, domain.KEAssetCreated, evType)
	assert.Nil(t, pub, "outbox event must be unpublished until the dispatcher polls")
	// Payload must not embed document content (§5.1: IDs only, no content).
	assertNotInPayload(t, payload, "hello")
}

// TestDocWriteSink_RollbackOnConflict: a version CAS failure (prevVersion
// wrong) must roll back the ENTIRE tx — no document update, no version, no
// orphan outbox event. This is the core atomicity guarantee.
func TestDocWriteSink_RollbackOnConflict(t *testing.T) {
	pool := sinkTestPool(t)
	sink := NewDocWriteSink(pool, outbox.NewStore())
	ctx := context.Background()

	author, ws, cleanup := seedUserWorkspace(t, pool)
	t.Cleanup(cleanup)

	// Seed a doc at version 1.
	doc := &domain.Document{
		ID: uuid.New(), WorkspaceID: ws, Title: "rb",
		Content: []domain.Block{{Type: "p", Text: "v1"}}, Format: "markdown",
		Status: domain.StatusDraft, IndexStatus: domain.IndexPending,
		ParseStatus: domain.ParseParsed, CreatedBy: author,
	}
	doc.UpdatedBy = &doc.CreatedBy
	ver := &domain.DocumentVersion{DocumentID: doc.ID, VersionNo: 1, Content: doc.Content, AuthorID: doc.CreatedBy}
	_, err := sink.WriteDoc(ctx, doc, ver, 0, true, domain.KnowledgeEvent{
		EventType: domain.KEAssetCreated, AggregateType: domain.AggKnowledgeAsset,
		AggregateID: doc.ID, WorkspaceID: &doc.WorkspaceID,
	})
	require.NoError(t, err)

	// Now attempt an UPDATE with the WRONG prevVersion (claim v1 but it's already
	// been bumped to 1 by the create — pass prevVersion=99 to force CAS miss).
	upd := *doc
	upd.Title = "rb-v2"
	upd.Content = []domain.Block{{Type: "p", Text: "v2"}}
	upd2 := &upd
	_, err = sink.WriteDoc(ctx, upd2, &domain.DocumentVersion{
		DocumentID: doc.ID, VersionNo: 2, Content: upd2.Content, AuthorID: doc.CreatedBy,
	}, 99, false, domain.KnowledgeEvent{
		EventType: domain.KEAssetVersionRequested, AggregateType: domain.AggKnowledgeAsset,
		AggregateID: doc.ID, WorkspaceID: &doc.WorkspaceID,
	})
	require.ErrorIs(t, err, service.ErrNotFound, "CAS conflict must surface as ErrNotFound")

	// The doc title must NOT have changed (update rolled back).
	var title string
	require.NoError(t, pool.QueryRow(ctx, `SELECT title FROM documents WHERE id=$1`, doc.ID).Scan(&title))
	assert.Equal(t, "rb", title, "document update must roll back on CAS conflict")

	// And NO orphan outbox event for the failed update — only the create's event
	// (aggregate_id = doc.ID, count = 1).
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE aggregate_id=$1`, doc.ID).Scan(&n))
	assert.Equal(t, 1, n, "no orphan outbox event must be written on rollback")
}

// assertNotInPayload fails the test if needle appears in the JSON payload blob
// — guards the §5.1 "no content in events" invariant.
func assertNotInPayload(t *testing.T, payload []byte, needle string) {
	t.Helper()
	if len(payload) == 0 {
		return
	}
	if stringContains(string(payload), needle) {
		t.Errorf("outbox payload must not contain document content %q; got %s", needle, string(payload))
	}
}

// stringContains is a tiny helper so the test file doesn't import strings.
func stringContains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
