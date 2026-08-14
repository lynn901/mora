package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
)

type DocumentRepo struct{ db *DB }

func NewDocumentRepo(db *DB) *DocumentRepo { return &DocumentRepo{db: db} }

func (r *DocumentRepo) List(ctx context.Context, q service.DocumentQuery) ([]domain.Document, int, error) {
	var args []any
	argIdx := 1
	sb := `SELECT ` + docCols + ` FROM documents WHERE status != 'deleted' AND workspace_id = $1`
	args = append(args, q.WorkspaceID)
	if q.DirectoryID != nil {
		sb += ` AND directory_id = $2`
		args = append(args, *q.DirectoryID)
		argIdx = 3
	}
	if q.Status != "" {
		argIdx++
		sb += ` AND status = $` + itoa(argIdx)
		args = append(args, q.Status)
	}
	if q.CreatedBy != nil {
		argIdx++
		sb += ` AND created_by = $` + itoa(argIdx)
		args = append(args, *q.CreatedBy)
	}
	if q.UpdatedAfter != nil {
		argIdx++
		sb += ` AND updated_at >= $` + itoa(argIdx)
		args = append(args, *q.UpdatedAfter)
	}
	// RBAC hard filter
	if !q.VisibleAll {
		if len(q.VisibleDocs) == 0 {
			sb += ` AND FALSE`
		} else {
			sb += ` AND id IN (`
			for i, id := range q.VisibleDocs {
				if i > 0 {
					sb += `,`
				}
				argIdx++
				sb += `$` + itoa(argIdx)
				args = append(args, id)
			}
			sb += `)`
		}
	}
	// count
	countSQL := `SELECT count(*) FROM (` + sb + `) c`
	var total int
	if err := r.db.Pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	sb += ` ORDER BY updated_at DESC LIMIT $` + itoa(argIdx+1) + ` OFFSET $` + itoa(argIdx+2)
	args = append(args, q.PageSize, q.Offset())
	rows, err := r.db.Pool.Query(ctx, sb, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []domain.Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *d)
	}
	return out, total, rows.Err()
}

func (r *DocumentRepo) Get(ctx context.Context, id domain.UUID) (*domain.Document, error) {
	d, err := scanDocument(r.db.Pool.QueryRow(ctx,
		`SELECT `+docCols+` FROM documents WHERE id=$1 AND status != 'deleted'`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return d, err
}

func (r *DocumentRepo) Create(ctx context.Context, d *domain.Document) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	now := time.Now().UTC()
	d.CreatedAt, d.UpdatedAt = now, now
	if d.VersionNo == 0 {
		d.VersionNo = 1
	}
	content, _ := json.Marshal(d.Content)
	// parse_status is NOT NULL with DEFAULT 'parsed'; honor that default in Go
	// so a Document{} seeded without ParseStatus (Block-authored, already
	// parsed — see chunk.go) doesn't insert NULL and violate the constraint.
	parseStatus := d.ParseStatus
	if parseStatus == "" {
		parseStatus = domain.ParseParsed
	}
	return r.db.Pool.QueryRow(ctx, `
		INSERT INTO documents (id, workspace_id, directory_id, title, content, content_text, format, status, index_status, version_no, created_by, updated_by, created_at, updated_at, storage_key, source_format, parse_status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) RETURNING id`,
		d.ID, d.WorkspaceID, d.DirectoryID, d.Title, content, d.ContentText, d.Format,
		d.Status, d.IndexStatus, d.VersionNo, d.CreatedBy, d.UpdatedBy, d.CreatedAt, d.UpdatedAt,
		nullIfEmpty(d.StorageKey), d.SourceFormat, nullIfEmpty(string(parseStatus))).Scan(&d.ID)
}

func (r *DocumentRepo) Update(ctx context.Context, d *domain.Document, prevVersion int) error {
	content, _ := json.Marshal(d.Content)
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE documents SET title=$3, content=$4, content_text=$5, format=$6, status=$7,
			index_status=$8, version_no=version_no+1, updated_by=$9, updated_at=now()
		WHERE id=$1 AND version_no=$2 AND status != 'deleted'`,
		d.ID, prevVersion, d.Title, content, d.ContentText, d.Format, d.Status, d.IndexStatus, d.UpdatedBy)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound // or version conflict
	}
	d.VersionNo = prevVersion + 1
	d.UpdatedAt = time.Now().UTC()
	return nil
}

func (r *DocumentRepo) SoftDelete(ctx context.Context, id, userID domain.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE documents SET status='deleted', updated_by=$2, updated_at=now() WHERE id=$1 AND status != 'deleted'`,
		id, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// DocumentIDsForTarget enumerates non-deleted document IDs affected by a
// permission target. For a directory it uses the ltree path to include the
// entire subtree (the directories table path column is LTREE with a GIST index).
func (r *DocumentRepo) DocumentIDsForTarget(ctx context.Context, targetType domain.TargetType, targetID domain.UUID) ([]domain.UUID, error) {
	var query string
	switch targetType {
	case domain.TargetDocument:
		query = `SELECT id FROM documents WHERE id=$1 AND status != 'deleted'`
	case domain.TargetDirectory:
		query = `SELECT d.id FROM documents d
			JOIN directories dir ON d.directory_id = dir.id
			WHERE d.status != 'deleted'
			  AND dir.path <@ (SELECT path FROM directories WHERE id=$1)`
	case domain.TargetWorkspace:
		query = `SELECT id FROM documents WHERE workspace_id=$1 AND status != 'deleted'`
	default:
		return nil, nil
	}
	rows, err := r.db.Pool.Query(ctx, query, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.UUID
	for rows.Next() {
		var id domain.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func itoa(n int) string {
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

// nullIfEmpty returns nil for an empty string so a NULL is written instead of
// ” (storage_key/parse_status default semantics). pgx accepts *string or nil.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
