package postgres

// Package postgres (user.go) implements UserRepo and RoleRepo.
//
// GET /api/v1/users is RBAC-scoped to prevent unauthorized user enumeration
// (PRD F1.4 / 07-security): a non-admin viewer only sees users who share at
// least one workspace the viewer can read (owner or read-allow grant, directly
// or via group membership), plus the viewer themselves. Admins see all active
// users. The workspace-readability predicate mirrors WorkspaceRepo.List so the
// two endpoints stay consistent.
//
// GET /api/v1/roles returns the role dictionary referenced by Permission.role_id;
// roles are static enough to be cached by consumers.

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/mora/service"
)

// --- User ---

type UserRepo struct{ db *DB }

func NewUserRepo(db *DB) *UserRepo { return &UserRepo{db: db} }

const userCols = `u.id, u.email, u.name, u.avatar_url, u.status, u.created_at, u.updated_at`

func scanUser(row pgx.Row) (*domain.User, error) {
	u := &domain.User{}
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.Status, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// canReadWorkspace returns a SQL predicate (and bound args) that is true when
// the user identified by the next placeholder can read the workspace aliased
// by `ws` — owner OR an allow permission whose role carries 'read', directly
// or via group membership. Mirrors WorkspaceRepo.List's visibility logic.
func canReadWorkspace(ws, placeholder string) string {
	return `(` + ws + `.owner_id = ` + placeholder + `
		OR EXISTS (SELECT 1 FROM permissions p
			JOIN roles ro ON ro.id = p.role_id
			WHERE p.target_type = 'workspace' AND p.target_id = ` + ws + `.id
			  AND p.effect = 'allow'
			  AND ro.permissions ? 'read'
			  AND ((p.subject_type = 'user' AND p.subject_id = ` + placeholder + `)
			       OR (p.subject_type = 'group' AND p.subject_id IN
			           (SELECT group_id FROM group_members WHERE user_id = ` + placeholder + `)))))`
}

func (r *UserRepo) List(ctx context.Context, q service.UserQuery) ([]domain.User, int, error) {
	var args []any
	var where string
	if q.IsAdmin {
		// Admin: all active users.
		args = append(args, "active")
		where = `u.status = $1`
	} else {
		// Non-admin: users who share at least one workspace readable by the
		// viewer, or the viewer themselves. Existence of unreadable users is
		// never revealed (no enumeration).
		args = append(args, q.ViewerID, "active")
		where = `u.status = $2 AND (
			u.id = $1
			OR EXISTS (
				SELECT 1 FROM workspaces w
				WHERE ` + canReadWorkspace("w", "$1") + `
				  AND ` + canReadWorkspace("w", "u.id") + `
			))`
	}
	argIdx := len(args)
	if q.Search != "" {
		argIdx++
		args = append(args, "%"+q.Search+"%")
		where += ` AND (u.name ILIKE $` + itoa(argIdx) + ` OR u.email ILIKE $` + itoa(argIdx) + `)`
	}

	countSQL := `SELECT count(DISTINCT u.id) FROM users u WHERE ` + where
	var total int
	if err := r.db.Pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	listSQL := `SELECT DISTINCT ` + userCols + ` FROM users u WHERE ` + where +
		` ORDER BY u.name, u.id LIMIT $` + itoa(argIdx+1) + ` OFFSET $` + itoa(argIdx+2)
	args = append(args, q.PageSize, q.Offset())
	rows, err := r.db.Pool.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *u)
	}
	return out, total, rows.Err()
}

// --- Role ---

type RoleRepo struct{ db *DB }

func NewRoleRepo(db *DB) *RoleRepo { return &RoleRepo{db: db} }

func (r *RoleRepo) List(ctx context.Context) ([]domain.Role, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, name, scope, workspace_id, permissions, is_system, created_at
		FROM roles ORDER BY is_system DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Role
	for rows.Next() {
		var ro domain.Role
		var perms []byte
		var wsID *domain.UUID
		if err := rows.Scan(&ro.ID, &ro.Name, &ro.Scope, &wsID, &perms, &ro.IsSystem, &ro.CreatedAt); err != nil {
			return nil, err
		}
		ro.WorkspaceID = wsID
		if len(perms) > 0 {
			_ = json.Unmarshal(perms, &ro.Permissions)
		}
		out = append(out, ro)
	}
	return out, rows.Err()
}

var _ service.UserRepo = (*UserRepo)(nil)
var _ service.RoleRepo = (*RoleRepo)(nil)
