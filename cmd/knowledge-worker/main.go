// Command knowledge-worker runs the Phase 1 Source/Asset control-plane consumer
// (design-docs/14 §5, D12). It is the async counterpart to mora-api's write
// handlers: mora-api writes outbox_events inside the user's transaction; this
// process ships them to Valkey Streams (embedded outbox-dispatcher) and then
// consumes the two Streams that drive asset jobs:
//
//   - source_events   (consumer group source_sync)        → Connector ingest
//   - knowledge_events (consumer group knowledge_projection) → projection/activation
//
// It reuses Phase 0's worker.JobStore (idempotent create + lease) for the 5
// job_type dispatch table (§5.2) and the AssetRegistry for the CAS activation
// (§7). A reconcile ticker (§3.3) runs the consistency scan for each workspace.
//
// First version does NOT expose a business API (§5.1); only a /healthz probe
// for docker-compose gating. The outbox-dispatcher runs as an in-process
// goroutine (§2.3 — promoted to its own cmd only when load warrants).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/module/knowledge/worker"
	"github.com/lynn901/mora/internal/platform/outbox"
)

// Stream + consumer-group names (design-docs/14 §2.2, §5.1). The outbox
// dispatcher ships events to these Streams; this worker consumes them.
const (
	StreamSourceEvents     = "source_events"
	StreamKnowledgeEvents  = "knowledge_events"
	GroupSourceSync        = "source_sync"
	GroupKnowledgeProj     = "knowledge_projection"
	SourceEventsDeadStream = "source_events:dead"
	KnowEventsDeadStream   = "knowledge_events:dead"

	defaultLeaseTTL      = 5 * time.Minute // worker.DefaultLeaseTTL; mirrored to avoid an import cycle in main
	readGroupBlock       = 5 * time.Second
	readGroupCount int64 = 10
	reclaimMinIdle       = 60 * time.Second
	reconcileInterval    = 5 * time.Minute
	dispatcherInterval   = 2 * time.Second
	dispatcherBatch      = 50
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// --- Postgres ---
	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://mora:mora@postgres:5432/mora?sslmode=disable"))
	if err != nil {
		log.Fatalf("knowledge-worker: pg connect: %v", err)
	}
	defer pool.Close()

	// --- Valkey (Redis-compatible) ---
	rdb := redis.NewClient(&redis.Options{Addr: env("VALKEY_URL", "valkey:6379")})
	defer rdb.Close()

	// --- Ports ---
	jobStore := postgres.NewJobRepo(pool)
	// AssetRegistry (§6 CAS / §3.3 reconcile) lands in a later Phase 1 slice;
	// the projection_build / asset_activate / reconcile_scan handlers stub out
	// until then. Wire it here when the port gains MarkProjectionReady /
	// Activate / ReconcileScan.
	publisher := outbox.NewValkeyPublisher(rdb, 100000)
	// *pgxpool.Pool satisfies outbox.DB (BeginTx); pass the pool directly.
	dispatcher := outbox.NewDispatcher(pool, map[string]outbox.StreamPublisher{
		outbox.KnowledgeEventsStream: publisher, // "knowledge_events"
		StreamSourceEvents:           publisher,
	}, dispatcherBatch, dispatcherInterval)

	// Job handlers (§5.2 dispatch table). Each handler owns one job_type's
	// business logic; the dispatcher loop drives acquire→run→mark.
	handlers := worker.Handlers{
		worker.JobSourceSync:      &worker.SourceSyncHandler{Pool: pool, Jobs: jobStore},
		worker.JobProjectionBuild: &worker.ProjectionBuildHandler{Pool: pool, Jobs: jobStore},
		worker.JobAssetActivate:   &worker.AssetActivateHandler{},
		worker.JobReconcileScan:   &worker.ReconcileHandler{Pool: pool},
		worker.JobLegacyBackfill:  &worker.LegacyBackfillHandler{Pool: pool},
	}
	runner := worker.NewRunner(worker.RunnerConfig{
		Jobs:      jobStore,
		Handlers:  handlers,
		LeaseTTL:  defaultLeaseTTL,
		Owner:     env("CONSUMER_NAME", "knowledge-worker-1"),
		Logf:      func(f string, a ...any) { log.Printf(f, a...) },
	})

	// Embedded outbox dispatcher (§2.3 — first version in-process).
	go func() {
		if err := dispatcher.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("knowledge-worker: dispatcher exited: %v", err)
		}
	}()

	// Stream consumers: source_events → SourceSync jobs; knowledge_events →
	// ProjectionBuild / AssetActivate jobs. Each maps an event to a dedupe-keyed
	// job.Create (idempotent) and ACKs the message.
	go consumeStream(ctx, rdb, StreamSourceEvents, GroupSourceSync,
		env("CONSUMER_NAME", "knowledge-worker-1"), mapSourceEvent(jobStore))
	go consumeStream(ctx, rdb, StreamKnowledgeEvents, GroupKnowledgeProj,
		env("CONSUMER_NAME", "knowledge-worker-1"), mapKnowledgeEvent(jobStore))

	// Crash-recovery reclaim for idle stream messages (mirrors rag-worker).
	go reclaimLoop(ctx, rdb, StreamSourceEvents, GroupSourceSync, env("CONSUMER_NAME", "knowledge-worker-1"))
	go reclaimLoop(ctx, rdb, StreamKnowledgeEvents, GroupKnowledgeProj, env("CONSUMER_NAME", "knowledge-worker-1"))

	// Reconcile ticker (§3.3): run the consistency scan for each workspace.
	go reconcileLoop(ctx, pool)

	log.Printf("knowledge-worker starting (consumer=%s)", env("CONSUMER_NAME", "knowledge-worker-1"))
	go startHealthServer(ctx, env("HEALTH_ADDR", ":8083"))

	// The job runner owns the acquire/dispatch loop; it blocks until ctx cancels.
	if err := runner.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("knowledge-worker: runner exited: %v", err)
	}
}

// startHealthServer serves /healthz until ctx is cancelled (docker-compose gate).
func startHealthServer(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Printf("knowledge-worker health listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health server: %v", err)
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// consumeStream reads messages from a Valkey Stream consumer group and applies
// the mapper to turn each into an idempotent job.Create, then ACKs. Mapping is
// the only Stream-specific logic — the read/ACK loop is generic. The block
// timeout keeps the loop responsive to ctx cancellation.
func consumeStream(
	ctx context.Context,
	rdb *redis.Client,
	stream, group, consumer string,
	mapper func(ctx context.Context, msgID, payload string) error,
) {
	// Ensure the consumer group exists (no-op if already present).
	_ = rdb.XGroupCreateMkStream(ctx, stream, group, "$").Err()
	for {
		if ctx.Err() != nil {
			return
		}
		res, err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: group, Consumer: consumer, Streams: []string{stream, ">"},
			Count: readGroupCount, Block: readGroupBlock, NoAck: false,
		}).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			log.Printf("knowledge-worker: xreadgroup %s: %v", stream, err)
			time.Sleep(time.Second)
			continue
		}
		for _, batch := range res {
			for _, msg := range batch.Messages {
				payload, _ := msg.Values["payload"].(string)
				if err := mapper(ctx, msg.ID, payload); err != nil {
					// Mapping is idempotent (job.Create dedupes); a failure here is
					// a malformed message. Dead-letter and ACK so it is not retried.
					deadLetter(ctx, rdb, stream+":dead", msg.ID, payload, err.Error())
				}
				if err := rdb.XAck(ctx, stream, group, msg.ID).Err(); err != nil && ctx.Err() == nil {
					log.Printf("knowledge-worker: xack %s %s: %v", stream, msg.ID, err)
				}
			}
		}
	}
}

// reclaimLoop steals idle stream messages (crash recovery, mirrors rag-worker's
// XAUTOCLAIM path). A consumer that died mid-processing leaves an un-ACKed
// message; after reclaimMinIdle another worker re-reads and re-maps it.
func reclaimLoop(ctx context.Context, rdb *redis.Client, stream, group, consumer string) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			msgs, _, err := rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
				Stream: stream, Group: group, Consumer: consumer,
				MinIdle: reclaimMinIdle, Count: readGroupCount, Start: "0-0",
			}).Result()
			if err != nil && ctx.Err() == nil {
				log.Printf("knowledge-worker: xautoclaim %s: %v", stream, err)
			}
			// Re-ACK the reclaimed messages; the dedupe_key on any job they
			// created makes re-processing a no-op (idempotent).
			for _, m := range msgs {
				_ = rdb.XAck(ctx, stream, group, m.ID).Err()
			}
		}
	}
}

// deadLetter writes a message to a dead-letter stream for inspection. Best-effort.
func deadLetter(ctx context.Context, rdb *redis.Client, stream, originID, payload, reason string) {
	fields := map[string]any{
		"payload":  payload,
		"_origin":  originID,
		"_reason":  reason,
		"_dead_at": time.Now().UTC().Format(time.RFC3339),
	}
	if err := rdb.XAdd(ctx, &redis.XAddArgs{Stream: stream, MaxLen: 10000, Approx: true, Values: fields}).Err(); err != nil {
		log.Printf("knowledge-worker: dead-letter %s: %v", stream, err)
	}
}

// reconcileLoop runs the §3.3 consistency scan for every workspace on a ticker.
// It is the safety net for divergences the CAS/outbox path missed (e.g. a
// crashed activation leaving current_version_id unset).
//
// Phase 1-1: AssetRegistry.ReconcileScan lands with the §3.3 reconcile
// deliverable; until then the loop is a no-op ticker so the worker's process
// shape is complete and observable. When ReconcileScan lands, replace the body
// of runReconcile with the per-workspace scan + ReconcileReport surfacing.
func reconcileLoop(ctx context.Context, pool *pgxpool.Pool) {
	t := time.NewTicker(reconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runReconcile(ctx, pool)
		}
	}
}

func runReconcile(ctx context.Context, pool *pgxpool.Pool) {
	// TODO(§3.3): once AssetRegistry.ReconcileScan lands, list workspaces and
	// run the scan per workspace, surfacing VersionCASFixed / ProjectionsQueued
	// / ProjectionsStaled / NeedsHuman. Kept as a no-op until the port arrives.
	_ = ctx
	_ = pool
}

// --- Stream → Job mappers ---

// mapSourceEvent turns a source_events message into a source_sync job. The
// dedupe_key shape is `sync:{source_id}:{revision}` (§5.2). The payload is a
// KnowledgeEvent with source.sync_requested semantics (§4.2).
func mapSourceEvent(jobs worker.JobStore) func(ctx context.Context, msgID, payload string) error {
	return func(ctx context.Context, msgID, payload string) error {
		var ev domain.KnowledgeEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return err
		}
		sourceID, revision := sourceSyncTarget(&ev)
		if sourceID == uuid.Nil {
			return nil // not a sync event we handle
		}
		_, err := jobs.Create(ctx, nil, domain.Job{
			JobType:      worker.JobSourceSync,
			SourceID:      &sourceID,
			TargetKey:     revision,
			DedupeKey:     worker.DedupeKey(worker.JobSourceSync, sourceID.String(), revision),
			MaxAttempt:    3,
			SourceEventID: nil,
		})
		if err != nil && !errors.Is(err, worker.ErrJobExists) {
			return err
		}
		return nil
	}
}

// mapKnowledgeEvent turns a knowledge_events message into the matching job:
// asset.version.requested → projection_build; asset.version.activated is a
// no-op here (activation is the CAS, already applied at publish time).
func mapKnowledgeEvent(jobs worker.JobStore) func(ctx context.Context, msgID, payload string) error {
	return func(ctx context.Context, msgID, payload string) error {
		var ev domain.KnowledgeEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return err
		}
		switch ev.EventType {
		case domain.KEAssetVersionRequested:
			return enqueueProjectionJobs(ctx, jobs, &ev)
		default:
			return nil
		}
	}
}

// enqueueProjectionJobs creates a projection_build job per required projection
// kind for the version named in the event payload (§7 rag-worker bridge). Each
// is idempotent on `proj:{version}:{kind}:{build_revision}`.
func enqueueProjectionJobs(ctx context.Context, jobs worker.JobStore, ev *domain.KnowledgeEvent) error {
	vid, err := uuid.Parse(payloadString(ev.Payload, "version_id"))
	if err != nil {
		return nil
	}
	assetID, _ := uuid.Parse(payloadString(ev.Payload, "asset_id"))
	buildRevision := payloadString(ev.Payload, "build_revision")
	required := payloadProjections(ev.Payload)
	for _, kind := range required {
		_, err := jobs.Create(ctx, nil, domain.Job{
			JobType:        worker.JobProjectionBuild,
			AssetID:        &assetID,
			AssetVersionID: &vid,
			TargetKey:      kind,
			BuildRevision:  buildRevision,
			DedupeKey:      worker.DedupeKey(worker.JobProjectionBuild, vid.String(), kind, buildRevision),
			MaxAttempt:     5,
		})
		if err != nil && !errors.Is(err, worker.ErrJobExists) {
			return err
		}
	}
	return nil
}

// sourceSyncTarget extracts the (source_id, revision) from a source.sync
// event payload. revision is the requested revision or "latest" when absent.
func sourceSyncTarget(ev *domain.KnowledgeEvent) (uuid.UUID, string) {
	if ev == nil {
		return uuid.Nil, ""
	}
	if ev.EventType != "source.sync_requested" {
		return uuid.Nil, ""
	}
	sid, err := uuid.Parse(payloadString(ev.Payload, "source_id"))
	if err != nil {
		return uuid.Nil, ""
	}
	rev := payloadString(ev.Payload, "requested_revision")
	if rev == "" {
		rev = "latest"
	}
	return sid, rev
}

func payloadString(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	v, ok := p[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		b, _ := json.Marshal(s)
		return string(b)
	}
}

func payloadProjections(p map[string]any) []string {
	if p == nil {
		return nil
	}
	v, ok := p["required_projections"]
	if !ok {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
