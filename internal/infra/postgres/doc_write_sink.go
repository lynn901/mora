package postgres

// doc_write_sink.go implements service.DocWriteSink — the transactional
// double-write hook for document writes (design-docs/13 §6.3, PR2 item ⑤).
//
// When wired into *service.DocumentService, Create/Update/Delete route here:
// the documents + document_versions writes AND the Knowledge Outbox event are
// committed in ONE database transaction, so the event is never lost relative
// to the state change (the core outbox guarantee, §6.3).
//
// The sink owns the transaction (the service layer stays pgx-free). It reuses
// the exact INSERT/UPDATE SQL of DocumentRepo/VersionRepo so behavior matches
// the non-tx path byte-for-byte — the only addition is the outbox_events row.
// The outbox row is written via outbox.Store.Record (same pgx.Tx), not by
// re-implementing the INSERT, so the envelope schema stays in one place.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// DocWriteSink is the postgres implementation of service.DocWriteSink. It
// composes the existing document/version SQL with an outbox.Store so a doc
// write and its Knowledge event commit atomically.
type DocWriteSink struct {
	pool   *pgxpool.Pool
	outbox *outbox.Store
}

// NewDocWriteSink builds a sink over a pool and the (stateless) outbox.Store.
func NewDocWriteSink(pool *pgxpool.Pool, store *outbox.Store) *DocWriteSink {
	return &DocWriteSink{pool: pool, outbox: store}
}

// Compile-time check.
var _ service.DocWriteSink = (*DocWriteSink)(nil)

// WriteDoc creates or updates a document + version snapshot inside one tx and
// records the Knowledge Outbox event in the SAME tx (§6.3). create=true →
// INSERT documents; create=false → UPDATE with prevVersion CAS.
//
// On any error the tx is rolled back — neither the document write nor the
// outbox row lands, preserving the atomic guarantee. The caller's *domain.
// Document is mutated to reflect persisted state (VersionNo/UpdatedAt) so
// downstream behavior matches the legacy non-tx path.
func (s *DocWriteSink) WriteDoc(ctx context.Context, d *domain.Document, v *domain.DocumentVersion, prevVersion int, create bool, ev domain.KnowledgeEvent) (*domain.Document, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck — documented pgx pattern

	if create {
		if err := createDocTx(ctx, tx, d); err != nil {
			return nil, err
		}
	} else {
		if err := updateDocTx(ctx, tx, d, prevVersion); err != nil {
			return nil, err
		}
	}
	if err := createVersionTx(ctx, tx, v); err != nil {
		return nil, err
	}
	// Knowledge Outbox event — same tx. destinations = knowledge_events only;
	// the legacy doc_events publish stays on the service's EventPublisher path
	// (the sink does NOT publish to doc_events, avoiding double-delivery to RAG).
	if err := s.outbox.Record(ctx, tx, ev, []string{outbox.KnowledgeEventsStream}); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// DeleteDoc soft-deletes a document + records the Knowledge Outbox event in
// one tx (§6.3). A no-rows-affected (already deleted / missing) returns
// service.ErrNotFound so the caller surfaces 404 (existence not leaked).
func (s *DocWriteSink) DeleteDoc(ctx context.Context, id, userID domain.UUID, ev domain.KnowledgeEvent) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	tag, err := tx.Exec(ctx,
		`UPDATE documents SET status='deleted', updated_by=$2, updated_at=now() WHERE id=$1 AND status != 'deleted'`,
		id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return service.ErrNotFound
	}
	if err := s.outbox.Record(ctx, tx, ev, []string{outbox.KnowledgeEventsStream}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- tx-scoped SQL (mirrors DocumentRepo/VersionRepo exactly) ---

// createDocTx is the tx-scoped twin of DocumentRepo.Create. Same SQL, same
// field-setting, so persisted state is identical to the non-tx path.
func createDocTx(ctx context.Context, tx pgx.Tx, d *domain.Document) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	now := time.Now().UTC()
	d.CreatedAt, d.UpdatedAt = now, now
	if d.VersionNo == 0 {
		d.VersionNo = 1
	}
	content, _ := json.Marshal(d.Content)
	return tx.QueryRow(ctx, `
		INSERT INTO documents (id, workspace_id, directory_id, title, content, content_text, format, status, index_status, version_no, created_by, updated_by, created_at, updated_at, storage_key, source_format, parse_status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id`,
		d.ID, d.WorkspaceID, d.DirectoryID, d.Title, content, d.ContentText, d.Format,
		d.Status, d.IndexStatus, d.VersionNo, d.CreatedBy, d.UpdatedBy, d.CreatedAt, d.UpdatedAt,
		nullIfEmpty(d.StorageKey), d.SourceFormat, nullIfEmpty(string(d.ParseStatus))).Scan(&d.ID)
}

// updateDocTx is the tx-scoped twin of DocumentRepo.Update. Same CAS
// (version_no=prevVersion AND status != 'deleted').
func updateDocTx(ctx context.Context, tx pgx.Tx, d *domain.Document, prevVersion int) error {
	content, _ := json.Marshal(d.Content)
	tag, err := tx.Exec(ctx, `
		UPDATE documents SET title=$3, content=$4, content_text=$5, format=$6, status=$7,
			index_status=$8, version_no=version_no+1, updated_by=$9, updated_at=now()
		WHERE id=$1 AND version_no=$2 AND status != 'deleted'`,
		d.ID, prevVersion, d.Title, content, d.ContentText, d.Format, d.Status, d.IndexStatus, d.UpdatedBy)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return service.ErrNotFound // or version conflict — caller maps to 404/409
	}
	d.VersionNo = prevVersion + 1
	d.UpdatedAt = time.Now().UTC()
	return nil
}

// createVersionTx is the tx-scoped twin of VersionRepo.Create.
func createVersionTx(ctx context.Context, tx pgx.Tx, v *domain.DocumentVersion) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	if v.CreatedAt.IsZero() {
		v.CreatedAt = time.Now().UTC()
	}
	content, _ := json.Marshal(v.Content)
	return tx.QueryRow(ctx, `
		INSERT INTO document_versions (id, document_id, version_no, content, content_text, diff_summary, author_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		v.ID, v.DocumentID, v.VersionNo, content, v.ContentText, v.DiffSummary, v.AuthorID, v.CreatedAt).Scan(&v.ID)
}
