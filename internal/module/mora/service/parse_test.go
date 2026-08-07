package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag/parser"
	"github.com/lynn901/mora/internal/pkg/errors"
)

// These tests exercise the parse service's RBAC-independent paths (admin
// bypass, size limit, object-store + event publish). The RBAC existence-non-
// leak guarantee is enforced by the engine.Check path wired in production;
// the handler-level test (handler package) covers the 404-not-403 behavior
// end-to-end. rbac.Engine is a concrete struct, so the service field is
// injected at wiring time, not stubbed here.

// fakeDocRepo is an in-memory DocumentRepo for parse-service tests.
type fakeDocRepo struct {
	docs map[domain.UUID]*domain.Document
}

func newFakeDocRepo(docs ...*domain.Document) *fakeDocRepo {
	m := map[domain.UUID]*domain.Document{}
	for _, d := range docs {
		m[d.ID] = d
	}
	return &fakeDocRepo{docs: m}
}
func (r *fakeDocRepo) List(ctx context.Context, q DocumentQuery) ([]domain.Document, int, error) {
	return nil, 0, nil
}
func (r *fakeDocRepo) Get(ctx context.Context, id domain.UUID) (*domain.Document, error) {
	if d, ok := r.docs[id]; ok {
		return d, nil
	}
	return nil, ErrNotFound
}
func (r *fakeDocRepo) Create(ctx context.Context, d *domain.Document) error {
	r.docs[d.ID] = d
	return nil
}
func (r *fakeDocRepo) Update(ctx context.Context, d *domain.Document, prev int) error { return nil }
func (r *fakeDocRepo) SoftDelete(ctx context.Context, id, user domain.UUID) error     { return nil }
func (r *fakeDocRepo) DocumentIDsForTarget(ctx context.Context, t domain.TargetType, id domain.UUID) ([]domain.UUID, error) {
	return nil, nil
}

// fakeObjectStore records Puts in-memory (no real S3).
type fakeObjectStore struct{ puts map[string][]byte }

func (s *fakeObjectStore) Put(ctx context.Context, key, ct string, data []byte) (string, error) {
	if s.puts == nil {
		s.puts = map[string][]byte{}
	}
	s.puts[key] = data
	return key, nil
}

// fakeParseQueue records published parse events.
type fakeParseQueue struct{ events []ParseEvent }

func (q *fakeParseQueue) PublishParse(ctx context.Context, ev ParseEvent) error {
	q.events = append(q.events, ev)
	return nil
}

// fakeProgressStore stubs the parse-progress read model.
type fakeProgressStore struct {
	result ParseProgressResult
	err    error
}

func (s *fakeProgressStore) GetParseProgress(ctx context.Context, docID string) (ParseProgressResult, error) {
	return s.result, s.err
}

// fakePreviewer stubs the chunk previewer.
type fakePreviewer struct{}

func (fakePreviewer) Preview(ctx context.Context, text string, opts parser.ParseOptions) (ChunkPreviewResult, error) {
	return ChunkPreviewResult{Total: 1, Strategy: "fixed", Chunks: []ChunkPreviewItem{{Text: text, ChunkIndex: 0}}}, nil
}

// fakeConfigStore stubs the parse config store.
type fakeConfigStore struct{}

func (fakeConfigStore) List(ctx context.Context, ws string) ([]ParseConfig, error) { return nil, nil }
func (fakeConfigStore) Get(ctx context.Context, id string) (ParseConfig, error) {
	return ParseConfig{}, nil
}
func (fakeConfigStore) Create(ctx context.Context, c ParseConfig) (ParseConfig, error) {
	return c, nil
}
func (fakeConfigStore) Update(ctx context.Context, id string, c ParseConfig) (ParseConfig, error) {
	return c, nil
}
func (fakeConfigStore) Delete(ctx context.Context, id string) error { return nil }

func TestParseService_UploadFile_AdminSucceedsAndPublishes(t *testing.T) {
	ws := uuid.New()
	user := uuid.New()
	docs := newFakeDocRepo()
	objects := &fakeObjectStore{}
	queue := &fakeParseQueue{}
	svc := NewParseService(docs, nil, queue, objects, &fakeProgressStore{}, fakePreviewer{}, fakeConfigStore{}, 10)
	auth := AuthContext{UserID: user, IsAdmin: true}
	out, err := svc.UploadFile(context.Background(), auth, UploadRequest{
		WorkspaceID: ws, Filename: "readme.md", MIME: "text/markdown", FileData: []byte("# Title"),
	})
	if err != nil {
		t.Fatalf("admin upload: %v", err)
	}
	if out.ParseStatus != "pending" {
		t.Errorf("parse_status = %q want pending", out.ParseStatus)
	}
	if len(queue.events) != 1 {
		t.Fatalf("expected 1 published parse event, got %d", len(queue.events))
	}
	if queue.events[0].EventType != "document.parse" {
		t.Errorf("event type = %q want document.parse", queue.events[0].EventType)
	}
	if queue.events[0].SourceFormat != "md" {
		t.Errorf("source format = %q want md", queue.events[0].SourceFormat)
	}
	if queue.events[0].StorageKey == "" {
		t.Errorf("storage key empty in published event")
	}
}

func TestParseService_UploadFile_SizeLimitRejected(t *testing.T) {
	ws := uuid.New()
	user := uuid.New()
	docs := newFakeDocRepo()
	objects := &fakeObjectStore{}
	queue := &fakeParseQueue{}
	svc := NewParseService(docs, nil, queue, objects, &fakeProgressStore{}, fakePreviewer{}, fakeConfigStore{}, 1) // 1MB cap
	auth := AuthContext{UserID: user, IsAdmin: true}
	big := make([]byte, 2<<20) // 2MB
	_, err := svc.UploadFile(context.Background(), auth, UploadRequest{
		WorkspaceID: ws, Filename: "big.txt", FileData: big,
	})
	if err == nil {
		t.Fatal("expected size-limit error, got nil")
	}
	if e := errors.As(err); e == nil || e.Code != errors.CodeBadRequest {
		t.Errorf("expected bad-request error, got %T: %v", err, err)
	}
}

func TestParseService_UploadFile_ObjectStoreKeyShape(t *testing.T) {
	ws := uuid.New()
	user := uuid.New()
	docs := newFakeDocRepo()
	objects := &fakeObjectStore{}
	queue := &fakeParseQueue{}
	svc := NewParseService(docs, nil, queue, objects, &fakeProgressStore{}, fakePreviewer{}, fakeConfigStore{}, 0)
	auth := AuthContext{UserID: user, IsAdmin: true}
	out, err := svc.UploadFile(context.Background(), auth, UploadRequest{
		WorkspaceID: ws, Filename: "doc.pdf", MIME: "application/pdf", FileData: []byte("%PDF-1.4"),
	})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if len(objects.puts) != 1 {
		t.Fatalf("expected 1 object put, got %d", len(objects.puts))
	}
	var key string
	for k := range objects.puts {
		key = k
	}
	wantPrefix := "mora/" + ws.String() + "/" + out.DocumentID.String() + "/source/"
	if len(key) < len(wantPrefix) || key[:len(wantPrefix)] != wantPrefix {
		t.Errorf("storage key prefix = %q want %q", key, wantPrefix)
	}
	// document row carries storage_key + source_format + parse_status=pending.
	d := docs.docs[out.DocumentID]
	if d.StorageKey != key {
		t.Errorf("document.storage_key = %q want %q", d.StorageKey, key)
	}
	if d.SourceFormat != "pdf" {
		t.Errorf("document.source_format = %q want pdf", d.SourceFormat)
	}
	if d.ParseStatus != domain.ParsePending {
		t.Errorf("document.parse_status = %q want pending", d.ParseStatus)
	}
}

func TestParseService_UploadFile_ObjectStoreMissing(t *testing.T) {
	ws := uuid.New()
	user := uuid.New()
	docs := newFakeDocRepo()
	svc := NewParseService(docs, nil, nil, nil, &fakeProgressStore{}, fakePreviewer{}, fakeConfigStore{}, 0)
	auth := AuthContext{UserID: user, IsAdmin: true}
	_, err := svc.UploadFile(context.Background(), auth, UploadRequest{
		WorkspaceID: ws, Filename: "x.txt", FileData: []byte("x"),
	})
	if err == nil {
		t.Fatal("expected error when object store is nil")
	}
	if e := errors.As(err); e == nil || e.Code != errors.CodeBadRequest {
		t.Errorf("expected bad-request, got %v", err)
	}
}

func TestParseService_Reparse_AdminEnqueuesAll(t *testing.T) {
	ws := uuid.New()
	user := uuid.New()
	d1 := &domain.Document{ID: uuid.New(), WorkspaceID: ws, StorageKey: "k1", SourceFormat: "pdf"}
	d2 := &domain.Document{ID: uuid.New(), WorkspaceID: ws, StorageKey: "k2", SourceFormat: "pdf"}
	docs := newFakeDocRepo(d1, d2)
	queue := &fakeParseQueue{}
	svc := NewParseService(docs, nil, queue, &fakeObjectStore{}, &fakeProgressStore{}, fakePreviewer{}, fakeConfigStore{}, 0)
	auth := AuthContext{UserID: user, IsAdmin: true}
	out, err := svc.Reparse(context.Background(), auth, ReparseRequest{
		WorkspaceID: ws, DocumentIDs: []domain.UUID{d1.ID, d2.ID, uuid.New()},
	})
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	// 2 real docs enqueued; the missing third silently skipped (no leak).
	if out.Enqueued != 2 {
		t.Errorf("enqueued = %d want 2", out.Enqueued)
	}
	if len(out.TaskIDs) != 2 {
		t.Errorf("task ids = %d want 2", len(out.TaskIDs))
	}
	// every published event is a document.reparse carrying the doc's storage_key.
	for _, ev := range queue.events {
		if ev.EventType != "document.reparse" {
			t.Errorf("event type = %q want document.reparse", ev.EventType)
		}
		if ev.StorageKey == "" {
			t.Errorf("reparse event missing storage_key")
		}
	}
}

func TestParseService_ParseProgress_AdminMissingDoc404(t *testing.T) {
	// Existence-non-leak: a missing document's parse-progress must surface as
	// NotFound (the service maps the store error to NotFound). With admin auth
	// the RBAC path is skipped, so this isolates the not-found mapping.
	user := uuid.New()
	svc := NewParseService(nil, nil, nil, nil, &fakeProgressStore{err: ErrProgressMissing}, nil, nil, 0)
	auth := AuthContext{UserID: user, IsAdmin: true}
	_, err := svc.ParseProgress(context.Background(), auth, uuid.New())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if e := errors.As(err); e == nil || e.Code != errors.CodeNotFound {
		t.Errorf("expected not-found, got %v", err)
	}
}

func TestParseService_ChunkPreview(t *testing.T) {
	svc := NewParseService(nil, nil, nil, nil, nil, fakePreviewer{}, nil, 0)
	out, err := svc.ChunkPreview(context.Background(), "sample text", parser.ParseOptions{})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if out.Total != 1 {
		t.Errorf("total = %d want 1", out.Total)
	}
}

// ErrProgressMissing simulates the store's not-found error for a missing doc.
var ErrProgressMissing = errors.NotFound("document not found")

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
