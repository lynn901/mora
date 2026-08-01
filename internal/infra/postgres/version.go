package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/pkg/pagination"
)

type VersionRepo struct{ db *DB }

func NewVersionRepo(db *DB) *VersionRepo { return &VersionRepo{db: db} }

func (r *VersionRepo) List(ctx context.Context, docID domain.UUID, p pagination.Params) ([]domain.DocumentVersion, int, error) {
	var total int
	if err := r.db.Pool.QueryRow(ctx, `SELECT count(*) FROM document_versions WHERE document_id=$1`, docID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, document_id, version_no, content, content_text, diff_summary, author_id, created_at
		FROM document_versions WHERE document_id=$1
		ORDER BY version_no DESC LIMIT $2 OFFSET $3`,
		docID, p.PageSize, p.Offset())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []domain.DocumentVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *v)
	}
	return out, total, rows.Err()
}

func (r *VersionRepo) Get(ctx context.Context, docID domain.UUID, versionNo int) (*domain.DocumentVersion, error) {
	v, err := scanVersion(r.db.Pool.QueryRow(ctx, `
		SELECT id, document_id, version_no, content, content_text, diff_summary, author_id, created_at
		FROM document_versions WHERE document_id=$1 AND version_no=$2`, docID, versionNo))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	return v, err
}

func (r *VersionRepo) Create(ctx context.Context, v *domain.DocumentVersion) error {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	v.CreatedAt = time.Now().UTC()
	content, _ := json.Marshal(v.Content)
	return r.db.Pool.QueryRow(ctx, `
		INSERT INTO document_versions (id, document_id, version_no, content, content_text, diff_summary, author_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		v.ID, v.DocumentID, v.VersionNo, content, v.ContentText, v.DiffSummary, v.AuthorID, v.CreatedAt).Scan(&v.ID)
}

func (r *VersionRepo) MaxVersionNo(ctx context.Context, docID domain.UUID) (int, error) {
	var max int
	err := r.db.Pool.QueryRow(ctx, `SELECT COALESCE(max(version_no),0) FROM document_versions WHERE document_id=$1`, docID).Scan(&max)
	return max, err
}

func scanVersion(row pgx.Row) (*domain.DocumentVersion, error) {
	v := &domain.DocumentVersion{}
	var content []byte
	err := row.Scan(&v.ID, &v.DocumentID, &v.VersionNo, &content, &v.ContentText, &v.DiffSummary, &v.AuthorID, &v.CreatedAt)
	if err != nil {
		return nil, err
	}
	if len(content) > 0 {
		_ = json.Unmarshal(content, &v.Content)
	}
	return v, nil
}

// --- Tag ---

type TagRepo struct{ db *DB }

func NewTagRepo(db *DB) *TagRepo { return &TagRepo{db: db} }

func (r *TagRepo) ListByWorkspace(ctx context.Context, wsID domain.UUID) ([]domain.Tag, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT id, workspace_id, name, color, parent_id, created_at FROM tags WHERE workspace_id=$1 ORDER BY name`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Tag
	for rows.Next() {
		var t domain.Tag
		var parentID *domain.UUID
		if err := rows.Scan(&t.ID, &t.WorkspaceID, &t.Name, &t.Color, &parentID, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.ParentID = parentID
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *TagRepo) Create(ctx context.Context, t *domain.Tag) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	t.CreatedAt = time.Now().UTC()
	return r.db.Pool.QueryRow(ctx,
		`INSERT INTO tags (id, workspace_id, name, color, parent_id, created_at) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		t.ID, t.WorkspaceID, t.Name, t.Color, t.ParentID, t.CreatedAt).Scan(&t.ID)
}

func (r *TagRepo) SetDocumentTags(ctx context.Context, docID domain.UUID, tagIDs []domain.UUID) error {
	tx, err := r.db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM document_tags WHERE document_id=$1`, docID); err != nil {
		return err
	}
	for _, tid := range tagIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO document_tags (document_id, tag_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, docID, tid); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// --- Comment ---

type CommentRepo struct{ db *DB }

func NewCommentRepo(db *DB) *CommentRepo { return &CommentRepo{db: db} }

func (r *CommentRepo) List(ctx context.Context, docID domain.UUID, blockID *domain.UUID) ([]domain.Comment, error) {
	q := `SELECT id, document_id, block_id, parent_id, author_id, content, mentions, resolved, resolved_by, resolved_at, created_at, updated_at
		FROM comments WHERE document_id=$1`
	args := []any{docID}
	if blockID != nil {
		q += ` AND block_id = $2`
		args = append(args, *blockID)
	}
	q += ` ORDER BY created_at`
	rows, err := r.db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Comment
	for rows.Next() {
		var c domain.Comment
		var blockID, parentID, resolvedBy *domain.UUID
		var mentions []domain.UUID
		var resolvedAt *time.Time
		if err := rows.Scan(&c.ID, &c.DocumentID, &blockID, &parentID, &c.AuthorID, &c.Content, &mentions, &c.Resolved, &resolvedBy, &resolvedAt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.BlockID, c.ParentID, c.ResolvedBy, c.ResolvedAt, c.Mentions = blockID, parentID, resolvedBy, resolvedAt, mentions
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *CommentRepo) Create(ctx context.Context, c *domain.Comment) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	return r.db.Pool.QueryRow(ctx, `
		INSERT INTO comments (id, document_id, block_id, parent_id, author_id, content, mentions, resolved, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		c.ID, c.DocumentID, c.BlockID, c.ParentID, c.AuthorID, c.Content, c.Mentions, c.Resolved, c.CreatedAt, c.UpdatedAt).Scan(&c.ID)
}

func (r *CommentRepo) Resolve(ctx context.Context, id, resolvedBy domain.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `UPDATE comments SET resolved=true, resolved_by=$2, resolved_at=now(), updated_at=now() WHERE id=$1`, id, resolvedBy)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}
