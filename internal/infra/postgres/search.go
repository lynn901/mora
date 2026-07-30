package postgres

import (
	"context"
	"time"

	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/wiki/search"
)

// SearchExec implements handler.SearchExecutor against PostgreSQL FTS.
type SearchExec struct {
	db *DB
}

func NewSearchExec(db *DB) *SearchExec { return &SearchExec{db: db} }

func (e *SearchExec) Search(ctx context.Context, q search.Query) ([]search.Result, int, error) {
	// total count
	countSQL := "SELECT count(*) FROM (" + q.SQL + ") c"
	var total int
	if err := e.db.Pool.QueryRow(ctx, countSQL, q.Args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := e.db.Pool.Query(ctx, q.SQL, q.Args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []search.Result
	for rows.Next() {
		var r search.Result
		var dirID *domain.UUID
		var updated time.Time
		if err := rows.Scan(&r.DocumentID, &r.Title, &r.Snippet, &r.Score, &r.WorkspaceID, &dirID, &updated); err != nil {
			return nil, 0, err
		}
		r.DirectoryID = dirID
		r.UpdatedAt = updated.UTC().Format(time.RFC3339)
		out = append(out, r)
	}
	return out, total, rows.Err()
}
