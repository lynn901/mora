package main

// Package main is the Wiki API service entrypoint. It wires the modular
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
	"github.com/wiki/wiki-backend/internal/domain"
	wh "github.com/wiki/wiki-backend/internal/module/wiki/handler"
	"github.com/wiki/wiki-backend/internal/module/wiki/service"
	"github.com/wiki/wiki-backend/internal/platform/auth"
	"github.com/wiki/wiki-backend/internal/platform/config"
	"github.com/wiki/wiki-backend/internal/platform/ratelimit"
	"github.com/wiki/wiki-backend/internal/infra/postgres"
	"github.com/wiki/wiki-backend/internal/module/wiki/collab"
	"github.com/wiki/wiki-backend/internal/module/wiki/event"
	auditpkg "github.com/wiki/wiki-backend/internal/platform/audit"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

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
	commentRepo := postgres.NewCommentRepo(db)
	auditRepo := postgres.NewAuditRepo(db)
	searchExec := postgres.NewSearchExec(db)
	auditLogger := auditpkg.NewLogger(auditRepo)

	// RBAC engine backed by the permission repo (which also implements the
	// engine's Repository: GrantsFor, DirectoryAncestors, DocumentLocation).
	engine := newRBACEngine(permRepo, dirRepo)

	// Event publisher: Redis if configured, else noop.
	var pub service.EventPublisher = event.NewNoopPublisher()

	docSvc := service.NewDocumentService(docRepo, verRepo, engine, pub)

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
	rbacH := wh.NewRBACHandler(permRepo)

	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(wh.CORSMiddleware())

	api := r.Group("/api/v1")
	// public
	api.POST("/auth/login", authH.Login)

	// authenticated
	authed := api.Group("")
	authed.Use(wh.AuthMiddleware(tm))
	authed.Use(wh.AuditMiddleware(auditLogger))
	docLimit := ratelimit.New(cfg.RateLimitDocPerMin)
	searchLimit := ratelimit.New(cfg.RateLimitSearchPerMin)

	authed.GET("/workspaces", wsH.List)
	authed.POST("/workspaces", wsH.Create)
	authed.GET("/workspaces/:id", wsH.Get)

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

	searchGroup := authed.Group("")
	searchGroup.Use(wh.RateLimitMiddleware(searchLimit))
	searchGroup.GET("/search", searchH.Search)

	authed.GET("/documents/:id/comments", commentH.List)
	authed.POST("/documents/:id/comments", commentH.Create)
	authed.POST("/comments/:id/resolve", commentH.Resolve)

	authed.GET("/permissions", rbacH.List)
	authed.POST("/permissions", rbacH.Grant)
	authed.DELETE("/permissions/:id", rbacH.Revoke)

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
		log.Printf("wiki-api listening on %s", cfg.HTTPAddr)
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
