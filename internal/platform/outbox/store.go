// Package outbox implements the transactional outbox (design-docs/12 §6.3,
// 13 §5.3). Producers write outbox_events rows IN THE SAME database
// transaction as their aggregate state changes; the Dispatcher polls
// unpublished events with FOR UPDATE SKIP LOCKED and publishes them to
// target Streams, recording an outbox_deliveries row per delivery.
//
// Phase 0 scope: the skeleton + polling/delivery loop. Knowledge consumers
// attach in Phase 1; Phase 0 only guarantees events are not lost (they sit
// in outbox_events until a consumer is wired). Doc-write transactions
// double-write a Knowledge Outbox event alongside the existing doc_events
// publish (§5.3) — the old RAG publisher keeps serving doc_events.
package outbox

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lynn901/mora/internal/domain"
)

// KnowledgeEventsStream is the Valkey Stream knowledge events are published to
// (design-docs/13 §5.1). Consumers (rag-worker's knowledge pipeline) read this
// stream via a consumer group. Phase 0 only guarantees events are not lost —
// they sit in the stream until a consumer is wired.
const KnowledgeEventsStream = "knowledge_events"

// Store records outbox_events inside a producer's transaction. It takes the
// SAME pgx.Tx the aggregate write uses, so the event is committed atomically
// with the state change — no separate publish call (§6.3).
type Store struct{}

// NewStore builds the outbox Store. Stateless; the transaction is supplied
// per call so one Store serves all producers.
func NewStore() *Store { return &Store{} }

// Record inserts an outbox_events row using tx (the producer's transaction).
// destinations is the set of Streams the event must reach (e.g. ["knowledge_events"]).
func (s *Store) Record(ctx context.Context, tx pgx.Tx, ev domain.KnowledgeEvent, destinations []string) error {
	if tx == nil {
		return ErrNoTx
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// aggregate_id is a UUID; keep it as text in the column for generality.
	_, err = tx.Exec(ctx, `
		INSERT INTO outbox_events
		  (aggregate_type, aggregate_id, event_type, event_version, workspace_id,
		   actor_type, actor_id, destinations, payload, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, $10)`,
		ev.AggregateType, ev.AggregateID,
		ev.EventType, ev.EventVersion,
		ev.WorkspaceID,
		string(ev.Actor.Type), ev.Actor.ID,
		destinations, payload,
		nonNilTime(ev.OccurredAt),
	)
	return err
}

// ErrNoTx is returned when Record is called without a transaction.
var ErrNoTx = &pgconn.PgError{Message: "outbox: record requires a transaction"}

// nonNilTime returns t, or now if t is zero, so occurred_at is never NULL.
func nonNilTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

// Event is a minimal row projection for the dispatcher.
type Event struct {
	ID            uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	EventType     string
	EventVersion  int
	WorkspaceID   *uuid.UUID
	Destinations  []string
	Payload       []byte
	OccurredAt    time.Time
	Attempt       int
}
