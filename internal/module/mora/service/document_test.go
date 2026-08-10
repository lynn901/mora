package service

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/pkg/pagination"
)

// These tests cover DocumentService lifecycle paths that the parse/upload path
// doesn't touch. rbac.Engine is a concrete struct injected at wiring time, so
// the admin-bypass path is exercised here; the RBAC engine.Check path is
// covered by the handler-level tests.

// fakeVersionRepo is an in-memory VersionRepo for document-service tests.
type fakeVersionRepo struct {
	versions map[domain.UUID][]domain.DocumentVersion
}

func newFakeVersionRepo() *fakeVersionRepo {
	return &fakeVersionRepo{versions: map[domain.UUID][]domain.DocumentVersion{}}
}

func (r *fakeVersionRepo) List(ctx context.Context, docID domain.UUID, p pagination.Params) ([]domain.DocumentVersion, int, error) {
	return nil, 0, nil
}
func (r *fakeVersionRepo) Get(ctx context.Context, docID domain.UUID, versionNo int) (*domain.DocumentVersion, error) {
	return nil, ErrNotFound
}
func (r *fakeVersionRepo) Create(ctx context.Context, v *domain.DocumentVersion) error {
	r.versions[v.DocumentID] = append(r.versions[v.DocumentID], *v)
	return nil
}
func (r *fakeVersionRepo) MaxVersionNo(ctx context.Context, docID domain.UUID) (int, error) { return 0, nil }

// noopEventPublisher records document events; mirrors event.NoopPublisher but
// stays in-package to avoid a cross-package test dependency.
type noopEventPublisher struct{ events []DocumentEvent }

func (p *noopEventPublisher) PublishDocumentEvent(ctx context.Context, evt DocumentEvent) error {
	p.events = append(p.events, evt)
	return nil
}
func (p *noopEventPublisher) PublishModelRebuild(ctx context.Context, workspaceID string) error { return nil }

// TestDocumentService_Create_SetsParseStatusParsed guards the regression in
// YS-86: Create must set ParseStatus=ParseParsed so the postgres INSERT does
// not pass NULL for the NOT NULL documents.parse_status column (which would
// violate the constraint and surface as HTTP 500 on POST /documents).
func TestDocumentService_Create_SetsParseStatusParsed(t *testing.T) {
	docs := newFakeDocRepo()
	versions := newFakeVersionRepo()
	svc := NewDocumentService(docs, versions, nil, &noopEventPublisher{})

	auth := AuthContext{UserID: uuid.New(), IsAdmin: true}
	in := &domain.Document{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		Title:       "hello",
		Content:     []domain.Block{{Type: "p", Text: "hi"}},
	}
	out, err := svc.Create(context.Background(), auth, in)
	if err != nil {
		t.Fatalf("Create: unexpected error %v", err)
	}
	if out.ParseStatus != domain.ParseParsed {
		t.Errorf("ParseStatus = %q, want %q (ParseParsed); a zero value \"\" "+
			"hits NULL via nullIfEmpty and violates documents.parse_status NOT NULL",
			out.ParseStatus, domain.ParseParsed)
	}
	// The persisted record must carry the same value (the fake stores the
	// pointer the service mutated, so this also asserts the repo saw it).
	if got := docs.docs[in.ID].ParseStatus; got != domain.ParseParsed {
		t.Errorf("persisted ParseStatus = %q, want %q", got, domain.ParseParsed)
	}
}

// recordingSink is a fake DocWriteSink that records the calls so the double-
// write routing can be asserted without a database.
type recordingSink struct {
	writes  []recordedWrite
	deletes []recordedDelete
	doc     *domain.Document
}

type recordedWrite struct {
	d           *domain.Document
	version     *domain.DocumentVersion
	prevVersion int
	create      bool
	ev          domain.KnowledgeEvent
}

type recordedDelete struct {
	id     domain.UUID
	userID domain.UUID
	ev     domain.KnowledgeEvent
}

func (s *recordingSink) WriteDoc(_ context.Context, d *domain.Document, v *domain.DocumentVersion, prevVersion int, create bool, ev domain.KnowledgeEvent) (*domain.Document, error) {
	s.writes = append(s.writes, recordedWrite{d: d, version: v, prevVersion: prevVersion, create: create, ev: ev})
	if s.doc != nil {
		return s.doc, nil
	}
	return d, nil
}
func (s *recordingSink) DeleteDoc(_ context.Context, id, userID domain.UUID, ev domain.KnowledgeEvent) error {
	s.deletes = append(s.deletes, recordedDelete{id: id, userID: userID, ev: ev})
	return nil
}

// TestDocumentService_DoubleWrite_RoutesCreateThroughSink: when a sink is
// wired, Create hands the doc + version + Knowledge event to the sink INSTEAD
// of the legacy docs.Create + versions.Create path (§6.3, PR2 item ⑤).
func TestDocumentService_DoubleWrite_RoutesCreateThroughSink(t *testing.T) {
	docs := newFakeDocRepo()
	versions := newFakeVersionRepo()
	sink := &recordingSink{}
	svc := NewDocumentService(docs, versions, nil, &noopEventPublisher{}).WithSink(sink)

	auth := AuthContext{UserID: uuid.New(), IsAdmin: true}
	in := &domain.Document{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		Title:       "hello",
		Content:     []domain.Block{{Type: "p", Text: "hi"}},
	}
	if _, err := svc.Create(context.Background(), auth, in); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(sink.writes) != 1 {
		t.Fatalf("sink.WriteDoc calls = %d, want 1", len(sink.writes))
	}
	w := sink.writes[0]
	if !w.create {
		t.Errorf("sink create flag = false, want true")
	}
	if w.version.DocumentID != in.ID {
		t.Errorf("sink version.DocumentID = %v, want %v", w.version.DocumentID, in.ID)
	}
	// Envelope carries IDs, not content (§5.1: no content/credentials in events).
	if w.ev.AggregateType != domain.AggKnowledgeAsset {
		t.Errorf("ev.AggregateType = %q, want %q", w.ev.AggregateType, domain.AggKnowledgeAsset)
	}
	if w.ev.AggregateID != in.ID {
		t.Errorf("ev.AggregateID = %v, want doc id %v", w.ev.AggregateID, in.ID)
	}
	if w.ev.EventType != domain.KEAssetCreated {
		t.Errorf("ev.EventType = %q, want %q", w.ev.EventType, domain.KEAssetCreated)
	}
	if w.ev.Actor.ID != auth.UserID {
		t.Errorf("ev.Actor.ID = %v, want %v", w.ev.Actor.ID, auth.UserID)
	}
	// The legacy path (docs.Create) must NOT have run when the sink took over.
	if _, ok := docs.docs[in.ID]; ok {
		t.Errorf("docs.Create ran despite sink routing — legacy path must be bypassed")
	}
}

// TestDocumentService_DoubleWrite_NoSinkIsUnchanged: with no sink wired, the
// service uses its legacy non-tx path (docs.Create + versions.Create + event).
// This is the regression red line — the double-write hook must not change
// behavior when the sink is absent.
func TestDocumentService_DoubleWrite_NoSinkIsUnchanged(t *testing.T) {
	docs := newFakeDocRepo()
	versions := newFakeVersionRepo()
	pub := &noopEventPublisher{}
	svc := NewDocumentService(docs, versions, nil, pub) // no WithSink

	auth := AuthContext{UserID: uuid.New(), IsAdmin: true}
	in := &domain.Document{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		Title:       "hello",
		Content:     []domain.Block{{Type: "p", Text: "hi"}},
	}
	if _, err := svc.Create(context.Background(), auth, in); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Legacy path wrote the doc + a version + an event.
	if _, ok := docs.docs[in.ID]; !ok {
		t.Errorf("docs.Create did not run on the no-sink path")
	}
	if len(versions.versions[in.ID]) != 1 {
		t.Errorf("versions.Create did not run on the no-sink path; got %d", len(versions.versions[in.ID]))
	}
	if len(pub.events) != 1 {
		t.Errorf("PublishDocumentEvent did not run on the no-sink path; got %d", len(pub.events))
	}
}

// TestDocumentService_DoubleWrite_RoutesDeleteThroughSink: Delete routes
// through the sink when wired; the legacy SoftDelete + event path does not run.
func TestDocumentService_DoubleWrite_RoutesDeleteThroughSink(t *testing.T) {
	docs := newFakeDocRepo()
	versions := newFakeVersionRepo()
	pub := &noopEventPublisher{}
	sink := &recordingSink{}
	svc := NewDocumentService(docs, versions, nil, pub).WithSink(sink)

	auth := AuthContext{UserID: uuid.New(), IsAdmin: true}
	in := &domain.Document{ID: uuid.New(), WorkspaceID: uuid.New(), Title: "x"}
	if err := docs.Create(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(context.Background(), auth, in.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(sink.deletes) != 1 {
		t.Fatalf("sink.DeleteDoc calls = %d, want 1", len(sink.deletes))
	}
	d := sink.deletes[0]
	if d.id != in.ID {
		t.Errorf("sink delete id = %v, want %v", d.id, in.ID)
	}
	if d.ev.EventType != domain.KEAssetDeprecated {
		t.Errorf("delete ev.EventType = %q, want %q", d.ev.EventType, domain.KEAssetDeprecated)
	}
	if len(pub.events) != 0 {
		t.Errorf("legacy PublishDocumentEvent ran despite sink routing; got %d events", len(pub.events))
	}
}
