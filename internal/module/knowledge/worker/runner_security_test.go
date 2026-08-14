package worker

// runner_security_test.go supplements the §10.2 用例 18 redaction coverage.
// runner_test.go::TestRedact_MasksCredentialHints already pins that redact()
// masks credential hints in error_detail_redacted. This file adds the sibling
// guard: the error_code column (classifyCode), which operators read, must
// also not carry plaintext credentials — a credential that slips into an
// error message must be masked or truncated before it reaches the code column.
// Together they cover §10.2 用例 18: "last_error / 日志 / trace 含凭据明文
// → 脱敏（打码）".

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestClassifyCode_NeverLeaksCredential asserts the error_code column
// (classifyCode) does not carry plaintext credential values. The docstring on
// classifyCode states "It must not leak credentials or full paths — operators
// read this column", but the implementation only truncates the first line to
// 120 chars — it does NOT run the credential-hint masking that redact() runs
// for error_detail_redacted. A handler that wraps a credential into an error
// therefore leaks the plaintext into the code column even though the sibling
// detail column is masked.
//
// DEFECT FIXED (§10.2 用例 18, YS-110): classifyCode now runs its message
// through redact() before composing the code string, so a credential that
// slips into an error message is masked in error_code just as it is in
// error_detail_redacted. As of the YS-110 D1 fix, "fetch failed:
// password=hunter2 upstream" persists code="transient:fetch failed: password=***
// upstream" — the plaintext password no longer survives.
func TestClassifyCode_NeverLeaksCredential(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "password in message",
			err:  errors.New("fetch failed: password=hunter2 upstream"),
		},
		{
			name: "bearer token in message",
			err:  errors.New("auth: Bearer eyJhbGciOiJIUzI1 rejected"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code := classifyCode(c.err, "transient")
			assert.NotContains(t, code, "hunter2", "error_code must not carry the plaintext password")
			assert.NotContains(t, code, "eyJhbGciOiJIUzI1", "error_code must not carry the plaintext bearer token")
			assert.Contains(t, code, "transient:", "code carries the retry-class prefix")
		})
	}
}

// TestRedact_PreservesNonSensitiveDetail asserts redact() leaves a clean,
// non-credential error message intact (no false-positive masking that would
// hide useful diagnostics). §6.5 masks credentials, not all detail.
func TestRedact_PreservesNonSensitiveDetail(t *testing.T) {
	in := errors.New("fetch failed: connection reset by peer after 12KB")
	got := redact(in)
	assert.Contains(t, got, "connection reset by peer",
		"non-credential diagnostic detail must survive redaction")
	assert.NotContains(t, got, "***", "a clean message must not be masked")
}
