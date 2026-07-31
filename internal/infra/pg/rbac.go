package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/rag"
)

// RBACResolver resolves read visibility for RAG. This standalone implementation
// resolves against the permissions table (allow on doc or ancestor subtree,
// minus deny). The canonical engine lives in YS-6 platform/rbac; RAG uses this
// when running standalone or until YS-6 injects its optimized resolver.
type RBACResolver struct {
	Pool *pgxpool.Pool
}

func NewRBACResolver(pool *pgxpool.Pool) *RBACResolver { return &RBACResolver{Pool: pool} }

const readerSQL = `
    SELECT DISTINCT subject FROM (
      SELECT p.subject_type || ':' || p.subject_id::text AS subject
      FROM permissions p
      WHERE p.effect = 'allow'
        AND (
          p.target_type = 'document'  AND p.target_id = $1
          OR p.target_type = 'directory' AND p.inherit_scope = 'subtree'
             AND p.target_id = (SELECT directory_id FROM documents WHERE id=$1)
          OR p.target_type = 'workspace' AND p.inherit_scope = 'subtree'
             AND p.target_id = (SELECT workspace_id FROM documents WHERE id=$1)
        )
        AND NOT EXISTS (
          SELECT 1 FROM permissions p2
          WHERE p2.effect = 'deny'
            AND p2.subject_id = p.subject_id AND p2.subject_type = p.subject_type
            AND (
              p2.target_type = 'document'  AND p2.target_id = $1
              OR p2.target_type = 'directory' AND p2.inherit_scope = 'subtree'
                 AND p2.target_id = (SELECT directory_id FROM documents WHERE id=$1)
              OR p2.target_type = 'workspace' AND p2.inherit_scope = 'subtree'
                 AND p2.target_id = (SELECT workspace_id FROM documents WHERE id=$1)
            )
        )
      UNION
      -- workspace owner is always a reader of their workspace's documents
      SELECT 'user:' || w.owner_id::text AS subject
      FROM workspaces w
      JOIN documents d ON d.workspace_id = w.id
      WHERE d.id = $1
    ) AS readers`

func (r *RBACResolver) ResolveReaders(ctx context.Context, docID string) ([]string, error) {
	rows, err := r.Pool.Query(ctx, readerSQL, docID)
	if err != nil {
		return nil, fmt.Errorf("resolve readers: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *RBACResolver) ViewerScope(ctx context.Context, userID string) (rag.ViewerScope, error) {
	scope := rag.ViewerScope{UserID: userID, SubjectIDs: []string{domain.UserSubject(userID)}}
	// group memberships
	rows, err := r.Pool.Query(ctx, `SELECT group_id FROM group_members WHERE user_id=$1`, userID)
	if err != nil {
		return scope, err
	}
	defer rows.Close()
	for rows.Next() {
		var gid string
		_ = rows.Scan(&gid)
		scope.SubjectIDs = append(scope.SubjectIDs, domain.GroupSubject(gid))
	}
	// visible workspaces (any allow on a workspace for the user or their groups)
	wsRows, err := r.Pool.Query(ctx, `
        SELECT DISTINCT target_id FROM permissions
        WHERE effect='allow' AND target_type='workspace'
          AND (subject_type='user' AND subject_id=$1
               OR subject_type='group' AND subject_id=ANY(SELECT group_id FROM group_members WHERE user_id=$1))`, userID)
	if err != nil {
		return scope, err
	}
	defer wsRows.Close()
	for wsRows.Next() {
		var wid string
		_ = wsRows.Scan(&wid)
		scope.WorkspaceIDs = append(scope.WorkspaceIDs, wid)
	}
	return scope, nil
}

var _ rag.RBACResolver = (*RBACResolver)(nil)
