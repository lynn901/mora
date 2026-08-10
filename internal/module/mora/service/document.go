package service

// Package service (document.go) orchestrates document lifecycle: create/update
// with version snapshotting, RBAC enforcement, content text extraction for FTS,
// and event publishing. It depends only on repository interfaces, keeping it
// unit-testable.

import (
	"context"

	stderrors "errors"

	"github.com/google/uuid"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/version"
	pkgerr "github.com/lynn901/mora/internal/pkg/errors"
	"github.com/lynn901/mora/internal/pkg/pagination"
	"github.com/lynn901/mora/internal/platform/rbac"
)

// ErrNotFound is the sentinel returned by repositories for missing records.
var ErrNotFound = stderrors.New("not found")

// DocumentService ties together document CRUD, versioning, RBAC, and events.
type DocumentService struct {
	docs     DocumentRepo
	versions VersionRepo
	rbac     *rbac.Engine
	events   EventPublisher
	// sink, when non-nil, routes Create/Update/Delete through a transactional
	// double-write: documents + document_versions + a Knowledge Outbox event
	// committed atomically (design-docs/13 §6.3, PR2 item ⑤). When nil the
	// service uses its legacy non-tx path — unchanged behavior (regression
	// red line). Set via WithSink.
	sink DocWriteSink
}

func NewDocumentService(docs DocumentRepo, versions VersionRepo, engine *rbac.Engine, events EventPublisher) *DocumentService {
	return &DocumentService{docs: docs, versions: versions, rbac: engine, events: events}
}

// WithSink attaches a transactional double-write sink and returns the service
// for chaining. Callers wire this at bootstrap when the outbox is configured.
func (s *DocumentService) WithSink(sink DocWriteSink) *DocumentService {
	s.sink = sink
	return s
}

// AuthContext carries the caller identity needed for RBAC + audit.
type AuthContext struct {
	UserID  domain.UUID
	Groups  []domain.UUID
	IsAdmin bool

	// SubjectType is the resolved principal kind (user / agent /
	// service_account). Internal-service callers without a delegated context
	// resolve to service_account (§4.4) — they get no admin bypass and only
	// the RBAC the service account actually has.
	SubjectType domain.SubjectType
	// IsServiceCaller marks internal-service callers (INTERNAL_SERVICE_TOKEN or
	// delegated). Service callers without a delegated context have restricted
	// capability and MUST NOT be treated as admin.
	IsServiceCaller bool
}

// Create creates a document, snapshots version 1, and publishes a create event.
func (s *DocumentService) Create(ctx context.Context, auth AuthContext, d *domain.Document) (*domain.Document, error) {
	if !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.UserID, auth.Groups, domain.TargetWorkspace, d.WorkspaceID, domain.ActionWrite)
		if err != nil {
			return nil, err
		}
		if !dec.Allowed {
			return nil, pkgerr.Forbidden("no write permission on workspace")
		}
	}
	d.Status = domain.StatusDraft
	d.IndexStatus = domain.IndexPending
	// Block-authored documents are already parsed content; mark them parsed so
	// the NOT NULL DEFAULT 'parsed' column on documents.parse_status (migration
	// 011) is never sent an explicit NULL via nullIfEmpty(""). Upload-file
	// documents get ParsePending set by the upload/parse path instead.
	d.ParseStatus = domain.ParseParsed
	d.CreatedBy = auth.UserID
	d.UpdatedBy = &auth.UserID
	d.VersionNo = 1
	d.ContentText = domain.BlockArray(d.Content).PlainText()

	if s.sink != nil {
		// Transactional double-write (§6.3): documents + version + Knowledge
		// Outbox event in one tx. The sink owns the tx and the outbox row.
		ev := s.knowledgeEventForDoc(EventCreate, d, 0, auth)
		return s.sink.WriteDoc(ctx, d, &domain.DocumentVersion{
			DocumentID: d.ID, VersionNo: 1, Content: d.Content, ContentText: d.ContentText,
			AuthorID: auth.UserID, DiffSummary: "initial",
		}, 0, true, ev)
	}

	if err := s.docs.Create(ctx, d); err != nil {
		return nil, err
	}
	if err := s.versions.Create(ctx, &domain.DocumentVersion{
		DocumentID: d.ID, VersionNo: 1, Content: d.Content, ContentText: d.ContentText,
		AuthorID: auth.UserID, DiffSummary: "initial",
	}); err != nil {
		return nil, err
	}
	_ = s.events.PublishDocumentEvent(ctx, DocumentEvent{
		Type: EventCreate, DocumentID: d.ID, WorkspaceID: d.WorkspaceID, VersionNo: 1,
	})
	return d, nil
}

// Get returns a document after RBAC read check. Existence never leaks: a
// missing doc and a forbidden doc both surface as 404 (PRD F1.5).
func (s *DocumentService) Get(ctx context.Context, auth AuthContext, id domain.UUID) (*domain.Document, error) {
	d, err := s.docs.Get(ctx, id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.UserID, auth.Groups, domain.TargetDocument, id, domain.ActionRead)
		if err != nil {
			return nil, err
		}
		if !dec.Allowed {
			return nil, pkgerr.NotFound("document not found")
		}
	}
	return d, nil
}

// Update updates a document: version conflict check, RBAC write, version
// snapshot with diff summary, event publish.
func (s *DocumentService) Update(ctx context.Context, auth AuthContext, id domain.UUID, prevVersion int,
	title string, content []domain.Block, status domain.DocumentStatus) (*domain.Document, error) {
	d, err := s.docs.Get(ctx, id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	if !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.UserID, auth.Groups, domain.TargetDocument, id, domain.ActionWrite)
		if err != nil {
			return nil, err
		}
		if !dec.Allowed {
			return nil, pkgerr.Forbidden("no write permission on document")
		}
	}
	if d.VersionNo != prevVersion {
		return nil, pkgerr.Conflict("version conflict")
	}
	oldContent := d.Content
	d.Title = title
	if content != nil {
		d.Content = content
	}
	d.ContentText = domain.BlockArray(d.Content).PlainText()
	if status != "" {
		d.Status = status
	}
	// Content changed → reset the index badge so the UI reflects pending re-index
	// and callers can poll for the new indexed state (AC-9/AC-10).
	if content != nil {
		d.IndexStatus = domain.IndexPending
	}
	d.UpdatedBy = &auth.UserID
	diffEntries := version.Diff(oldContent, d.Content)
	if s.sink != nil {
		// Transactional double-write (§6.3): UPDATE documents (prevVersion CAS)
		// + version snapshot + Knowledge Outbox event in one tx.
		ev := s.knowledgeEventForDoc(EventUpdate, d, prevVersion, auth)
		return s.sink.WriteDoc(ctx, d, &domain.DocumentVersion{
			DocumentID: d.ID, VersionNo: d.VersionNo, Content: d.Content, ContentText: d.ContentText,
			AuthorID: auth.UserID, DiffSummary: version.Summary(diffEntries),
		}, prevVersion, false, ev)
	}
	if err := s.docs.Update(ctx, d, prevVersion); err != nil {
		return nil, err
	}
	_ = s.versions.Create(ctx, &domain.DocumentVersion{
		DocumentID: d.ID, VersionNo: d.VersionNo, Content: d.Content, ContentText: d.ContentText,
		AuthorID: auth.UserID, DiffSummary: version.Summary(diffEntries),
	})
	_ = s.events.PublishDocumentEvent(ctx, DocumentEvent{
		Type: EventUpdate, DocumentID: d.ID, WorkspaceID: d.WorkspaceID, VersionNo: d.VersionNo,
		PrevVersionNo: prevVersion,
	})
	return d, nil
}

// Delete soft-deletes a document and publishes a delete event.
func (s *DocumentService) Delete(ctx context.Context, auth AuthContext, id domain.UUID) error {
	if !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.UserID, auth.Groups, domain.TargetDocument, id, domain.ActionAdmin)
		if err != nil {
			return err
		}
		if !dec.Allowed {
			return pkgerr.Forbidden("no admin permission to delete document")
		}
	}
	d, err := s.docs.Get(ctx, id)
	if err != nil {
		return mapNotFound(err)
	}
	if s.sink != nil {
		// Transactional double-write (§6.3): soft-delete + Knowledge Outbox
		// event in one tx.
		ev := s.knowledgeEventForDoc(EventDelete, &domain.Document{
			ID: id, WorkspaceID: d.WorkspaceID,
		}, 0, auth)
		return s.sink.DeleteDoc(ctx, id, auth.UserID, ev)
	}
	if err := s.docs.SoftDelete(ctx, id, auth.UserID); err != nil {
		return mapNotFound(err)
	}
	_ = s.events.PublishDocumentEvent(ctx, DocumentEvent{
		Type: EventDelete, DocumentID: id, WorkspaceID: d.WorkspaceID,
	})
	return nil
}

// Rollback rolls a document back to a prior version, producing a NEW version
// (PRD AC-6: 回滚产生新版本). History is never overwritten.
func (s *DocumentService) Rollback(ctx context.Context, auth AuthContext, id domain.UUID, targetVersion int) (*domain.Document, error) {
	if !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.UserID, auth.Groups, domain.TargetDocument, id, domain.ActionWrite)
		if err != nil {
			return nil, err
		}
		if !dec.Allowed {
			return nil, pkgerr.Forbidden("no write permission to rollback")
		}
	}
	d, err := s.docs.Get(ctx, id)
	if err != nil {
		return nil, mapNotFound(err)
	}
	v, err := s.versions.Get(ctx, id, targetVersion)
	if err != nil {
		return nil, mapNotFound(err)
	}
	oldContent := d.Content
	d.Content = v.Content
	d.ContentText = v.ContentText
	d.UpdatedBy = &auth.UserID
	if err := s.docs.Update(ctx, d, d.VersionNo); err != nil {
		return nil, err
	}
	diffEntries := version.Diff(oldContent, d.Content)
	_ = s.versions.Create(ctx, &domain.DocumentVersion{
		DocumentID: d.ID, VersionNo: d.VersionNo, Content: d.Content, ContentText: d.ContentText,
		AuthorID:    auth.UserID,
		DiffSummary: "rollback to v" + itoa2(targetVersion) + "; " + version.Summary(diffEntries),
	})
	_ = s.events.PublishDocumentEvent(ctx, DocumentEvent{
		Type: EventUpdate, DocumentID: d.ID, WorkspaceID: d.WorkspaceID, VersionNo: d.VersionNo,
	})
	return d, nil
}

// ListVersions returns the version history of a document after an RBAC read
// check (existence never leaks). Used by GET /documents/:id/versions.
func (s *DocumentService) ListVersions(ctx context.Context, auth AuthContext, id domain.UUID, p pagination.Params) ([]domain.DocumentVersion, int, error) {
	if _, err := s.Get(ctx, auth, id); err != nil {
		return nil, 0, err
	}
	return s.versions.List(ctx, id, p)
}

// GetVersion returns a specific version snapshot after an RBAC read check.
// Used by GET /documents/:id?version=N so callers can read historical content.
func (s *DocumentService) GetVersion(ctx context.Context, auth AuthContext, id domain.UUID, versionNo int) (*domain.DocumentVersion, error) {
	if _, err := s.Get(ctx, auth, id); err != nil {
		return nil, err
	}
	v, err := s.versions.Get(ctx, id, versionNo)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return v, nil
}

// DiffVersions computes the block-level diff between two versions.
func (s *DocumentService) DiffVersions(ctx context.Context, auth AuthContext, id domain.UUID, fromNo, toNo int) ([]version.DiffEntry, error) {
	if !auth.IsAdmin {
		dec, err := s.rbac.Check(ctx, auth.UserID, auth.Groups, domain.TargetDocument, id, domain.ActionRead)
		if err != nil {
			return nil, err
		}
		if !dec.Allowed {
			return nil, pkgerr.NotFound("document not found")
		}
	}
	from, err := s.versions.Get(ctx, id, fromNo)
	if err != nil {
		return nil, mapNotFound(err)
	}
	to, err := s.versions.Get(ctx, id, toNo)
	if err != nil {
		return nil, mapNotFound(err)
	}
	return version.Diff(from.Content, to.Content), nil
}

// ListVisible returns documents the caller may read (RBAC filtered).
func (s *DocumentService) ListVisible(ctx context.Context, auth AuthContext, q DocumentQuery) ([]domain.Document, int, error) {
	vis, err := s.rbac.VisibleDocuments(ctx, auth.UserID, auth.Groups, q.WorkspaceID)
	if err != nil {
		return nil, 0, err
	}
	if _, all := vis[domain.UUID{}]; all || auth.IsAdmin {
		q.VisibleAll = true
	} else {
		for id := range vis {
			q.VisibleDocs = append(q.VisibleDocs, id)
		}
	}
	return s.docs.List(ctx, q)
}

// knowledgeEventForDoc builds the Knowledge Outbox envelope for a document
// write (design-docs/13 §6.3, PR2 item ⑤). The envelope carries only IDs +
// action — never content (§5.1) — so a leaked outbox row cannot expose document
// text. destinations is fixed to [knowledge_events]; the legacy doc_events
// publish stays on its own path (the sink does not touch doc_events).
//
// The aggregate is the knowledge_asset the document represents. Phase 0 has no
// knowledge_assets row yet (no backfill, §16.1), so AggregateID = the document
// id — the Phase 1 reconciler maps document → asset. Event types follow the
// domain.KnowledgeEventTypes vocabulary (asset.* / permission.*).
func (s *DocumentService) knowledgeEventForDoc(t DocumentEventType, d *domain.Document, prevVersion int, auth AuthContext) domain.KnowledgeEvent {
	ws := d.WorkspaceID
	evType := domain.KEAssetCreated
	switch t {
	case EventUpdate:
		evType = domain.KEAssetVersionRequested
	case EventDelete:
		evType = domain.KEAssetDeprecated
	case EventPermissionChange:
		evType = domain.KEPermissionChanged
	}
	return domain.KnowledgeEvent{
		EventID:       uuid.NewString(),
		EventType:     evType,
		EventVersion:  1,
		AggregateType: domain.AggKnowledgeAsset,
		AggregateID:   d.ID,
		WorkspaceID:   &ws,
		Actor:         domain.EventActor{Type: domain.SubjectUser, ID: auth.UserID},
		Payload: map[string]any{
			"document_id":     d.ID.String(),
			"workspace_id":    ws.String(),
			"version_no":      d.VersionNo,
			"prev_version_no": prevVersion,
		},
	}
}

func mapNotFound(err error) error {
	if stderrors.Is(err, ErrNotFound) {
		return pkgerr.NotFound("not found")
	}
	return err
}

func itoa2(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
