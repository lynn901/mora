// Command mcp-server runs the Wiki MCP Server (design doc 06). It supports two
// transports (HTTP/SSE default, stdio P2) and two backends:
//
//   - Mock (--mock or MCP_USE_MOCK=1): in-memory WikiClient + token/session/
//     audit/rate-limit stores seeded with sample data. Lets the MCP module run
//     end-to-end before YS-6 (Wiki backend) / YS-8 (RAG) are integrated — the
//     "mock 先行" strategy from the YS-4 dependency plan.
//   - Production: real HTTP WikiClient + PostgreSQL + Valkey stores, used once
//     the upstream services and infra are available.
//
// Usage:
//
//	MCP_USE_MOCK=1 ./mcp-server                 # HTTP/SSE on :8081, mock data
//	./mcp-server --transport stdio --api-token wki_...   # stdio, fixed token
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/wiki/wiki-backend/internal/module/mcp/audit"
	"github.com/wiki/wiki-backend/internal/module/mcp/auth"
	"github.com/wiki/wiki-backend/internal/module/mcp/resource"
	"github.com/wiki/wiki-backend/internal/module/mcp/server"
	"github.com/wiki/wiki-backend/internal/module/mcp/tool"
	"github.com/wiki/wiki-backend/internal/module/mcp/wikiclient"
	"github.com/wiki/wiki-backend/internal/platform/config"
	"github.com/wiki/wiki-backend/internal/platform/rbac"
)

func main() {
	transport := flag.String("transport", "", "transport: http (default) or stdio")
	apiToken := flag.String("api-token", "", "stdio: API token plaintext (or MCP_API_TOKEN)")
	useMock := flag.Bool("mock", false, "use in-memory mock WikiClient + stores (standalone dev)")
	flag.Parse()

	cfg := config.FromEnv()
	if *transport != "" {
		cfg.Transport = *transport
	}
	if *apiToken == "" {
		*apiToken = os.Getenv("MCP_API_TOKEN")
	}
	if os.Getenv("MCP_USE_MOCK") == "1" {
		*useMock = true
	}
	if cfg.Transport == "" {
		cfg.Transport = "http"
	}

	var (
		wikiClient wikiclient.WikiClient
		tokenStore auth.TokenStore
		sessions   server.SessionStore
		auditStore audit.Store
		limiter    auth.RateLimiter
		stdioToken *auth.TokenRecord // resolved token for stdio mode
	)

	if *useMock || (cfg.PostgresDSN == "" && cfg.ValkeyURL == "") {
		wikiClient, tokenStore, stdioToken = buildMock()
		sessions = server.NewMemorySessionStore()
		auditStore = audit.NewMemoryStore()
		limiter = auth.NewMemoryRateLimiter()
		log.Printf("MCP server starting in MOCK mode (transport=%s)", cfg.Transport)
	} else {
		// Production wiring: real HTTP Wiki client + PG + Valkey.
		wikiClient = wikiclient.NewHTTPClient(cfg.WikiAPIURL, cfg.InternalToken)

		pool, err := pgxpool.New(context.Background(), cfg.PostgresDSN)
		if err != nil {
			log.Fatalf("postgres connect: %v", err)
		}
		tokenStore = auth.NewPostgresTokenStore(pool)
		sessions = server.NewPostgresSessionStore(pool)
		auditStore = audit.NewPostgresStore(pool)

		rdb := redis.NewClient(&redis.Options{Addr: cfg.ValkeyURL})
		limiter = auth.NewValkeyRateLimiter(rdb)
		log.Printf("MCP server starting in PROD mode (transport=%s, wiki_api=%s)", cfg.Transport, cfg.WikiAPIURL)
	}

	// Build tools + resources.
	resReg := resource.NewRegistry(wikiClient)
	srv := server.NewServer(resReg, sessions, auditStore, limiter, cfg.ServerName, cfg.ServerVersion,
		server.WithRateLimits(cfg.RateLimitRead, cfg.RateLimitWrite),
		server.WithProtocolVersion(cfg.ProtocolVersion),
	)
	srv.RegisterTool(tool.NewSearchTool(wikiClient))
	srv.RegisterTool(tool.NewGetDocumentTool(wikiClient))
	srv.RegisterTool(tool.NewListDocumentsTool(wikiClient))
	srv.RegisterTool(tool.NewGetTagsTool(wikiClient))
	srv.RegisterTool(tool.NewCreateDraftTool(wikiClient))
	srv.RegisterTool(tool.NewUpdateDocumentTool(wikiClient))

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if cfg.Transport == "stdio" {
		// stdio: resolve the fixed token once, then serve newline-delimited JSON-RPC.
		rec := stdioToken
		if rec == nil && *apiToken != "" {
			hash := hashToken(*apiToken)
			r, err := tokenStore.Lookup(ctx, hash)
			if err != nil || r == nil || !r.IsValid(time.Now()) {
				log.Fatalf("stdio: token invalid or not found")
			}
			rec = r
		}
		in, out := server.DefaultStdioBindings()
		if err := srv.RunStdio(ctx, rec, in, out); err != nil {
			log.Fatalf("stdio: %v", err)
		}
		return
	}

	// HTTP/SSE transport.
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// Public routes: health + metrics (no token required).
	srv.PublicRoutes(r)
	// Protected routes: all /mcp protocol + admin endpoints require a token.
	authed := r.Group("/")
	authed.Use(auth.AuthMiddleware(tokenStore))
	srv.HTTPTransport(authed, auditStore)

	log.Printf("MCP HTTP/SSE listening on %s", cfg.HTTPAddr)
	if err := r.Run(cfg.HTTPAddr); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

// hashToken mirrors auth.HashToken without importing the private helper.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// buildMock seeds an in-memory WikiClient + token store with sample data and a
// dev token, returning the resolved token record for stdio mode.
func buildMock() (wikiclient.WikiClient, auth.TokenStore, *auth.TokenRecord) {
	mock := wikiclient.NewMock()
	const wsEng = "ws-eng-0001"
	const wsSales = "ws-sales-0001"
	const dirRoot = "dir-eng-root-0001"

	mock.AddWorkspace(wikiclient.Workspace{ID: wsEng, Name: "工程团队", Slug: "eng", OwnerID: "user-1"})
	mock.AddWorkspace(wikiclient.Workspace{ID: wsSales, Name: "销售团队", Slug: "sales", OwnerID: "user-2"})
	mock.AddDirectory(wikiclient.DirectoryNode{ID: dirRoot, Name: "工程文档", Path: "", SortOrder: 1}, wsEng)
	mock.AddDocument(wikiclient.DocumentMeta{
		ID: "doc-api-0001", WorkspaceID: wsEng, DirectoryID: dirRoot, Title: "API 设计规范",
		Status: "published", IndexStatus: "indexed", VersionNo: 5, Tags: []string{"api"},
		CreatedBy: "user-1", UpdatedAt: "2026-07-29T08:00:00Z",
	},
		"# API 设计规范\n\n分页采用 page/page_size 参数，响应包含 total/page/page_size。\n\nRESTful 资源命名使用复数名词。",
		"markdown",
		[]wikiclient.VersionSummary{
			{VersionNo: 5, DiffSummary: "补充分页说明", AuthorID: "user-1", CreatedAt: "2026-07-29T08:00:00Z"},
			{VersionNo: 4, DiffSummary: "初始版本", AuthorID: "user-1", CreatedAt: "2026-07-20T08:00:00Z"},
		},
	)
	mock.AddDocument(wikiclient.DocumentMeta{
		ID: "doc-onboarding-0002", WorkspaceID: wsEng, DirectoryID: "", Title: "新人入职指南",
		Status: "published", IndexStatus: "indexed", VersionNo: 2, Tags: []string{"guide"},
		CreatedBy: "user-1", UpdatedAt: "2026-07-25T08:00:00Z",
	},
		"# 新人入职指南\n\n欢迎加入工程团队。请先配置开发环境。",
		"markdown",
		[]wikiclient.VersionSummary{{VersionNo: 2, DiffSummary: "更新环境清单", AuthorID: "user-1", CreatedAt: "2026-07-25T08:00:00Z"}},
	)
	mock.AddTags(wsEng, []wikiclient.Tag{{ID: "tag-api", Name: "api"}, {ID: "tag-guide", Name: "guide"}})

	// ACL: user-1 has read+write on eng, read only on sales.
	mock.GrantWrite("user-1", wsEng)
	mock.GrantRead("user-1", wsSales)

	// Dev token bound to user-1 with readwrite scope.
	const devToken = "wki_dev_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
	tokenStore := auth.NewMemoryTokenStore()
	rec := &auth.TokenRecord{
		ID:           "tok-dev-0001",
		Name:         "dev-token",
		Prefix:       "wki_dev_a1b2",
		IdentityType: rbac.IdentityUser,
		IdentityID:   "user-1",
		IdentityName: "Dev User",
		Scope:        rbac.ScopeReadWrite,
		CreatedAt:    time.Now().UTC(),
	}
	tokenStore.Add(hashToken(devToken), rec)
	// Also add a readonly token for testing scope enforcement.
	const roToken = "wki_ro_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"
	roRec := &auth.TokenRecord{
		ID:           "tok-ro-0002",
		Name:         "readonly-token",
		Prefix:       "wki_ro_a1b2",
		IdentityType: rbac.IdentityUser,
		IdentityID:   "user-1",
		IdentityName: "Dev User",
		Scope:        rbac.ScopeReadOnly,
		CreatedAt:    time.Now().UTC(),
	}
	tokenStore.Add(hashToken(roToken), roRec)

	fmt.Fprintln(os.Stderr, "=== MCP Mock Mode ===")
	fmt.Fprintln(os.Stderr, "Dev token (readwrite):", devToken)
	fmt.Fprintln(os.Stderr, "Readonly token:       ", roToken)
	fmt.Fprintln(os.Stderr, "======================")
	return mock, tokenStore, rec
}
