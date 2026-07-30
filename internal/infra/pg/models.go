package pg

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wiki/wiki-backend/internal/domain"
)

// ModelStore persists embedding_models config (03-data-model.md §2.7).
type ModelStore struct {
	Pool *pgxpool.Pool
}

func NewModelStore(pool *pgxpool.Pool) *ModelStore { return &ModelStore{Pool: pool} }

func (s *ModelStore) GetActive(ctx context.Context) (domain.EmbeddingModel, error) {
	row := s.Pool.QueryRow(ctx, modelSelect+` WHERE status='active' ORDER BY created_at DESC LIMIT 1`)
	return scanModel(row)
}

func (s *ModelStore) GetByID(ctx context.Context, id string) (domain.EmbeddingModel, error) {
	row := s.Pool.QueryRow(ctx, modelSelect+` WHERE id=$1`, id)
	return scanModel(row)
}

func (s *ModelStore) List(ctx context.Context) ([]domain.EmbeddingModel, error) {
	rows, err := s.Pool.Query(ctx, modelSelect+` ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EmbeddingModel
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *ModelStore) Upsert(ctx context.Context, m domain.EmbeddingModel) (domain.EmbeddingModel, error) {
	row := s.Pool.QueryRow(ctx, `
        INSERT INTO embedding_models (provider, model_name, dimension, max_token, instruction_query, instruction_doc, status)
        VALUES ($1,$2,$3,$4,$5,$6,'active')
        ON CONFLICT (provider, model_name) DO UPDATE SET
            dimension=EXCLUDED.dimension, max_token=EXCLUDED.max_token,
            instruction_query=EXCLUDED.instruction_query, instruction_doc=EXCLUDED.instruction_doc,
            updated_at=now()
        RETURNING `+modelCols, m.Provider, m.ModelName, m.Dimension, m.MaxToken, m.InstructionQuery, m.InstructionDoc)
	return scanModel(row)
}

func (s *ModelStore) SetActive(ctx context.Context, id string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE embedding_models SET status='inactive' WHERE status='active'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE embedding_models SET status='active', updated_at=now() WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const modelCols = `id, provider, model_name, dimension, max_token, instruction_query, instruction_doc, status, created_at, updated_at`
const modelSelect = `SELECT ` + modelCols + ` FROM embedding_models`

func scanModel(row interface{ Scan(dest ...any) error }) (domain.EmbeddingModel, error) {
	var m domain.EmbeddingModel
	var iq, idoc *string
	err := row.Scan(&m.ID, &m.Provider, &m.ModelName, &m.Dimension, &m.MaxToken, &iq, &idoc, &m.Status, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return m, err
	}
	if iq != nil {
		m.InstructionQuery = *iq
	}
	if idoc != nil {
		m.InstructionDoc = *idoc
	}
	return m, nil
}

// keep time import used (timestamps scanned into time.Time)
var _ = time.Now
