package pg

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
)

// ParseConfig is the domain view of a parse_configs row (10 §7). A template
// carries a ParseOptions JSONB the upload handler can reference by id, or the
// API can inline overrides on top of it.
type ParseConfig struct {
	ID          string
	WorkspaceID *string // nil = global default
	Name        string
	Config      map[string]any
	IsDefault   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ParseConfigStore persists parse_configs templates (10 §7).
type ParseConfigStore struct {
	Pool *pgxpool.Pool
}

func NewParseConfigStore(pool *pgxpool.Pool) *ParseConfigStore { return &ParseConfigStore{Pool: pool} }

const parseConfigSelect = `SELECT id, workspace_id, name, config, is_default, created_at, updated_at FROM parse_configs`

func scanParseConfig(row interface{ Scan(dest ...any) error }) (ParseConfig, error) {
	var c ParseConfig
	var wsID *string
	var cfg []byte
	if err := row.Scan(&c.ID, &wsID, &c.Name, &cfg, &c.IsDefault, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return c, err
	}
	if wsID != nil {
		c.WorkspaceID = wsID
	}
	if len(cfg) > 0 {
		_ = json.Unmarshal(cfg, &c.Config)
	}
	return c, nil
}

// List returns parse_configs for a workspace (workspace-scoped + global).
func (s *ParseConfigStore) List(ctx context.Context, workspaceID string) ([]ParseConfig, error) {
	rows, err := s.Pool.Query(ctx, parseConfigSelect+` WHERE workspace_id IS NULL OR workspace_id=$1 ORDER BY is_default DESC, name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ParseConfig
	for rows.Next() {
		c, err := scanParseConfig(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get returns a single config by id; the caller enforces workspace scoping.
func (s *ParseConfigStore) Get(ctx context.Context, id string) (ParseConfig, error) {
	row := s.Pool.QueryRow(ctx, parseConfigSelect+` WHERE id=$1`, id)
	return scanParseConfig(row)
}

// Create inserts a new config. workspaceID may be "" for a global default.
func (s *ParseConfigStore) Create(ctx context.Context, c ParseConfig) (ParseConfig, error) {
	cfg, _ := json.Marshal(c.Config)
	var wsIDArg any
	if c.WorkspaceID != nil && *c.WorkspaceID != "" {
		wsIDArg = *c.WorkspaceID
	}
	row := s.Pool.QueryRow(ctx, `
        INSERT INTO parse_configs (workspace_id, name, config, is_default)
        VALUES ($1, $2, $3, $4)
        RETURNING id, workspace_id, name, config, is_default, created_at, updated_at`,
		wsIDArg, c.Name, cfg, c.IsDefault)
	return scanParseConfig(row)
}

// Update modifies a config's name/config/default flag.
func (s *ParseConfigStore) Update(ctx context.Context, id string, c ParseConfig) (ParseConfig, error) {
	cfg, _ := json.Marshal(c.Config)
	row := s.Pool.QueryRow(ctx, `
        UPDATE parse_configs SET name=$2, config=$3, is_default=$4, updated_at=now()
        WHERE id=$1
        RETURNING id, workspace_id, name, config, is_default, created_at, updated_at`,
		id, c.Name, cfg, c.IsDefault)
	return scanParseConfig(row)
}

// Delete removes a config.
func (s *ParseConfigStore) Delete(ctx context.Context, id string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM parse_configs WHERE id=$1`, id)
	return err
}

// keep domain import for future ParseOptions binding.
var _ = domain.ParseParsed
