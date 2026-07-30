package server

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Session is one MCP session (design doc 06 §7.3, 03 §2.8 mcp_sessions).
type Session struct {
	ID           string
	TokenID      string
	Transport    string // http_sse / stdio
	ClientInfo   map[string]any
	Capabilities map[string]any
	StartedAt    time.Time
	EndedAt      *time.Time
}

// SessionStore persists MCP sessions. Implementations:
//   - MemorySessionStore: in-memory (single-replica dev/test).
//   - PostgresSessionStore: mcp_sessions table (design doc 03 §2.8).
type SessionStore interface {
	Create(ctx context.Context, s *Session) error
	Get(ctx context.Context, id string) (*Session, error)
	End(ctx context.Context, id string) error
	// CountActive returns the number of non-ended sessions (for metrics).
	CountActive(ctx context.Context) (int, error)
}

// MemorySessionStore is an in-memory session store.
type MemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

// NewMemorySessionStore returns an empty in-memory session store.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{sessions: make(map[string]*Session)}
}

// Create implements SessionStore.
func (s *MemorySessionStore) Create(_ context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess.ID == "" {
		sess.ID = uuid.NewString()
	}
	if sess.StartedAt.IsZero() {
		sess.StartedAt = time.Now().UTC()
	}
	s.sessions[sess.ID] = sess
	return nil
}

// Get implements SessionStore.
func (s *MemorySessionStore) Get(_ context.Context, id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, nil
	}
	cp := *sess
	return &cp, nil
}

// End implements SessionStore.
func (s *MemorySessionStore) End(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[id]; ok {
		now := time.Now().UTC()
		sess.EndedAt = &now
	}
	return nil
}

// CountActive implements SessionStore.
func (s *MemorySessionStore) CountActive(_ context.Context) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, sess := range s.sessions {
		if sess.EndedAt == nil {
			n++
		}
	}
	return n, nil
}

// PostgresSessionStore persists sessions to mcp_sessions.
type PostgresSessionStore struct {
	pool *pgxpool.Pool
}

// NewPostgresSessionStore returns a PG-backed session store.
func NewPostgresSessionStore(pool *pgxpool.Pool) *PostgresSessionStore {
	return &PostgresSessionStore{pool: pool}
}

// Create implements SessionStore.
func (s *PostgresSessionStore) Create(ctx context.Context, sess *Session) error {
	if sess.ID == "" {
		sess.ID = uuid.NewString()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO mcp_sessions (id, token_id, transport, client_info, capabilities, started_at)
		VALUES ($1, $2, $3, $4, $5, now())`,
		sess.ID, sess.TokenID, sess.Transport, sess.ClientInfo, sess.Capabilities)
	return err
}

// Get implements SessionStore.
func (s *PostgresSessionStore) Get(ctx context.Context, id string) (*Session, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT id, token_id, transport, client_info, capabilities, started_at, ended_at
		FROM mcp_sessions WHERE id = $1`, id)
	var sess Session
	var clientInfo, capabilities map[string]any
	err := row.Scan(&sess.ID, &sess.TokenID, &sess.Transport, &clientInfo, &capabilities, &sess.StartedAt, &sess.EndedAt)
	if err != nil {
		return nil, nil // treat not-found as nil
	}
	sess.ClientInfo = clientInfo
	sess.Capabilities = capabilities
	return &sess, nil
}

// End implements SessionStore.
func (s *PostgresSessionStore) End(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE mcp_sessions SET ended_at = now() WHERE id = $1`, id)
	return err
}

// CountActive implements SessionStore.
func (s *PostgresSessionStore) CountActive(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_sessions WHERE ended_at IS NULL`).Scan(&n)
	return n, err
}
