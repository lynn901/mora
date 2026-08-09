// Package worker consumes document events from Valkey Streams and drives the
// RAG pipeline with idempotency, exponential-backoff retry and dead-lettering
// (05-rag-pipeline-design.md §2.4, §3.1).
package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/rag"
	"github.com/lynn901/mora/internal/module/rag/pipeline"
)

// Worker owns the consume loop, idempotency, the task state machine and retry.
// The Pipeline (stateless) does the actual indexing; the Worker decides when to
// retry, when to dead-letter, and when to ACK.
type Worker struct {
	Queue       rag.EventQueue
	Idem        rag.IdempotencyStore
	Status      rag.IndexStatusStore
	Pipeline    *pipeline.Pipeline
	Consumer    string // consumer name within rag_pipeline_group
	Group       string // consumer group name
	Cfg         pipeline.Config
	Sleep       func(ctx context.Context, d time.Duration) error
	Logf        func(format string, args ...any)
	DeadLetters int64 // count of dead-lettered events (for metrics/alerting)
}

func New(w Worker) *Worker {
	if w.Consumer == "" {
		w.Consumer = "rag-worker-1"
	}
	if w.Group == "" {
		w.Group = "rag_pipeline_group"
	}
	if w.Cfg.ChunkSize == 0 {
		w.Cfg = pipeline.DefaultConfig()
	}
	if w.Sleep == nil {
		w.Sleep = func(ctx context.Context, d time.Duration) error {
			select {
			case <-time.After(d):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if w.Logf == nil {
		w.Logf = func(string, ...any) {}
	}
	return &w
}

// Run is the main consume loop. Blocks until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		msgs, err := w.Queue.ReadGroup(ctx, w.Consumer, 10, 5*time.Second)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return ctx.Err()
			}
			w.Logf("rag/worker: readgroup error: %v", err)
			if err := w.Sleep(ctx, time.Second); err != nil {
				return err
			}
			continue
		}
		for _, msg := range msgs {
			w.Process(ctx, msg)
		}
	}
}

// Reclaim steals idle messages (crash recovery via XAUTOCLAIM, 05 §2.4).
func (w *Worker) Reclaim(ctx context.Context, minIdle time.Duration) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msgs, err := w.Queue.Claim(ctx, w.Consumer, minIdle, 10)
		if err != nil {
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		for _, msg := range msgs {
			w.Process(ctx, msg)
		}
	}
}

// Process handles one message: idempotent skip → retry loop → ack/dead-letter.
// Exported so it can be driven in single-message tests and embedded in custom loops.
func (w *Worker) Process(ctx context.Context, msg rag.QueueMessage) {
	ev := msg.DocEvent

	// 1. Idempotent task lookup. An already-indexed task means a prior run
	//    completed this event → ACK and skip (no duplicate vectors).
	task, err := w.Status.UpsertTask(ctx, domain.IndexingTask{
		DocumentID: ev.DocumentID,
		EventID:    ev.EventID,
		EventType:  ev.EventType,
		Status:     domain.TaskPending,
		MaxAttempt: w.Cfg.MaxAttempt,
		Payload:    ev.Payload,
	})
	if err != nil {
		w.Logf("rag/worker: upsert task for %s: %v (nack, will retry)", ev.EventID, err)
		return // leave un-ACKed; will be reclaimed
	}
	if task.Status == domain.TaskIndexed {
		_ = w.Queue.Ack(ctx, msg)
		return
	}

	// 2. Mark processing.
	_ = w.Status.SetDocumentIndexStatus(ctx, ev.DocumentID, domain.IndexProcessing, "")
	_ = w.Status.UpdateTaskStatus(ctx, task.ID, domain.TaskProcessing, task.Attempt, "")

	// 3. Retry loop with exponential backoff (10s/30s/90s).
	maxAttempt := w.Cfg.MaxAttempt
	if task.MaxAttempt > 0 && task.MaxAttempt < maxAttempt {
		maxAttempt = task.MaxAttempt
	}
	var lastErr error
	for attempt := 1; attempt <= maxAttempt; attempt++ {
		if ctx.Err() != nil {
			return
		}
		err := w.Pipeline.RunOnce(ctx, ev)
		if err == nil {
			// success: record completion, mark indexed, ACK.
			_ = w.Status.UpdateTaskStatus(ctx, task.ID, domain.TaskIndexed, attempt, "")
			if _, markErr := w.Idem.MarkSeen(ctx, ev.EventID, 24*time.Hour); markErr != nil {
				w.Logf("rag/worker: mark seen %s: %v", ev.EventID, markErr)
			}
			// two-stage decoupling (10 §4.1): a successful parse publishes a
			// follow-on document.create so the existing RAG pipeline indexes
			// the freshly-written content. ParseOpts are forwarded so the
			// indexing stage picks the configured chunking strategy.
			if ev.EventType == domain.EventDocumentParse || ev.EventType == domain.EventDocumentReparse {
				w.publishParseFollowOn(ctx, ev)
			}
			_ = w.Queue.Ack(ctx, msg)
			w.Logf("rag/worker: processed %s (%s) attempt %d", ev.EventID, ev.EventType, attempt)
			return
		}
		lastErr = err
		w.Logf("rag/worker: %s attempt %d failed: %v", ev.EventID, attempt, err)
		_ = w.Status.UpdateTaskStatus(ctx, task.ID, domain.TaskFailed, attempt, err.Error())
		if attempt < maxAttempt {
			if sleepErr := w.Sleep(ctx, w.Cfg.Backoff(attempt-1)); sleepErr != nil {
				return // ctx cancelled
			}
		}
	}

	// 4. Final failure: dead-letter + alert + ACK the original (now in dead stream).
	reason := fmt.Sprintf("failed after %d attempts: %v", maxAttempt, lastErr)
	_ = w.Queue.MoveToDeadLetter(ctx, msg, reason)
	_ = w.Status.SetDocumentIndexStatus(ctx, ev.DocumentID, domain.IndexFailed, lastErr.Error())
	_ = w.Status.UpdateTaskStatus(ctx, task.ID, domain.TaskFailed, maxAttempt, reason)
	w.DeadLetters++
	w.Logf("rag/worker: DEAD-LETTER %s: %s", ev.EventID, reason)
	_ = w.Queue.Ack(ctx, msg)
}

// publishParseFollowOn emits the document.create event that drives the second
// stage of the two-stage decoupling (10 §4.1): after a successful parse, the
// freshly-written content must be indexed. The new event re-enters the same
// doc_events stream and is consumed by this same worker's create path.
//
// The follow-on carries the chunking strategy from the parse payload so the
// indexing stage uses the configured chunker. It uses a fresh event_id (so it
// is not deduped against the parse event). Publish failures are best-effort:
// the parse already succeeded and the content is persisted; a missed follow-on
// is recoverable via the compensation scanner (which re-publishes pending
// indexing tasks).
func (w *Worker) publishParseFollowOn(ctx context.Context, ev domain.DocEvent) {
	if w.Queue == nil {
		return
	}
	follow := domain.DocEvent{
		EventID:     "parse-idx-" + ev.DocumentID + "-" + ev.EventID,
		EventType:   domain.EventDocumentCreate,
		DocumentID:  ev.DocumentID,
		WorkspaceID: ev.WorkspaceID,
		VersionNo:   ev.VersionNo,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	// forward parse_opts so handleIndex can apply the chunking strategy. The
	// pipeline's handleIndex currently reads Config.Strategy (process-wide);
	// per-event strategy override is a follow-up — for now the parse_opts are
	// carried so a future change can route by them without re-publishing.
	if ev.Payload != nil {
		follow.Payload = map[string]any{"parse_opts": ev.Payload["parse_opts"]}
	}
	if _, err := w.Queue.Publish(ctx, follow); err != nil {
		w.Logf("rag/worker: parse follow-on publish for %s: %v (will retry via compensation)", ev.DocumentID, err)
	}
}
