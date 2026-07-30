package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/rag"
)

// DocumentStore loads document snapshots for indexing. This is a basic
// standalone implementation reading documents/document_versions; YS-6 may
// substitute a richer one (block rendering, attachment text injection).
type DocumentStore struct {
	Pool *pgxpool.Pool
}

func NewDocumentStore(pool *pgxpool.Pool) *DocumentStore { return &DocumentStore{Pool: pool} }

func (s *DocumentStore) GetSnapshot(ctx context.Context, docID string, version int) (rag.DocumentSnapshot, error) {
	var snap rag.DocumentSnapshot
	var content []byte
	var contentText, directoryID *string
	if version > 0 {
		err := s.Pool.QueryRow(ctx, `
            SELECT d.id, d.workspace_id, d.directory_id, d.title, v.content, v.content_text, d.format, v.version_no, d.status
            FROM documents d JOIN document_versions v ON v.document_id = d.id
            WHERE d.id=$1 AND v.version_no=$2`, docID, version).
			Scan(&snap.DocumentID, &snap.WorkspaceID, &directoryID, &snap.Title, &content, &contentText, &snap.Format, &snap.VersionNo, &snap.Status)
		if err != nil {
			return snap, fmt.Errorf("get snapshot v%d: %w", version, err)
		}
	} else {
		err := s.Pool.QueryRow(ctx, `
            SELECT id, workspace_id, directory_id, title, content, content_text, format, 1, status
            FROM documents WHERE id=$1`, docID).
			Scan(&snap.DocumentID, &snap.WorkspaceID, &directoryID, &snap.Title, &content, &contentText, &snap.Format, &snap.VersionNo, &snap.Status)
		if err != nil {
			return snap, fmt.Errorf("get snapshot: %w", err)
		}
	}
	snap.Content = content
	if contentText != nil {
		snap.ContentText = *contentText
	}
	if directoryID != nil {
		snap.DirectoryID = *directoryID
	}
	// tags
	rows, _ := s.Pool.Query(ctx, `SELECT t.name FROM document_tags dt JOIN tags t ON t.id=dt.tag_id WHERE dt.document_id=$1`, docID)
	defer rows.Close()
	for rows.Next() {
		var t string
		_ = rows.Scan(&t)
		snap.Tags = append(snap.Tags, t)
	}
	return snap, nil
}

func (s *DocumentStore) PublishedDocumentIDs(ctx context.Context, cursor string, limit int) ([]string, string, error) {
	q := `SELECT id FROM documents WHERE status='published'`
	args := []any{}
	if cursor != "" {
		args = append(args, cursor)
		q += ` AND id > $1`
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY id LIMIT $%d`, len(args))
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []string
	var last string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		out = append(out, id)
		last = id
	}
	next := ""
	if len(out) == limit {
		next = last
	}
	return out, next, rows.Err()
}

var _ rag.DocumentStore = (*DocumentStore)(nil)
var _ = domain.DocPublished
