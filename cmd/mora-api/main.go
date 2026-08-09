package main

// Package main is the Mora API service entrypoint. It wires the modular
// monolith: config → pgx pool → repositories → services → handlers → router.
// Per architecture §2.1, this is a modular monolith; MCP Server and RAG Worker
// have separate cmd entrypoints (out of YS-6 scope).

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/mq"
	"github.com/lynn901/mora/internal/infra/objstore"
	"github.com/lynn901/mora/internal/infra/pg"
	"github.com/lynn901/mora/internal/infra/postgres"
	"github.com/lynn901/mora/internal/infra/qdrant"
	"github.com/lynn901/mora/internal/infra/ragwiring"
	"github.com/lynn901/mora/internal/module/mora/collab"
	"github.com/lynn901/mora/internal/module/mora/event"
	wh "github.com/lynn901/mora/internal/module/mora/handler"
	"github.com/lynn901/mora/internal/module/mora/parsewiring"
	"github.com/lynn901/mora/internal/module/mora/service"
	ragsearch "github.com/lynn901/mora/internal/module/rag/search"
	"github.com/lynn901/mora/internal/pkg/response"
	auditpkg "github.com/lynn901/mora/internal/platform/audit"
	"github.com/lynn901/mora/internal/platform/auth"
	"github.com/lynn901/mora/internal/platform/config"
	"github.com/lynn901/mora/internal/platform/ratelimit"
	"github.com/redis/go-redis/v9"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Configure the Qdrant collection-name prefix before any search/index path
	// resolves a collection. Defaults to "mora_chunks_"; RAG_COLLECTION_PREFIX
	// overrides it.
	domain.SetCollectionPrefix(cfg.QdrantCollectionPrefix)

	pool, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("db pool: %v", err)
	}
	defer pool.Close()

	db := postgres.NewDB(pool)
	wsRepo := postgres.NewWorkspaceRepo(db)
	dirRepo := postgres.NewDirectoryRepo(db)
	docRepo := postgres.NewDocumentRepo(db)
	verRepo := postgres.NewVersionRepo(db)
	permRepo := postgres.NewPermissionRepo(db)
	userRepo := postgres.NewUserRepo(db)
	roleRepo := postgres.NewRoleRepo(db)
	commentRepo := postgres.NewCommentRepo(db)
	auditRepo := postgres.NewAuditRepo(db)
	searchExec := postgres.NewSearchExec(db)
	auditLogger := auditpkg.NewLogger(auditRepo)
	tagRepo := postgres.NewTagRepo(db)

	// RBAC engine backed by the permission repo (which also implements the
	// engine's Repository: GrantsFor, DirectoryAncestors, DocumentLocation).
	engine := newRBACEngine(permRepo, dirRepo, docRepo)

	// Event publisher: Valkey Streams (real) when configured, else noop. The
	// QueuePublisher maps service.DocumentEvent → domain.DocEvent and publishes
	// through mq.ValkeyQueue so the rag-worker consumes one canonical stream
	// format + event-type vocabulary (previously Noop never reached the worker).
	var pub service.EventPublisher = event.NewNoopPublisher()
	rdb := newRedisClient(cfg.ValkeyURL)
	if rdb != nil {
		defer rdb.Close()
		pub = event.NewQueuePublisher(mq.New(rdb))
	}

	docSvc := service.NewDocumentService(docRepo, verRepo, engine, pub)

	// RAG index-status + embedding-model stores (shared with rag-worker; the
	// mora-api exposes the admin/index-status HTTP routes the MCP + E2E expect).
	indexStatus := pg.NewIndexStatusStore(pool)
	modelStore := pg.NewModelStore(pool)
	providerFactory := &ragwiring.DefaultProviderFactory{TEIURL: cfg.TEIURL, OllamaURL: cfg.OllamaURL, RerankModel: cfg.RerankerModel}

	// Multi-format parsing (10 §4, §7): object storage + parse stores + the
	// parse service + handler. The handler routes are registered below; the
	// rag-worker owns the actual parse execution (it consumes document.parse).
	objStore := objstore.New(cfg.MinioEndpoint, cfg.MinioAccessKey, cfg.MinioSecretKey, cfg.MinioBucket, cfg.MinioRegion)
	objStore.Secure = cfg.MinioSecure
	// Ensure the bucket exists so uploads don't fail on a fresh MinIO. Best-effort:
	// if MinIO is unreachable (e.g. dev without object storage), the API still
	// serves non-parse routes; uploads will surface a clear error.
	if cfg.MinioAccessKey != "" {
		if err := objStore.EnsureBucket(context.Background()); err != nil {
			log.Printf("warn: ensure minio bucket %s: %v (parse uploads will fail until fixed)", cfg.MinioBucket, err)
		}
	}
	parseTaskStore := pg.NewParseTaskStore(pool)
	parseConfigStore := pg.NewParseConfigStore(pool)
	// parse queue: reuse the ValkeyQueue (same stream the rag-worker consumes).
	var parseQueue service.ParseTaskQueue
	if rdb != nil {
		parseQueue = parsewiring.QueueAdapter{Queue: mq.New(rdb)}
	} else {
		parseQueue = &noopParseQueue{}
	}
	parseSvc := service.NewParseService(
		docRepo, engine, parseQueue,
		parsewiring.ObjectStoreAdapter{Store: objStore},
		parsewiring.ProgressAdapter{Store: parseTaskStore},
		parsewiring.ChunkPreviewerImpl{},
		parseConfigStoreAdapter{store: parseConfigStore},
		cfg.ParseMaxFileMB,
	)
	parseH := wh.NewParseHandler(parseSvc)

	tm := auth.NewTokenManager(cfg.JWTSecret, cfg.JWTTTL)
	userLookup := &pgUserLookup{db: db}
	collabHub := collab.NewHub(cfg.CollabMaxConcurrent)

	// handlers
	authH := wh.NewAuthHandler(userLookup, tm)
	wsH := wh.NewWorkspaceHandler(wsRepo)
	dirH := wh.NewDirectoryHandler(dirRepo)
	docH := wh.NewDocumentHandler(docSvc)
	searchH := wh.NewSearchHandler(engine, searchExec, cfg.FTSConfig)
	commentH := wh.NewCommentHandler(commentRepo)
	permSvc := service.NewPermissionService(permRepo, docRepo, pub)
	rbacH := wh.NewRBACHandler(permSvc, engine)
	tagH := wh.NewTagHandler(tagRepo)
	indexStatusH := wh.NewIndexStatusHandler(indexStatus, docSvc)
	modelH := wh.NewEmbeddingModelHandler(modelStore, providerFactory, pub)
	userH := wh.NewUserHandler(userRepo)
	roleH := wh.NewRoleHandler(roleRepo)

	// RAG hybrid search (Dense+BM25+rerank) — mounts POST /api/v1/rag/search,
	// the endpoint the MCP search_knowledge_base tool calls. The searcher reuses
	// the same RAG ports the rag-worker indexes through (Qdrant, PG FTS, TEI) so
	// search and indexing stay consistent. RBAC is enforced as a hard filter on
	// both paths using the caller's ViewerScope.
	ragSearcher := ragsearch.New(ragsearch.HybridSearcher{
		Models:  modelStore,
		Factory: providerFactory,
		Vectors: qdrant.New(cfg.QdrantURL),
		FTS:     &pg.FTSStore{Pool: pool, Config: cfg.FTSConfig},
		RBAC:    pg.NewRBACResolver(pool),
	})

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(wh.CORSMiddleware())

	api := r.Group("/api/v1")
	// public
	api.POST("/auth/login", authH.Login)

	// authenticated
	authed := api.Group("")
	authed.Use(wh.AuthMiddleware(tm, cfg.InternalToken))
	authed.Use(wh.AuditMiddleware(auditLogger))
	docLimit := ratelimit.New(cfg.RateLimitDocPerMin)
	searchLimit := ratelimit.New(cfg.RateLimitSearchPerMin)

	authed.GET("/workspaces", wsH.List)
	authed.POST("/workspaces", wsH.Create)
	authed.GET("/workspaces/:workspace_id", wsH.Get)
	authed.GET("/workspaces/:workspace_id/tags", tagH.List)

	authed.GET("/workspaces/:workspace_id/directories", dirH.ListTree)
	authed.POST("/workspaces/:workspace_id/directories", dirH.Create)
	authed.DELETE("/directories/:id", dirH.Delete)

	docGroup := authed.Group("")
	docGroup.Use(wh.RateLimitMiddleware(docLimit))
	docGroup.GET("/workspaces/:workspace_id/documents", docH.List)
	docGroup.POST("/workspaces/:workspace_id/documents", docH.Create)
	docGroup.GET("/documents/:id", docH.Get)
	docGroup.PATCH("/documents/:id", docH.Update)
	docGroup.DELETE("/documents/:id", docH.Delete)
	docGroup.GET("/documents/:id/versions", docH.ListVersions)
	docGroup.GET("/documents/:id/versions/diff", docH.DiffVersions)
	docGroup.POST("/documents/:id/versions/:version_no/rollback", docH.Rollback)
	docGroup.GET("/documents/:id/index-status", indexStatusH.Get)

	// Multi-format document parsing (10 §7.2). Upload/reparse use the doc rate
	// limit; chunk-preview a lighter one; parse-progress + configs are reads.
	docGroup.POST("/workspaces/:workspace_id/documents/upload", parseH.Upload)
	docGroup.POST("/workspaces/:workspace_id/documents/reparse", parseH.Reparse)
	docGroup.GET("/documents/:id/parse-progress", parseH.ParseProgress)
	docGroup.GET("/workspaces/:workspace_id/parse-configs", parseH.ListConfigs)
	docGroup.POST("/workspaces/:workspace_id/parse-configs", parseH.CreateConfig)
	docGroup.PUT("/workspaces/:workspace_id/parse-configs/:cid", parseH.UpdateConfig)
	docGroup.DELETE("/workspaces/:workspace_id/parse-configs/:cid", parseH.DeleteConfig)
	previewLimit := ratelimit.New(120)
	previewGroup := authed.Group("")
	previewGroup.Use(wh.RateLimitMiddleware(previewLimit))
	previewGroup.POST("/rag/chunk-preview", parseH.ChunkPreview)

	searchGroup := authed.Group("")
	searchGroup.Use(wh.RateLimitMiddleware(searchLimit))
	searchGroup.GET("/search", searchH.Search)
	// RAG semantic hybrid search (consumed by MCP search_knowledge_base).
	searchGroup.POST("/rag/search", ragSearchHandler(ragSearcher))

	authed.GET("/documents/:id/comments", commentH.List)
	authed.POST("/documents/:id/comments", commentH.Create)
	authed.POST("/comments/:id/resolve", commentH.Resolve)

	authed.GET("/permissions", rbacH.List)
	authed.POST("/permissions", rbacH.Grant)
	authed.DELETE("/permissions/:id", rbacH.Revoke)
	authed.POST("/permissions/check", rbacH.Check)

	// Embedding-model admin (API 04 §9.2): list/upsert/test/rebuild.
	authed.GET("/admin/embedding-models", modelH.List)
	authed.POST("/admin/embedding-models", modelH.Upsert)
	authed.POST("/admin/embedding-models/:id/test", modelH.Test)
	authed.POST("/admin/embedding-models/:id/rebuild", modelH.Rebuild)

	// identity listing (04-api-contract §3.5): RBAC-scoped users + role dictionary.
	authed.GET("/users", userH.List)
	authed.GET("/roles", roleH.List)

	// health
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/ready", func(c *gin.Context) {
		if err := pool.Ping(c.Request.Context()); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not ready"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	})

	// collaboration WebSocket
	r.GET("/api/v1/ws/collab/:document_id", func(c *gin.Context) {
		// Minimal WS upgrade; full Yjs sync handled by yjs-server (Node.js).
		// This endpoint owns presence/cursor relay + admission control.
		serveCollab(c, collabHub, tm)
	})

	srv := &http.Server{Addr: cfg.HTTPAddr, Handler: r}
	go func() {
		log.Printf("mora-api listening on %s", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// pgUserLookup implements handler.UserLookup against the users table.
type pgUserLookup struct{ db *postgres.DB }

func (u *pgUserLookup) Authenticate(ctx context.Context, email, password string) (*domain.User, []domain.UUID, error) {
	user := &domain.User{}
	err := u.db.Pool.QueryRow(ctx,
		`SELECT id, email, name, password_hash, status FROM users WHERE email=$1 AND status='active'`,
		email).Scan(&user.ID, &user.Email, &user.Name, &user.PasswordHash, &user.Status)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil, errInvalidCreds
		}
		return nil, nil, err
	}
	if !wh.CheckPassword(user.PasswordHash, password) {
		return nil, nil, errInvalidCreds
	}
	// load groups
	rows, err := u.db.Pool.Query(ctx, `SELECT group_id FROM group_members WHERE user_id=$1`, user.ID)
	if err != nil {
		return user, nil, nil
	}
	defer rows.Close()
	var groups []domain.UUID
	for rows.Next() {
		var g domain.UUID
		if err := rows.Scan(&g); err == nil {
			groups = append(groups, g)
		}
	}
	return user, groups, nil
}

var errInvalidCreds = errString("invalid credentials")

type errString string

func (e errString) Error() string { return string(e) }

// ragSearchRequest mirrors the MCP moraclient.SearchRequest body (04 §9).
type ragSearchRequest struct {
	Query       string   `json:"query"`
	WorkspaceID string   `json:"workspace_id"`
	DirectoryID string   `json:"directory_id"`
	Tags        []string `json:"tags"`
	TopK        int      `json:"top_k"`
	TopN        int      `json:"top_n"`
	Rerank      bool     `json:"rerank"`
}

// ragSearchHandler returns a Gin handler that runs RAG hybrid search as the
// authenticated caller. The MCP Server reaches this via the internal service
// token (X-Identity-Id carries the principal); JWT callers use their own id.
func ragSearchHandler(s *ragsearch.HybridSearcher) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req ragSearchRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			response.Fail(c, err)
			return
		}
		st := wh.MustAuth(c)
		res, err := s.Search(c.Request.Context(), ragsearch.SearchRequest{
			Query:       req.Query,
			UserID:      st.UserID.String(),
			IsAdmin:     st.IsAdmin,
			WorkspaceID: req.WorkspaceID,
			DirectoryID: req.DirectoryID,
			Tags:        req.Tags,
			TopK:        req.TopK,
			TopN:        req.TopN,
			Rerank:      req.Rerank,
		})
		if err != nil {
			response.Fail(c, err)
			return
		}
		response.OK(c, res)
	}
}

// newRedisClient builds a Redis/Valkey client from a URL that may be either a
// "redis://..." scheme URL or a bare "host:port" addr (the rag-worker/mcp-server
// use bare addrs). Returns nil when url is empty (Noop publisher path).
func newRedisClient(url string) *redis.Client {
	if url == "" {
		return nil
	}
	if opts, err := redis.ParseURL(url); err == nil {
		return redis.NewClient(opts)
	}
	return redis.NewClient(&redis.Options{Addr: url})
}

// parseConfigStoreAdapter bridges *pg.ParseConfigStore (concrete infra) to the
// service.ParseConfigStore port. The mora-api main owns this glue so the
// service layer never imports infra/pg directly (avoids a layering cycle).
type parseConfigStoreAdapter struct{ store *pg.ParseConfigStore }

func (a parseConfigStoreAdapter) List(ctx context.Context, workspaceID string) ([]service.ParseConfig, error) {
	cfgs, err := a.store.List(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	out := make([]service.ParseConfig, len(cfgs))
	for i, c := range cfgs {
		out[i] = service.ParseConfig{ID: c.ID, WorkspaceID: c.WorkspaceID, Name: c.Name, Config: c.Config, IsDefault: c.IsDefault}
	}
	return out, nil
}
func (a parseConfigStoreAdapter) Get(ctx context.Context, id string) (service.ParseConfig, error) {
	c, err := a.store.Get(ctx, id)
	if err != nil {
		return service.ParseConfig{}, err
	}
	return service.ParseConfig{ID: c.ID, WorkspaceID: c.WorkspaceID, Name: c.Name, Config: c.Config, IsDefault: c.IsDefault}, nil
}
func (a parseConfigStoreAdapter) Create(ctx context.Context, c service.ParseConfig) (service.ParseConfig, error) {
	out, err := a.store.Create(ctx, pg.ParseConfig{Name: c.Name, Config: c.Config, IsDefault: c.IsDefault, WorkspaceID: c.WorkspaceID})
	if err != nil {
		return service.ParseConfig{}, err
	}
	return service.ParseConfig{ID: out.ID, WorkspaceID: out.WorkspaceID, Name: out.Name, Config: out.Config, IsDefault: out.IsDefault}, nil
}
func (a parseConfigStoreAdapter) Update(ctx context.Context, id string, c service.ParseConfig) (service.ParseConfig, error) {
	out, err := a.store.Update(ctx, id, pg.ParseConfig{Name: c.Name, Config: c.Config, IsDefault: c.IsDefault})
	if err != nil {
		return service.ParseConfig{}, err
	}
	return service.ParseConfig{ID: out.ID, WorkspaceID: out.WorkspaceID, Name: out.Name, Config: out.Config, IsDefault: out.IsDefault}, nil
}
func (a parseConfigStoreAdapter) Delete(ctx context.Context, id string) error {
	return a.store.Delete(ctx, id)
}

// noopParseQueue is the fallback parse queue when no Valkey is configured
// (unit tests, single-node without RAG). It records events in-memory so the
// upload path still returns 202 without crashing.
type noopParseQueue struct{ events []service.ParseEvent }

func (q *noopParseQueue) PublishParse(ctx context.Context, ev service.ParseEvent) error {
	q.events = append(q.events, ev)
	return nil
}
