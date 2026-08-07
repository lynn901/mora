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
