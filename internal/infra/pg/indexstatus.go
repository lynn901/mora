package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wiki/wiki-backend/internal/domain"
	"github.com/wiki/wiki-backend/internal/module/rag"
)

// IndexStatusStore persists indexing_tasks, chunks metadata and the
// documents.index_status badge (03-data-model.md §2.7).
type IndexStatusStore struct {
	Pool *pgxpool.Pool
}

func NewIndexStatusStore(pool *pgxpool.Pool) *IndexStatusStore { return &IndexStatusStore{Pool: pool} }

func (s *IndexStatusStore) UpsertTask(ctx context.Context, task domain.IndexingTask) (domain.IndexingTask, error) {
	payload, _ := json.Marshal(task.Payload)
	row := s.Pool.QueryRow(ctx, `
        INSERT INTO indexing_tasks (document_id, event_id, event_type, status, attempt, max_attempt, payload, model_id)
        VALUES ($1, $2, $3, COALESCE(NULLIF($4,''),'pending'), 0, COALESCE($5,0,3), $6, $7)
        ON CONFLICT (document_id, event_id) DO UPDATE SET updated_at = now()
        RETURNING id, document_id, event_id, event_type, status, attempt, max_attempt, payload, error_message, model_id, created_at, updated_at`,
		task.DocumentID, task.EventID, string(task.EventType), string(task.Status), task.MaxAttempt, payload, nil)
	return scanTask(row)
}

func (s *IndexStatusStore) UpdateTaskStatus(ctx context.Context, taskID string, status domain.IndexingTaskStatus, attempt int, errMsg string) error {
	_, err := s.Pool.Exec(ctx, `
        UPDATE indexing_tasks SET status=$2, attempt=$3, error_message=$4, updated_at=now() WHERE id=$1`,
		taskID, string(status), attempt, errMsg)
	return err
}

func (s *IndexStatusStore) GetTask(ctx context.Context, taskID string) (domain.IndexingTask, error) {
	row := s.Pool.QueryRow(ctx, taskSelect+` WHERE id=$1`, taskID)
	return scanTask(row)
}

const taskSelect = `SELECT id, document_id, event_id, event_type, status, attempt, max_attempt, payload, error_message, model_id, created_at, updated_at FROM indexing_tasks`

func scanTask(row interface{ Scan(dest ...any) error }) (domain.IndexingTask, error) {
	var t domain.IndexingTask
	var etype, status, payload, errMsg, modelID *string
	if err := row.Scan(&t.ID, &t.DocumentID, &t.EventID, &etype, &status, &t.Attempt, &t.MaxAttempt, &payload, &errMsg, &modelID, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return t, err
	}
	if etype != nil {
		t.EventType = domain.EventType(*etype)
	}
	if status != nil {
		t.Status = domain.IndexingTaskStatus(*status)
	}
	if modelID != nil {
		t.ModelID = *modelID
	}
	if errMsg != nil {
		t.ErrorMessage = *errMsg
	}
	if payload != nil {
		_ = json.Unmarshal([]byte(*payload), &t.Payload)
	}
	return t, nil
}

func (s *IndexStatusStore) SetDocumentIndexStatus(ctx context.Context, docID string, status domain.IndexStatus, errMsg string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE documents SET index_status=$2 WHERE id=$1`, docID, string(status))
	return err
}

func (s *IndexStatusStore) GetDocumentIndexStatus(ctx context.Context, docID string) (rag.IndexStatusInfo, error) {
	var info rag.IndexStatusInfo
	var status string
	var lastIdx *time.Time
	var chunkCount int
	err := s.Pool.QueryRow(ctx, `
        SELECT d.index_status, (
            SELECT count(*) FROM chunks c WHERE c.document_id = d.id
        )
        FROM documents d WHERE d.id=$1`, docID).Scan(&status, &chunkCount)
	if err != nil {
		return info, err
	}
	info.IndexStatus = domain.IndexStatus(status)
	info.LastIndexedAt = lastIdx
	info.ChunkCount = chunkCount
	return info, nil
}

func (s *IndexStatusStore) RecordChunks(ctx context.Context, docID string, version int, modelID string, chunks []domain.Chunk) error {
	// idempotent upsert by (document_id, version_no, chunk_index) — deterministic
	// qdrant_point_id makes this safe to redo.
	for _, c := range chunks {
		meta, _ := json.Marshal(c.Metadata)
		_, err := s.Pool.Exec(ctx, `
            INSERT INTO chunks (document_id, version_no, chunk_index, text, token_count, section_path, model_id, qdrant_point_id, metadata)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
            ON CONFLICT (document_id, version_no, chunk_index) DO UPDATE SET
                text=EXCLUDED.text, token_count=EXCLUDED.token_count, section_path=EXCLUDED.section_path,
                model_id=EXCLUDED.model_id, qdrant_point_id=EXCLUDED.qdrant_point_id, metadata=EXCLUDED.metadata`,
			docID, version, c.ChunkIndex, c.Text, c.TokenCount, c.SectionPath, modelID, c.QdrantPointID, meta)
		if err != nil {
			return fmt.Errorf("record chunk %d: %w", c.ChunkIndex, err)
		}
	}
	return nil
}

func (s *IndexStatusStore) DeleteChunkMeta(ctx context.Context, docID string, version int) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM chunks WHERE document_id=$1 AND version_no=$2`, docID, version)
	return err
}

func (s *IndexStatusStore) DeleteAllChunkMeta(ctx context.Context, docID string) error {
	_, err := s.Pool.Exec(ctx, `DELETE FROM chunks WHERE document_id=$1`, docID)
	return err
}

func (s *IndexStatusStore) PendingTasks(ctx context.Context, cutoff time.Time, limit int) ([]domain.IndexingTask, error) {
	rows, err := s.Pool.Query(ctx, taskSelect+`
        WHERE status IN ('pending','failed') AND updated_at < $1 ORDER BY updated_at LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.IndexingTask
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
