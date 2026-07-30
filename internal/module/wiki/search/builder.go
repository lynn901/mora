package search

// Package search builds PostgreSQL full-text search queries with BM25 ranking,
// multi-dimensional filters, and strict RBAC filtering at the SQL layer
// (PRD F1.5, AC-8). RBAC is a hard constraint: documents outside the user's
// visible set never enter the result (存在性不泄露).

import (
	"strconv"
	"strings"

	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/pkg/pagination"
)

// Filter holds all multi-dimensional search filter dimensions.
type Filter struct {
	Query        string
	WorkspaceID  domain.UUID
	DirectoryID  *domain.UUID
	Tag          *domain.UUID
	CreatedBy    *domain.UUID
	UpdatedAfter *string // RFC3339
	UpdatedBefore *string
	DocType      string
	Sort         string // relevance | updated
	// RBAC visibility (hard constraint):
	VisibleDocs []domain.UUID // explicit doc IDs the user may read
	VisibleAll  bool          // true if workspace-wide read grant
	pagination.Params
	FTSConfig string // chinese_zh or simple
}

// Result is a single search hit.
type Result struct {
	DocumentID  domain.UUID `json:"document_id"`
	Title       string      `json:"title"`
	Snippet     string      `json:"snippet"`
	Highlight   []string    `json:"highlight"`
	Score       float64     `json:"score"`
	WorkspaceID domain.UUID `json:"workspace_id"`
	DirectoryID *domain.UUID `json:"directory_id,omitempty"`
	UpdatedAt   string      `json:"updated_at"`
}

// Query holds a built SQL statement and its arguments.
type Query struct {
	SQL  string
	Args []any
}

// Build constructs the FTS search query. It uses ts_rank_cd for BM25-style
// relevance scoring and applies RBAC via a hard WHERE filter.
func (f Filter) Build() Query {
	var sb strings.Builder
	var args []any
	argIdx := 1
	cfg := f.FTSConfig
	if cfg == "" {
		cfg = "simple"
	}

	sb.WriteString(`SELECT d.id, d.title,
		ts_headline(`)
	sb.WriteString(quoteIdent(cfg))
	sb.WriteString(`, coalesce(d.title,'') || ' ' || coalesce(d.content_text,''), plainto_tsquery('`)
	sb.WriteString(quoteIdent(cfg))
	sb.WriteString(`', $1), 'StartSel=<em>,StopSel=</em>') AS snippet,
		ts_rank_cd(to_tsvector('`)
	sb.WriteString(quoteIdent(cfg))
	sb.WriteString(`, coalesce(d.title,'') || ' ' || coalesce(d.content_text,'')), plainto_tsquery('`)
	sb.WriteString(quoteIdent(cfg))
	sb.WriteString(`', $1)) AS score,
		d.workspace_id, d.directory_id, d.updated_at
	FROM documents d
	WHERE d.status != 'deleted'
	  AND to_tsvector('`)
	sb.WriteString(quoteIdent(cfg))
	sb.WriteString(`, coalesce(d.title,'') || ' ' || coalesce(d.content_text,'')) @@ plainto_tsquery('`)
	sb.WriteString(quoteIdent(cfg))
	sb.WriteString(`', $1)`)
	args = append(args, f.Query)

	// Workspace filter
	sb.WriteString(` AND d.workspace_id = $`)
	sb.WriteString(itoa(argIdx))
	args = append(args, f.WorkspaceID)
	argIdx++

	// Directory filter
	if f.DirectoryID != nil {
		sb.WriteString(` AND d.directory_id = $`)
		sb.WriteString(itoa(argIdx))
		args = append(args, *f.DirectoryID)
		argIdx++
	}

	// Tag filter (join document_tags)
	if f.Tag != nil {
		sb.WriteString(` AND EXISTS (SELECT 1 FROM document_tags dt WHERE dt.document_id = d.id AND dt.tag_id = $`)
		sb.WriteString(itoa(argIdx))
		sb.WriteString(`)`)
		args = append(args, *f.Tag)
		argIdx++
	}

	// CreatedBy filter
	if f.CreatedBy != nil {
		sb.WriteString(` AND d.created_by = $`)
		sb.WriteString(itoa(argIdx))
		args = append(args, *f.CreatedBy)
		argIdx++
	}

	// UpdatedAfter / Before
	if f.UpdatedAfter != nil {
		sb.WriteString(` AND d.updated_at >= $`)
		sb.WriteString(itoa(argIdx))
		args = append(args, *f.UpdatedAfter)
		argIdx++
	}
	if f.UpdatedBefore != nil {
		sb.WriteString(` AND d.updated_at <= $`)
		sb.WriteString(itoa(argIdx))
		args = append(args, *f.UpdatedBefore)
		argIdx++
	}

	// RBAC hard filter: only visible documents. If VisibleAll, no extra filter
	// (user has workspace-wide read). Otherwise restrict to explicit ID set;
	// empty set means nothing visible.
	if !f.VisibleAll {
		if len(f.VisibleDocs) == 0 {
			// No visible documents: force empty result without leaking existence.
			sb.WriteString(` AND FALSE`)
		} else {
			sb.WriteString(` AND d.id IN (`)
			for i, id := range f.VisibleDocs {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteString(`$`)
				sb.WriteString(itoa(argIdx))
				args = append(args, id)
				argIdx++
			}
			sb.WriteString(`)`)
		}
	}

	// Order
	if f.Sort == "updated" {
		sb.WriteString(` ORDER BY d.updated_at DESC`)
	} else {
		sb.WriteString(` ORDER BY score DESC`)
	}

	// Pagination
	limit := f.PageSize
	if limit < 1 {
		limit = pagination.DefaultPageSize
	}
	offset := f.Offset()
	sb.WriteString(` LIMIT `)
	sb.WriteString(itoa(limit))
	sb.WriteString(` OFFSET `)
	sb.WriteString(itoa(offset))

	return Query{SQL: sb.String(), Args: args}
}

// quoteIdent returns the FTS config name for safe interpolation. The config is
// interpolated into string literals, so it MUST be a strict allowlist of
// [a-zA-Z0-9_] only — anything else (quotes, semicolons, spaces) is rejected
// and falls back to "simple" to prevent SQL injection.
func quoteIdent(s string) string {
	if !isSafeIdent(s) {
		return "simple"
	}
	return s
}

func isSafeIdent(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_':
		default:
			return false
		}
	}
	return true
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
