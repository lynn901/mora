// Package connector defines the Source Connector port (design-docs/14 §4.1,
// design-docs/12 §7.1).
//
// A Connector fetches a fixed-revision input into a Mora-provided ContentSink.
// It does NOT decide asset permissions, publish state, or governance profile —
// those are Mora's responsibility (不变量: Connector 与类型 Provider 分离, §10.6).
//
// A Connector only ever receives a Run snapshot (already-redacted Source config
// + credential_version). It NEVER receives allowed_asset_ids or a user Token
// (§10.2 用例 19) — the port's signature makes that impossible: there is no
// field for either on ValidateRequest or Source.
//
// The ContentSink is Mora-provided; connectors may ONLY write to the task-
// isolated dir or designated MinIO prefix (§7.1). The sink enforces size/quota
// limits and computes the content hash on close.
package connector

import (
	"context"
	"errors"
	"io"
)

// SourceType mirrors domain.SourceType without importing domain (keeps the
// connector port dependency-free — infra/connector implements it).
type SourceType string

const (
	SourceFile   SourceType = "file"
	SourceURLAPI SourceType = "url_api"
	SourceGit    SourceType = "git"
)

// ValidateRequest carries the redacted Source config a Connector validates
// against BEFORE any Run is queued (§7.2 鉴权 sync + 校验). It deliberately
// has no credential plaintext and no user/agent identity.
type ValidateRequest struct {
	URINormalized   string
	SyncPolicy      map[string]any // non-executable: schedule/cursor/rate/max_bytes
	TrustLevel      string         // untrusted|trusted|internal
	RequestedAssetType string
}

// Source is the immutable, redacted snapshot a Connector operates on (§7.2).
// CredentialRef is a pointer into a Secret manager; the Connector NEVER reads
// plaintext here — it asks a CredentialProvider (wired by the worker) for the
// short-lived decrypted value, held only in memory.
type Source struct {
	ID             string
	SourceType     SourceType
	URINormalized  string
	SyncPolicy     map[string]any
	TrustLevel     string
	// CredentialRef points at a Secret manager / encrypted-credential store.
	// The connector does NOT dereference it directly; the worker resolves it
	// via CredentialProvider and passes the plaintext (in-memory only) to the
	// adapter that needs it (e.g. git credentials helper).
	CredentialRef string
	// CredentialVersion is pinned at Run create time so a credential rotation
	// does not drift an in-flight Run (§7.2 / §10.2 用例 17).
	CredentialVersion string
}

// Revision is a resolved, content-addressed source revision (§4.3).
type Revision struct {
	Value    string // file: mtime_ns+size or hash; url_api: ETag/Last-Modified/cursor; git: commit_sha
	IsLatest bool    // true when Value was resolved as "latest" (no requested_revision)
}

// Locator is the non-executable position of fetched content (§4.1). It carries
// only enough for the worker to re-read the content later (MinIO key, temp
// path); it is never a command and never carries credentials.
type Locator struct {
	Kind string // "minio" | "file"
	Key  string // MinIO object key / temp path under the isolated prefix
}

// FetchManifest is what a Connector returns from Fetch (§4.1). Each entry MUST
// carry a stable target_key, asset_type, content hash and content locator.
type FetchManifest struct {
	Revision Revision
	Entries []ManifestEntry
}

// ManifestEntry is one fetched target (§4.1). The target_key MUST be stable
// across syncs for the same logical content (§4.3) — file uses a normalized
// path hash, URL/API a stable ID or normalized URL hash, Git repo+path.
type ManifestEntry struct {
	TargetKey   string
	AssetType   string // document | codebase | memory | skill
	ContentHash string
	Locator     Locator
	Metadata    map[string]any // never carries credentials
}

// SourceConnector is the port each source adapter implements (§4.1).
//
// The port is the Phase 2 reuse contract (architecture red line: names and
// signatures are frozen). Implementations live in internal/infra/connector;
// the knowledge module depends on this port, never on a concrete adapter.
type SourceConnector interface {
	// Type identifies the adapter (file | url_api | git).
	Type() SourceType
	// Validate checks source config, network reachability, credentials and
	// license BEFORE any Run is queued (§7.2). Must be side-effect free w.r.t.
	// the source itself — no writes, no state changes; a network probe is OK.
	Validate(ctx context.Context, req ValidateRequest) error
	// ResolveRevision resolves the source's current revision (or a requested
	// one) WITHOUT fetching content. The Run snapshot stores this.
	ResolveRevision(ctx context.Context, src Source) (Revision, error)
	// Fetch pulls the content at revision into sink. Each manifest entry MUST
	// carry a stable target_key, asset_type, content hash and content locator.
	Fetch(ctx context.Context, src Source, rev Revision, sink ContentSink) (FetchManifest, error)
	// Health reports whether the adapter can reach its dependencies (the local
	// filesystem for file, egress for url_api, git for git).
	Health(ctx context.Context) error
}

// ContentSink is Mora-provided; connectors may ONLY write to the task-isolated
// dir or designated MinIO prefix (§7.1). It enforces size/quota limits and
// computes the content hash on close.
type ContentSink interface {
	// Write returns a writer scoped to targetKey; the connector writes content
	// then closes. The sink computes content_hash on close and returns it.
	Write(ctx context.Context, targetKey string) (ContentWriter, error)
}

// ContentWriter is a write+close handle the sink returns. Close finalizes the
// content (flushes to MinIO, computes the hash) and returns the hash + locator.
// Writing past max_bytes returns ErrContentTooLarge and discards the partial.
type ContentWriter interface {
	io.Writer
	io.Closer
	// Hash returns the computed content hash once the writer is closed.
	Hash() string
	// Locator returns where the content was stored (non-executable).
	Locator() Locator
}

// --- classified connector errors (§4.2 error_code mapping) ---

// Err is a classified connector error. Workers map Code to
// source_sync_runs.error_code (§4.2 / §6.5 retry-vs-dead).
type Err struct {
	Code    string
	Message string
	Cause   error
}

func (e *Err) Error() string {
	if e.Cause != nil {
		return e.Code + ": " + e.Message + ": " + e.Cause.Error()
	}
	return e.Code + ": " + e.Message
}

func (e *Err) Unwrap() error { return e.Cause }

// Stable connector error codes (§4.3, §10.1).
var (
	ErrInvalidConfig  = &Err{Code: "invalid_config", Message: "connector: invalid source config"}
	ErrUnreachable   = &Err{Code: "unreachable", Message: "connector: source unreachable"}
	ErrUnauthorized  = &Err{Code: "unauthorized", Message: "connector: credentials rejected"}
	ErrRevisionNotFound = &Err{Code: "revision_not_found", Message: "connector: requested revision not found"}
	ErrContentTooLarge = &Err{Code: "content_too_large", Message: "connector: content exceeded size limit"}
	ErrPathTraversal  = &Err{Code: "path_traversal", Message: "connector: path traversal blocked"}
	ErrDecompressionBomb = &Err{Code: "compression_bomb", Message: "connector: decompression ratio exceeded"}
	ErrProtocolBlocked = &Err{Code: "protocol_blocked", Message: "connector: protocol not allowed"}
	ErrMimeMismatch   = &Err{Code: "mime_mismatch", Message: "connector: MIME / extension mismatch"}
	ErrFetch          = &Err{Code: "fetch_failed", Message: "connector: fetch failed"}
)

// Is reports whether the error's chain contains a *connector.Err with the
// given code (convenience for adapter tests).
func Is(err error, code string) bool {
	var e *Err
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == code
}

// CredentialProvider resolves a CredentialRef + version into a short-lived
// decrypted value held only in memory (§6.5). The worker wires a concrete
// implementation (Secret manager / encrypted-column store) and passes it to
// adapters that need credentials (git). Adapters never persist it.
type CredentialProvider interface {
	Resolve(ctx context.Context, ref, version string) (string, error)
}

// Registry maps a SourceType to its adapter. The worker uses it to dispatch a
// source_sync_run to the right SourceConnector without a switch growing.
type Registry struct {
	adapters map[SourceType]SourceConnector
}

// NewRegistry builds an empty Registry.
func NewRegistry() *Registry { return &Registry{adapters: make(map[SourceType]SourceConnector)} }

// Register associates an adapter with its type. Later calls for the same type
// replace the previous adapter.
func (r *Registry) Register(c SourceConnector) { r.adapters[c.Type()] = c }

// Get returns the adapter for a type, or false if none registered.
func (r *Registry) Get(t SourceType) (SourceConnector, bool) {
	c, ok := r.adapters[t]
	return c, ok
}
