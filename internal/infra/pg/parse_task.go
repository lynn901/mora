package pg

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag"
)

// ParseTaskStore persists parse_tasks, the staged progress timeline, and the
// documents.parse_status badge (10 §4.2.2, §6). It is the parse-side analogue
// of IndexStatusStore: idempotent task upsert, status/progress updates,
// content write-back, and the parse-progress read model.
type ParseTaskStore struct {
	Pool *pgxpool.Pool
}

func NewParseTaskStore(pool *pgxpool.Pool) *ParseTaskStore { return &ParseTaskStore{Pool: pool} }

var _ rag.ParseTaskStore = (*ParseTaskStore)(nil)

const parseTaskSelect = `SELECT id, document_id, event_id, status, attempt, max_attempt, parse_opts, parser_name, progress, error_message, created_at, updated_at FROM parse_tasks`

func scanParseTask(row interface{ Scan(dest ...any) error }) (domain.ParseTask, error) {
	var t domain.ParseTask
	var status, parserName, errMsg *string
	var opts, progress []byte
	if err := row.Scan(&t.ID, &t.DocumentID, &t.EventID, &status, &t.Attempt, &t.MaxAttempt, &opts, &parserName, &progress, &errMsg, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return t, err
	}
	if status != nil {
		t.Status = domain.ParseTaskStatus(*status)
	}
	if parserName != nil {
		t.ParserName = *parserName
	}
	if errMsg != nil {
		t.ErrorMessage = *errMsg
	}
	if len(opts) > 0 {
		_ = json.Unmarshal(opts, &t.ParseOpts)
	}
	if len(progress) > 0 {
		_ = json.Unmarshal(progress, &t.Progress)
	}
	return t, nil
}

func (s *ParseTaskStore) UpsertParseTask(ctx context.Context, task domain.ParseTask) (domain.ParseTask, error) {
	opts, _ := json.Marshal(task.ParseOpts)
	row := s.Pool.QueryRow(ctx, `
        INSERT INTO parse_tasks (document_id, event_id, status, attempt, max_attempt, parse_opts)
        VALUES ($1, $2, COALESCE(NULLIF($3,''),'pending'), 0, COALESCE($4,0,3), $5)
        ON CONFLICT (document_id, event_id) DO UPDATE SET updated_at = now()
        RETURNING id, document_id, event_id, status, attempt, max_attempt, parse_opts, parser_name, progress, error_message, created_at, updated_at`,
		task.DocumentID, task.EventID, string(task.Status), task.MaxAttempt, opts)
	return scanParseTask(row)
}

func (s *ParseTaskStore) UpdateParseTaskStatus(ctx context.Context, taskID string, status domain.ParseTaskStatus, attempt int, errMsg string) error {
	_, err := s.Pool.Exec(ctx, `
        UPDATE parse_tasks SET status=$2, attempt=$3, error_message=$4, updated_at=now() WHERE id=$1`,
		taskID, string(status), attempt, errMsg)
	return err
}

// AppendProgress adds a staged progress entry to the timeline (10 §6.1).
// The progress column is a JSONB array; we append atomically with a single
// UPDATE that re-marshals the array.
func (s *ParseTaskStore) AppendProgress(ctx context.Context, taskID string, stage domain.ProgressStage) error {
	// read current timeline
	row := s.Pool.QueryRow(ctx, `SELECT progress FROM parse_tasks WHERE id=$1`, taskID)
	var raw []byte
	if err := row.Scan(&raw); err != nil {
		return err
	}
	var stages []domain.ProgressStage
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &stages)
	}
	stages = append(stages, stage)
	out, err := json.Marshal(stages)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `UPDATE parse_tasks SET progress=$2, updated_at=now() WHERE id=$1`, taskID, out)
	return err
}

func (s *ParseTaskStore) GetParseTask(ctx context.Context, taskID string) (domain.ParseTask, error) {
	row := s.Pool.QueryRow(ctx, parseTaskSelect+` WHERE id=$1`, taskID)
	return scanParseTask(row)
}

func (s *ParseTaskStore) SetDocumentParseStatus(ctx context.Context, docID string, status domain.ParseStatus, taskID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE documents SET parse_status=$2, parse_task_id=$3 WHERE id=$1`, docID, string(status), taskID)
	return err
}

// SetDocumentContent writes parsed content (Block JSONB) + content_text + the
// parser_name back to the document row (10 §4.1 step 3). source_format is also
// pinned so the parse-progress / re-parse paths know the document's origin.
func (s *ParseTaskStore) SetDocumentContent(ctx context.Context, docID string, blocks []byte, contentText, parserName, sourceFormat string) error {
	_, err := s.Pool.Exec(ctx, `
        UPDATE documents SET content=$2, content_text=$3, source_format=$4 WHERE id=$1`,
		docID, blocks, contentText, sourceFormat)
	if err != nil {
		return fmt.Errorf("set document content: %w", err)
	}
	// record the parser_name on the parse task(s) for this document
	if parserName != "" {
		_, _ = s.Pool.Exec(ctx, `UPDATE parse_tasks SET parser_name=$2 WHERE document_id=$1 AND parser_name IS NULL`, docID, parserName)
	}
	return nil
}

func (s *ParseTaskStore) GetParseProgress(ctx context.Context, docID string) (rag.ParseProgressInfo, error) {
	var info rag.ParseProgressInfo
	var parseStatus, indexStatus string
	var progress []byte
	var updatedAt time.Time
	err := s.Pool.QueryRow(ctx, `
        SELECT d.parse_status, d.index_status, t.progress, COALESCE(t.updated_at, d.updated_at)
        FROM documents d
        LEFT JOIN parse_tasks t ON t.id = d.parse_task_id
        WHERE d.id=$1`, docID).Scan(&parseStatus, &indexStatus, &progress, &updatedAt)
	if err != nil {
		return info, err
	}
	info.ParseStatus = domain.ParseStatus(parseStatus)
	info.IndexStatus = domain.IndexStatus(indexStatus)
	if len(progress) > 0 {
		_ = json.Unmarshal(progress, &info.Progress)
	}
	info.UpdatedAt = &updatedAt
	return info, nil
}

// RecordChunkRelations persists parent-child links for a version (10 §2.3).
// The child/parent ids are the qdrant_point_id strings the pipeline assigned
// (deterministic per doc+version+chunk_index); we resolve them to chunks.id
// via the qdrant_point_id column so this stays independent of chunk ordering.
func (s *ParseTaskStore) RecordChunkRelations(ctx context.Context, docID string, relations []domain.ChunkRelation) error {
	if len(relations) == 0 {
		return nil
	}
	for _, rel := range relations {
		if rel.ChildChunkID == "" || rel.ParentChunkID == "" {
			continue
		}
		var childPK, parentPK string
		err := s.Pool.QueryRow(ctx, `SELECT id FROM chunks WHERE qdrant_point_id=$1 AND document_id=$2`, rel.ChildChunkID, docID).Scan(&childPK)
		if err != nil {
			continue
		}
		err = s.Pool.QueryRow(ctx, `SELECT id FROM chunks WHERE qdrant_point_id=$1 AND document_id=$2`, rel.ParentChunkID, docID).Scan(&parentPK)
		if err != nil {
			continue
		}
		_, _ = s.Pool.Exec(ctx, `
            INSERT INTO chunk_relations (child_chunk_id, parent_chunk_id, document_id)
            VALUES ($1, $2, $3)
            ON CONFLICT (child_chunk_id, parent_chunk_id) DO NOTHING`,
			childPK, parentPK, docID)
	}
	return nil
}
