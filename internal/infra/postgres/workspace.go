package postgres

// Package postgres implements the repository interfaces using pgx against
// PostgreSQL 16. SQL is parameterized (no string concatenation of user input)
// per the security design (07-security §10: SQL injection prevention).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
	"github.com/lynn901/mora/internal/pkg/pagination"
)

type DB struct {
	Pool *pgxpool.Pool
}

func NewDB(pool *pgxpool.Pool) *DB { return &DB{Pool: pool} }

// errNotFound is re-exported as service.ErrNotFound so the service layer can
// detect missing records via errors.Is.
var errNotFound = service.ErrNotFound

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// --- Workspace ---

type WorkspaceRepo struct{ db *DB }

func NewWorkspaceRepo(db *DB) *WorkspaceRepo { return &WorkspaceRepo{db: db} }

func (r *WorkspaceRepo) List(ctx context.Context, userID domain.UUID) ([]domain.Workspace, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT w.id, w.name, w.slug, w.description, w.owner_id, w.settings, w.created_at, w.updated_at
		FROM workspaces w
		WHERE w.owner_id = $1
		   OR EXISTS (SELECT 1 FROM permissions p
		   	JOIN roles ro ON ro.id = p.role_id
		   	WHERE p.target_type = 'workspace' AND p.target_id = w.id
		   	  AND p.effect = 'allow'
		   	  AND ((p.subject_type = 'user' AND p.subject_id = $1)
		   	       OR (p.subject_type = 'group' AND p.subject_id IN
		   	           (SELECT group_id FROM group_members WHERE user_id = $1)))
		   	  AND ro.permissions ? 'read')
		ORDER BY w.created_at`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Workspace
	for rows.Next() {
		var w domain.Workspace
		var settings []byte
		if err := rows.Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.OwnerID, &settings, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		if len(settings) > 0 {
			_ = json.Unmarshal(settings, &w.Settings)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *WorkspaceRepo) Get(ctx context.Context, id domain.UUID) (*domain.Workspace, error) {
	w := &domain.Workspace{}
	var settings []byte
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, name, slug, description, owner_id, settings, created_at, updated_at
		FROM workspaces WHERE id = $1`, id).
		Scan(&w.ID, &w.Name, &w.Slug, &w.Description, &w.OwnerID, &settings, &w.CreatedAt, &w.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(settings) > 0 {
		_ = json.Unmarshal(settings, &w.Settings)
	}
	return w, nil
}

func (r *WorkspaceRepo) Create(ctx context.Context, w *domain.Workspace) error {
	if w.ID == uuid.Nil {
		w.ID = uuid.New()
	}
	now := time.Now().UTC()
	w.CreatedAt, w.UpdatedAt = now, now
	settings, _ := json.Marshal(w.Settings)
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO workspaces (id, name, slug, description, owner_id, settings, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		w.ID, w.Name, w.Slug, w.Description, w.OwnerID, settings, w.CreatedAt, w.UpdatedAt).Scan(&w.ID)
	if isUniqueViolation(err) {
		return fmt.Errorf("workspace slug already exists")
	}
	return err
}

// --- Directory ---

type DirectoryRepo struct{ db *DB }

func NewDirectoryRepo(db *DB) *DirectoryRepo { return &DirectoryRepo{db: db} }

func (r *DirectoryRepo) ListByWorkspace(ctx context.Context, wsID domain.UUID) ([]domain.Directory, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, workspace_id, parent_id, name, path::text, sort_order, created_at, updated_at
		FROM directories WHERE workspace_id = $1 ORDER BY sort_order, name`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Directory
	for rows.Next() {
		var d domain.Directory
		var parentID *domain.UUID
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &parentID, &d.Name, &d.Path, &d.SortOrder, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.ParentID = parentID
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *DirectoryRepo) Get(ctx context.Context, id domain.UUID) (*domain.Directory, error) {
	d := &domain.Directory{}
	var parentID *domain.UUID
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, workspace_id, parent_id, name, path::text, sort_order, created_at, updated_at
		FROM directories WHERE id = $1`, id).
		Scan(&d.ID, &d.WorkspaceID, &parentID, &d.Name, &d.Path, &d.SortOrder, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound
	}
	d.ParentID = parentID
	return d, err
}

func (r *DirectoryRepo) Create(ctx context.Context, d *domain.Directory) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	now := time.Now().UTC()
	d.CreatedAt, d.UpdatedAt = now, now
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO directories (id, workspace_id, parent_id, name, path, sort_order, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		d.ID, d.WorkspaceID, d.ParentID, d.Name, d.Path, d.SortOrder, d.CreatedAt, d.UpdatedAt)
	return err
}

func (r *DirectoryRepo) Update(ctx context.Context, d *domain.Directory) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE directories SET name=$2, sort_order=$3, updated_at=now() WHERE id=$1`,
		d.ID, d.Name, d.SortOrder)
	return err
}

func (r *DirectoryRepo) Delete(ctx context.Context, id domain.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM directories WHERE id=$1`, id)
	return err
}

// Ancestors returns directory IDs root-first via the ltree path.
func (r *DirectoryRepo) Ancestors(ctx context.Context, dirID domain.UUID) ([]domain.UUID, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id FROM directories
		WHERE workspace_id = (SELECT workspace_id FROM directories WHERE id=$1)
		  AND path @> (SELECT path FROM directories WHERE id=$1)
		ORDER BY path::text`, dirID)
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

// --- helpers shared by repos ---

func scanDocument(row pgx.Row) (*domain.Document, error) {
	d := &domain.Document{}
	var content []byte
	var dirID *domain.UUID
	var updatedBy *domain.UUID
	err := row.Scan(&d.ID, &d.WorkspaceID, &dirID, &d.Title, &content, &d.ContentText, &d.Format,
		&d.Status, &d.IndexStatus, &d.VersionNo, &d.CreatedBy, &updatedBy, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	d.DirectoryID = dirID
	d.UpdatedBy = updatedBy
	if len(content) > 0 {
		_ = json.Unmarshal(content, &d.Content)
	}
	return d, nil
}

const docCols = `id, workspace_id, directory_id, title, content, content_text, format, status, index_status, version_no, created_by, updated_by, created_at, updated_at`

var _ pagination.Params

var _ service.WorkspaceRepo = (*WorkspaceRepo)(nil)
var _ service.DirectoryRepo = (*DirectoryRepo)(nil)
