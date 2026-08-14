package moraclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

// HTTPClient is the production MoraClient: it calls the Mora API + RAG search
// REST endpoints over HTTP, propagating the caller identity via headers so the
// Mora/RAG services enforce RBAC server-side (design doc 06 §6.3, 02 §2.2).
//
// Identity propagation headers:
//   - X-Identity-Type / X-Identity-Id / X-Identity-Name: the token-bound
//     principal. NOTE (design-docs/13 §4.4): these headers are DEPRECATED on
//     the mora-api side — the API no longer trusts X-Identity-* for identity
//     or admin; an internal caller must present a delegated JWT instead. They
//     are still sent for backward compatibility but confer no authority. The
//     MCP Server should obtain a delegated context via
//     POST /internal/v1/authz/delegated and send it as the Bearer token.
//   - X-Token-Scope: the token capability envelope (defence-in-depth; the Mora
//     layer also enforces scope for write endpoints).
//   - Authorization: INTERNAL_SERVICE_TOKEN — proves service identity only;
//     without a delegated JWT the call degrades to a restricted service_account
//     (§4.4), never admin.
type HTTPClient struct {
	baseURL       string
	internalToken string
	http          *http.Client
}

// NewHTTPClient returns a REST-backed MoraClient.
func NewHTTPClient(baseURL, internalToken string) *HTTPClient {
	return &HTTPClient{
		baseURL:       strings.TrimRight(baseURL, "/"),
		internalToken: internalToken,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *HTTPClient) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.internalToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.internalToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return data, resp.StatusCode, nil
}

// setIdentityHeaders propagates the caller principal onto the outbound request.
func (c *HTTPClient) identityHeaders(auth *AuthContext, h http.Header) {
	if auth == nil {
		return
	}
	h.Set("X-Identity-Type", string(auth.IdentityType))
	h.Set("X-Identity-Id", auth.IdentityID)
	h.Set("X-Identity-Name", auth.IdentityName)
	h.Set("X-Token-Scope", string(auth.Scope))
	if auth.IsAdmin {
		h.Set("X-Identity-Admin", "true")
	}
}

// mapStatus translates Mora API HTTP status into a domain error for the
// existence-leak / forbidden semantics (design doc 06 §6.4).
func mapStatus(status int) error {
	switch status {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusForbidden:
		return domainerr.ErrForbidden
	case http.StatusNotFound:
		return ErrNotExist()
	case http.StatusUnauthorized:
		return domainerr.ErrUnauthorized
	default:
		return fmt.Errorf("upstream status %d", status)
	}
}

// envelope is the standard Mora API response wrapper (design doc 04 §1.3).
// The Mora API returns {code,data,message}; the message field carries the
// upstream error detail (previously decoded as "msg", which was always empty).
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (c *HTTPClient) get(ctx context.Context, auth *AuthContext, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	if c.internalToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.internalToken)
	}
	c.identityHeaders(auth, req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if err := mapStatus(resp.StatusCode); err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("upstream code %d: %s", env.Code, env.Message)
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

func (c *HTTPClient) post(ctx context.Context, auth *AuthContext, path string, body, out any) error {
	return c.sendJSON(ctx, auth, http.MethodPost, path, body, out)
}

// sendJSON issues an authenticated JSON request, maps the HTTP status, and
// unwraps the {code,data,message} envelope into out. Used by POST/PATCH writes.
func (c *HTTPClient) sendJSON(ctx context.Context, auth *AuthContext, method, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.internalToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.internalToken)
	}
	c.identityHeaders(auth, req.Header)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if err := mapStatus(resp.StatusCode); err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("upstream code %d: %s", env.Code, env.Message)
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}

// ListWorkspaces calls GET /api/v1/workspaces.
func (c *HTTPClient) ListWorkspaces(ctx context.Context, auth *AuthContext) ([]Workspace, error) {
	var resp struct {
		Items []Workspace `json:"items"`
	}
	if err := c.get(ctx, auth, "/api/v1/workspaces", &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// GetDirectoryTree calls GET /api/v1/workspaces/{id}/directories.
func (c *HTTPClient) GetDirectoryTree(ctx context.Context, auth *AuthContext, workspaceID string) ([]DirectoryNode, error) {
	var resp struct {
		Items []DirectoryNode `json:"items"`
	}
	if err := c.get(ctx, auth, "/api/v1/workspaces/"+workspaceID+"/directories", &resp); err != nil {
		if isNotExist(err) {
			return nil, ErrNotExist()
		}
		return nil, err
	}
	return resp.Items, nil
}

// GetDocumentMeta calls GET /api/v1/documents/{id} and strips the body.
func (c *HTTPClient) GetDocumentMeta(ctx context.Context, auth *AuthContext, documentID string) (*DocumentMeta, error) {
	var doc Document
	if err := c.get(ctx, auth, "/api/v1/documents/"+documentID, &doc); err != nil {
		if isNotExist(err) {
			return nil, ErrNotExist()
		}
		return nil, err
	}
	return &doc.DocumentMeta, nil
}

// GetDocument calls GET /api/v1/documents/{id}.
func (c *HTTPClient) GetDocument(ctx context.Context, auth *AuthContext, documentID string, format string, versionNo int) (*Document, error) {
	path := "/api/v1/documents/" + documentID
	q := []string{}
	if format != "" {
		q = append(q, "format="+format)
	}
	if versionNo > 0 {
		q = append(q, "version="+strconv.Itoa(versionNo))
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	var doc Document
	if err := c.get(ctx, auth, path, &doc); err != nil {
		if isNotExist(err) {
			return nil, ErrNotExist()
		}
		return nil, err
	}
	return &doc, nil
}

// ListDocuments calls GET /api/v1/workspaces/{ws}/documents.
func (c *HTTPClient) ListDocuments(ctx context.Context, auth *AuthContext, p ListDocumentsParams) ([]DocumentMeta, int, error) {
	path := "/api/v1/workspaces/" + p.WorkspaceID + "/documents?page=" + strconv.Itoa(max1(p.Page)) + "&page_size=" + strconv.Itoa(max1(p.PageSize))
	if p.DirectoryID != "" {
		path += "&directory_id=" + p.DirectoryID
	}
	if p.Tag != "" {
		path += "&tag=" + p.Tag
	}
	if p.Status != "" {
		path += "&status=" + p.Status
	}
	var resp struct {
		Items []DocumentMeta `json:"items"`
		Total int            `json:"total"`
	}
	if err := c.get(ctx, auth, path, &resp); err != nil {
		if isNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	return resp.Items, resp.Total, nil
}

// GetTags calls GET /api/v1/workspaces/{id}/tags.
func (c *HTTPClient) GetTags(ctx context.Context, auth *AuthContext, workspaceID string) ([]Tag, error) {
	var resp struct {
		Items []Tag `json:"items"`
	}
	if err := c.get(ctx, auth, "/api/v1/workspaces/"+workspaceID+"/tags", &resp); err != nil {
		if isNotExist(err) {
			return nil, ErrNotExist()
		}
		return nil, err
	}
	return resp.Items, nil
}

// GetDocumentVersions calls GET /api/v1/documents/{id}/versions.
func (c *HTTPClient) GetDocumentVersions(ctx context.Context, auth *AuthContext, documentID string) ([]VersionSummary, error) {
	var resp struct {
		Items []VersionSummary `json:"items"`
	}
	if err := c.get(ctx, auth, "/api/v1/documents/"+documentID+"/versions", &resp); err != nil {
		if isNotExist(err) {
			return nil, ErrNotExist()
		}
		return nil, err
	}
	return resp.Items, nil
}

// Search calls POST /api/v1/rag/search.
func (c *HTTPClient) Search(ctx context.Context, auth *AuthContext, req SearchRequest) (*SearchResult, error) {
	var resp SearchResult
	if err := c.post(ctx, auth, "/api/v1/rag/search", req, &resp); err != nil {
		if isNotExist(err) {
			return &SearchResult{}, nil
		}
		return nil, err
	}
	return &resp, nil
}

// CreateDraft calls POST /api/v1/workspaces/{ws}/documents with status=draft.
// The mora-api create handler accepts `markdown` (string) or `content` ([]Block);
// Content here is a string (Markdown or Block JSON), dispatched by Format.
func (c *HTTPClient) CreateDraft(ctx context.Context, auth *AuthContext, req CreateDraftRequest) (*DraftResult, error) {
	body := map[string]any{
		"title": req.Title,
	}
	if req.ParentID != "" {
		body["directory_id"] = req.ParentID
	}
	if req.Format == "blocks" {
		body["content"] = json.RawMessage(req.Content)
		body["format"] = "blocks"
	} else {
		body["markdown"] = req.Content
		body["format"] = "markdown"
	}
	var doc Document
	if err := c.post(ctx, auth, "/api/v1/workspaces/"+req.WorkspaceID+"/documents", body, &doc); err != nil {
		return nil, err
	}
	return &DraftResult{
		DraftID:    doc.ID,
		VersionNo:  doc.VersionNo,
		DocumentID: doc.ID,
		ReviewURL:  "/review/" + doc.ID,
	}, nil
}

// UpdateDocument calls PATCH /api/v1/documents/{id} (produces a new draft version).
// The mora-api update handler accepts `markdown` (string) or `content` ([]Block);
// it has no `format` field. Content here is a string dispatched by Format.
func (c *HTTPClient) UpdateDocument(ctx context.Context, auth *AuthContext, req UpdateDocumentRequest) (*DraftResult, error) {
	body := map[string]any{
		"status":  "draft",
		"summary": req.Summary,
	}
	if req.Format == "blocks" {
		body["content"] = json.RawMessage(req.Content)
	} else {
		body["markdown"] = req.Content
	}
	var doc Document
	if err := c.sendJSON(ctx, auth, http.MethodPatch, "/api/v1/documents/"+req.DocumentID, body, &doc); err != nil {
		return nil, err
	}
	return &DraftResult{
		DraftID:    doc.ID,
		VersionNo:  doc.VersionNo,
		DocumentID: doc.ID,
		ReviewURL:  "/review/" + doc.ID,
	}, nil
}

// WikiStatus calls GET /api/v1/wiki-spaces/{id}/status (design doc 16 §7.3).
// Read-only: a missing or unauthorized Space maps to ErrNotExist so the
// wiki_status tool yields an empty result (§8.2 — no existence leak).
func (c *HTTPClient) WikiStatus(ctx context.Context, auth *AuthContext, wikiSpaceID string) (*WikiSpaceStatus, error) {
	var st WikiSpaceStatus
	if err := c.get(ctx, auth, "/api/v1/wiki-spaces/"+wikiSpaceID+"/status", &st); err != nil {
		if isNotExist(err) {
			return nil, ErrNotExist()
		}
		return nil, err
	}
	return &st, nil
}

// WikiPagePropose calls POST /api/v1/wiki-spaces/{id}/maintenance-runs with
// trigger=ingest (design doc 16 §7.3 / §11.3). It lands a candidate proposal
// asynchronously — never publishes directly. Write: ErrForbidden on missing
// write perm. An idempotent retry surfaces the existing run id (200 envelope).
func (c *HTTPClient) WikiPagePropose(ctx context.Context, auth *AuthContext, req WikiPageProposeRequest) (*WikiPageProposeResult, error) {
	body := map[string]any{
		"trigger":  "ingest",
		"page_key": req.PageKey,
	}
	if req.AnswerRef != nil {
		body["answer_ref"] = req.AnswerRef
	}
	var run WikiPageProposeResult
	// trigger=maintenance-run returns 201 (new) or 200 (idempotent retry); both
	// unwrap to a run object via the standard envelope.
	if err := c.post(ctx, auth, "/api/v1/wiki-spaces/"+req.WikiSpaceID+"/maintenance-runs", body, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func isNotExist(err error) bool {
	return domainerr.Is(err, domainerr.ErrNotFound)
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
