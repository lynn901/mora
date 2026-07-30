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
	// Config is the PostgreSQL text-search configuration name used by to_tsvector
	// / ts_headline / plainto_tsquery. It MUST match the config the documents
	// GIN index was built with (003_documents.up.sql picks chinese_zh when
	// zhparser is installed, else simple). Default "simple" so BM25 works on a
	// stock postgres:16 image without zhparser; wiki-api injects cfg.FTSConfig.
	Config string
}

// NewFTSStore returns an FTSStore using the "simple" text-search configuration
// (safe on stock postgres without zhparser). Use SetConfig / the Config field to
// switch to "chinese_zh" when zhparser is installed.
func NewFTSStore(pool *pgxpool.Pool) *FTSStore { return &FTSStore{Pool: pool, Config: "simple"} }

// tsConfig returns a sanitized text-search configuration identifier safe to
// interpolate into the query (TS config names are identifiers, not bind params).
func (s *FTSStore) tsConfig() string {
	cfg := s.Config
	if cfg == "" {
		cfg = "simple"
	}
	var b strings.Builder
	for _, r := range cfg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" {
		return "simple"
	}
	return out
}

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
	cfg := s.tsConfig()
	q := `
        SELECT d.id, d.title,
               ts_headline(` + cfg + `, coalesce(d.content_text,''), q.qry) AS snippet,
               ts_rank_cd(to_tsvector(` + cfg + `, coalesce(d.title,'') || ' ' || coalesce(d.content_text,'')), q.qry) AS score,
               d.workspace_id
        FROM documents d, plainto_tsquery(` + cfg + `, $1) AS q(qry)
        WHERE to_tsvector(` + cfg + `, coalesce(d.title,'') || ' ' || coalesce(d.content_text,'')) @@ q.qry
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
