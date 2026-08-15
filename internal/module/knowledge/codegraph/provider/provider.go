// Package provider defines the CodeGraph provider port (design-docs/17 §3,
// 12 §10.2). The provider receives an already-authorized, read-only snapshot
// locator and commit — never Git credentials. It returns graph artifacts, query
// results and diagnostics. Path normalization, source_tree_hash validation,
// projection registration, CAS activation and cleanup are Mora's job — the
// provider cannot widen its read scope (§10.1 prompt-injection guard).
//
// Implementations:
//   - internal/infra/codegraph.NoopProvider — deterministic fallback when no
//     sidecar is configured (returns capability_unavailable, never synthesizes
//     results, §3.3).
//   - internal/infra/codegraph.SidecarProvider — the sidecar RPC adapter; the
//     real codegraph sidecar is a stdio-MCP / Unix-socket daemon (per the
//     YS-131 deployment retrospective), not an HTTP server, so this adapter
//     speaks the daemon-socket protocol with mTLS/short-lived service creds.
//
// The worker (internal/module/knowledge/worker) bridges a concrete provider to
// the build handler; the service (internal/module/knowledge/codegraph/service)
// owns the query-time validation + fail-closed path. MCP tools MUST NOT bypass
// the provider (§10.2 red line).
package provider

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Capability is the signed capability envelope the provider operates under
// (12 §10.1 / 17 §3.1). Mora computes WorkspaceID + AuthzRevision from the
// acting principal's RBAC, issues a DecisionID via authz.Service, and trims the
// visible asset set + read budget. The provider MUST NOT read beyond this
// scope; the snapshot locator + commit are fixed at call time.
type Capability struct {
	WorkspaceID     uuid.UUID   `json:"workspace_id"`
	AuthzRevision   int64       `json:"authz_revision"`
	DecisionID      uuid.UUID   `json:"decision_id"` // from authz.Service.IssueDecision
	ExpiresAt       time.Time   `json:"expires_at"`
	AllowedAssetIDs []uuid.UUID `json:"allowed_asset_ids,omitempty"` // trimmed visible codebase assets
	MaxReadBytes    int         `json:"max_read_bytes,omitempty"`
	MaxReadFiles    int         `json:"max_read_files,omitempty"`
	MaxResults      int         `json:"max_results,omitempty"`
}

// CodeGraphCapabilities describes what the provider can do (17 §3.1). It is the
// static capability snapshot the provider advertises; a language/operation that
// did not pass the §7.2 contract test is NOT exposed via MCP (§6.2).
type CodeGraphCapabilities struct {
	Languages       []string `json:"languages"`         // declared supported languages
	Operations      []string `json:"operations"`        // explore|search|files|node|callers|callees|impact|status
	MaxRepoSize     int64    `json:"max_repo_size,omitempty"`
	MaxFiles        int      `json:"max_files,omitempty"`
	IncrementalSync bool     `json:"incremental_sync,omitempty"`
	IndexSchemaVer  string   `json:"index_schema_version,omitempty"`
	ExtractionVer   string   `json:"extraction_version,omitempty"`
}

// Locator is the non-executable snapshot placement the provider receives
// (17 §3.1 BuildRequest.SnapshotLocator). It names a MinIO key prefix / temp
// path that holds the materialized read-only source tree — never credentials,
// never a runnable checkout locator the provider could mutate.
type Locator struct {
	// ObjectStorePrefix is the MinIO key prefix holding the snapshot (codebase
	// Asset version content). The provider materializes from this, read-only.
	ObjectStorePrefix string `json:"object_store_prefix,omitempty"`
	// TempPath is an already-materialized read-only working tree path, when the
	// build handler pre-materialized it (Phase 3 path). Empty when the provider
	// materializes from ObjectStorePrefix itself.
	TempPath string `json:"temp_path,omitempty"`
}

// BuildRequest is the input to Build (17 §3.1). Only a read-only snapshot
// locator + commit + the caller-computed source_tree_hash are carried — no
// Git secrets. The provider builds its graph + source tree from the snapshot.
type BuildRequest struct {
	SnapshotLocator Locator    `json:"snapshot_locator"`
	Commit          string     `json:"commit"`           // fixed commit_sha
	SourceTreeHash  string     `json:"source_tree_hash"` // materialized working-tree hash (Mora-computed)
	Capability      Capability `json:"capability"`
}

// BuildStats are the build-time counts returned with BuildResult (17 §3.1).
type BuildStats struct {
	Files  int `json:"files"`
	Symbols int `json:"symbols"`
	Edges  int `json:"edges"`
}

// BuildResult is the build outcome (17 §3.1 / §10.2). The field set is fixed by
// the contract: graph_ref / source_tree_ref / commit / source_tree_hash /
// provider_version / provider_build_digest / index_schema_version /
// extraction_version / capabilities_snapshot. Mora verifies Commit ==
// BuildRequest.Commit and SourceTreeHash == BuildRequest.SourceTreeHash before
// registering the projection; a mismatch is fail-closed (discard, no build).
type BuildResult struct {
	GraphRef             string               `json:"graph_ref"`             // provider-side graph handle
	SourceTreeRef        string               `json:"source_tree_ref"`       // provider-side read-only source-tree handle (same lifetime as graph_ref)
	Commit               string               `json:"commit"`               // MUST equal BuildRequest.Commit
	SourceTreeHash       string               `json:"source_tree_hash"`      // MUST equal BuildRequest.SourceTreeHash
	ProviderVersion      string               `json:"provider_version"`
	ProviderBuildDigest  string               `json:"provider_build_digest"`
	IndexSchemaVersion   string               `json:"index_schema_version"`
	ExtractionVersion    string               `json:"extraction_version"`
	CapabilitiesSnapshot CodeGraphCapabilities `json:"capabilities_snapshot"`
	Stats               BuildStats           `json:"stats,omitempty"`
}

// CodeLoc is the location anchor every result MUST carry (17 §3.2): commit,
// file path, line range, symbol. A result without a commit is never returned —
// an expired revision must NOT masquerade as the current result (§4.2).
type CodeLoc struct {
	Commit    string `json:"commit"`
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line,omitempty"`
	Symbol    string `json:"symbol,omitempty"` // symbol name (definer / caller)
	Kind      string `json:"kind,omitempty"`   // function|method|class|...
}

// CodeHit is a search/explore/impact hit carrying its location + score + snippet.
type CodeHit struct {
	Loc     CodeLoc `json:"loc"`
	Score   float64 `json:"score,omitempty"`
	Snippet string  `json:"snippet,omitempty"`
}

// CodeEdge is a directed relationship between two symbols (calls|defines|implements).
type CodeEdge struct {
	From CodeLoc `json:"from"`
	To   CodeLoc `json:"to"`
	Kind string  `json:"kind"` // calls|defines|implements
}

// CodeNode is a single symbol definition with its signature + doc.
type CodeNode struct {
	Loc       CodeLoc `json:"loc"`
	Kind      string  `json:"kind"`
	Signature string `json:"signature,omitempty"`
	Docstring string `json:"docstring,omitempty"`
}

// FileTree is the files listing (code_files). Nodes are nested directory
// entries; leaves carry a CodeLoc-less file descriptor.
type FileTree struct {
	Path  string     `json:"path"`
	Files []FileNode `json:"files,omitempty"`
	Dirs  []FileTree `json:"dirs,omitempty"`
}

// FileNode is one file in a FileTree.
type FileNode struct {
	Path   string `json:"path"`
	Lines  int    `json:"lines,omitempty"`
	Commit string `json:"commit"`
}

// --- query request/result types (17 §3.1 CodeGraphProvider methods) ---

// ExploreRequest is the combined query for code_explore (§6.2).
type ExploreRequest struct {
	Query    string   `json:"query"`
	Language string   `json:"language,omitempty"`
	Limit    int      `json:"limit,omitempty"`
}

// ExploreResult is the explore outcome: hits + the symbols they resolve to.
type ExploreResult struct {
	Hits    []CodeHit `json:"hits,omitempty"`
	Nodes   []CodeNode `json:"nodes,omitempty"`
	Commit  string    `json:"commit"`
}

// CodeSearchRequest is the code_search input (§6.2).
type CodeSearchRequest struct {
	Query    string   `json:"query"`
	Language string   `json:"language,omitempty"`
	PathGlob string   `json:"path_glob,omitempty"`
	Limit    int      `json:"limit,omitempty"`
}

// FilesRequest is the code_files input (§6.2).
type FilesRequest struct {
	PathPrefix string `json:"path_prefix,omitempty"`
}

// NodeRequest names a symbol to resolve (code_node / code_callers / code_callees).
type NodeRequest struct {
	Symbol   string `json:"symbol"`
	Language string `json:"language,omitempty"`
	Path     string `json:"path,omitempty"` // disambiguator when the symbol name repeats
}

// ImpactRequest is the code_impact input (§6.2): "what does changing X affect".
type ImpactRequest struct {
	Symbol   string `json:"symbol"`
	Language string `json:"language,omitempty"`
	Path     string `json:"path,omitempty"`
	Depth    int    `json:"depth,omitempty"`
}

// GraphStatus is the code_status result: active graph version metadata.
type GraphStatus struct {
	Commit            string               `json:"commit"`
	SourceTreeHash    string               `json:"source_tree_hash"`
	ProviderVersion   string               `json:"provider_version"`
	IndexSchemaVersion string              `json:"index_schema_version"`
	Capabilities       CodeGraphCapabilities `json:"capabilities"`
	Stats             BuildStats           `json:"stats,omitempty"`
	Stale             bool                 `json:"stale,omitempty"` // a newer build failed; serving last good graph (§15)
}

// Sentinel capability errors (17 §15 fault table). The service maps these to the
// fail-closed response metadata: the system MUST NOT confuse provider fault,
// authorized-empty, and genuine-no-results (§15).
var (
	// ErrCapabilityUnavailable: the provider is not configured / the sidecar is
	// down. Queries return graph version metadata only + this sentinel; the
	// caller surfaces capability_unavailable, never faked results.
	ErrCapabilityUnavailable = errors.New("codegraph: capability_unavailable")
	// ErrSourceSnapshotUnavailable: the source tree is missing or its hash does
	// not match (§4.2). Fail closed — do NOT return possibly-misaligned source.
	ErrSourceSnapshotUnavailable = errors.New("codegraph: source_snapshot_unavailable")
	// ErrAssetVersionMismatch: capability.asset_version ≠ graph_ref-bound version
	// (§4.2 / §7.2). The query is rejected.
	ErrAssetVersionMismatch = errors.New("codegraph: capability asset_version mismatch")
)

// CodeGraphProvider is the port (12 §10.2 / 17 §3.1). A concrete provider
// (sidecar) implements it; the worker + service bridge it. MCP tools route
// through the service, never directly to a third-party ToolHandler (§10.2).
//
// Build is the only write-ish method (creates a graph); it receives no Git
// credentials. The read methods receive a graph_ref the build returned. Health
// + Capabilities back the Compose healthcheck / §7.2 contract tests. Delete
// retires a graph (administrative; NOT in the default agent toolset, §11.3).
type CodeGraphProvider interface {
	Capabilities(ctx context.Context) (CodeGraphCapabilities, error)
	Build(ctx context.Context, req BuildRequest) (BuildResult, error)
	Explore(ctx context.Context, graphRef string, req ExploreRequest) (ExploreResult, error)
	Search(ctx context.Context, graphRef string, req CodeSearchRequest) ([]CodeHit, error)
	Files(ctx context.Context, graphRef string, req FilesRequest) (FileTree, error)
	Node(ctx context.Context, graphRef string, req NodeRequest) (CodeNode, error)
	Callers(ctx context.Context, graphRef string, req NodeRequest) ([]CodeEdge, error)
	Callees(ctx context.Context, graphRef string, req NodeRequest) ([]CodeEdge, error)
	Impact(ctx context.Context, graphRef string, req ImpactRequest) ([]CodeHit, error)
	Status(ctx context.Context, graphRef string) (GraphStatus, error)
	Delete(ctx context.Context, graphRef string) error
	Health(ctx context.Context) error
}
