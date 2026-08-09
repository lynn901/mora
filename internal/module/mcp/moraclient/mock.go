package moraclient

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	domainerr "github.com/lynn901/mora/internal/pkg/errors"
)

// Mock is an in-memory MoraClient with an embedded RBAC model for standalone
// development and tests. It is safe for concurrent use.
//
// ACL model (deliberately simple but realistic for testing):
//   - Each identity has a set of workspace read/write grants.
//   - Document visibility derives from its workspace grant.
//   - Read without grant => ErrNotExist (existence-leak prevention path).
//   - Write without grant => ErrForbidden.
//   - Token scope is enforced by the MCP auth layer BEFORE reaching here; the
//     mock additionally honours scope as a defence-in-depth check.
type Mock struct {
	mu sync.RWMutex

	workspaces  map[string]*Workspace
	directories map[string]*mockDirectory
	documents   map[string]*mockDocument
	tags        map[string][]Tag // workspaceID -> tags

	// acl: identityID -> {read: set[wsID], write: set[wsID]}
	acl map[string]*mockACL

	// draft store for create_draft / update_document
	drafts map[string]*DraftResult
}

type mockDirectory struct {
	DirectoryNode
	workspaceID string
}

type mockDocument struct {
	DocumentMeta
	content  string
	format   string
	versions []VersionSummary
}

type mockACL struct {
	read  map[string]struct{}
	write map[string]struct{}
}

// NewMock returns an empty in-memory MoraClient.
func NewMock() *Mock {
	return &Mock{
		workspaces:  make(map[string]*Workspace),
		directories: make(map[string]*mockDirectory),
		documents:   make(map[string]*mockDocument),
		tags:        make(map[string][]Tag),
		acl:         make(map[string]*mockACL),
		drafts:      make(map[string]*DraftResult),
	}
}

// GrantRead grants an identity read access to a workspace.
func (m *Mock) GrantRead(identityID, workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.acl[identityID]
	if a == nil {
		a = &mockACL{read: map[string]struct{}{}, write: map[string]struct{}{}}
		m.acl[identityID] = a
	}
	a.read[workspaceID] = struct{}{}
}

// GrantWrite grants an identity write access to a workspace.
func (m *Mock) GrantWrite(identityID, workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.acl[identityID]
	if a == nil {
		a = &mockACL{read: map[string]struct{}{}, write: map[string]struct{}{}}
		m.acl[identityID] = a
	}
	a.read[workspaceID] = struct{}{}
	a.write[workspaceID] = struct{}{}
}

// RevokeRead removes an identity's read access to a workspace (tests only).
func (m *Mock) RevokeRead(identityID, workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.acl[identityID]; a != nil {
		delete(a.read, workspaceID)
		delete(a.write, workspaceID)
	}
}

// AddWorkspace inserts a workspace.
func (m *Mock) AddWorkspace(w Workspace) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workspaces[w.ID] = &w
}

// AddDirectory inserts a directory under a workspace.
func (m *Mock) AddDirectory(d DirectoryNode, workspaceID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.directories[d.ID] = &mockDirectory{DirectoryNode: d, workspaceID: workspaceID}
}

// AddDocument inserts a document. Content/format body included.
func (m *Mock) AddDocument(meta DocumentMeta, content, format string, versions []VersionSummary) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.documents[meta.ID] = &mockDocument{DocumentMeta: meta, content: content, format: format, versions: versions}
}

// AddTags sets the tag taxonomy for a workspace.
func (m *Mock) AddTags(workspaceID string, tags []Tag) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tags[workspaceID] = tags
}

func (m *Mock) aclFor(identityID string) *mockACL {
	return m.acl[identityID]
}

func (m *Mock) canRead(auth *AuthContext, workspaceID string) bool {
	if auth == nil {
		return false
	}
	a := m.aclFor(auth.IdentityID)
	if a == nil {
		return false
	}
	_, ok := a.read[workspaceID]
	return ok
}

func (m *Mock) canWrite(auth *AuthContext, workspaceID string) bool {
	if auth == nil || !auth.Scope.AllowsWrite() {
		return false
	}
	a := m.aclFor(auth.IdentityID)
	if a == nil {
		return false
	}
	_, ok := a.write[workspaceID]
	return ok
}

// ListWorkspaces returns workspaces the caller can read.
func (m *Mock) ListWorkspaces(_ context.Context, auth *AuthContext) ([]Workspace, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.aclFor(auth.IdentityID)
	out := make([]Workspace, 0)
	if a == nil {
		return out, nil
	}
	for _, w := range m.workspaces {
		if _, ok := a.read[w.ID]; ok {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetDirectoryTree returns the visible directory tree (documents included as
// metadata only). Invisible nodes/documents are filtered out. Path encodes the
// ancestor id chain joined by "/", e.g. "parentID/childID"; a root has an empty
// Path. Callers without read access get ErrNotExist (no existence leak).
func (m *Mock) GetDirectoryTree(_ context.Context, auth *AuthContext, workspaceID string) ([]DirectoryNode, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.canRead(auth, workspaceID) {
		return nil, ErrNotExist()
	}
	all := make([]*mockDirectory, 0)
	for _, d := range m.directories {
		if d.workspaceID == workspaceID {
			all = append(all, d)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].SortOrder < all[j].SortOrder })
	byID := make(map[string]*DirectoryNode, len(all))
	var rootIDs []string
	for _, d := range all {
		node := d.DirectoryNode
		node.Documents = m.docsInDir(workspaceID, d.ID)
		byID[d.ID] = &node
		if d.Path == "" {
			rootIDs = append(rootIDs, d.ID)
		} else {
			parts := strings.Split(d.Path, "/")
			parentID := parts[len(parts)-1]
			if p, ok := byID[parentID]; ok {
				p.Children = append(p.Children, node)
			} else {
				rootIDs = append(rootIDs, d.ID)
			}
		}
	}
	roots := make([]DirectoryNode, 0, len(rootIDs))
	for _, id := range rootIDs {
		if n, ok := byID[id]; ok {
			roots = append(roots, *n)
		}
	}
	// If no directories, still surface root-level documents.
	if len(roots) == 0 {
		roots = append(roots, DirectoryNode{Documents: m.docsInDir(workspaceID, "")})
	}
	return roots, nil
}

func (m *Mock) docsInDir(workspaceID, directoryID string) []DocumentMeta {
	out := make([]DocumentMeta, 0)
	for _, doc := range m.documents {
		if doc.WorkspaceID == workspaceID && doc.DirectoryID == directoryID {
			out = append(out, doc.DocumentMeta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}

// GetDocumentMeta returns metadata or ErrNotExist (no existence leak).
func (m *Mock) GetDocumentMeta(_ context.Context, auth *AuthContext, documentID string) (*DocumentMeta, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	doc, ok := m.documents[documentID]
	if !ok || !m.canRead(auth, doc.WorkspaceID) {
		return nil, ErrNotExist()
	}
	meta := doc.DocumentMeta
	return &meta, nil
}

// GetDocument returns body or ErrNotExist (no existence leak).
func (m *Mock) GetDocument(_ context.Context, auth *AuthContext, documentID string, format string, versionNo int) (*Document, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	doc, ok := m.documents[documentID]
	if !ok || !m.canRead(auth, doc.WorkspaceID) {
		return nil, ErrNotExist()
	}
	body := doc.content
	outFormat := doc.format
	if format != "" {
		outFormat = format
		if format == "markdown" && doc.format == "blocks" {
			body = blocksToMarkdown(doc.content)
		}
	}
	// body may be markdown (not JSON); wrap it as a JSON string so RawMessage is valid.
	bodyJSON, _ := json.Marshal(body)
	return &Document{
		DocumentMeta: doc.DocumentMeta,
		Content:      bodyJSON,
		Format:       outFormat,
	}, nil
}

// ListDocuments lists documents under a workspace/directory, RBAC-filtered.
func (m *Mock) ListDocuments(_ context.Context, auth *AuthContext, p ListDocumentsParams) ([]DocumentMeta, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.canRead(auth, p.WorkspaceID) {
		return nil, 0, nil // empty set, not an error (existence-leak prevention)
	}
	out := make([]DocumentMeta, 0)
	for _, doc := range m.documents {
		if doc.WorkspaceID != p.WorkspaceID {
			continue
		}
		if p.DirectoryID != "" && doc.DirectoryID != p.DirectoryID {
			continue
		}
		if p.Status != "" && doc.Status != p.Status {
			continue
		}
		if p.Tag != "" {
			found := false
			for _, t := range doc.Tags {
				if t == p.Tag {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, doc.DocumentMeta)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	total := len(out)
	if p.Page <= 0 {
		p.Page = 1
	}
	if p.PageSize <= 0 {
		p.PageSize = 20
	}
	start := (p.Page - 1) * p.PageSize
	if start >= total {
		return []DocumentMeta{}, total, nil
	}
	end := start + p.PageSize
	if end > total {
		end = total
	}
	return out[start:end], total, nil
}

// GetTags returns the workspace tag taxonomy (workspace visible => readable).
func (m *Mock) GetTags(_ context.Context, auth *AuthContext, workspaceID string) ([]Tag, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.canRead(auth, workspaceID) {
		return nil, ErrNotExist()
	}
	return m.tags[workspaceID], nil
}

// GetDocumentVersions returns version history (read permission).
func (m *Mock) GetDocumentVersions(_ context.Context, auth *AuthContext, documentID string) ([]VersionSummary, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	doc, ok := m.documents[documentID]
	if !ok || !m.canRead(auth, doc.WorkspaceID) {
		return nil, ErrNotExist()
	}
	return doc.versions, nil
}

// Search runs a simple in-memory keyword match for MCP tool and RBAC tests.
func (m *Mock) Search(_ context.Context, auth *AuthContext, req SearchRequest) (*SearchResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a := m.aclFor(auth.IdentityID)
	q := strings.ToLower(req.Query)
	hits := make([]SearchHit, 0)
	for _, doc := range m.documents {
		if req.WorkspaceID != "" && doc.WorkspaceID != req.WorkspaceID {
			continue
		}
		if a == nil {
			continue
		}
		if _, ok := a.read[doc.WorkspaceID]; !ok {
			continue // RBAC hard filter: invisible docs never returned
		}
		body := strings.ToLower(doc.content)
		title := strings.ToLower(doc.Title)
		if !strings.Contains(body, q) && !strings.Contains(title, q) {
			continue
		}
		// crude score: title match weighted higher.
		score := 0.5
		if strings.Contains(title, q) {
			score = 0.95
		}
		snippet := makeSnippet(doc.content, q)
		hits = append(hits, SearchHit{
			DocumentID:  doc.ID,
			Title:       doc.Title,
			ChunkText:   snippet,
			ChunkIndex:  0,
			Score:       score,
			DenseScore:  score,
			BM25Score:   score,
			WorkspaceID: doc.WorkspaceID,
			SourceURL:   "/workspaces/" + doc.WorkspaceID + "/documents/" + doc.ID,
		})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	total := len(hits)
	topN := req.TopN
	if topN <= 0 {
		topN = 10
	}
	if topN > total {
		topN = total
	}
	return &SearchResult{Items: hits[:topN], Total: total}, nil
}

// CreateDraft creates a draft (write permission required; ErrForbidden otherwise).
func (m *Mock) CreateDraft(_ context.Context, auth *AuthContext, req CreateDraftRequest) (*DraftResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.canWrite(auth, req.WorkspaceID) {
		return nil, domainerr.ErrForbidden
	}
	draftID := "draft-" + req.WorkspaceID + "-" + req.Title
	res := &DraftResult{
		DraftID:   draftID,
		VersionNo: 1,
		ReviewURL: "/review/" + draftID,
	}
	m.drafts[draftID] = res
	return res, nil
}

// UpdateDocument creates a new draft version (write permission required).
func (m *Mock) UpdateDocument(_ context.Context, auth *AuthContext, req UpdateDocumentRequest) (*DraftResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	doc, ok := m.documents[req.DocumentID]
	if !ok || !m.canWrite(auth, doc.WorkspaceID) {
		return nil, domainerr.ErrForbidden
	}
	draftID := "draft-" + req.DocumentID
	res := &DraftResult{
		DraftID:    draftID,
		VersionNo:  doc.VersionNo + 1,
		ReviewURL:  "/review/" + draftID,
		DocumentID: req.DocumentID,
	}
	m.drafts[draftID] = res
	return res, nil
}

func makeSnippet(content, q string) string {
	idx := strings.Index(strings.ToLower(content), q)
	if idx < 0 {
		if len(content) > 120 {
			return content[:120] + "..."
		}
		return content
	}
	start := idx - 40
	if start < 0 {
		start = 0
	}
	end := idx + len(q) + 80
	if end > len(content) {
		end = len(content)
	}
	return content[start:end]
}

// blocksToMarkdown is a minimal converter for mock responses.
func blocksToMarkdown(blocks string) string {
	return blocks
}
