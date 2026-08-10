//go:build integration

// Package outbox_test contains integration tests for the transactional outbox
// against a live PostgreSQL instance. Skipped unless DATABASE_URL is set (run
// with: DATABASE_URL=... go test -tags=integration ./internal/platform/outbox/...).
//
// These verify the SQL contract that unit tests can't: the FOR UPDATE SKIP
// LOCKED claim, the outbox_deliveries UPSERT, and the published_at write that
// marks an event fully dispatched (§6.3).
package outbox_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/platform/outbox"
)

func TestMain(m *testing.M) {
	if os.Getenv("DATABASE_URL") == "" {
		os.Exit(0) // skip whole package when no DB configured
	}
	os.Exit(m.Run())
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

// resetOutbox truncates the outbox tables so tests start clean.
func resetOutbox(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, "TRUNCATE outbox_deliveries, outbox_events CASCADE")
	require.NoError(t, err)
}

// recordEvent inserts an outbox_events row directly (bypassing the Store) for
// dispatcher-only test setups.
func recordEvent(t *testing.T, pool *pgxpool.Pool, ev domain.KnowledgeEvent, dests []string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO outbox_events
		  (aggregate_type, aggregate_id, event_type, event_version, workspace_id,
		   actor_type, actor_id, destinations, payload, occurred_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now())
		RETURNING id`,
		ev.AggregateType, ev.AggregateID, ev.EventType, ev.EventVersion,
		ev.WorkspaceID, string(ev.Actor.Type), ev.Actor.ID,
		dests, []byte(`{"event_id":"`+ev.EventID+`"}`),
	).Scan(&id)
	require.NoError(t, err)
	return id
}

// TestStore_RecordInsertsRow: Store.Record writes an outbox_events row inside
// the caller's transaction; the row is visible after commit and unpublished.
func TestStore_RecordInsertsRow(t *testing.T) {
	pool := newPool(t)
	resetOutbox(t, pool)
	store := outbox.NewStore()
	ctx := context.Background()

	ws := uuid.New()
	aggID := uuid.New()
	userID := uuid.New()
	ev := domain.KnowledgeEvent{
		EventID: uuid.New().String(), EventType: domain.KEAssetCreated,
		EventVersion: 1, AggregateType: domain.AggKnowledgeAsset, AggregateID: aggID,
		WorkspaceID: &ws,
		Actor:       domain.EventActor{Type: domain.SubjectUser, ID: userID},
		OccurredAt:  time.Now().UTC(),
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	require.NoError(t, err)
	defer tx.Rollback(ctx)
	require.NoError(t, store.Record(ctx, tx, ev, []string{outbox.KnowledgeEventsStream}))
	require.NoError(t, tx.Commit(ctx))

	var (
		gotAggType, gotEvType string
		gotPublished          *time.Time
		gotDests              []string
	)
	err = pool.QueryRow(ctx, `
		SELECT aggregate_type, event_type, published_at, destinations
		FROM outbox_events WHERE aggregate_id = $1`, aggID).
		Scan(&gotAggType, &gotEvType, &gotPublished, &gotDests)
	require.NoError(t, err)
	assert.Equal(t, domain.AggKnowledgeAsset, gotAggType)
	assert.Equal(t, domain.KEAssetCreated, gotEvType)
	assert.Nil(t, gotPublished, "freshly recorded event must be unpublished")
	assert.Equal(t, []string{outbox.KnowledgeEventsStream}, gotDests)
}

// TestStore_RecordRequiresTx: calling Record with a nil tx returns ErrNoTx and
// writes nothing — the outbox contract is "same transaction as the aggregate".
func TestStore_RecordRequiresTx(t *testing.T) {
	pool := newPool(t)
	resetOutbox(t, pool)
	store := outbox.NewStore()
	err := store.Record(context.Background(), nil, domain.KnowledgeEvent{}, []string{"knowledge_events"})
	require.Error(t, err)
}

// TestDispatcher_PollPublishesAndMarks: a recorded event is claimed by Poll,
// published to the fake publisher, and marked published_at. A second Poll finds
// nothing new.
func TestDispatcher_PollPublishesAndMarks(t *testing.T) {
	pool := newPool(t)
	resetOutbox(t, pool)
	ctx := context.Background()

	ws := uuid.New()
	ev := domain.KnowledgeEvent{
		EventID: uuid.New().String(), EventType: domain.KEAssetVersionRequested,
		EventVersion: 1, AggregateType: domain.AggKnowledgeAsset, AggregateID: uuid.New(),
		WorkspaceID: &ws, Actor: domain.EventActor{Type: domain.SubjectUser, ID: uuid.New()},
		OccurredAt: time.Now().UTC(),
	}
	recordEvent(t, pool, ev, []string{outbox.KnowledgeEventsStream})

	pub := newFakePublisher()
	d := outbox.NewDispatcher(pool, map[string]outbox.StreamPublisher{
		outbox.KnowledgeEventsStream: pub,
	}, 10, time.Second)
	require.NoError(t, d.Poll(ctx))

	// publisher received exactly one payload on the knowledge_events stream.
	assert.Equal(t, 1, len(pub.published[outbox.KnowledgeEventsStream]), "event must be published")

	// event is now marked published.
	var publishedAt *time.Time
	err := pool.QueryRow(ctx, `SELECT published_at FROM outbox_events WHERE aggregate_id = $1`, ev.AggregateID).Scan(&publishedAt)
	require.NoError(t, err)
	require.NotNil(t, publishedAt, "published_at must be set after successful delivery")

	// outbox_deliveries recorded the successful delivery.
	var deliveredAt *time.Time
	err = pool.QueryRow(ctx, `SELECT delivered_at FROM outbox_deliveries WHERE outbox_event_id = (SELECT id FROM outbox_events WHERE aggregate_id = $1) AND stream = $2`,
		ev.AggregateID, outbox.KnowledgeEventsStream).Scan(&deliveredAt)
	require.NoError(t, err)
	require.NotNil(t, deliveredAt, "delivery row must record delivered_at")

	// second poll finds nothing.
	pub2 := newFakePublisher()
	d2 := outbox.NewDispatcher(pool, map[string]outbox.StreamPublisher{
		outbox.KnowledgeEventsStream: pub2,
	}, 10, time.Second)
	require.NoError(t, d2.Poll(ctx))
	assert.Equal(t, 0, len(pub2.published[outbox.KnowledgeEventsStream]), "already-published event must not be re-sent")
}

// TestDispatcher_PollUnmappedStreamKeepsUnpublished: an event whose destination
// has no registered publisher stays unpublished with a delivery error row —
// Phase 0 resilience (§5.3): missing consumer is not fatal, event is not lost.
func TestDispatcher_PollUnmappedStreamKeepsUnpublished(t *testing.T) {
	pool := newPool(t)
	resetOutbox(t, pool)
	ctx := context.Background()

	ev := domain.KnowledgeEvent{
		EventID: uuid.New().String(), EventType: domain.KEAgentCreated,
		EventVersion: 1, AggregateType: domain.AggAgent, AggregateID: uuid.New(),
		Actor:      domain.EventActor{Type: domain.SubjectUser, ID: uuid.New()},
		OccurredAt: time.Now().UTC(),
	}
	recordEvent(t, pool, ev, []string{"some_other_stream"})

	// dispatcher has a publisher for knowledge_events, NOT some_other_stream.
	d := outbox.NewDispatcher(pool, map[string]outbox.StreamPublisher{
		outbox.KnowledgeEventsStream: newFakePublisher(),
	}, 10, time.Second)
	require.NoError(t, d.Poll(ctx))

	var publishedAt *time.Time
	err := pool.QueryRow(ctx, `SELECT published_at FROM outbox_events WHERE aggregate_id = $1`, ev.AggregateID).Scan(&publishedAt)
	require.NoError(t, err)
	assert.Nil(t, publishedAt, "event with an unmapped stream must stay unpublished")

	var lastErr *string
	err = pool.QueryRow(ctx, `SELECT last_error FROM outbox_deliveries WHERE stream = $1 AND outbox_event_id = (SELECT id FROM outbox_events WHERE aggregate_id = $2)`,
		"some_other_stream", ev.AggregateID).Scan(&lastErr)
	require.NoError(t, err)
	require.NotNil(t, lastErr, "unmapped stream must record a delivery error")
	assert.Contains(t, *lastErr, "no publisher")
}

// TestDispatcher_PollFailedPublishRetries: when the publisher returns an error,
// the event stays unpublished, a delivery error is recorded, and a subsequent
// Poll with a working publisher succeeds (retry path).
func TestDispatcher_PollFailedPublishRetries(t *testing.T) {
	pool := newPool(t)
	resetOutbox(t, pool)
	ctx := context.Background()

	ev := domain.KnowledgeEvent{
		EventID: uuid.New().String(), EventType: domain.KEAssetDeprecated,
		EventVersion: 1, AggregateType: domain.AggKnowledgeAsset, AggregateID: uuid.New(),
		Actor:      domain.EventActor{Type: domain.SubjectUser, ID: uuid.New()},
		OccurredAt: time.Now().UTC(),
	}
	recordEvent(t, pool, ev, []string{outbox.KnowledgeEventsStream})

	// first poll: publisher fails.
	bad := newFakePublisher()
	bad.err = errBoom{}
	d := outbox.NewDispatcher(pool, map[string]outbox.StreamPublisher{
		outbox.KnowledgeEventsStream: bad,
	}, 10, time.Second)
	require.NoError(t, d.Poll(ctx))

	var publishedAt *time.Time
	err := pool.QueryRow(ctx, `SELECT published_at FROM outbox_events WHERE aggregate_id = $1`, ev.AggregateID).Scan(&publishedAt)
	require.NoError(t, err)
	assert.Nil(t, publishedAt, "failed publish must leave event unpublished")

	// second poll: working publisher succeeds.
	good := newFakePublisher()
	d2 := outbox.NewDispatcher(pool, map[string]outbox.StreamPublisher{
		outbox.KnowledgeEventsStream: good,
	}, 10, time.Second)
	require.NoError(t, d2.Poll(ctx))
	assert.Equal(t, 1, len(good.published[outbox.KnowledgeEventsStream]), "retry poll must publish the event")

	err = pool.QueryRow(ctx, `SELECT published_at FROM outbox_events WHERE aggregate_id = $1`, ev.AggregateID).Scan(&publishedAt)
	require.NoError(t, err)
	require.NotNil(t, publishedAt, "event must be marked published after retry")
}

// --- helpers ---

type errBoom struct{}

func (errBoom) Error() string { return "publish boom" }

// fakePublisher (local copy to avoid importing the internal test stub across
// the build-tag boundary) records published payloads per stream.
type fakePublisher struct {
	published map[string][][]byte
	err       error
}

func newFakePublisher() *fakePublisher { return &fakePublisher{published: map[string][][]byte{}} }

func (f *fakePublisher) Publish(_ context.Context, stream string, payload []byte) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.published[stream] = append(f.published[stream], payload)
	return "fake-id", nil
}
