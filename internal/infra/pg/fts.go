// Package pg implements the RAG PostgreSQL-backed ports (BM25 full-text search,
// index-status store, embedding-model store) with pgx. RBAC is enforced in SQL
// on this path (the Dense path enforces it in Qdrant payload) — existence of
// unauthorized documents is never leaked (PRD F1.5).
package pg

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wiki/wiki-backend/internal/module/rag"
)

// FTSStore runs BM25 retrieval over documents.content_text with a GIN index
// (03-data-model.md §2.3) and an SQL-layer RBAC filter.
type FTSStore struct {
	Pool *pgxpool.Pool
	// VisibilitySQL is an optional SQL fragment appended to the WHERE clause to
	// enforce RBAC; it receives the subject-id array as a parameter. If empty, a
	// default resolution against the permissions table is used. YS-6 may inject
	// its optimized rbac_doc_visible(...) predicate here.
	VisibilitySQL string
}

func NewFTSStore(pool *pgxpool.Pool) *FTSStore { return &FTSStore{Pool: pool} }

const defaultVisibilitySQL = `
  EXISTS (
    SELECT 1 FROM permissions p
    WHERE p.effect = 'allow'
      AND p.subject_id = ANY($3)
      AND (
        p.target_type = 'document' AND p.target_id = d.id
        OR p.target_type = 'directory' AND p.inherit_scope = 'subtree'
           AND p.target_id = d.directory_id
        OR p.target_type = 'workspace' AND p.inherit_scope = 'subtree'
           AND p.target_id = d.workspace_id
      )
  )
  AND NOT EXISTS (
    SELECT 1 FROM permissions p2
    WHERE p2.effect = 'deny'
      AND p2.subject_id = ANY($3)
      AND (
        p2.target_type = 'document' AND p2.target_id = d.id
        OR p2.target_type = 'directory' AND p2.inherit_scope = 'subtree'
           AND p2.target_id = d.directory_id
        OR p2.target_type = 'workspace' AND p2.inherit_scope = 'subtree'
           AND p2.target_id = d.workspace_id
      )
  )`

func (s *FTSStore) SearchBM25(ctx context.Context, req rag.FTSRequest) ([]rag.FTSHit, error) {
	vis := s.VisibilitySQL
	if vis == "" {
		vis = defaultVisibilitySQL
	}
	q := `
        SELECT d.id, d.title,
               ts_headline('chinese_zh', coalesce(d.content_text,''), q.qry) AS snippet,
               ts_rank_cd(to_tsvector('chinese_zh', coalesce(d.title,'') || ' ' || coalesce(d.content_text,'')), q.qry) AS score,
               d.workspace_id
        FROM documents d, plainto_tsquery('chinese_zh', $1) AS q(qry)
        WHERE to_tsvector('chinese_zh', coalesce(d.title,'') || ' ' || coalesce(d.content_text,'')) @@ q.qry
          AND d.status = 'published'
          AND ($5::text = '' OR d.workspace_id = $5)
          AND ($6::text = '' OR d.directory_id::text = $6)
          AND `
	q += vis
	q += `
        ORDER BY score DESC
        LIMIT $2`
	rows, err := s.Pool.Query(ctx, q, req.Query, req.TopK, req.VisibleTo, nil, strOrNull(req.WorkspaceID), strOrNull(req.DirectoryID))
	if err != nil {
		return nil, fmt.Errorf("bm25 query: %w", err)
	}
	defer rows.Close()
	var out []rag.FTSHit
	for rows.Next() {
		var h rag.FTSHit
		if err := rows.Scan(&h.DocumentID, &h.Title, &h.ChunkText, &h.Score, &h.WorkspaceID); err != nil {
			return nil, err
		}
		h.ChunkIndex = 0
		out = append(out, h)
	}
	return out, rows.Err()
}

func strOrNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ensure strings import is used (kept for future filter building).
var _ = strings.TrimSpace
