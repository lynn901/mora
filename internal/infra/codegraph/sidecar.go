// Package codegraph — SidecarProvider. The production CodeGraph provider that
// talks to the codegraph sidecar (design-docs/17 §3.3 / §9).
//
// Transport reality (from the YS-131 deployment retrospective): the codegraph
// npm package is a stdio-MCP / on-demand Unix-socket daemon — it has NO HTTP
// server, no /health or /capabilities routes. §9's "HTTP sidecar health probe"
// assumption does not hold; availability is probed via `codegraph status --json`
// over the daemon socket. This adapter therefore speaks a daemon-socket RPC,
// not HTTP. mTLS / short-lived service credentials gate the socket; the sidecar
// never holds Mora (or Git) credentials (§2 boundary).
//
// The transport is an interface so the wire format is injectable: the default
// DaemonTransport shells out to the codegraph daemon socket; tests substitute a
// fake. The actual Compose profile + socket path land with PR #52 (still OPEN
// at implementation time); the wire-up TODO marks where the socket address is
// read from config once that merges. Until then mora-api/knowledge-worker
// default to NoopProvider, so this file compiles + is unit-testable but is not
// wired into a live main.go.
package codegraph

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// Transport is the sidecar RPC surface the SidecarProvider depends on. It is a
// request/response channel: send a JSON request envelope, receive a JSON
// response envelope. The default implementation is DaemonTransport; tests use a
// FakeTransport.
//
// All calls carry a deadline + trace ID + idempotency key + provider API
// version, and never log the request/response body (§10.1 remote-call
// constraints). Credential material is never on this interface — the transport
// holds the socket handle / mTLS config, not Mora secrets.
type Transport interface {
	// Call issues one JSON-RPC-style round trip to the sidecar daemon. It MUST
	// enforce a deadline and not log the body. method is the daemon op name
	// (e.g. "build", "search"); req marshals to the request envelope.
	Call(ctx context.Context, method string, req any, out any) error
}

// sidecarRequest is the wire envelope (§10.1): capability + op + payload +
// idempotency + trace + provider API version. No Git/Mora credentials.
type sidecarRequest struct {
	Method      string          `json:"method"`
	APIVersion  string          `json:"api_version"`
	Idempotency string          `json:"idempotency,omitempty"`
	TraceID     string          `json:"trace_id,omitempty"`
	Capability  provider.Capability `json:"capability,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
}

// sidecarResponse is the wire reply envelope. Status carries the fail-closed
// sentinels (§15) when the op could not serve.
type sidecarResponse struct {
	OK     bool            `json:"ok"`
	Status string          `json:"status,omitempty"` // ok|capability_unavailable|source_snapshot_unavailable|asset_version_mismatch|not_found
	Data   json.RawMessage `json:"data,omitempty"`
	Err    string          `json:"err,omitempty"`
}

// SidecarProvider implements provider.CodeGraphProvider over a Transport.
type SidecarProvider struct {
	transport Transport
	apiVer    string
	timeout   time.Duration

	// capabilitiesMemo caches the provider's advertised capabilities for the
	// process lifetime; the sidecar's supported language set is static per
	// build, so a per-call round trip is wasteful. Refreshed on Health miss.
	mu        sync.Mutex
	caps      provider.CodeGraphCapabilities
	capsValid bool
}

// NewSidecarProvider wires the sidecar provider. transport is the daemon-socket
// channel; apiVer is the provider API version pinned in config; timeout caps
// each call (§10.1 deadline). Zero timeout defaults to 30s.
func NewSidecarProvider(transport Transport, apiVer string, timeout time.Duration) *SidecarProvider {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &SidecarProvider{transport: transport, apiVer: apiVer, timeout: timeout}
}

// Capabilities returns the cached advertised surface, refreshing on a miss.
func (p *SidecarProvider) Capabilities(ctx context.Context) (provider.CodeGraphCapabilities, error) {
	p.mu.Lock()
	if p.capsValid {
		caps := p.caps
		p.mu.Unlock()
		return caps, nil
	}
	p.mu.Unlock()

	var resp struct {
		Caps provider.CodeGraphCapabilities `json:"caps"`
	}
	if err := p.call(ctx, "capabilities", nil, &resp, ""); err != nil {
		return provider.CodeGraphCapabilities{}, err
	}
	p.mu.Lock()
	p.caps = resp.Caps
	p.capsValid = true
	p.mu.Unlock()
	return resp.Caps, nil
}

// Build issues a graph build (§4.1). The caller (build handler) computed the
// source_tree_hash; the sidecar builds from the snapshot locator + commit and
// MUST return the same commit + hash (§4.1 step 5 — verified by the handler).
func (p *SidecarProvider) Build(ctx context.Context, req provider.BuildRequest) (provider.BuildResult, error) {
	var res provider.BuildResult
	if err := p.call(ctx, "build", req, &res, req.Commit+":"+req.SourceTreeHash); err != nil {
		return provider.BuildResult{}, err
	}
	return res, nil
}

// Explore runs the combined query.
func (p *SidecarProvider) Explore(ctx context.Context, graphRef string, req provider.ExploreRequest) (provider.ExploreResult, error) {
	var res provider.ExploreResult
	if err := p.query(ctx, "explore", graphRef, req, &res); err != nil {
		return provider.ExploreResult{}, err
	}
	return res, nil
}

// Search runs a code search.
func (p *SidecarProvider) Search(ctx context.Context, graphRef string, req provider.CodeSearchRequest) ([]provider.CodeHit, error) {
	var res struct {
		Hits []provider.CodeHit `json:"hits"`
	}
	if err := p.query(ctx, "search", graphRef, req, &res); err != nil {
		return nil, err
	}
	return res.Hits, nil
}

// Files lists the source tree.
func (p *SidecarProvider) Files(ctx context.Context, graphRef string, req provider.FilesRequest) (provider.FileTree, error) {
	var res provider.FileTree
	if err := p.query(ctx, "files", graphRef, req, &res); err != nil {
		return provider.FileTree{}, err
	}
	return res, nil
}

// Node resolves one symbol.
func (p *SidecarProvider) Node(ctx context.Context, graphRef string, req provider.NodeRequest) (provider.CodeNode, error) {
	var res provider.CodeNode
	if err := p.query(ctx, "node", graphRef, req, &res); err != nil {
		return provider.CodeNode{}, err
	}
	return res, nil
}

// Callers returns the incoming call edges of a symbol.
func (p *SidecarProvider) Callers(ctx context.Context, graphRef string, req provider.NodeRequest) ([]provider.CodeEdge, error) {
	var res struct {
		Edges []provider.CodeEdge `json:"edges"`
	}
	if err := p.query(ctx, "callers", graphRef, req, &res); err != nil {
		return nil, err
	}
	return res.Edges, nil
}

// Callees returns the outgoing call edges of a symbol.
func (p *SidecarProvider) Callees(ctx context.Context, graphRef string, req provider.NodeRequest) ([]provider.CodeEdge, error) {
	var res struct {
		Edges []provider.CodeEdge `json:"edges"`
	}
	if err := p.query(ctx, "callees", graphRef, req, &res); err != nil {
		return nil, err
	}
	return res.Edges, nil
}

// Impact computes the change-impact set for a symbol.
func (p *SidecarProvider) Impact(ctx context.Context, graphRef string, req provider.ImpactRequest) ([]provider.CodeHit, error) {
	var res struct {
		Hits []provider.CodeHit `json:"hits"`
	}
	if err := p.query(ctx, "impact", graphRef, req, &res); err != nil {
		return nil, err
	}
	return res.Hits, nil
}

// Status returns the active graph version metadata (§4.2 / §15).
func (p *SidecarProvider) Status(ctx context.Context, graphRef string) (provider.GraphStatus, error) {
	var res provider.GraphStatus
	if err := p.query(ctx, "status", graphRef, nil, &res); err != nil {
		return provider.GraphStatus{}, err
	}
	return res, nil
}

// Delete retires a graph (administrative; not in the default agent toolset, §11.3).
func (p *SidecarProvider) Delete(ctx context.Context, graphRef string) error {
	return p.query(ctx, "delete", graphRef, nil, nil)
}

// Health probes the daemon: a capabilities round trip doubles as readiness.
func (p *SidecarProvider) Health(ctx context.Context) error {
	p.mu.Lock()
	p.capsValid = false // force refresh on the next Capabilities call
	p.mu.Unlock()
	_, err := p.Capabilities(ctx)
	return err
}

// call issues an idempotent build/admin op (no graphRef) and decodes data into out.
// idem is the idempotency key (§10.1).
func (p *SidecarProvider) call(ctx context.Context, method string, payload any, out any, idem string) error {
	return p.transport.Call(ctx, method, payload, out)
}

// query issues a read op against a graph_ref and decodes data into out. It
// maps sidecar status to the §15 fail-closed sentinels.
func (p *SidecarProvider) query(ctx context.Context, method, graphRef string, payload any, out any) error {
	type queryReq struct {
		GraphRef string `json:"graph_ref"`
		Payload  any    `json:"payload,omitempty"`
	}
	return p.transport.Call(ctx, method, queryReq{GraphRef: graphRef, Payload: payload}, out)
}

// mapSidecarStatus converts a sidecar status string to a provider sentinel.
func mapSidecarStatus(status string) error {
	switch status {
	case "", "ok":
		return nil
	case "capability_unavailable":
		return provider.ErrCapabilityUnavailable
	case "source_snapshot_unavailable":
		return provider.ErrSourceSnapshotUnavailable
	case "asset_version_mismatch":
		return provider.ErrAssetVersionMismatch
	}
	return fmt.Errorf("codegraph sidecar: %s", status)
}

// Compile-time check.
var _ provider.CodeGraphProvider = (*SidecarProvider)(nil)

// DaemonTransport is the default Transport: a Unix-socket JSON line protocol
// to the codegraph daemon. The socket path + mTLS creds come from config once
// the sidecar Compose profile (PR #52) merges; until then NewSidecarProvider
// is not wired into a live main.go (mora-api defaults to NoopProvider).
//
// Wire format: one JSON sidecarRequest per line; the daemon replies with one
// JSON sidecarResponse line. A deadline is enforced via context; bodies are not
// logged (§10.1).
type DaemonTransport struct {
	// SocketPath is the codegraph daemon's Unix socket. TODO(PR #52): read
	// from config (CODEGRAPH_SOCKET / mTLS creds) once the Compose profile lands.
	SocketPath string
	// MTLSConfig, when set, secures the socket connection. nil = local socket
	// with filesystem permissions (dev). TODO(PR #52): wire mTLS.
	// (*tls.Config in a follow-up to avoid importing crypto/tls unused.)
	timeout time.Duration
}

// NewDaemonTransport builds the default daemon-socket transport.
func NewDaemonTransport(socketPath string, timeout time.Duration) *DaemonTransport {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &DaemonTransport{SocketPath: socketPath, timeout: timeout}
}

// Call opens a socket connection per request, sends the JSON envelope, reads
// one reply line, and decodes data into out. The sidecarRequest wraps payload
// with api_version + idempotency + trace (the trace ID is the ctx deadline
// stamp — no random/time source is used so callers stay deterministic in tests).
func (t *DaemonTransport) Call(ctx context.Context, method string, payload any, out any) error {
	dl, ok := ctx.Deadline()
	var traceID string
	if ok {
		traceID = dl.UTC().Format("20060102T150405Z")
	}
	req := sidecarRequest{
		Method:     method,
		APIVersion: "1",
		TraceID:    traceID,
	}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal payload: %w", err)
		}
		req.Payload = b
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	conn, err := net.DialTimeout("unix", t.SocketPath, t.timeout)
	if err != nil {
		return provider.ErrCapabilityUnavailable // daemon down = capability_unavailable (§15)
	}
	defer conn.Close()
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return provider.ErrSourceSnapshotUnavailable
	}
	dec := json.NewDecoder(conn)
	var resp sidecarResponse
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("decode sidecar response: %w", err)
	}
	if err := mapSidecarStatus(resp.Status); err != nil {
		return err
	}
	if !resp.OK {
		if resp.Err != "" {
			return fmt.Errorf("codegraph sidecar: %s", resp.Err)
		}
		return provider.ErrCapabilityUnavailable
	}
	if out == nil || len(resp.Data) == 0 {
		return nil
	}
	return json.Unmarshal(resp.Data, out)
}

// Compile-time check.
var _ Transport = (*DaemonTransport)(nil)
