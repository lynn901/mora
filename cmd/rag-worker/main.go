// Command rag-worker runs the RAG indexing pipeline consumer (05 §2.3). It reads
// document events from Valkey Streams, drives the pipeline (extract → chunk →
// embed → Qdrant → receipt) with idempotency/retry/dead-letter, and also runs a
// crash-recovery reclaim loop for idle messages. Configuration is env-based,
// matching deployments/docker-compose.yml.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/mq"
	"github.com/lynn901/mora/internal/infra/objstore"
	"github.com/lynn901/mora/internal/infra/pg"
	"github.com/lynn901/mora/internal/infra/qdrant"
	"github.com/lynn901/mora/internal/infra/ragwiring"
	"github.com/lynn901/mora/internal/module/rag/parser"
	"github.com/lynn901/mora/internal/module/rag/pipeline"
	"github.com/lynn901/mora/internal/module/rag/worker"
	"github.com/redis/go-redis/v9"
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

	// Configure the Qdrant collection-name prefix before indexing resolves a
	// collection. Must match mora-api (search path). Defaults to "mora_chunks_";
	// RAG_COLLECTION_PREFIX overrides it.
	domain.SetCollectionPrefix(env("RAG_COLLECTION_PREFIX", "mora_chunks_"))

	// --- Postgres ---
	pool, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://mora:mora@postgres:5432/mora?sslmode=disable"))
	if err != nil {
		log.Fatalf("pg connect: %v", err)
	}
	defer pool.Close()

	// --- Valkey (Redis-compatible) ---
	rdb := redis.NewClient(&redis.Options{Addr: env("VALKEY_URL", "valkey:6379")})
	defer rdb.Close()

	// --- RAG ports ---
	models := pg.NewModelStore(pool)
	docs := pg.NewDocumentStore(pool)
	rbac := pg.NewRBACResolver(pool)
	vectors := qdrant.New(env("QDRANT_URL", "http://qdrant:6333"))
	status := pg.NewIndexStatusStore(pool)
	queue := mq.New(rdb)
	idem := &ragwiring.ValkeyIdempotencyStore{Rdb: rdb}
	factory := &ragwiring.DefaultProviderFactory{
		TEIURL:      env("TEI_URL", "http://tei:8080"),
		OllamaURL:   env("OLLAMA_URL", "http://ollama:11434"),
		RerankModel: env("RERANKER_MODEL", ""),
	}

	// --- multi-format parsing (10 §4) ---
	// Object storage: MinIO/S3-compatible. When not configured (no endpoint),
	// parse events surface ErrNotConfigured and dead-letter; the existing
	// document.create pipeline is unaffected.
	objStore := objstore.New(
		env("MINIO_ENDPOINT", "minio:9000"),
		env("MINIO_ACCESS_KEY", ""),
		env("MINIO_SECRET_KEY", ""),
		env("MINIO_BUCKET", "mora"),
		env("MINIO_REGION", "us-east-1"),
	)
	parseStore := pg.NewParseTaskStore(pool)
	// parser registry: pure-Go parsers (P0/P1); the sidecar (P2 multimodal) is
	// registered first so multimodal opts route to it when configured.
	registry := parser.DefaultRegistry()
	if sidecarURL := env("MORA_PARSER_URL", ""); sidecarURL != "" {
		registry = parser.NewRegistry()
		registry.Register(parser.NewSidecarParser(sidecarURL))
		for _, p := range []parser.Parser{
			parser.MarkdownParser{}, parser.HTMLParser{}, parser.JSONParser{},
			parser.CSVParser{}, parser.PDFParser{}, parser.DocxParser{},
			parser.XlsxParser{}, parser.PptxParser{}, parser.EpubParser{},
			parser.MhtmlParser{}, parser.TextParser{},
		} {
			registry.Register(p)
		}
	}

	pipe := pipeline.New(pipeline.Pipeline{
		Cfg:  pipeline.DefaultConfig(),
		Docs: docs, RBAC: rbac, Vectors: vectors, Models: models, Factory: factory, Status: status,
		Parser:     parser.TextParser{}, // fallback; registry.Lookup is preferred in handleParse
		Registry:   registry,
		Objects:    objStore,
		ParseStore: parseStore,
		// Wire the pipeline logger so skip/index/error diagnostics are visible.
		// Without this, pipeline.New defaults Logf to a no-op and every skip
		// (e.g. a draft document) or swallowed error becomes invisible — the
		// "silent stuck" failure mode of DEFECT-06.
		Logf: func(f string, a ...any) { log.Printf(f, a...) },
	})

	w := worker.New(worker.Worker{
		Queue: queue, Idem: idem, Status: status, Pipeline: pipe,
		Consumer: env("CONSUMER_NAME", "rag-worker-1"),
		Cfg:      pipeline.DefaultConfig(),
		Logf:     func(f string, a ...any) { log.Printf(f, a...) },
	})

	// crash-recovery reclaim loop (steal idle > 60s messages)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.Reclaim(ctx, 60*time.Second); err != nil && ctx.Err() == nil {
					log.Printf("reclaim error: %v", err)
				}
			}
		}
	}()

	// compensation scanner: re-publish pending/stale tasks
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runCompensation(ctx, pool, queue)
			}
		}
	}()

	log.Printf("rag-worker starting (consumer=%s)", env("CONSUMER_NAME", "rag-worker-1"))

	// Health probe: the worker has no public HTTP port, but docker compose needs
	// a healthcheck to gate dependents on service_healthy. This lightweight
	// /healthz server (default :8082) returns 200 while the process is alive.
	go startHealthServer(ctx, env("HEALTH_ADDR", ":8082"))

	if err := w.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("rag-worker exited: %v", err)
	}
	_ = domain.EventDocumentCreate // keep domain import
}

// startHealthServer serves /healthz until ctx is cancelled. It only reports
// process liveness (the worker loop owns its own retry/dead-letter handling).
func startHealthServer(ctx context.Context, addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		log.Printf("rag-worker health listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health server: %v", err)
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// runCompensation re-publishes events for stale pending/failed tasks so they are
// not lost on a crash before stream ACK (05 §2.5).
func runCompensation(ctx context.Context, pool *pgxpool.Pool, queue *mq.ValkeyQueue) {
	store := pg.NewIndexStatusStore(pool)
	tasks, err := store.PendingTasks(ctx, time.Now().Add(-5*time.Minute), 100)
	if err != nil {
		log.Printf("compensation scan error: %v", err)
		return
	}
	for _, t := range tasks {
		ev := domain.DocEvent{
			EventID:    t.EventID,
			EventType:  t.EventType,
			DocumentID: t.DocumentID,
			Timestamp:  time.Now().UTC().Format(time.RFC3339),
		}
		if _, err := queue.Publish(ctx, ev); err != nil {
			log.Printf("compensation re-publish %s: %v", t.EventID, err)
		}
	}
}
