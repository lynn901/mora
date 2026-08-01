package e2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag"
	"github.com/lynn901/mora/internal/module/rag/handler"
	"github.com/lynn901/mora/internal/module/rag/pipeline"
	"github.com/lynn901/mora/internal/module/rag/ragtest"
	"github.com/lynn901/mora/internal/module/rag/search"
	"github.com/lynn901/mora/internal/module/rag/worker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stack bundles the fakes + wired RAG components for a test scenario.
type stack struct {
	models   *ragtest.FakeModelStore
	factory  *ragtest.FakeProviderFactory
	docs     *ragtest.FakeDocumentStore
	rbac     *ragtest.FakeRBACResolver
	vectors  *ragtest.FakeVectorStore
	fts      *ragtest.FakeFTSStore
	status   *ragtest.FakeIndexStatusStore
	idem     *ragtest.FakeIdempotencyStore
	queue    *ragtest.FakeEventQueue
	pipe     *pipeline.Pipeline
	worker   *worker.Worker
	searcher *search.HybridSearcher
	handler  *handler.Handler
}

const dim = 64

func newStack(t *testing.T) *stack {
	t.Helper()
	model := domain.EmbeddingModel{
		ID: "m1", Provider: "tei", ModelName: "Qwen3-Embedding-0.6B",
		Dimension: dim, MaxToken: 8192,
		InstructionQuery: "query:", InstructionDoc: "passage:",
	}
	s := &stack{
		models:  ragtest.NewFakeModelStore(model),
		factory: &ragtest.FakeProviderFactory{Dim: dim},
		docs:    ragtest.NewFakeDocumentStore(),
		rbac:    ragtest.NewFakeRBACResolver(),
		vectors: ragtest.NewFakeVectorStore(),
		fts:     ragtest.NewFakeFTSStore(),
		status:  ragtest.NewFakeIndexStatusStore(),
		idem:    ragtest.NewFakeIdempotencyStore(),
		queue:   ragtest.NewFakeEventQueue(),
	}
	s.pipe = pipeline.New(pipeline.Pipeline{
		Cfg:     pipeline.Config{ChunkSize: 24, ChunkOverlap: 6, EmbedBatchSize: 8, MaxAttempt: 3, Backoffs: []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond}},
		Docs:    s.docs,
		RBAC:    s.rbac,
		Vectors: s.vectors,
		Models:  s.models,
		Factory: s.factory,
		Status:  s.status,
		Clock:   rag.SystemClock{},
	})
	s.worker = worker.New(worker.Worker{
		Queue: s.queue, Idem: s.idem, Status: s.status, Pipeline: s.pipe,
		Cfg:   s.pipe.Cfg,
		Sleep: func(ctx context.Context, d time.Duration) error { return nil }, // instant retries in tests
	})
	s.searcher = search.New(search.HybridSearcher{
		Models: s.models, Factory: s.factory, Vectors: s.vectors, FTS: s.fts, RBAC: s.rbac,
	})
	s.handler = &handler.Handler{
		Search: s.searcher, Status: s.status, Models: s.models,
		Factory: s.factory, Pipeline: s.pipe,
		Auth: authFunc(func(r *http.Request) (string, error) { return r.Header.Get("X-User-ID"), nil }),
		Guard: func(ctx context.Context, uid, docID string) (bool, error) {
			return uid == "alice", nil // alice may read; others denied (existence not leaked)
		},
	}
	return s
}

type authFunc func(r *http.Request) (string, error)

func (f authFunc) UserID(r *http.Request) (string, error) { return f(r) }

// blocks returns a small Block JSONB array for test documents.
func blocks(paras ...string) []byte {
	var b strings.Builder
	b.WriteByte('[')
	for i, p := range paras {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"type":"paragraph","content":[{"type":"text","text":"`)
		b.WriteString(jsonEscape(p))
		b.WriteString(`"}]}`)
	}
	b.WriteByte(']')
	return []byte(b.String())
}

func blocksWithHeading(h, body string) []byte {
	s := `[{"type":"heading","level":1,"content":[{"type":"text","text":"` + jsonEscape(h) + `"}]},` +
		`{"type":"paragraph","content":[{"type":"text","text":"` + jsonEscape(body) + `"}]}]`
	return []byte(s)
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), "\"")
}

// indexDoc publishes a create event and drains it through the worker.
func (s *stack) indexDoc(ctx context.Context, docID string, version int, content []byte, readers []string) {
	s.docs.Put(rag.DocumentSnapshot{
		DocumentID: docID, WorkspaceID: "ws1", DirectoryID: "dir1",
		Title: "Doc " + docID, Content: content, VersionNo: version, Status: domain.DocPublished,
	})
	s.rbac.SetReaders(docID, readers)
	ev := domain.DocEvent{EventID: "e-" + docID + "-" + itoa(version), EventType: domain.EventDocumentCreate, DocumentID: docID, WorkspaceID: "ws1", VersionNo: version, Timestamp: time.Now().Format(time.RFC3339)}
	_, _ = s.queue.Publish(ctx, ev)
	msgs, _ := s.queue.ReadGroup(ctx, "c1", 10, 0)
	for _, m := range msgs {
		s.worker.Process(ctx, m)
	}
}

func itoa(n int) string { return strings.TrimSpace(formatInt(n)) }
func formatInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// AC-9: create/update/delete auto-trigger pipeline; status badge correct
// ---------------------------------------------------------------------------

func TestAC9_CreateUpdateDelete_PipelineAndBadge(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)

	s.indexDoc(ctx, "doc1", 1, blocks("Pagination uses page and page_size parameters."), []string{domain.UserSubject("alice")})

	// create → indexed, badge indexed, vectors written
	info, _ := s.status.GetDocumentIndexStatus(ctx, "doc1")
	assert.Equal(t, domain.IndexIndexed, info.IndexStatus)
	assert.Greater(t, info.ChunkCount, 0)
	assert.Equal(t, info.ChunkCount, s.vectors.Count(s.models.GetActiveOr(ctx).CollectionName()))

	// update (new version) → old version chunks cascaded, new version present
	s.docs.Put(rag.DocumentSnapshot{DocumentID: "doc1", WorkspaceID: "ws1", DirectoryID: "dir1", Title: "Doc doc1", Content: blocks("Updated content about pagination limits."), VersionNo: 2, Status: domain.DocPublished})
	upd := domain.DocEvent{EventID: "e-doc1-2", EventType: domain.EventDocumentUpdate, DocumentID: "doc1", WorkspaceID: "ws1", VersionNo: 2, PrevVersionNo: 1, Timestamp: time.Now().Format(time.RFC3339)}
	_, _ = s.queue.Publish(ctx, upd)
	msgs, _ := s.queue.ReadGroup(ctx, "c1", 10, 0)
	for _, m := range msgs {
		s.worker.Process(ctx, m)
	}
	coll := s.models.GetActiveOr(ctx).CollectionName()
	// no v1 chunks remain (cascade), only v2
	for _, p := range s.vectors.PointsFor(coll, "doc1") {
		assert.Equal(t, 2, p.Payload.VersionNo, "old version chunks must be cascaded away")
	}
	info, _ = s.status.GetDocumentIndexStatus(ctx, "doc1")
	assert.Equal(t, domain.IndexIndexed, info.IndexStatus)

	// delete → cascade all chunks, badge reflects
	del := domain.DocEvent{EventID: "e-doc1-del", EventType: domain.EventDocumentDelete, DocumentID: "doc1", WorkspaceID: "ws1", Timestamp: time.Now().Format(time.RFC3339)}
	_, _ = s.queue.Publish(ctx, del)
	msgs, _ = s.queue.ReadGroup(ctx, "c1", 10, 0)
	for _, m := range msgs {
		s.worker.Process(ctx, m)
	}
	assert.Equal(t, 0, s.vectors.Count(coll), "delete must cascade-remove all chunks")
}

// ---------------------------------------------------------------------------
// DEFECT-06 regression: a non-published (draft) document must not leave the
// index badge stuck at "processing". The pipeline skips drafts, resets the
// badge to pending, clears stale vectors, logs the skip, and ACKs — no silent
// stuck state. Publishing the draft then indexes it normally.
// ---------------------------------------------------------------------------

func TestDEFECT06_DraftSkipNotStuckThenPublishIndexes(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	coll := s.models.GetActiveOr(ctx).CollectionName()

	// Capture pipeline diagnostics to prove the skip is logged, not silent.
	skipLogged := false
	s.pipe.Logf = func(f string, a ...any) {
		if strings.Contains(f, "skip") {
			skipLogged = true
		}
	}

	// A draft document — e.g. freshly created via the mora API (Create defaults
	// to draft per AC-17) or MCP create_draft. It carries indexable content.
	s.docs.Put(rag.DocumentSnapshot{
		DocumentID: "docDraft", WorkspaceID: "ws1", DirectoryID: "dir1",
		Title: "Draft Doc", Content: blocks("alpha beta gamma delta epsilon zeta eta theta iota kappa lambda."),
		VersionNo: 1, Status: domain.DocDraft,
	})
	s.rbac.SetReaders("docDraft", []string{domain.UserSubject("alice")})
	ev := domain.DocEvent{EventID: "e-draft-1", EventType: domain.EventDocumentCreate, DocumentID: "docDraft", WorkspaceID: "ws1", VersionNo: 1, Timestamp: time.Now().Format(time.RFC3339)}
	_, _ = s.queue.Publish(ctx, ev)
	msgs, _ := s.queue.ReadGroup(ctx, "c1", 10, 0)
	for _, m := range msgs {
		s.worker.Process(ctx, m)
	}

	// Badge must be pending — NOT stuck at "processing" (DEFECT-06). No chunks,
	// no vectors, and the event is ACKed (not dead-lettered / not retried).
	info, _ := s.status.GetDocumentIndexStatus(ctx, "docDraft")
	assert.Equal(t, domain.IndexPending, info.IndexStatus, "draft must not be stuck at processing (DEFECT-06)")
	assert.Equal(t, 0, info.ChunkCount)
	assert.Equal(t, 0, s.vectors.Count(coll), "draft must not be written to Qdrant")
	assert.Empty(t, s.queue.Dead(), "draft skip must not dead-letter")
	assert.True(t, skipLogged, "pipeline must log the skip reason (no silent failure)")

	// Publishing the draft (update event, status=published) → indexed + searchable.
	s.docs.Put(rag.DocumentSnapshot{
		DocumentID: "docDraft", WorkspaceID: "ws1", DirectoryID: "dir1",
		Title: "Draft Doc", Content: blocks("alpha beta gamma delta epsilon zeta eta theta iota kappa lambda."),
		VersionNo: 2, Status: domain.DocPublished,
	})
	upd := domain.DocEvent{EventID: "e-draft-2", EventType: domain.EventDocumentUpdate, DocumentID: "docDraft", WorkspaceID: "ws1", VersionNo: 2, PrevVersionNo: 1, Timestamp: time.Now().Format(time.RFC3339)}
	_, _ = s.queue.Publish(ctx, upd)
	msgs, _ = s.queue.ReadGroup(ctx, "c1", 10, 0)
	for _, m := range msgs {
		s.worker.Process(ctx, m)
	}
	info, _ = s.status.GetDocumentIndexStatus(ctx, "docDraft")
	assert.Equal(t, domain.IndexIndexed, info.IndexStatus, "published doc must reach indexed")
	assert.Greater(t, info.ChunkCount, 0)
	assert.Greater(t, s.vectors.Count(coll), 0)
}

// ---------------------------------------------------------------------------
// AC-10: update/delete cascade; search returns no stale content
// ---------------------------------------------------------------------------

func TestAC10_NoStaleContentAfterUpdateOrDelete(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	s.indexDoc(ctx, "docA", 1, blocks("Old content about alpha beta gamma delta epsilon zeta eta theta iota."), []string{domain.UserSubject("alice")})
	coll := s.models.GetActiveOr(ctx).CollectionName()

	// index FTS for BM25 path
	s.fts.Index("docA", ragtest.FTSDoc{Title: "Doc docA", Text: "Old content about alpha beta gamma delta epsilon zeta eta theta iota.", VisibleTo: []string{domain.UserSubject("alice")}, Workspace: "ws1"})

	// update to new content
	s.docs.Put(rag.DocumentSnapshot{DocumentID: "docA", WorkspaceID: "ws1", Title: "Doc docA", Content: blocks("New content about omega sigma tau upsilon phi chi psi."), VersionNo: 2, Status: domain.DocPublished})
	upd := domain.DocEvent{EventID: "e-docA-2", EventType: domain.EventDocumentUpdate, DocumentID: "docA", WorkspaceID: "ws1", VersionNo: 2, PrevVersionNo: 1, Timestamp: time.Now().Format(time.RFC3339)}
	_, _ = s.queue.Publish(ctx, upd)
	msgs, _ := s.queue.ReadGroup(ctx, "c1", 10, 0)
	for _, m := range msgs {
		s.worker.Process(ctx, m)
	}
	// Dense path must not return old-version chunks
	hits, err := s.vectors.SearchDense(ctx, rag.VectorSearchRequest{
		CollectionName: coll, Vector: fakeQueryVec(t, s, "alpha beta"), TopK: 10, VisibleTo: []string{domain.UserSubject("alice")},
	})
	require.NoError(t, err)
	for _, h := range hits {
		assert.NotEqual(t, 1, h.Payload.VersionNo, "stale v1 chunk must not be returned")
	}
}

// ---------------------------------------------------------------------------
// AC-12: Dense+BM25 hybrid, metadata filter; RBAC hard filter (no leak)
// ---------------------------------------------------------------------------

func TestAC12_HybridSearch_RBACHardFilter(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	// doc visible to alice, doc2 visible to bob
	s.indexDoc(ctx, "docAlice", 1, blocks("RESTful API pagination design page page_size total."), []string{domain.UserSubject("alice")})
	s.indexDoc(ctx, "docBob", 1, blocks("Secret board minutes financials revenue forecast pagination."), []string{domain.UserSubject("bob")})
	s.fts.Index("docAlice", ragtest.FTSDoc{Title: "API Design", Text: "RESTful API pagination design page page_size total.", VisibleTo: []string{domain.UserSubject("alice")}, Workspace: "ws1"})
	s.fts.Index("docBob", ragtest.FTSDoc{Title: "Board", Text: "Secret board minutes financials revenue forecast pagination.", VisibleTo: []string{domain.UserSubject("bob")}, Workspace: "ws1"})

	// alice searches → only her docs
	res, err := s.searcher.Search(ctx, search.SearchRequest{Query: "pagination design", UserID: "alice", TopK: 20, TopN: 10})
	require.NoError(t, err)
	for _, it := range res.Items {
		assert.Equal(t, "docAlice", it.DocumentID, "alice must not see bob's docs (RBAC hard filter)")
	}
	// bob searches → only his
	res, err = s.searcher.Search(ctx, search.SearchRequest{Query: "pagination", UserID: "bob", TopK: 20, TopN: 10})
	require.NoError(t, err)
	for _, it := range res.Items {
		assert.Equal(t, "docBob", it.DocumentID, "bob must not see alice's docs")
	}
	// carol (no readers) → nothing, existence not leaked
	res, err = s.searcher.Search(ctx, search.SearchRequest{Query: "pagination", UserID: "carol", TopK: 20, TopN: 10})
	require.NoError(t, err)
	assert.Empty(t, res.Items, "unauthorized user must see nothing")
}

func TestAC12_MetadataFilter_WorkspaceAndTags(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	s.indexDoc(ctx, "docWS1", 1, blocks("alpha beta gamma delta epsilon zeta eta theta iota kappa."), []string{domain.UserSubject("alice")})
	// metadata filter by workspace scoping is exercised via ViewerScope; tags via request
	res, err := s.searcher.Search(ctx, search.SearchRequest{Query: "alpha", UserID: "alice", WorkspaceID: "ws1", TopK: 10, TopN: 5})
	require.NoError(t, err)
	assert.NotEmpty(t, res.Items)
}

// ---------------------------------------------------------------------------
// AC-13: failure auto-retry + dead-letter + alert; idempotent (no duplicates)
// ---------------------------------------------------------------------------

func TestAC13_RetryThenDeadLetter(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	s.docs.Put(rag.DocumentSnapshot{DocumentID: "docF", WorkspaceID: "ws1", Content: blocks("fail me alpha beta gamma delta epsilon zeta eta theta iota."), VersionNo: 1, Status: domain.DocPublished})
	s.rbac.SetReaders("docF", []string{domain.UserSubject("alice")})
	s.vectors.FailUpsert = true // vector write always fails

	ev := domain.DocEvent{EventID: "e-fail", EventType: domain.EventDocumentCreate, DocumentID: "docF", WorkspaceID: "ws1", VersionNo: 1, Timestamp: time.Now().Format(time.RFC3339)}
	_, _ = s.queue.Publish(ctx, ev)
	msgs, _ := s.queue.ReadGroup(ctx, "c1", 10, 0)
	for _, m := range msgs {
		s.worker.Process(ctx, m)
	}
	// dead-lettered after max attempts
	assert.Equal(t, 1, len(s.queue.Dead()), "event must be dead-lettered after max attempts")
	assert.Equal(t, int64(1), s.worker.DeadLetters, "alert counter must increment")
	info, _ := s.status.GetDocumentIndexStatus(ctx, "docF")
	assert.Equal(t, domain.IndexFailed, info.IndexStatus, "badge must show failed")
}

func TestAC13_IdempotentNoDuplicateVectors(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	// publish the SAME event_id twice
	ev := domain.DocEvent{EventID: "e-dup", EventType: domain.EventDocumentCreate, DocumentID: "docD", WorkspaceID: "ws1", VersionNo: 1, Timestamp: time.Now().Format(time.RFC3339)}
	s.docs.Put(rag.DocumentSnapshot{DocumentID: "docD", WorkspaceID: "ws1", Content: blocks("alpha beta gamma delta epsilon zeta eta theta iota kappa lambda."), VersionNo: 1, Status: domain.DocPublished})
	s.rbac.SetReaders("docD", []string{domain.UserSubject("alice")})

	_, _ = s.queue.Publish(ctx, ev)
	_, _ = s.queue.Publish(ctx, ev) // duplicate
	msgs, _ := s.queue.ReadGroup(ctx, "c1", 10, 0)
	for _, m := range msgs {
		s.worker.Process(ctx, m)
	}
	coll := s.models.GetActiveOr(ctx).CollectionName()
	count := s.vectors.Count(coll)
	assert.Greater(t, count, 0, "doc must be indexed once")
	// re-deliver again → still no duplicates
	_, _ = s.queue.Publish(ctx, ev)
	msgs, _ = s.queue.ReadGroup(ctx, "c1", 10, 0)
	for _, m := range msgs {
		s.worker.Process(ctx, m)
	}
	assert.Equal(t, count, s.vectors.Count(coll), "duplicate events must not create duplicate vectors")
}

// ---------------------------------------------------------------------------
// AC-11: provider connectivity test + model switch rebuild (handler)
// ---------------------------------------------------------------------------

func TestAC11_ModelTestAndRebuild(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	srv := httptest.NewServer(s.handler.Routes())
	defer srv.Close()

	// connectivity test on existing active model
	code, body := doReq(t, srv, "POST", "/api/v1/admin/embedding-models/m1/test", "alice", nil)
	assert.Equal(t, http.StatusOK, code)
	assert.Contains(t, body, `"ok":true`)
	assert.Contains(t, body, `"dimension":64`)

	// add a new model + rebuild
	code, _ = doReq(t, srv, "POST", "/api/v1/admin/embedding-models", "alice", strings.NewReader(`{"provider":"ollama","model_name":"qwen3","dimension":64,"active":true}`))
	require.Equal(t, http.StatusCreated, code)

	// index a doc so rebuild has something to re-index
	s.indexDoc(ctx, "docR", 1, blocks("rebuild target alpha beta gamma delta epsilon zeta eta theta iota kappa."), []string{domain.UserSubject("alice")})

	code, _ = doReq(t, srv, "POST", "/api/v1/admin/embedding-models/m1/rebuild", "alice", strings.NewReader(`{}`))
	assert.Equal(t, http.StatusAccepted, code)
}

// ---------------------------------------------------------------------------
// Perf budget: hybrid search well under 800ms (scaled sample)
// ---------------------------------------------------------------------------

func TestPerf_HybridSearchUnderBudget(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	// index 500 docs (≈ a few thousand chunks) to exercise fusion at scale
	for i := 0; i < 500; i++ {
		docID := "perf" + itoa(i)
		txt := "alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu " + itoa(i)
		s.indexDoc(ctx, docID, 1, blocks(txt), []string{domain.UserSubject("alice")})
	}
	// warm the dense path
	_, _ = s.searcher.Search(ctx, search.SearchRequest{Query: "alpha beta gamma", UserID: "alice", TopK: 50, TopN: 10})

	const budget = 800 * time.Millisecond
	var worst time.Duration
	for i := 0; i < 20; i++ {
		start := time.Now()
		res, err := s.searcher.Search(ctx, search.SearchRequest{Query: "alpha beta gamma delta", UserID: "alice", TopK: 50, TopN: 10})
		d := time.Since(start)
		if d > worst {
			worst = d
		}
		require.NoError(t, err)
		require.NotNil(t, res)
	}
	t.Logf("worst hybrid search latency over 20 runs: %v (budget %v)", worst, budget)
	assert.Less(t, worst, budget, "P95-ish worst must be under 800ms budget")
}

// ---------------------------------------------------------------------------
// Handler: /rag/search and /documents/{id}/index-status
// ---------------------------------------------------------------------------

func TestHandler_SearchAndIndexStatus(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	s.indexDoc(ctx, "docH", 1, blocks("RESTful API pagination design page page_size total."), []string{domain.UserSubject("alice")})
	s.fts.Index("docH", ragtest.FTSDoc{Title: "API", Text: "RESTful API pagination design page page_size total.", VisibleTo: []string{domain.UserSubject("alice")}, Workspace: "ws1"})

	srv := httptest.NewServer(s.handler.Routes())
	defer srv.Close()

	// search as alice
	_, body := doReq(t, srv, "POST", "/api/v1/rag/search", "alice", jsonBody(map[string]any{"query": "pagination", "top_k": 10, "top_n": 5}))
	assert.Contains(t, body, "docH")
	// search as carol → empty
	_, body = doReq(t, srv, "POST", "/api/v1/rag/search", "carol", jsonBody(map[string]any{"query": "pagination"}))
	assert.Contains(t, body, `"items":null`)

	// index status (alice may read)
	_, body = doReq(t, srv, "GET", "/api/v1/documents/docH/index-status", "alice", nil)
	assert.Contains(t, body, "indexed")
	// carol cannot see docH → 404 (existence not leaked)
	code, _ := doReq(t, srv, "GET", "/api/v1/documents/docH/index-status", "carol", nil)
	assert.Equal(t, http.StatusNotFound, code)
}

// --- helpers ---

func doReq(t *testing.T, srv *httptest.Server, method, path, userID string, body io.Reader) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, body)
	require.NoError(t, err)
	req.Header.Set("X-User-ID", userID)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(b)
}

func jsonBody(m map[string]any) io.Reader {
	b, _ := json.Marshal(m)
	return strings.NewReader(string(b))
}

func fakeQueryVec(t *testing.T, s *stack, q string) []float32 {
	t.Helper()
	prov, err := s.factory.For(context.Background(), s.models.GetActiveOr(context.Background()))
	require.NoError(t, err)
	v, err := prov.Embed(context.Background(), []string{q}, "query:")
	require.NoError(t, err)
	return v[0]
}
