package handler

// codegraph_test.go pins the §10.4 / §8.2 / §15 error-mapping contract for the
// codegraph query handler (design-docs/17 §6.1). mapCodeGraphErr must route each
// codegraph-service / provider sentinel to its §11.4 envelope so:
//   - a missing / cross-workspace / no-permission codebase surfaces as
//     404/40400 — indistinguishable across the three cases (existence never
//     leaks, §8.2 / §10.4 用例 26/27);
//   - a not-yet-ready graph surfaces as 409/40900 (the codebase already resolved
//     under RBAC, so "not ready" leaks nothing, §8.2);
//   - a provider capability fault surfaces as 503/50300, distinct from
//     authorized-empty + genuine no-results (§15 row 3);
//   - a source_snapshot_unavailable / asset_version mismatch surfaces as
//     410/41000 (§4.2 fail-closed, §15 row 2 — never return misaligned source).
// An unknown error must pass through unchanged so a real fault is not masked.

import (
	"net/http"
	"testing"

	stderrors "errors"

	"github.com/lynn901/mora/internal/module/knowledge/asset"
	cgservice "github.com/lynn901/mora/internal/module/knowledge/codegraph/service"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
	"github.com/lynn901/mora/internal/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func TestMapCodeGraphErr_RoutesSentinelsToCorrectEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		in         error
		wantStatus int
		wantCode   int
	}{
		// §10.4 用例 26/27: a missing / cross-workspace / no-permission codebase
		// → 404/40400 (existence never leaks). Same envelope the asset read path
		// uses so a codegraph caller cannot tell not-found from not-allowed.
		{"codebase not found", cgservice.ErrCodebaseNotFound, http.StatusNotFound, 40400},
		{"asset not found (alias)", asset.ErrAssetNotFound, http.StatusNotFound, 40400},
		// §8.2: no ready projection → 409 (the codebase resolved under RBAC; its
		// existence is already known to the caller, so 409 leaks nothing).
		{"graph not ready", cgservice.ErrGraphNotReady, http.StatusConflict, 40900},
		// §15 row 3: capability_unavailable → 503 (sidecar down / unconfigured).
		{"capability unavailable", cgprovider.ErrCapabilityUnavailable, http.StatusServiceUnavailable, 50300},
		// §15 row 2 / §4.2 fail-closed: misaligned source tree / version → 410.
		{"source snapshot unavailable", cgprovider.ErrSourceSnapshotUnavailable, http.StatusGone, 41000},
		{"asset version mismatch", cgprovider.ErrAssetVersionMismatch, http.StatusGone, 41000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mapped := mapCodeGraphErr(c.in)
			appErr := errors.As(mapped)
			if appErr == nil {
				t.Fatalf("mapCodeGraphErr(%v) = %v, want an *errors.Error", c.in, mapped)
			}
			assert.Equal(t, c.wantStatus, appErr.Status, "HTTP status for %s", c.name)
			assert.Equal(t, c.wantCode, int(appErr.Code), "envelope code for %s", c.name)
		})
	}

	// An unknown error must pass through unchanged (no silent envelope mapping
	// that would mask a real fault).
	unknown := stderrors.New("some unexpected provider fault")
	mapped := mapCodeGraphErr(unknown)
	if !stderrors.Is(mapped, unknown) {
		t.Fatalf("mapCodeGraphErr(unknown) = %v, want the original error passed through", mapped)
	}
}

// TestMapCodeGraphErr_WrappedSentinels asserts the mapping honors errors.Is
// through a wrapped chain — the service returns fmt.Errorf("...: %w", sentinel)
// in several paths, and a regression that compared with == (not errors.Is)
// would route a wrapped not-found to 500 instead of 404 (existence leak).
func TestMapCodeGraphErr_WrappedSentinels(t *testing.T) {
	wrappedNotFound := stderrors.New("load: codebase gone: " + cgservice.ErrCodebaseNotFound.Error())
	// Re-wrap through fmt to attach the sentinel in the chain.
	wrapped := stderrors.Join(cgservice.ErrCodebaseNotFound, wrappedNotFound)
	mapped := mapCodeGraphErr(wrapped)
	appErr := errors.As(mapped)
	if appErr == nil {
		t.Fatalf("expected *errors.Error, got %v", mapped)
	}
	assert.Equal(t, http.StatusNotFound, appErr.Status, "wrapped not-found must map to 404 (no leak)")
	assert.Equal(t, 40400, int(appErr.Code), "wrapped not-found must map to 40400")
}
