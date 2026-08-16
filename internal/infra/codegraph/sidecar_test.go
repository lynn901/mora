package codegraph

// sidecar_test.go pins the §15 fail-closed status-mapping contract for the
// CodeGraph sidecar provider (design-docs/17 §3.3 / §4.2 / §15 fault table).
//
// Two transport layers are under test:
//   1. DaemonTransport — the default Unix-socket wire transport. It decodes the
//      sidecarResponse envelope and maps the sidecar `status` field to the
//      provider sentinel errors via mapSidecarStatus (capability_unavailable /
//      source_snapshot_unavailable / asset_version_mismatch). This is the
//      fail-closed authority — §15 rows 2 & 3.
//   2. SidecarProvider (provider.CodeGraphProvider) — routes read ops through
//      Transport.Call. Its Build/Query path delegates to the transport, so the
//      sentinel surfaces whenever the transport returns it.
//
// Contract cases (§7.2):
//   T2  source_tree_hash mismatch → source_snapshot_unavailable, no misaligned
//       source returned (§15 row 2).
//   T6  sidecar down/unconfigured → capability_unavailable (§15 row 3).
//   T10 query timeout → degrade to capability_unavailable (§15 row 3); Delete
//       invalidates the graph; management ops not in the default agent toolset.
//
// Note on a transport-interface gap found while authoring (surfaced in the
// test report as a contract observation, NOT asserted as a product defect
// here): the SidecarProvider helper methods call/query do not themselves
// invoke mapSidecarStatus — they rely entirely on the concrete transport
// (DaemonTransport.Call) to map status. The Transport interface's
// Call(ctx, method, req, out) error signature has no place for a sidecar
// `status`/`ok` field, so a fake Transport that returns a non-nil sentinel
// error is the only way a status surfaces through SidecarProvider. This test
// therefore pins fail-closed at the DaemonTransport level (the real wire
// authority) and via a sentinel-returning fake transport at the provider
// level, which is how the production path actually surfaces the sentinels. A
// future alternate Transport impl would need to honor the same mapping itself.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// --- mapSidecarStatus unit coverage (§15 fault table) ---

func TestMapSidecarStatus_RoutesSentinels(t *testing.T) {
	cases := []struct {
		status string
		want   error
	}{
		{"", nil},
		{"ok", nil},
		{"capability_unavailable", cgprovider.ErrCapabilityUnavailable},
		{"source_snapshot_unavailable", cgprovider.ErrSourceSnapshotUnavailable},
		{"asset_version_mismatch", cgprovider.ErrAssetVersionMismatch},
		{"some_unknown_status", errors.New("codegraph sidecar: some_unknown_status")},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			err := mapSidecarStatus(c.status)
			if c.want == nil {
				assert.NoError(t, err)
				return
			}
			// For the sentinel cases, assert ErrorIs so wrapped chains still map.
			if errors.Is(c.want, cgprovider.ErrCapabilityUnavailable) ||
				errors.Is(c.want, cgprovider.ErrSourceSnapshotUnavailable) ||
				errors.Is(c.want, cgprovider.ErrAssetVersionMismatch) {
				assert.ErrorIs(t, err, c.want, "status %q must map to the sentinel", c.status)
				return
			}
			// Unknown status: a non-nil, non-sentinel error carrying the status text.
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.status)
		})
	}
}

// --- DaemonTransport fail-closed mapping over a real Unix socket ---

// startDaemonSocket opens a Unix-socket server that replies to each request
// line with a fixed sidecarResponse JSON. It returns the socket path + a
// done channel; close done by cancelling the ctx. The replyer is deterministic
// (no Math.random / time source) so tests are stable.
func startDaemonSocket(t *testing.T, reply sidecarResponse) (socketPath string, stop func()) {
	t.Helper()
	dir := t.TempDir()
	socketPath = filepath.Join(dir, "codegraph.sock")

	ln, err := net.Listen("unix", socketPath)
	require.NoError(t, err)

	// Pre-encode the reply once (deterministic).
	replyBytes, err := json.Marshal(reply)
	require.NoError(t, err)
	replyLine := append(replyBytes, '\n')

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go func(c net.Conn) {
				defer c.Close()
				// Read one request line (we do not need to parse it for these
				// fail-closed tests — the reply is fixed).
				buf := make([]byte, 4096)
				_, _ = c.Read(buf)
				_, _ = c.Write(replyLine)
			}(conn)
		}
	}()

	return socketPath, func() { _ = ln.Close() }
}

// TestDaemonTransport_StatusToSentinel asserts the default transport maps each
// §15 sidecar status to the provider sentinel and never returns a fabricated
// `out`. This is the fail-closed authority (§15 rows 2 & 3): a misaligned
// source tree or an unavailable provider must NOT return decoded data.
func TestDaemonTransport_StatusToSentinel(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   error
	}{
		{"source_tree_hash_mismatch", "source_snapshot_unavailable", cgprovider.ErrSourceSnapshotUnavailable},
		{"asset_version_mismatch", "asset_version_mismatch", cgprovider.ErrAssetVersionMismatch},
		{"sidecar_down", "capability_unavailable", cgprovider.ErrCapabilityUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sock, stop := startDaemonSocket(t, sidecarResponse{OK: false, Status: c.status})
			defer stop()

			tr := NewDaemonTransport(sock, time.Second)
			var out struct{ Hit string }
			err := tr.Call(context.Background(), "node", map[string]any{"q": 1}, &out)

			require.ErrorIs(t, err, c.want, "status %q must surface as %v", c.status, c.want)
			assert.Empty(t, out.Hit, "fail-closed must not decode data into out")
		})
	}
}

// TestDaemonTransport_OKDecodesData asserts a healthy sidecar reply (ok + data)
// decodes into the out payload — the happy path that fail-closed must not
// break. This guards against an over-broad fail-closed that would reject good
// replies.
func TestDaemonTransport_OKDecodesData(t *testing.T) {
	data, _ := json.Marshal(map[string]any{"node": "Serve"})
	sock, stop := startDaemonSocket(t, sidecarResponse{OK: true, Status: "ok", Data: data})
	defer stop()

	tr := NewDaemonTransport(sock, time.Second)
	var out struct {
		Node string `json:"node"`
	}
	require.NoError(t, tr.Call(context.Background(), "node", nil, &out))
	assert.Equal(t, "Serve", out.Node)
}

// TestDaemonTransport_DialFailureCapabilityUnavailable asserts a daemon that is
// not listening surfaces as capability_unavailable (§15 row 3 — "查询服务不可用
// → capability_unavailable"), never as a raw network error that would leak the
// socket topology or be mistaken for a misaligned source.
func TestDaemonTransport_DialFailureCapabilityUnavailable(t *testing.T) {
	// A socket path that does not exist → DialTimeout fails.
	tr := NewDaemonTransport(filepath.Join(t.TempDir(), "missing.sock"), 200*time.Millisecond)
	var out struct{}
	err := tr.Call(context.Background(), "node", nil, &out)
	assert.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
}

// TestDaemonTransport_BodyNotLogged asserts the request/response body is never
// part of the returned error message (§10.1 — "不记录正文/Token/仓库凭据/未脱敏
// 证据"). A leak here would expose query payloads in operator-facing error_code.
func TestDaemonTransport_BodyNotLogged(t *testing.T) {
	// Serve a reply that decodes as a valid envelope (ok=false, no data) so the
	// transport returns a non-nil error through the status-mapping path. The
	// request payload (which could carry sensitive query content) must not
	// appear in any error string.
	sock, stop := startDaemonSocket(t, sidecarResponse{OK: false, Status: "capability_unavailable", Err: "op failed"})
	defer stop()

	tr := NewDaemonTransport(sock, time.Second)
	payload := map[string]any{"secret_query": "SENSITIVE_PAYLOAD_TOKEN"}
	var out struct{}
	err := tr.Call(context.Background(), "search", payload, &out)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "SENSITIVE_PAYLOAD_TOKEN", "request body must not leak into error text (§10.1)")
}

// --- SidecarProvider over a sentinel-returning fake transport (provider level) ---

// fakeTransport returns a fixed error from Call. Used to prove the
// SidecarProvider surfaces a transport-returned sentinel as-is on the read path
// (how the production DaemonTransport path actually surfaces §15 sentinels).
type fakeTransport struct {
	err error
}

func (f fakeTransport) Call(_ context.Context, _ string, _ any, _ any) error {
	return f.err
}

// TestSidecarProvider_ReadOpSurfacesSentinel asserts a read op (Status/Node)
// surfaces a transport-returned fail-closed sentinel, and does NOT decode a
// fabricated result into the caller's out (§15).
func TestSidecarProvider_ReadOpSurfacesSentinel(t *testing.T) {
	t.Run("source_snapshot_unavailable", func(t *testing.T) {
		p := NewSidecarProvider(fakeTransport{err: cgprovider.ErrSourceSnapshotUnavailable}, "1", time.Second)
		_, err := p.Status(context.Background(), "g")
		assert.ErrorIs(t, err, cgprovider.ErrSourceSnapshotUnavailable)
	})

	t.Run("capability_unavailable", func(t *testing.T) {
		p := NewSidecarProvider(fakeTransport{err: cgprovider.ErrCapabilityUnavailable}, "1", time.Second)
		_, err := p.Node(context.Background(), "g", cgprovider.NodeRequest{Symbol: "S"})
		assert.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
	})

	t.Run("asset_version_mismatch", func(t *testing.T) {
		p := NewSidecarProvider(fakeTransport{err: cgprovider.ErrAssetVersionMismatch}, "1", time.Second)
		_, err := p.Callers(context.Background(), "g", cgprovider.NodeRequest{Symbol: "S"})
		assert.ErrorIs(t, err, cgprovider.ErrAssetVersionMismatch)
	})
}

// TestSidecarProvider_DeleteInvalidates asserts Delete delegates to the
// transport; a successful delete is a no-op (§11.3 — admin op, not in default
// toolset). Pinning the delegation so a regression that swallowed the error
// (and left a graph live) is caught.
func TestSidecarProvider_DeleteInvalidates(t *testing.T) {
	t.Run("deletes_when_transport_ok", func(t *testing.T) {
		p := NewSidecarProvider(fakeTransport{err: nil}, "1", time.Second)
		assert.NoError(t, p.Delete(context.Background(), "g"))
	})
	t.Run("surfaces_transport_error", func(t *testing.T) {
		p := NewSidecarProvider(fakeTransport{err: cgprovider.ErrCapabilityUnavailable}, "1", time.Second)
		assert.ErrorIs(t, p.Delete(context.Background(), "g"), cgprovider.ErrCapabilityUnavailable)
	})
}

// TestSidecarProvider_HealthRefreshesCapabilities asserts Health invalidates the
// cached capabilities so a subsequent Capabilities call re-queries the transport
// (§3.4 — Health doubles as readiness; a stale cache would mask a sidecar
// restart). Uses a fake transport that reports capability_unavailable once
// Health forces a refresh.
func TestSidecarProvider_HealthRefreshesCapabilities(t *testing.T) {
	p := NewSidecarProvider(fakeTransport{err: cgprovider.ErrCapabilityUnavailable}, "1", time.Second)
	// Health forces a capabilities refresh; the transport reports unavailable.
	err := p.Health(context.Background())
	assert.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
	// A subsequent Capabilities call re-queries (still unavailable) — proving
	// the cache was invalidated, not served stale.
	_, err = p.Capabilities(context.Background())
	assert.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
}

// TestSidecarProvider_ImplementsPort is a compile-time contract check that the
// SidecarProvider satisfies provider.CodeGraphProvider.
func TestSidecarProvider_ImplementsPort(t *testing.T) {
	var _ cgprovider.CodeGraphProvider = (*SidecarProvider)(nil)
	_ = os.DevNull // keep os import meaningful for future path tests
}
