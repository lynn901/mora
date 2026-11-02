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

	// wiki store for wiki_status / wiki_page_propose (design doc 16 §7.3).
	// Minimal in-memory model so the MCP tools are exercisable in mock mode.
	wikiSpaces map[string]*WikiSpaceStatus
	wikiRuns   map[string]*WikiPageProposeResult // runID -> run

	// codegraph store for the code_* tools (design-docs/17 §6.2). A codebase is
	// a knowledge_assets row owned by a workspace; the mock models only the
	// bits the query tools need: a workspace binding (for RBAC), an active
	// graph's commit + source_tree_hash, and a small set of seeded symbols /
	// files / edges so explore/search/node/callers/callees/impact return
	// non-empty results in mock mode.
	codebases map[string]*mockCodebase

	// skill store for the skill_* tools (design-docs/19 §6.2/§6.3). A skill is
	// a knowledge_assets row owned by a workspace; the mock models the delivery
	// surface: a skill id → workspace + SKILL.md header + manifest (files) +
	// delivery_mode the agent's effective binding yields. The agent-level gate
	// is approximated by the workspace read ACL (canRead on the skill's
	// workspace) — an unbound agent sees ErrNotExist (no leak — §8.2). Proposals
	// land a candidate in-memory (never published).
	skills    map[string]*mockSkill
	proposals map[string]*SkillProposeResult
}

// mockSkill is the in-memory model the skill_* tools query. It binds a skill
// id to its workspace (so canRead gates it — §8.2 no-leak) and holds the
// SKILL.md header frontmatter + the file manifest (paths + hashes) so
// skill_read / skill_resources are exercisable. DeliveryMode models the
// agent's effective binding: tool/inline permit raw reads; summary does not.
type mockSkill struct {
	WorkspaceID  string
	Header       map[string]any
	DeliveryMode string
	ContentHash  string
	Manifest     SkillManifest
	// resource bytes keyed by manifest path (progressive read backing).
	Resources map[string][]byte
	// VersionNo + AssetVersionID for the skill_read envelope.
	VersionNo      int64
	AssetVersionID string
}

// AddWikiSpace seeds a Wiki Space status record for wiki_status tests.
func (m *Mock) AddWikiSpace(st WikiSpaceStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.wikiSpaces[st.WikiSpaceID] = &st
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
		wikiSpaces:  make(map[string]*WikiSpaceStatus),
		wikiRuns:    make(map[string]*WikiPageProposeResult),
		codebases:   make(map[string]*mockCodebase),
		skills:      make(map[string]*mockSkill),
		proposals:   make(map[string]*SkillProposeResult),
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

// WikiStatus returns the Wiki Space status (§7.3). Read permission on the
// Space's workspace is required; otherwise ErrNotExist (§8.2 — no leak).
func (m *Mock) WikiStatus(_ context.Context, auth *AuthContext, wikiSpaceID string) (*WikiSpaceStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.wikiSpaces[wikiSpaceID]
	if !ok || !m.canRead(auth, st.WorkspaceID) {
		return nil, ErrNotExist()
	}
	out := *st
	return &out, nil
}

// WikiPagePropose lands a candidate proposal run (§7.3/§11.3). Write perm on
// the Space's workspace required; the run is recorded in-memory so a poll of
// wiki_status observes it. Never publishes directly.
func (m *Mock) WikiPagePropose(_ context.Context, auth *AuthContext, req WikiPageProposeRequest) (*WikiPageProposeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.wikiSpaces[req.WikiSpaceID]
	if !ok || !m.canWrite(auth, st.WorkspaceID) {
		return nil, domainerr.ErrForbidden
	}
	// Deterministic run id without time/rand: workspace + page + run count.
	n := len(m.wikiRuns)
	runID := "run-" + req.WikiSpaceID + "-" + req.PageKey + "-" + itoa(n+1)
	res := &WikiPageProposeResult{
		RunID:   runID,
		Status:  "queued",
		PageKey: req.PageKey,
	}
	m.wikiRuns[runID] = res
	// Surface the run in the Space's last_run so a follow-up wiki_status sees it.
	st.LastRun = &MaintenanceRun{
		ID: runID, TriggerType: "ingest", Status: "queued",
	}
	return res, nil
}

// --- CodeGraph mock surface (design-docs/17 §6.2) ---

// MockCodebase is the in-memory model a code_* tool queries (design-docs/17
// §6.2). It binds a codebase id to its workspace (so canRead gates it — §8.2
// no-leak) and holds the active graph's commit + source_tree_hash + seeded
// files / symbols / edges. The mock is deliberately tiny: it proves the tool
// wiring + RBAC gate, not the graph engine.
type MockCodebase struct {
	WorkspaceID    string
	Commit         string
	SourceTreeHash string
	Files          []CodeFileNode
	Symbols        []CodeNodeDef
	Edges          []CodeEdge // calls|defines|implements
}

// internal alias keeps the field-name code below stable.
type mockCodebase = MockCodebase

// AddCodebase seeds a codebase for the code_* tools. Deterministic commit +
// hash are filled when the seeder left them blank so §3.2 "every result carries
// a commit" holds without time/rand.
func (m *Mock) AddCodebase(cb MockCodebase) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Deterministic commit + hash when the seeder left them blank so §3.2
	// "every result carries a commit" holds without time/rand.
	if cb.Commit == "" {
		cb.Commit = "commit-" + cb.WorkspaceID
	}
	if cb.SourceTreeHash == "" {
		cb.SourceTreeHash = "sha256:" + cb.Commit
	}
	id := "codebase-" + cb.WorkspaceID
	m.codebases[id] = &cb
}

// resolveCodebase is the single RBAC chokepoint for the code_* tools. A missing
// codebase OR a caller without read access on its workspace → ErrNotExist
// (§8.2 no-leak — the Agent cannot tell not-found from not-allowed). Caller
// must hold the read lock.
func (m *Mock) resolveCodebase(auth *AuthContext, codebaseID string) (*mockCodebase, bool) {
	cb, ok := m.codebases[codebaseID]
	if !ok || !m.canRead(auth, cb.WorkspaceID) {
		return nil, false
	}
	return cb, true
}

// --- Skill mock surface (design-docs/19 §6.2 / §6.3, Phase 5-4) ---

// MockSkill is the seeder shape for AddSkill: the skill's workspace binding
// (for RBAC), the SKILL.md header frontmatter, the effective delivery_mode,
// and the manifest files (path + bytes + hash). A nil/empty header Name falls
// back to id-derived so skill_list always carries a name.
type MockSkill struct {
	ID             string
	WorkspaceID    string
	Header         map[string]any
	DeliveryMode   string
	Manifest       []SkillFileEntry
	Resources      map[string][]byte
	VersionNo      int64
	AssetVersionID string
}

// AddSkill seeds a skill for the skill_* tools. The id MUST be stable (the
// skill_* tools key on it). DeliveryMode defaults to "tool" (full manifest +
// progressive reads) when the seeder left it blank. ContentHash is filled
// deterministically when blank so skill_read's content_hash anchor holds
// without time/rand. Resources may be nil (skill_resources then yields
// ErrNotExist — the manifest entry has no bytes, like a symlink).
func (m *Mock) AddSkill(s MockSkill) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.DeliveryMode == "" {
		s.DeliveryMode = "tool"
	}
	if s.AssetVersionID == "" {
		s.AssetVersionID = "aver-" + s.ID
	}
	skill := &mockSkill{
		WorkspaceID:    s.WorkspaceID,
		Header:         s.Header,
		DeliveryMode:   s.DeliveryMode,
		ContentHash:    "sha256:" + s.ID,
		Resources:      s.Resources,
		VersionNo:      s.VersionNo,
		AssetVersionID: s.AssetVersionID,
	}
	if skill.Header == nil {
		skill.Header = map[string]any{"name": s.ID}
	}
	if _, ok := skill.Header["name"].(string); !ok || skill.Header["name"] == "" {
		skill.Header["name"] = s.ID
	}
	// Build the manifest: entry_count + total_size derived from the seeder's
	// files; hashes filled deterministically when blank.
	var total int64
	for i := range s.Manifest {
		f := s.Manifest[i]
		if f.Hash == "" {
			f.Hash = "sha256:" + f.Path
		}
		total += f.Size
		skill.Manifest.Files = append(skill.Manifest.Files, f)
	}
	skill.Manifest.EntryCount = len(skill.Manifest.Files)
	skill.Manifest.TotalSize = total
	skill.Manifest.ContentHash = skill.ContentHash
	m.skills[s.ID] = skill
}

// resolveSkill is the single RBAC chokepoint for the skill_* tools. A missing
// skill OR a caller without read access on its workspace → ErrNotExist (§8.2
// no-leak — the Agent cannot tell not-found from not-allowed). Caller must
// hold the read lock.
func (m *Mock) resolveSkill(auth *AuthContext, skillID string) (*mockSkill, bool) {
	sk, ok := m.skills[skillID]
	if !ok || !m.canRead(auth, sk.WorkspaceID) {
		return nil, false
	}
	return sk, true
}

// SkillList (skill_list). Enumerates the skills the agent's workspace ACL
// grants read on (the mock approximates the agent-level binding gate with the
// workspace read ACL — an unbound agent sees an empty list, no leak — §8.2).
// A nil auth → empty list.
func (m *Mock) SkillList(_ context.Context, auth *AuthContext) (*SkillListResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	items := make([]SkillListItem, 0)
	if auth == nil {
		return &SkillListResult{Items: items, Total: 0}, nil
	}
	a := m.aclFor(auth.IdentityID)
	for id, sk := range m.skills {
		if a == nil {
			continue
		}
		if _, ok := a.read[sk.WorkspaceID]; !ok {
			continue // RBAC gate: invisible skills never returned
		}
		item := SkillListItem{
			AssetID:      id,
			Name:         id,
			DeliveryMode: sk.DeliveryMode,
			ContentHash:  sk.ContentHash,
			VersionNo:    sk.VersionNo,
		}
		if n, ok := sk.Header["name"].(string); ok && n != "" {
			item.Name = n
		}
		if d, ok := sk.Header["description"].(string); ok && d != "" {
			item.Description = d
		}
		if v, ok := sk.Header["version"].(string); ok && v != "" {
			item.Version = v
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return &SkillListResult{Items: items, Total: len(items)}, nil
}

// SkillRead (skill_read). Returns the SKILL.md header + manifest, trimmed by
// the skill's delivery_mode (summary mode gets a capability_summary projection,
// no raw manifest). Missing/no-perm → ErrNotExist (no leak — §8.2).
func (m *Mock) SkillRead(_ context.Context, auth *AuthContext, assetID, versionSpec string) (*SkillReadResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sk, ok := m.resolveSkill(auth, assetID)
	if !ok {
		return nil, ErrNotExist()
	}
	_ = versionSpec // mock has a single version per skill
	res := &SkillReadResult{
		AssetID:             assetID,
		AssetVersionID:      sk.AssetVersionID,
		VersionNo:           sk.VersionNo,
		DeliveryMode:        sk.DeliveryMode,
		Header:              sk.Header,
		CompatibilityReport: map[string]any{"status": "ok"},
		ContentHash:         sk.ContentHash,
	}
	switch sk.DeliveryMode {
	case "summary":
		// Summary only: capability summary projection, no raw file list.
		res.CapabilitySummary = map[string]any{
			"entry_count": sk.Manifest.EntryCount,
			"total_size":  sk.Manifest.TotalSize,
		}
	default: // tool / inline
		manifest := sk.Manifest
		res.Manifest = &manifest
	}
	return res, nil
}

// SkillResources (skill_resources). Progressively reads one manifest file.
// Summary-mode bindings do not permit raw reads → ErrNotExist (no leak —
// §8.2). The path must match a manifest entry. A manifest entry with no
// resource bytes (symlink-like) → ErrNotExist.
func (m *Mock) SkillResources(_ context.Context, auth *AuthContext, assetID, versionSpec, resourcePath string) (*SkillResourceContent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	sk, ok := m.resolveSkill(auth, assetID)
	if !ok {
		return nil, ErrNotExist()
	}
	_ = versionSpec
	if sk.DeliveryMode == "summary" {
		// Summary mode does not permit raw resource reads — the agent received
		// only the capability summary, not the file inventory.
		return nil, ErrNotExist()
	}
	for _, f := range sk.Manifest.Files {
		if f.Path == resourcePath {
			body, hasBytes := sk.Resources[resourcePath]
			if !hasBytes {
				// A manifest entry with no bytes (symlink-like) is not readable.
				return nil, ErrNotExist()
			}
			return &SkillResourceContent{
				Path:        f.Path,
				Hash:        f.Hash,
				Kind:        f.Kind,
				Content:     body,
				ContentHash: sk.ContentHash,
			}, nil
		}
	}
	return nil, ErrNotExist()
}

// SkillPropose (skill_propose). Lands a candidate proposal in-memory (never
// published). Write perm on the workspace required (canWrite); a read-only /
// no-context caller is refused as ErrNotExist (mapped upstream to 404, no
// leak — §8.2). The candidate is stored so a follow-up could track it.
func (m *Mock) SkillPropose(_ context.Context, auth *AuthContext, req SkillProposeRequest) (*SkillProposeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.canWrite(auth, req.WorkspaceID) {
		// Write-denied is indistinguishable from not-found (no leak — §8.2).
		return nil, ErrNotExist()
	}
	n := len(m.proposals)
	// Deterministic ids without time/rand: workspace + name + count.
	assetID := "skill-" + req.WorkspaceID + "-" + req.Name + "-" + itoa(n+1)
	res := &SkillProposeResult{
		AssetID:         assetID,
		AssetVersionID:  "aver-" + assetID,
		ReviewRequestID: "review-" + assetID,
		Status:          "candidate",
	}
	m.proposals[assetID] = res
	return res, nil
}

// CodeStatus (code_status). Read-gated; missing/no-perm → ErrNotExist.
func (m *Mock) CodeStatus(_ context.Context, auth *AuthContext, codebaseID string) (*CodeGraphStatus, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cb, ok := m.resolveCodebase(auth, codebaseID)
	if !ok {
		return nil, ErrNotExist()
	}
	symbols := len(cb.Symbols)
	edges := len(cb.Edges)
	files := len(cb.Files)
	return &CodeGraphStatus{
		Commit:          cb.Commit,
		SourceTreeHash:  cb.SourceTreeHash,
		ProviderVersion: "mock-1.0",
		Stats:           CodeGraphBuildStats{Files: files, Symbols: symbols, Edges: edges},
	}, nil
}

// CodeFiles (code_files). Read-gated; pathPrefix filters naively.
func (m *Mock) CodeFiles(_ context.Context, auth *AuthContext, codebaseID, pathPrefix string) (*CodeFileTree, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cb, ok := m.resolveCodebase(auth, codebaseID)
	if !ok {
		return nil, ErrNotExist()
	}
	tree := &CodeFileTree{Path: ""}
	for _, f := range cb.Files {
		if pathPrefix == "" || strings.HasPrefix(f.Path, pathPrefix) {
			tree.Files = append(tree.Files, f)
		}
	}
	return tree, nil
}

// CodeSearch (code_search). Naive substring match over the seeded symbols'
// signatures + docstrings. Empty when no matches (normal success).
func (m *Mock) CodeSearch(_ context.Context, auth *AuthContext, codebaseID string, req CodeSearchQuery) (*CodeHits, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cb, ok := m.resolveCodebase(auth, codebaseID)
	if !ok {
		return nil, ErrNotExist()
	}
	q := strings.ToLower(req.Query)
	hits := []CodeHit{}
	for _, s := range cb.Symbols {
		hay := strings.ToLower(s.Signature + " " + s.Docstring + " " + s.Loc.Symbol)
		if q != "" && strings.Contains(hay, q) {
			hits = append(hits, CodeHit{Loc: s.Loc, Snippet: s.Signature})
		}
	}
	hits = trimHits(hits, req.Limit)
	return &CodeHits{Items: hits, Commit: cb.Commit}, nil
}

// CodeExplore (code_explore). Returns matching symbols as hits + nodes.
func (m *Mock) CodeExplore(_ context.Context, auth *AuthContext, codebaseID string, req CodeExploreQuery) (*CodeExploreResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cb, ok := m.resolveCodebase(auth, codebaseID)
	if !ok {
		return nil, ErrNotExist()
	}
	q := strings.ToLower(req.Query)
	hits := []CodeHit{}
	nodes := []CodeNodeDef{}
	for _, s := range cb.Symbols {
		hay := strings.ToLower(s.Signature + " " + s.Docstring + " " + s.Loc.Symbol)
		if q == "" || strings.Contains(hay, q) {
			hits = append(hits, CodeHit{Loc: s.Loc, Snippet: s.Signature})
			nodes = append(nodes, s)
		}
	}
	hits = trimHits(hits, req.Limit)
	return &CodeExploreResult{Hits: hits, Nodes: nodes, Commit: cb.Commit}, nil
}

// CodeNode (code_node). Resolves the first symbol matching req.Symbol (path
// disambiguates when provided). Missing symbol → nil node (empty result, not
// an error — §15 authorized-empty).
func (m *Mock) CodeNode(_ context.Context, auth *AuthContext, codebaseID string, req CodeSymbolQuery) (*CodeNodeDef, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cb, ok := m.resolveCodebase(auth, codebaseID)
	if !ok {
		return nil, ErrNotExist()
	}
	for _, s := range cb.Symbols {
		if s.Loc.Symbol == req.Symbol && (req.Path == "" || s.Loc.Path == req.Path) {
			out := s
			return &out, nil
		}
	}
	return nil, nil
}

// CodeCallers (code_callers). Edges where To.Symbol == req.Symbol.
func (m *Mock) CodeCallers(_ context.Context, auth *AuthContext, codebaseID string, req CodeSymbolQuery) (*CodeEdges, error) {
	return m.codeEdgesFor(auth, codebaseID, req, func(e CodeEdge) bool {
		return e.To.Symbol == req.Symbol && (req.Path == "" || e.To.Path == req.Path)
	})
}

// CodeCallees (code_callees). Edges where From.Symbol == req.Symbol.
func (m *Mock) CodeCallees(_ context.Context, auth *AuthContext, codebaseID string, req CodeSymbolQuery) (*CodeEdges, error) {
	return m.codeEdgesFor(auth, codebaseID, req, func(e CodeEdge) bool {
		return e.From.Symbol == req.Symbol && (req.Path == "" || e.From.Path == req.Path)
	})
}

// codeEdgesFor is the shared callers/callees selector. Read-gated; empty on no
// matches (authorized-empty, §15).
func (m *Mock) codeEdgesFor(auth *AuthContext, codebaseID string, req CodeSymbolQuery, match func(CodeEdge) bool) (*CodeEdges, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cb, ok := m.resolveCodebase(auth, codebaseID)
	if !ok {
		return nil, ErrNotExist()
	}
	out := []CodeEdge{}
	for _, e := range cb.Edges {
		if match(e) {
			out = append(out, e)
		}
	}
	return &CodeEdges{Items: out}, nil
}

// CodeImpact (code_impact). Naive transitive closure over call edges up to
// req.Depth (default 2): the symbol + everything that calls it (recursively).
func (m *Mock) CodeImpact(_ context.Context, auth *AuthContext, codebaseID string, req CodeImpactQuery) (*CodeHits, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cb, ok := m.resolveCodebase(auth, codebaseID)
	if !ok {
		return nil, ErrNotExist()
	}
	depth := req.Depth
	if depth <= 0 {
		depth = 2
	}
	// BFS over callers (edges where To == current). Seed with the named symbol.
	seen := map[string]bool{req.Symbol: true}
	frontier := []string{req.Symbol}
	hits := []CodeHit{}
	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, sym := range frontier {
			for _, e := range cb.Edges {
				if e.To.Symbol == sym && !seen[e.From.Symbol] {
					seen[e.From.Symbol] = true
					next = append(next, e.From.Symbol)
					hits = append(hits, CodeHit{Loc: e.From, Score: float64(depth - d)})
				}
			}
		}
		frontier = next
	}
	return &CodeHits{Items: hits, Commit: cb.Commit}, nil
}

// --- Context Broker converged-retrieval mock surface (design-docs/19 §11.4,
// Phase 6-5) ---
// The mock approximates the Broker's converged retrieval so the knowledge_search
// / knowledge_context MCP tools are exercisable in mock mode. It does NOT
// re-implement the Broker's ten-step pipeline (intent routing, authority
// policy scoring, budgeter, citation builder) — it folds the mock's seeded
// documents / codebases / skills into unified KnowledgeCandidate shapes, gated
// by the workspace read ACL (canRead), and keeps the three empty states
// distinguishable (§7.3): no read grant → empty-authorized, no seeded match →
// empty-no-results, never an error to the Agent (§8.2 no-leak). Type-specialized
// tools are NOT flattened here — this is only the converged retrieval path.

// knowledgeIntent guesses the §4.1 intent from the query for the mock's
// candidate ordering. The real Broker routes via IntentRouter (§4.2); the mock
// only needs a stable label so the response carries an intent.
func knowledgeIntent(q string) string {
	low := strings.ToLower(q)
	switch {
	case strings.Contains(low, "为什么") || strings.Contains(low, "决策") || strings.Contains(low, "why") || strings.Contains(low, "rationale"):
		return "rationale"
	case strings.Contains(low, "实现") || strings.Contains(low, "代码") || strings.Contains(low, "函数") || strings.Contains(low, "commit") || strings.Contains(low, "revision"):
		return "revision"
	case strings.Contains(low, "如何") || strings.Contains(low, "流程") || strings.Contains(low, "步骤") || strings.Contains(low, "how") || strings.Contains(low, "runbook"):
		return "procedure"
	default:
		return "spec"
	}
}

// knowledgeCandidates folds the mock's seeded documents (and codebases/skills
// when their ids match the asset_types filter) into unified candidates. It is
// the mock's stand-in for the Broker's parallel port fan-out + dedup. Caller
// must hold the read lock. An empty filter set = all types (§4.2 rule 1).
func (m *Mock) knowledgeCandidates(auth *AuthContext, req knowledgeCandidateQuery) []KnowledgeCandidate {
	a := m.aclFor(auth.IdentityID)
	q := strings.ToLower(req.Query)
	wantType := map[string]bool{}
	for _, t := range req.AssetTypes {
		wantType[t] = true
	}
	out := make([]KnowledgeCandidate, 0)
	// Documents → document candidates (version anchor = version_no-derived id).
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
		if len(wantType) > 0 && !wantType["document"] {
			continue
		}
		body := strings.ToLower(doc.content)
		title := strings.ToLower(doc.Title)
		if q != "" && !strings.Contains(body, q) && !strings.Contains(title, q) {
			continue
		}
		score := 0.5
		if q == "" || strings.Contains(title, q) {
			score = 0.9
		}
		out = append(out, KnowledgeCandidate{
			AssetID:   doc.ID,
			AssetType: "document",
			Title:     doc.Title,
			Snippet:   makeSnippet(doc.content, q),
			Score:     score,
			Authority: 0.9,
			Freshness: 0.8,
			Citation: CandidateCitation{
				VersionOrRevision: itoa(doc.VersionNo),
				Locator:           map[string]any{"workspace_id": doc.WorkspaceID},
			},
		})
	}
	// Codebases → codebase candidates (version anchor = commit). A codebase is
	// surfaced as one candidate per matching symbol; the mock folds the first
	// symbol hit so code_intent queries return something in mock mode.
	for id, cb := range m.codebases {
		if req.WorkspaceID != "" && cb.WorkspaceID != req.WorkspaceID {
			continue
		}
		if a == nil {
			continue
		}
		if _, ok := a.read[cb.WorkspaceID]; !ok {
			continue
		}
		if len(wantType) > 0 && !wantType["codebase"] {
			continue
		}
		for _, s := range cb.Symbols {
			hay := strings.ToLower(s.Signature + " " + s.Docstring + " " + s.Loc.Symbol)
			if q == "" || strings.Contains(hay, q) {
				out = append(out, KnowledgeCandidate{
					AssetID:   id,
					AssetType: "codebase",
					Title:     s.Loc.Symbol,
					Snippet:   s.Signature,
					Score:     0.6,
					Authority: 0.5,
					Citation: CandidateCitation{
						VersionOrRevision: cb.Commit,
						Locator: map[string]any{
							"file":       s.Loc.Path,
							"start_line": s.Loc.StartLine,
							"symbol":     s.Loc.Symbol,
						},
					},
				})
				break // one candidate per codebase in mock mode
			}
		}
	}
	// Skills → skill candidates (version anchor = package version).
	for id, sk := range m.skills {
		if req.WorkspaceID != "" && sk.WorkspaceID != req.WorkspaceID {
			continue
		}
		if a == nil {
			continue
		}
		if _, ok := a.read[sk.WorkspaceID]; !ok {
			continue
		}
		if len(wantType) > 0 && !wantType["skill"] {
			continue
		}
		name, _ := sk.Header["name"].(string)
		if name == "" {
			name = id
		}
		desc, _ := sk.Header["description"].(string)
		hay := strings.ToLower(name + " " + desc)
		if q != "" && !strings.Contains(hay, q) {
			continue
		}
		out = append(out, KnowledgeCandidate{
			AssetID:   id,
			AssetType: "skill",
			Title:     name,
			Snippet:   desc,
			Score:     0.5,
			Authority: 0.3,
			Citation: CandidateCitation{
				VersionOrRevision: sk.AssetVersionID,
			},
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if req.MaxItems > 0 && len(out) > req.MaxItems {
		out = out[:req.MaxItems]
	}
	return out
}

// knowledgeCandidateQuery is the narrowed scope the mock fan-out reads.
type knowledgeCandidateQuery struct {
	Query       string
	WorkspaceID string
	AssetTypes  []string
	MaxItems    int
}

// KnowledgeSearch (knowledge_search). Folds seeded assets into converged
// candidates, gated by the workspace read ACL. No read grant / no match →
// empty result (the three states are indistinguishable at the mock layer but
// never an error to the Agent — §8.2 no-leak). Intent is guessed for the
// response label; policy_version/authz_revision are mock placeholders.
func (m *Mock) KnowledgeSearch(_ context.Context, auth *AuthContext, req KnowledgeSearchRequest) (*KnowledgeSearchResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if auth == nil {
		return &KnowledgeSearchResult{Candidates: []KnowledgeCandidate{}, Intent: knowledgeIntent(req.Query)}, nil
	}
	cands := m.knowledgeCandidates(auth, knowledgeCandidateQuery{
		Query:       req.Query,
		WorkspaceID: req.WorkspaceID,
		AssetTypes:  req.AssetTypes,
		MaxItems:    req.MaxItems,
	})
	return &KnowledgeSearchResult{
		Intent:        knowledgeIntent(req.Query),
		PolicyVersion: 1,
		AuthzRevision: 1,
		Candidates:    cands,
	}, nil
}

// KnowledgeContext (knowledge_context). Same converged fan-out as
// KnowledgeSearch, plus a mock truncation report when the candidate count
// exceeds max_items (naming the type continue-tools — §6.2 never silently
// truncates). include_content is honored by omitting the snippet body when
// false (directory + citation only — the body is a progressive read, §6.2).
func (m *Mock) KnowledgeContext(_ context.Context, auth *AuthContext, req KnowledgeContextRequest) (*KnowledgeContextResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if auth == nil {
		return &KnowledgeContextResult{Candidates: []KnowledgeCandidate{}, Intent: knowledgeIntent(req.Query)}, nil
	}
	maxItems := req.MaxItems
	cands := m.knowledgeCandidates(auth, knowledgeCandidateQuery{
		Query:       req.Query,
		WorkspaceID: req.WorkspaceID,
		AssetTypes:  req.AssetTypes,
		MaxItems:    0, // fetch all, then truncate so the report names the dropped ids
	})
	var trunc TruncationReport
	if maxItems > 0 && len(cands) > maxItems {
		dropped := cands[maxItems:]
		ids := make([]string, 0, len(dropped))
		for _, c := range dropped {
			ids = append(ids, c.AssetID)
		}
		trunc = TruncationReport{
			Reason:            "budget_full",
			TruncatedAssetIDs: ids,
			ContinueTools:     []string{"get_document", "code_node", "skill_resources"},
		}
		cands = cands[:maxItems]
	}
	// include_content=false → directory + citation only (§6.2): drop the snippet
	// body so the agent gets the catalog, then progressive-reads the body via
	// the type tool.
	if !req.IncludeContent {
		for i := range cands {
			cands[i].Snippet = ""
		}
	}
	return &KnowledgeContextResult{
		Intent:        knowledgeIntent(req.Query),
		PolicyVersion: 1,
		AuthzRevision: 1,
		Candidates:    cands,
		Truncation:    trunc,
	}, nil
}

// trimHits caps a hit slice at limit (0 = unlimited).
func trimHits(hits []CodeHit, limit int) []CodeHit {
	if limit > 0 && len(hits) > limit {
		return hits[:limit]
	}
	return hits
}

// itoa is a tiny int->string to avoid pulling strconv into the mock.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
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
