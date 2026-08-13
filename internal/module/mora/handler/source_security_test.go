package handler

// source_security_test.go covers the §10.2 credential-isolation cases for
// the Source management REST surface (design-docs/14 §10.2 用例 15).
//
// 用例 15: knowledge_sources.uri_normalized must never persist embedded
// credentials. The handler strips them (stripURICredentials → egress.RedactURL)
// BEFORE the service sees the input; the service then stores what it is given.
// We assert the strip at the handler boundary so a regression that passes the
// raw URI through is caught here, before it reaches persistence.
//
// Cases 16 (Run snapshot frozen + redacted), 17 (credential_version pinned), and
// 18 (error detail redacted) live in the service / worker packages respectively
// (service_security_test.go, runner_test.go::TestRedact_MasksCredentialHints).

import (
	"testing"

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
