package outbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StreamPublisher publishes a serialized event payload to a named Stream.
// The Valkey knowledge_events queue implements this (see infra/mq).
type StreamPublisher interface {
	Publish(ctx context.Context, stream string, payload []byte) (id string, err error)
}

// DB is the minimal pooling surface the Dispatcher needs. infra/postgres.DB
// satisfies it (BeginTx returns a pgx.Tx).
type DB interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Dispatcher polls unpublished outbox_events with FOR UPDATE SKIP LOCKED,
// publishes to each destination Stream, records outbox_deliveries, and marks
// the event published once all destinations succeed (§6.3).
type Dispatcher struct {
	db       DB
	streams  map[string]StreamPublisher // stream name -> publisher
	batch    int
	interval time.Duration
}

// NewDispatcher builds a Dispatcher. streams maps destination Stream name to
// its publisher; events whose destinations include an unmapped stream are
// marked with a per-stream delivery error but not fatal (§5.3 resilience).
func NewDispatcher(db DB, streams map[string]StreamPublisher, batch int, interval time.Duration) *Dispatcher {
	if batch <= 0 {
		batch = 50
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &Dispatcher{db: db, streams: streams, batch: batch, interval: interval}
}

// Poll claims a batch of unpublished events and publishes them. All required
// streams delivered -> writes published_at. Uses FOR UPDATE SKIP LOCKED so
// multiple dispatchers don't contend (§6.3).
func (d *Dispatcher) Poll(ctx context.Context) error {
	tx, err := d.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // safe: commit handled explicitly on success path

	rows, err := tx.Query(ctx, `
		SELECT id, aggregate_type, aggregate_id, event_type, event_version,
		       workspace_id, destinations, payload, occurred_at, attempt
		FROM outbox_events
		WHERE published_at IS NULL
		ORDER BY occurred_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, d.batch)
	if err != nil {
		return err
	}
	events, err := scanEvents(rows)
	rows.Close()
	if err != nil {
		return err
	}

	published := 0
	for _, ev := range events {
		allDelivered, err := d.deliver(ctx, tx, ev)
		if err != nil {
			// Unexpected error: record the failure and attempt+1; do not abort
			// the whole batch (other events can still ship this round).
			if ferr := d.recordFailure(ctx, tx, ev, err); ferr != nil {
				return ferr
			}
			continue
		}
		if allDelivered {
			if _, err := tx.Exec(ctx, `UPDATE outbox_events SET published_at = now(), attempt = attempt + 1, last_error = NULL WHERE id = $1`, ev.ID); err != nil {
				return err
			}
			published++
			continue
		}
		// Partial / all-failed delivery: keep the event unpublished so it is
		// re-polled next round, but bump attempt so a permanently stuck event
		// is observable (dead-letter heuristics read attempt + last_error).
		if _, err := tx.Exec(ctx, `UPDATE outbox_events SET attempt = attempt + 1 WHERE id = $1`, ev.ID); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	_ = published
	return nil
}

// deliver publishes the event to each destination, recording an
// outbox_deliveries row per stream. Returns allDelivered=true when every
// destination has a successful delivery (or already had one).
func (d *Dispatcher) deliver(ctx context.Context, tx pgx.Tx, ev Event) (bool, error) {
	allDelivered := true
	for _, stream := range ev.Destinations {
		pub, ok := d.streams[stream]
		if !ok {
			// No publisher wired for this stream (Phase 0: knowledge_events has
			// no consumer yet). Record a delivery row with last_error so the
			// event stays unpublished and is retried; this is expected, not fatal.
			_, _ = tx.Exec(ctx, `
				INSERT INTO outbox_deliveries (outbox_event_id, stream, delivery_attempt, last_error)
				VALUES ($1,$2,$3,$4)
				ON CONFLICT (outbox_event_id, stream) DO UPDATE SET
					delivery_attempt = outbox_deliveries.delivery_attempt + 1,
					last_error = EXCLUDED.last_error`,
				ev.ID, stream, 1, "no publisher registered for stream")
			allDelivered = false
			continue
		}
		pubID, err := pub.Publish(ctx, stream, ev.Payload)
		if err != nil {
			_, _ = tx.Exec(ctx, `
				INSERT INTO outbox_deliveries (outbox_event_id, stream, delivery_attempt, last_error)
				VALUES ($1,$2,$3,$4)
				ON CONFLICT (outbox_event_id, stream) DO UPDATE SET
					delivery_attempt = outbox_deliveries.delivery_attempt + 1,
					last_error = EXCLUDED.last_error`,
				ev.ID, stream, 1, err.Error())
			allDelivered = false
			continue
		}
		_, _ = tx.Exec(ctx, `
			INSERT INTO outbox_deliveries (outbox_event_id, stream, delivery_attempt, delivered_at)
			VALUES ($1,$2,$3,now())
			ON CONFLICT (outbox_event_id, stream) DO UPDATE SET
				delivery_attempt = outbox_deliveries.delivery_attempt + 1,
				delivered_at = now(), last_error = NULL`,
			ev.ID, stream, 1)
		_ = pubID
	}
	return allDelivered, nil
}

func (d *Dispatcher) recordFailure(ctx context.Context, tx pgx.Tx, ev Event, cause error) error {
	_, err := tx.Exec(ctx, `
		UPDATE outbox_events
		SET attempt = attempt + 1, last_error = $2
		WHERE id = $1`, ev.ID, cause.Error())
	return err
}

// Run polls in a loop until ctx is cancelled, sleeping interval between batches.
func (d *Dispatcher) Run(ctx context.Context) error {
	t := time.NewTicker(d.interval)
	defer t.Stop()
	for {
		if err := d.Poll(ctx); err != nil && !errors.Is(err, context.Canceled) {
			// Log via error return on next iteration; keep polling.
			_ = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func scanEvents(rows pgx.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var ev Event
		var wsID *uuid.UUID
		var dests []string
		var payload []byte
		var aggType, evType string
		var aggID uuid.UUID
		var evVer int
		var occurred time.Time
		var attempt int
		if err := rows.Scan(&ev.ID, &aggType, &aggID, &evType, &evVer, &wsID, &dests, &payload, &occurred, &attempt); err != nil {
			return nil, fmt.Errorf("outbox: scan: %w", err)
		}
		ev.AggregateType = aggType
		ev.AggregateID = aggID
		ev.EventType = evType
		ev.EventVersion = evVer
		ev.WorkspaceID = wsID
		ev.Destinations = dests
		ev.Payload = payload
		ev.OccurredAt = occurred
		ev.Attempt = attempt
		out = append(out, ev)
	}
	return out, rows.Err()
}
