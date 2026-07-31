// Package audit records every MCP tool/resource call. Records are append-only
// (INSERT never UPDATE/DELETE) to satisfy the immutable-audit requirement
// (design doc 06 §7.1, 03 §2.8 mcp_tool_calls). Forbidden calls additionally
// bump the mcp_forbidden_total Prometheus metric.
package audit

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ResultStatus is the outcome category of a tool/resource call.
type ResultStatus string

const (
	StatusSuccess   ResultStatus = "success"
	StatusForbidden ResultStatus = "forbidden"
	StatusError     ResultStatus = "error"
)

// Record is one tool/resource call audit entry (design doc 06 §7.1, 03 §2.8).
type Record struct {
	ID             string
	SessionID      string
	TokenID        string
	IdentityID     string
	ToolName       string
	ParamsSummary  map[string]any
	ResultStatus   ResultStatus
	TargetResource string
	DurationMS     int
	AuditLogID     string
	CreatedAt      time.Time
}

// Filter selects tool-call records for the admin query endpoint (design doc 04 §10).
type Filter struct {
	TokenID  string
	ToolName string
	Since    *time.Time
	Limit    int
}

// Store persists audit records. Implementations:
//   - MemoryStore: in-memory ring, for tests / standalone dev.
//   - PostgresStore: appends to mcp_tool_calls + audit_logs (design doc 03 §2.8/§2.6).
type Store interface {
	Record(ctx context.Context, r *Record) error
	List(ctx context.Context, f Filter) ([]Record, error)
}

// MemoryStore is an in-memory audit store.
type MemoryStore struct {
	mu      sync.Mutex
	records []Record
}

// NewMemoryStore returns an empty in-memory audit store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

// Record appends a record (assigning an ID + timestamp if empty).
func (s *MemoryStore) Record(_ context.Context, r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now().UTC()
	}
	s.records = append(s.records, *r)
	return nil
}

// List returns records matching the filter, newest first.
func (s *MemoryStore) List(_ context.Context, f Filter) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	out := make([]Record, 0, limit)
	for i := len(s.records) - 1; i >= 0 && len(out) < limit; i-- {
		r := s.records[i]
		if f.TokenID != "" && r.TokenID != f.TokenID {
			continue
		}
		if f.ToolName != "" && r.ToolName != f.ToolName {
			continue
		}
		if f.Since != nil && r.CreatedAt.Before(*f.Since) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// PostgresStore appends to mcp_tool_calls and writes a corresponding
// audit_logs entry (append-only). Both inserts run in one transaction so the
// tool-call record and its audit-log linkage stay consistent.
type PostgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgresStore returns a PG-backed audit store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// Record implements Store.
func (s *PostgresStore) Record(ctx context.Context, r *Record) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// audit_logs.target_id is a UUID column; pass NULL when the target resource
	// is empty or not a valid UUID (many tool calls have no specific target).
	var targetID any
	if r.TargetResource != "" {
		if _, err := uuid.Parse(r.TargetResource); err == nil {
			targetID = r.TargetResource
		}
	}

	// audit_logs is append-only (design doc 03 §2.6): actor, action, target,
	// detail. We insert first to obtain an audit_log_id to reference.
	var auditLogID string
	err = tx.QueryRow(ctx, `
		INSERT INTO audit_logs (actor_type, actor_id, action, target_type, target_id, detail, created_at)
		VALUES ('api_token', $1, $2, 'mcp_tool', $3, $4, now())
		RETURNING id`,
		r.TokenID, "mcp:"+r.ToolName, targetID, r.ParamsSummary,
	).Scan(&auditLogID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO mcp_tool_calls (id, session_id, tool_name, params_summary, result_status, target_resource, duration_ms, audit_log_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now())`,
		uuid.NewString(), r.SessionID, r.ToolName, r.ParamsSummary,
		string(r.ResultStatus), r.TargetResource, r.DurationMS, auditLogID,
	)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, f Filter) ([]Record, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT c.id, c.session_id, c.tool_name, c.params_summary, c.result_status, c.target_resource, c.duration_ms, c.audit_log_id, c.created_at
		FROM mcp_tool_calls c
		JOIN mcp_sessions s ON c.session_id = s.id
		WHERE ($1::text = '' OR s.token_id::text = $1)
		  AND ($2::text = '' OR c.tool_name = $2)
		ORDER BY c.created_at DESC LIMIT $3`,
		f.TokenID, f.ToolName, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Record, 0)
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.ID, &r.SessionID, &r.ToolName, &r.ParamsSummary,
			&r.ResultStatus, &r.TargetResource, &r.DurationMS, &r.AuditLogID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
