package handler

// source_security_test.go covers the §10.2 credential-isolation cases for
// the Source management REST surface (design-docs/14 §10.2 用例 15), plus the
// §10.4 error-envelope mapping contract every source handler must honor.
//
// 用例 15: knowledge_sources.uri_normalized must never persist embedded
// credentials. The handler strips them (stripURICredentials → egress.RedactURL)
// BEFORE the service sees the input; the service then stores what it is given.
// We assert the strip at the handler boundary so a regression that passes the
// raw URI through is caught here, before it reaches persistence.
//
// §10.4 用例 25/27/29: every mutating/read source handler must route service
// errors through mapSourceErr so ErrSourceForbidden → 403/40300 (existence
// leaks only on a genuine write denial) and the not-found sentinels →
// 404/40400 (existence never leaks on a read). A regression that passes the
// raw service error to response.Fail surfaces as a 500 (code=50000) — the
// exact defect that hit CreateSource before this guard was added. The e2e
// suite (tests/e2e/source_security_test.go) exercises the full path live;
// this unit test pins the mapping table itself.

import (
	stderrors "errors"
	"net/http"
	"testing"

	srcsvc "github.com/lynn901/mora/internal/module/knowledge/source/service"
	"github.com/lynn901/mora/internal/pkg/errors"
	"github.com/stretchr/testify/assert"
)

// TestStripURICredentials_RemovesEmbeddedUserinfo asserts the handler strips
// user:pass@ from a URI before it reaches the service (§10.2 用例 15). This is
// the §6.5 red line: plaintext never persists in uri_normalized.
func TestStripURICredentials_RemovesEmbeddedUserinfo(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "https user:pass",
			raw:  "https://alice:s3cr3t@git.acme.internal/repo.git",
			want: "https://git.acme.internal/repo.git",
		},
		{
			name: "https with query token stripped-of-userinfo-only",
			raw:  "https://deploy:token@cdn.example/file.pkg?sig=abc",
			want: "https://cdn.example/file.pkg?sig=abc",
		},
		{
			name: "ssh-style already-no-userinfo unchanged",
			raw:  "git@github.com:acme/repo.git",
			want: "git@github.com:acme/repo.git",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripURICredentials(c.raw)
			assert.Equal(t, c.want, got, "userinfo must be stripped, host/path preserved")
			// The password must NEVER appear in the stripped form.
			assert.NotContains(t, got, "s3cr3t", "password must not survive stripping")
			assert.NotContains(t, got, "token", "password must not survive stripping")
		})
	}
}

// TestStripURICredentials_PlaintextNeverInStoredForm asserts that after
// stripping, the result carries no `user:pass@` shape at all — a downstream
// persist of uri_normalized therefore cannot contain plaintext (§10.2 用例 15
// "明文不落库"). This is the guarantee the service layer relies on.
func TestStripURICredentials_PlaintextNeverInStoredForm(t *testing.T) {
	raw := "https://ci-runner:supersecret@git.corp/build.git"
	got := stripURICredentials(raw)
	assert.NotContains(t, got, "supersecret", "plaintext password must not be in the stored URI")
	assert.NotContains(t, got, "ci-runner", "plaintext username must not be in the stored URI")
	assert.Contains(t, got, "git.corp", "host must be preserved")
}

// TestMapSourceErr_RoutesSentinelsToCorrectEnvelope pins the §10.4 用例
// 25/27/29 error-mapping contract: mapSourceErr must route each source-service
// sentinel to its §11.4 envelope so a permission denial surfaces as 403/40300
// (not a 500/50000 that the raw error would produce) and the not-found
// sentinels surface as 404/40400 (existence never leaks). This is the guard
// that was missing on the Create handler before the §10.4 用例 25 fix — a
// read-only caller's CreateSource returned ErrSourceForbidden which, routed
// raw, became a 500 existence-leak instead of the intended 403.
func TestMapSourceErr_RoutesSentinelsToCorrectEnvelope(t *testing.T) {
	cases := []struct {
		name   string
		in     error
		wantStatus int
		wantCode   int
	}{
		// §10.4 用例 25/29: a write/governance RBAC denial → 403/40300.
		{"forbidden", srcsvc.ErrSourceForbidden, http.StatusForbidden, 40300},
		// §10.4 用例 27: not-found sentinels → 404/40400 (no existence leak).
		{"source not found", srcsvc.ErrSourceNotFound, http.StatusNotFound, 40400},
		{"run not found", srcsvc.ErrRunNotFound, http.StatusNotFound, 40400},
		{"review not found", srcsvc.ErrReviewNotFound, http.StatusNotFound, 40400},
		// ETag / idempotency conflicts → 409/40900.
		{"etag conflict", srcsvc.ErrSourceConflict, http.StatusConflict, 40900},
		{"idempotency conflict", srcsvc.ErrIdempotencyConflict, http.StatusConflict, 40900},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mapped := mapSourceErr(c.in)
			appErr := errors.As(mapped)
			if appErr == nil {
				t.Fatalf("mapSourceErr(%v) = %v, want an *errors.Error", c.in, mapped)
			}
			assert.Equal(t, c.wantStatus, appErr.Status, "HTTP status for %s", c.name)
			assert.Equal(t, c.wantCode, int(appErr.Code), "envelope code for %s", c.name)
		})
	}

	// An unknown error must pass through unchanged (no silent envelope mapping
	// that would mask a real fault).
	unknown := stderrors.New("some unexpected fault")
	assert.Equal(t, unknown, mapSourceErr(unknown), "an unmapped error must pass through")
}
