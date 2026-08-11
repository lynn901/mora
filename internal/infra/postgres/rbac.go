package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
)

// --- Permission / RBAC ---

type PermissionRepo struct{ db *DB }

func NewPermissionRepo(db *DB) *PermissionRepo { return &PermissionRepo{db: db} }

func (r *PermissionRepo) List(ctx context.Context, targetType domain.TargetType, targetID, subjectID *domain.UUID) ([]domain.Permission, error) {
	q := `SELECT id, subject_type, subject_id, role_id, target_type, target_id, effect, inherit_scope, created_at, created_by
		FROM permissions WHERE 1=1`
	var args []any
	if targetType != "" {
		args = append(args, string(targetType))
		q += ` AND target_type = $1`
	}
	if targetID != nil {
		args = append(args, *targetID)
		q += ` AND target_id = $` + itoa(len(args))
	}
	if subjectID != nil {
		args = append(args, *subjectID)
		q += ` AND subject_id = $` + itoa(len(args))
	}
	q += ` ORDER BY created_at`
	rows, err := r.db.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Permission
	for rows.Next() {
		var p domain.Permission
		var createdBy *domain.UUID
		if err := rows.Scan(&p.ID, &p.SubjectType, &p.SubjectID, &p.RoleID, &p.TargetType, &p.TargetID, &p.Effect, &p.InheritScope, &p.CreatedAt, &createdBy); err != nil {
			return nil, err
		}
		p.CreatedBy = createdBy
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *PermissionRepo) Grant(ctx context.Context, p *domain.Permission) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	p.CreatedAt = time.Now().UTC()
	return r.db.Pool.QueryRow(ctx, `
		INSERT INTO permissions (id, subject_type, subject_id, role_id, target_type, target_id, effect, inherit_scope, created_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		p.ID, p.SubjectType, p.SubjectID, p.RoleID, p.TargetType, p.TargetID, p.Effect, p.InheritScope, p.CreatedAt, p.CreatedBy).Scan(&p.ID)
}

func (r *PermissionRepo) Revoke(ctx context.Context, id domain.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM permissions WHERE id=$1`, id)
	return err
}

// Get returns a single permission by id. Used by revoke to learn the target
// (type + id) before deletion so affected documents can be enumerated.
func (r *PermissionRepo) Get(ctx context.Context, id domain.UUID) (*domain.Permission, error) {
	var p domain.Permission
	var createdBy *domain.UUID
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id, subject_type, subject_id, role_id, target_type, target_id, effect, inherit_scope, created_at, created_by
		 FROM permissions WHERE id=$1`, id).
		Scan(&p.ID, &p.SubjectType, &p.SubjectID, &p.RoleID, &p.TargetType, &p.TargetID, &p.Effect, &p.InheritScope, &p.CreatedAt, &createdBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	p.CreatedBy = createdBy
	return &p, nil
}

// GrantsFor resolves effective grants for a subject within a workspace.
// It joins roles to expand role.permissions JSONB into individual actions,
// and includes direct user grants, group grants (via group_members), and
// service_account grants. A service_account subject is resolved upstream by
// authz.Service.rbacSubject (agent-self → its ServiceAccountID), so it reaches
// GrantsFor as subjectID — the matching grant row carries
// subject_type='service_account' with the same id in subject_id.
// Grants are scoped to the given workspace: workspace-level grants on OTHER
// workspaces do not leak here (prevents cross-workspace RBAC bypass).
func (r *PermissionRepo) GrantsFor(ctx context.Context, subjectID domain.UUID, groupIDs []domain.UUID, workspaceID domain.UUID) ([]domain.Grant, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT p.subject_type, p.subject_id,
		       jsonb_array_elements_text(ro.permissions) AS action,
		       p.target_type, p.target_id, p.effect, p.inherit_scope
		FROM permissions p
		JOIN roles ro ON ro.id = p.role_id
		WHERE ((p.subject_type = 'user' AND p.subject_id = $1)
		    OR (p.subject_type = 'service_account' AND p.subject_id = $1)
		    OR (p.subject_type = 'group' AND p.subject_id = ANY($2::uuid[])))
		  AND (
		    (p.target_type = 'workspace' AND p.target_id = $3)
		    OR (p.target_type = 'directory' AND p.target_id IN (SELECT id FROM directories WHERE workspace_id = $3))
		    OR (p.target_type = 'document'  AND p.target_id IN (SELECT id FROM documents WHERE workspace_id = $3))
		  )
		ORDER BY p.created_at`, subjectID, groupIDsArray(groupIDs), workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Grant
	for rows.Next() {
		var g domain.Grant
		var action string
		if err := rows.Scan(&g.SubjectType, &g.SubjectID, &action, &g.TargetType, &g.TargetID, &g.Effect, &g.InheritScope); err != nil {
			return nil, err
		}
		g.Actions = append(g.Actions, domain.Action(action))
		out = append(out, g)
	}
	return out, rows.Err()
}

// DocumentLocation returns the workspace_id and directory_id for a document.
func (r *PermissionRepo) DocumentLocation(ctx context.Context, docID domain.UUID) (domain.UUID, domain.UUID, error) {
	var wsID, dirID domain.UUID
	err := r.db.Pool.QueryRow(ctx, `SELECT workspace_id, COALESCE(directory_id, '00000000-0000-0000-0000-000000000000') FROM documents WHERE id=$1`, docID).Scan(&wsID, &dirID)
	return wsID, dirID, err
}

func groupIDsArray(ids []domain.UUID) []uuid.UUID {
	if len(ids) == 0 {
		return []uuid.UUID{}
	}
	return ids
}
