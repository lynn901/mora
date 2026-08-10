//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// Config & suite bootstrap
// ---------------------------------------------------------------------------

type config struct {
	BaseURL       string // mora-api, e.g. http://localhost:8990
	MCPURL        string // mcp-server, e.g. http://localhost:8081
	DatabaseURL   string // for seeding non-admin users/tokens (RBAC tests)
	InternalToken string // INTERNAL_SERVICE_TOKEN (trusted internal caller)
	AdminEmail    string
	AdminPassword string
	DevToken      string        // seeded MCP dev token (readwrite, admin-bound)
	IndexTimeout  time.Duration // poll window for index_status -> indexed
}

func loadConfig() config {
	c := config{
		BaseURL:       envOr("E2E_BASE_URL", "http://localhost:8990"),
		MCPURL:        envOr("E2E_MCP_URL", "http://localhost:8081"),
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		InternalToken: envOr("INTERNAL_SERVICE_TOKEN", "mora-internal-token"),
		AdminEmail:    envOr("E2E_ADMIN_EMAIL", "admin@mora.local"),
		AdminPassword: envOr("E2E_ADMIN_PASSWORD", "admin123"),
		DevToken:      envOr("E2E_DEV_TOKEN", "mora_dev_a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4"),
	}
	c.IndexTimeout = durationOr("E2E_INDEX_TIMEOUT", 120*time.Second)
	return c
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func durationOr(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// Suite is the shared E2E fixture container. Tests run serially (no t.Parallel)
// so cached JWTs/tokens and a single DB pool are safe to reuse.
type Suite struct {
	suite.Suite
	cfg config

	pool *pgxpool.Pool

	adminJWT string

	aliceUserID string
	bobUserID   string
	aliceJWT    string
	bobJWT      string

	// MCP tokens bound to seeded identities (only when pool != nil).
	aliceRWToken  string
	aliceROToken  string
	bobROToken    string
	expiredToken  string
	revokeableTok string

	viewerRoleID string
	editorRoleID string
}

func TestMain(m *testing.M) {
	if os.Getenv("E2E_BASE_URL") == "" {
		// skip whole package when no target configured
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSuite(t *testing.T) {
	suite.Run(t, new(Suite))
}

func (s *Suite) SetupSuite() {
	s.cfg = loadConfig()

	// Health gate: refuse to run against a stack that is not ready, so a
	// misconfigured environment surfaces as a clear skip rather than a flood
	// of connection-refused failures.
	if !s.probe(s.cfg.BaseURL + "/healthz") {
		s.T().Skipf("mora-api not ready at %s (GET /healthz failed)", s.cfg.BaseURL)
	}
	if !s.probe(s.cfg.MCPURL + "/mcp/health") {
		s.T().Skipf("mcp-server not ready at %s (GET /mcp/health failed)", s.cfg.MCPURL)
	}

	var err error
	s.adminJWT, err = s.login(s.cfg.AdminEmail, s.cfg.AdminPassword)
	require.NoErrorf(s.T(), err, "admin login failed (email=%s)", s.cfg.AdminEmail)

	if s.cfg.DatabaseURL != "" {
		s.pool, err = pgxpool.New(context.Background(), s.cfg.DatabaseURL)
		require.NoError(s.T(), err, "opening DATABASE_URL pool")
		s.seedFixtures()
		s.viewerRoleID = s.roleID("viewer")
		s.editorRoleID = s.roleID("editor")
	}
}

func (s *Suite) TearDownSuite() {
	if s.pool != nil {
		s.cleanupFixtures()
		s.pool.Close()
	}
}

// requireDB skips a test that needs DATABASE_URL-backed fixtures (non-admin
// users, custom API tokens) when the pool is unavailable.
func (s *Suite) requireDB(reason string) {
	if s.pool == nil {
		s.T().Skipf("DATABASE_URL not set — skipping test needing %s", reason)
	}
}

// ---------------------------------------------------------------------------
// HTTP client + envelope helpers
// ---------------------------------------------------------------------------

type envelope struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

func (e *envelope) ok() bool { return e != nil && e.Code == 0 }

type Client struct {
	base       string
	hc         *http.Client
	bearer     string // JWT or INTERNAL_SERVICE_TOKEN
	identityID string // deprecated X-Identity-Id (§4.4: ignored by the API)
}

func newClient(base, bearer, identityID string) *Client {
	return &Client{
		base:       strings.TrimRight(base, "/"),
		hc:         &http.Client{Timeout: 30 * time.Second},
		bearer:     bearer,
		identityID: identityID,
	}
}

func (s *Suite) adminClient() *Client         { return newClient(s.cfg.BaseURL, s.adminJWT, "") }
func (s *Suite) jwtClient(jwt string) *Client { return newClient(s.cfg.BaseURL, jwt, "") }

// internalClient acts as a trusted internal service. Since §4.4, the
// INTERNAL_SERVICE_TOKEN alone degrades to a restricted service_account (never
// admin), and X-Identity-Id is deprecated (ignored). To act as an end-principal
// the caller must present a delegated JWT (POST /internal/v1/authz/delegated),
// not an identity header. The identityID param is retained only for callers
// that have not yet migrated; it is NOT trusted by the API.
func (s *Suite) internalClient(identityID string) *Client {
	return newClient(s.cfg.BaseURL, s.cfg.InternalToken, identityID)
}

func (s *Suite) mcpClient(token string) *Client { return newClient(s.cfg.MCPURL, token, "") }

func (c *Client) setAuth(req *http.Request) {
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	// §4.4: X-Identity-Id is deprecated and ignored by the API. Still sent
	// for compatibility, but it confers no identity or privilege.
	if c.identityID != "" {
		req.Header.Set("X-Identity-Id", c.identityID)
	}
}

// raw performs an HTTP call and returns status, headers and body bytes.
func (c *Client) raw(method, path string, body any, headers map[string]string) (int, http.Header, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return 0, nil, nil, err
	}
	c.setAuth(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data, nil
}

// call performs a JSON call, parses the mora-api {code,data,message} envelope,
// and unmarshals data into out (when non-nil and HTTP 2xx).
func (c *Client) call(method, path string, body any, out any, headers map[string]string) (int, *envelope, []byte) {
	status, _, data, err := c.raw(method, path, body, headers)
	if err != nil {
		panic(fmt.Sprintf("%s %s: %v", method, path, err))
	}
	env := &envelope{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, env) // some endpoints (MCP) return non-envelope JSON
	}
	if out != nil && env.ok() && len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, out)
	}
	return status, env, data
}

func (c *Client) get(path string, out any) (int, *envelope) {
	st, env, _ := c.call(http.MethodGet, path, nil, out, nil)
	return st, env
}
func (c *Client) post(path string, req, out any) (int, *envelope) {
	st, env, _ := c.call(http.MethodPost, path, req, out, nil)
	return st, env
}
func (c *Client) patch(path string, req, out any, headers map[string]string) (int, *envelope) {
	st, env, _ := c.call(http.MethodPatch, path, req, out, headers)
	return st, env
}
func (c *Client) del(path string) int {
	st, _, _, _ := c.raw(http.MethodDelete, path, nil, nil)
	return st
}

func (s *Suite) probe(url string) bool {
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

func (s *Suite) login(email, pw string) (string, error) {
	cl := newClient(s.cfg.BaseURL, "", "")
	body := map[string]string{"email": email, "password": pw}
	_, env, _ := cl.call(http.MethodPost, "/api/v1/auth/login", body, nil, nil)
	if !env.ok() {
		return "", fmt.Errorf("login failed: code=%d msg=%s", env.Code, env.Message)
	}
	var out struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(env.Data, &out)
	if out.Token == "" {
		return "", fmt.Errorf("login: empty token")
	}
	return out.Token, nil
}

// ---------------------------------------------------------------------------
// DB fixtures (non-admin users + API tokens). Requires DATABASE_URL.
// ---------------------------------------------------------------------------

func (s *Suite) seedFixtures() {
	ctx := context.Background()
	s.aliceUserID = s.seedUser(ctx, "e2e_alice@mora.local", "Alice E2E", "alice123")
	s.bobUserID = s.seedUser(ctx, "e2e_bob@mora.local", "Bob E2E", "bob123")

	var err error
	s.aliceJWT, err = s.login("e2e_alice@mora.local", "alice123")
	require.NoError(s.T(), err, "alice login")
	s.bobJWT, err = s.login("e2e_bob@mora.local", "bob123")
	require.NoError(s.T(), err, "bob login")

	s.aliceRWToken = s.seedToken(ctx, "e2e-alice-rw", s.aliceUserID, "readwrite", nil)
	s.aliceROToken = s.seedToken(ctx, "e2e-alice-ro", s.aliceUserID, "readonly", nil)
	s.bobROToken = s.seedToken(ctx, "e2e-bob-ro", s.bobUserID, "readonly", nil)
	past := time.Now().Add(-1 * time.Hour)
	s.expiredToken = s.seedToken(ctx, "e2e-expired", s.aliceUserID, "readwrite", &past)
	s.revokeableTok = s.seedToken(ctx, "e2e-revoke", s.aliceUserID, "readwrite", nil)
}

func (s *Suite) cleanupFixtures() {
	ctx := context.Background()
	// Best-effort: remove e2e tokens + users created by this run.
	for _, email := range []string{"e2e_alice@mora.local", "e2e_bob@mora.local"} {
		_, _ = s.pool.Exec(ctx, `DELETE FROM api_tokens WHERE name LIKE 'e2e-%'`)
		// Clean docs/dirs owned by these users to avoid FK blocks, then users.
		var uid string
		_ = s.pool.QueryRow(ctx, `SELECT id FROM users WHERE email=$1`, email).Scan(&uid)
		if uid != "" {
			_, _ = s.pool.Exec(ctx, `DELETE FROM permissions WHERE subject_id=$1 OR target_id IN (SELECT id FROM documents WHERE created_by=$1)`, uid)
			_, _ = s.pool.Exec(ctx, `DELETE FROM documents WHERE created_by=$1`, uid)
		}
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM api_tokens WHERE name LIKE 'e2e-%'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM users WHERE email IN ('e2e_alice@mora.local','e2e_bob@mora.local')`)
	// Clean any e2e-tagged docs/workspaces created by admin during runs.
	_, _ = s.pool.Exec(ctx, `DELETE FROM documents WHERE title LIKE 'E2E-%'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM permissions WHERE target_id IN (SELECT id FROM workspaces WHERE slug LIKE 'e2e-%')`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM workspaces WHERE slug LIKE 'e2e-%'`)
}

func (s *Suite) seedUser(ctx context.Context, email, name, password string) string {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	require.NoError(s.T(), err)
	var id string
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status, password_hash)
		 VALUES ($1,$2,'active',$3)
		 ON CONFLICT (email) DO UPDATE SET password_hash = EXCLUDED.password_hash
		 RETURNING id`,
		email, name, string(hash)).Scan(&id)
	require.NoError(s.T(), err, "seed user %s", email)
	return id
}

// seedToken inserts an api_tokens row and returns the plaintext token.
func (s *Suite) seedToken(ctx context.Context, name, identityID, scope string, expires *time.Time) string {
	plaintext := "mora_e2e_" + randHex(20)
	hash := sha256Hex(plaintext)
	prefix := plaintext
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO api_tokens (name, token_hash, prefix, identity_type, identity_id, scope, expires_at)
		 VALUES ($1,$2,$3,'user',$4,$5,$6)
		 ON CONFLICT (token_hash) DO NOTHING`,
		name, hash, prefix, identityID, scope, expires)
	require.NoError(s.T(), err, "seed token %s", name)
	return plaintext
}

// revokeToken flips revoked_at on a seeded token (AC-19 instant revocation).
func (s *Suite) revokeToken(plaintext string) {
	s.requireDB("token revocation")
	_, err := s.pool.Exec(context.Background(),
		`UPDATE api_tokens SET revoked_at = now() WHERE token_hash = $1`, sha256Hex(plaintext))
	require.NoError(s.T(), err, "revoke token")
}

func (s *Suite) roleID(name string) string {
	var id string
	err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM roles WHERE name=$1 AND is_system=true`, name).Scan(&id)
	require.NoError(s.T(), err, "lookup system role %s", name)
	return id
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// Domain models (JSON shapes verified against internal/domain + handlers)
// ---------------------------------------------------------------------------

type workspace struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Slug    string `json:"slug"`
	OwnerID string `json:"owner_id"`
}

type directory struct {
	ID          string      `json:"id"`
	WorkspaceID string      `json:"workspace_id"`
	ParentID    *string     `json:"parent_id,omitempty"`
	Name        string      `json:"name"`
	Path        string      `json:"path"`
	Children    []directory `json:"children,omitempty"`
}

type document struct {
	ID          string           `json:"id"`
	WorkspaceID string           `json:"workspace_id"`
	DirectoryID *string          `json:"directory_id,omitempty"`
	Title       string           `json:"title"`
	Content     []map[string]any `json:"content"`
	Format      string           `json:"format"`
	Status      string           `json:"status"`
	IndexStatus string           `json:"index_status"`
	VersionNo   int              `json:"version_no"`
	CreatedBy   string           `json:"created_by"`
}

type permission struct {
	ID           string `json:"id"`
	SubjectType  string `json:"subject_type"`
	SubjectID    string `json:"subject_id"`
	RoleID       string `json:"role_id"`
	TargetType   string `json:"target_type"`
	TargetID     string `json:"target_id"`
	Effect       string `json:"effect"`
	InheritScope string `json:"inherit_scope"`
}

type paged struct {
	Items    []json.RawMessage `json:"items"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

type searchHit struct {
	DocumentID  string  `json:"document_id"`
	Title       string  `json:"title"`
	Snippet     string  `json:"snippet"`
	Score       float64 `json:"score"`
	WorkspaceID string  `json:"workspace_id"`
}

type ragHit struct {
	DocumentID string  `json:"document_id"`
	Title      string  `json:"title"`
	ChunkText  string  `json:"chunk_text"`
	Score      float32 `json:"score"`
	DenseScore float32 `json:"dense_score"`
	BM25Score  float32 `json:"bm25_score"`
}

type ragResult struct {
	Items []ragHit `json:"items"`
	Total int      `json:"total"`
}

// ---------------------------------------------------------------------------
// Mora API helpers
// ---------------------------------------------------------------------------

func (s *Suite) createWorkspace(cl *Client, name, slug string) workspace {
	var ws workspace
	st, env := cl.post("/api/v1/workspaces", map[string]string{"name": name, "slug": slug}, &ws)
	require.Equalf(s.T(), http.StatusCreated, st, "create workspace: code=%d msg=%s", env.Code, env.Message)
	return ws
}

func (s *Suite) createDirectory(cl *Client, wsID, parentID, name string) directory {
	body := map[string]any{"name": name}
	if parentID != "" {
		body["parent_id"] = parentID
	}
	var dir directory
	st, env := cl.post("/api/v1/workspaces/"+wsID+"/directories", body, &dir)
	require.Equalf(s.T(), http.StatusCreated, st, "create dir: code=%d msg=%s", env.Code, env.Message)
	return dir
}

func (s *Suite) directoryTree(cl *Client, wsID string) []directory {
	var out struct{ Items []directory }
	cl.get("/api/v1/workspaces/"+wsID+"/directories", &out)
	return out.Items
}

func (s *Suite) createDocMarkdown(cl *Client, wsID, title, markdown string) document {
	var doc document
	st, env := cl.post("/api/v1/workspaces/"+wsID+"/documents",
		map[string]any{"title": title, "markdown": markdown}, &doc)
	require.Equalf(s.T(), http.StatusCreated, st, "create doc: code=%d msg=%s", env.Code, env.Message)
	return doc
}

func (s *Suite) createDocBlocks(cl *Client, wsID, title string, blocks []map[string]any) document {
	var doc document
	st, env := cl.post("/api/v1/workspaces/"+wsID+"/documents",
		map[string]any{"title": title, "content": blocks, "format": "blocks"}, &doc)
	require.Equalf(s.T(), http.StatusCreated, st, "create doc(blocks): code=%d msg=%s", env.Code, env.Message)
	return doc
}

// publishDoc patches status=published (and optionally new content), returning
// the updated document. prevVersion drives the If-Match optimistic lock.
func (s *Suite) publishDoc(cl *Client, docID string, prevVersion int, markdown string) document {
	body := map[string]any{"status": "published"}
	if markdown != "" {
		body["markdown"] = markdown
	}
	var doc document
	st, env := cl.patch("/api/v1/documents/"+docID, body, &doc,
		map[string]string{"If-Match": fmt.Sprintf("%d", prevVersion)})
	require.Equalf(s.T(), http.StatusOK, st, "publish doc: code=%d msg=%s", env.Code, env.Message)
	return doc
}

func (s *Suite) getDoc(cl *Client, docID string) (document, int, *envelope) {
	var doc document
	st, env := cl.get("/api/v1/documents/"+docID, &doc)
	return doc, st, env
}

func (s *Suite) updateDoc(cl *Client, docID string, prevVersion int, markdown string) (document, int, *envelope) {
	var doc document
	st, env := cl.patch("/api/v1/documents/"+docID,
		map[string]any{"markdown": markdown}, &doc,
		map[string]string{"If-Match": fmt.Sprintf("%d", prevVersion)})
	return doc, st, env
}

func (s *Suite) deleteDoc(cl *Client, docID string) int {
	return cl.del("/api/v1/documents/" + docID)
}

func (s *Suite) grantPermission(cl *Client, subjectType, subjectID, roleID, targetType, targetID, effect string) permission {
	body := map[string]any{
		"subject_type": subjectType, "subject_id": subjectID,
		"role_id": roleID, "target_type": targetType, "target_id": targetID,
	}
	if effect != "" {
		body["effect"] = effect
	}
	var p permission
	st, env := cl.post("/api/v1/permissions", body, &p)
	require.Equalf(s.T(), http.StatusCreated, st, "grant perm: code=%d msg=%s", env.Code, env.Message)
	return p
}

func (s *Suite) revokePermission(cl *Client, permID string) int {
	return cl.del("/api/v1/permissions/" + permID)
}

func (s *Suite) searchFTS(cl *Client, wsID, q string, extra map[string]string) paged {
	path := "/api/v1/search?workspace_id=" + wsID + "&q=" + q
	for k, v := range extra {
		path += "&" + k + "=" + v
	}
	var p paged
	cl.get(path, &p)
	return p
}

func (s *Suite) ragSearch(cl *Client, query, wsID string, topN int) (ragResult, int, *envelope) {
	body := map[string]any{"query": query, "top_n": topN}
	if wsID != "" {
		body["workspace_id"] = wsID
	}
	var r ragResult
	st, env := cl.post("/api/v1/rag/search", body, &r)
	return r, st, env
}

// waitForIndexStatus polls GET /documents/:id until index_status reaches want
// or timeout. Returns the final document. want defaults to "indexed".
func (s *Suite) waitForIndexStatus(cl *Client, docID, want string) document {
	if want == "" {
		want = "indexed"
	}
	deadline := time.Now().Add(s.cfg.IndexTimeout)
	var last document
	for time.Now().Before(deadline) {
		doc, st, _ := s.getDoc(cl, docID)
		if st == http.StatusOK {
			last = doc
			if doc.IndexStatus == want {
				return doc
			}
			if doc.IndexStatus == "failed" {
				s.T().Fatalf("document %s index_status=failed (rag-worker/TEI/Qdrant not healthy?)", docID)
			}
		}
		time.Sleep(2 * time.Second)
	}
	s.T().Fatalf("document %s index_status never reached %q (last=%s) within %s — check rag-worker/TEI/Qdrant/Valkey",
		docID, want, last.IndexStatus, s.cfg.IndexTimeout)
	return last
}

// ---------------------------------------------------------------------------
// MCP JSON-RPC helpers
// ---------------------------------------------------------------------------

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type mcpSession struct {
	client     *Client
	sessionID  string
	nextID     int
	lastErr    *rpcError
	lastStatus int
}

func newMCPSession(cl *Client) *mcpSession { return &mcpSession{client: cl, nextID: 1} }

// initialize performs the MCP initialize handshake and captures the session id.
func (s *Suite) mcpInitialize(cl *Client) *mcpSession {
	ms := newMCPSession(cl)
	id := ms.nextID
	ms.nextID++
	req := map[string]any{
		"jsonrpc": "2.0", "id": id, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"roots": map[string]any{"listChanged": true}},
			"clientInfo":      map[string]any{"name": "e2e-test", "version": "1.0.0"},
		},
	}
	st, hdr, data, err := cl.raw(http.MethodPost, "/mcp", req, nil)
	require.NoErrorf(s.T(), err, "mcp initialize request")
	require.Equalf(s.T(), http.StatusOK, st, "mcp initialize status: %s", string(data))
	var r rpcResp
	require.NoErrorf(s.T(), json.Unmarshal(data, &r), "mcp initialize parse: %s", string(data))
	require.Nilf(s.T(), r.Error, "mcp initialize error: %+v", r.Error)
	ms.sessionID = hdr.Get("Mcp-Session-Id")
	require.NotEmpty(s.T(), ms.sessionID, "Mcp-Session-Id header missing")

	// notifications/initialized (no id → HTTP 202, no body)
	nreq := map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}
	nst, _, _, nerr := cl.raw(http.MethodPost, "/mcp", nreq,
		map[string]string{"Mcp-Session-Id": ms.sessionID})
	require.NoErrorf(s.T(), nerr, "mcp initialized notification")
	require.Truef(s.T(), nst == http.StatusAccepted || nst == http.StatusOK, "mcp initialized status=%d", nst)
	return ms
}

func (ms *mcpSession) toolsList() []map[string]any {
	r := ms.result("tools/list", nil)
	var out struct {
		Tools []map[string]any `json:"tools"`
	}
	_ = json.Unmarshal(r, &out)
	return out.Tools
}

func (ms *mcpSession) toolsCall(name string, args map[string]any) (map[string]any, *rpcError) {
	r := ms.result("tools/call", map[string]any{"name": name, "arguments": args})
	if r == nil {
		// RPC-level error (auth/scope/dispatch/upstream). Surface the lastErr so
		// callers can distinguish failure from an empty success result.
		return nil, ms.lastErr
	}
	// tools/call may return result.content[] or a top-level error.
	var res struct {
		Content []map[string]any `json:"content"`
		IsError bool             `json:"isError"`
	}
	_ = json.Unmarshal(r, &res)
	parsed := map[string]any{"isError": res.IsError, "content": res.Content}
	// Parse the first text content as JSON if possible (tool payloads are JSON-stringified).
	if len(res.Content) > 0 {
		if txt, ok := res.Content[0]["text"].(string); ok {
			var v any
			if json.Unmarshal([]byte(txt), &v) == nil {
				parsed["data"] = v
			} else {
				parsed["text"] = txt
			}
		}
	}
	return parsed, nil
}

// result issues a JSON-RPC request that is expected to succeed and returns the
// raw result payload. On RPC error it returns nil result + the error.
func (ms *mcpSession) result(method string, params any) json.RawMessage {
	id := ms.nextID
	ms.nextID++
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	st, _, data, err := ms.client.raw(http.MethodPost, "/mcp", req,
		map[string]string{"Mcp-Session-Id": ms.sessionID})
	if err != nil {
		panic(fmt.Sprintf("mcp %s: %v", method, err))
	}
	var r rpcResp
	_ = json.Unmarshal(data, &r)
	// Stash error on the session for callers that want to inspect failure path.
	if r.Error != nil {
		ms.lastErr = r.Error
		ms.lastStatus = st
		return nil
	}
	ms.lastErr = nil
	ms.lastStatus = st
	return r.Result
}

func (ms *mcpSession) resourcesList() []map[string]any {
	r := ms.result("resources/list", nil)
	var out struct {
		Resources []map[string]any `json:"resources"`
	}
	_ = json.Unmarshal(r, &out)
	return out.Resources
}

func (ms *mcpSession) resourcesRead(uri string) []map[string]any {
	r := ms.result("resources/read", map[string]any{"uri": uri})
	var out struct {
		Contents []map[string]any `json:"contents"`
	}
	_ = json.Unmarshal(r, &out)
	return out.Contents
}

func (ms *mcpSession) deleteSession() int {
	st, _, _, _ := ms.client.raw(http.MethodDelete, "/mcp", nil,
		map[string]string{"Mcp-Session-Id": ms.sessionID})
	return st
}

// toolCallsAudit queries the MCP admin audit endpoint for tool-call records.
// The audit endpoint lives on the MCP server (not mora-api) and returns a bare
// {"items":[...]} JSON (not the mora-api {code,data,message} envelope), so the
// response is parsed directly.
func (s *Suite) toolCallsAudit(cl *Client, query string) []map[string]any {
	path := "/mcp/tool-calls"
	if query != "" {
		path += "?" + query
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	_, _, data, err := s.mcpClient(s.cfg.DevToken).raw(http.MethodGet, path, nil, nil)
	if err == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	return out.Items
}

// helper to create a simple paragraph block for content assertions.
func paragraphBlock(text string) map[string]any {
	return map[string]any{
		"type": "paragraph",
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
}

func codeBlock(language, code string) map[string]any {
	return map[string]any{
		"type":  "codeBlock",
		"attrs": map[string]any{"language": language},
		"content": []map[string]any{
			{"type": "text", "text": code},
		},
	}
}

func headingBlock(level int, text string) map[string]any {
	return map[string]any{
		"type":  "heading",
		"attrs": map[string]any{"level": level},
		"content": []map[string]any{
			{"type": "text", "text": text},
		},
	}
}
