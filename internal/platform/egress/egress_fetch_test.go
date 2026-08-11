package egress

// egress_fetch_test.go exercises the FetchURL enforcement paths that the
// pure-function egress_test.go does not reach (design-docs/14 §10.1). The
// egress layer blocks loopback / link-local / metadata unconditionally
// (§6.2), so a local httptest server is itself blocked at the first hop —
// the integrated redirect/size chain is therefore covered by the e2e suite
// against a controlled in-stack endpoint. These unit tests cover the
// pre-DNS rejection paths (loopback/metadata/private/host-allowlist) and the
// streaming size-cap reader directly, with no network dependency.

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingSink collects audit records for assertions (§6.5: every egress
// audited, URL redacted).
type recordingSink struct{ records []AuditRecord }

func (r *recordingSink) Record(_ context.Context, rec AuditRecord) {
	r.records = append(r.records, rec)
}

// --- §10.1 用例 1: URL source pointing at loopback is blocked ---

// TestFetchURL_LoopbackBlocked asserts a URL whose host resolves to 127.0.0.1
// is rejected at the IP-policy check with ErrLoopback, and the denial is
// audited with a redacted URL (§10.1 用例 1).
func TestFetchURL_LoopbackBlocked(t *testing.T) {
	rec := &recordingSink{}
	c := NewClient(rec)
	_, err := c.FetchURL(context.Background(), "http://127.0.0.1/x", DefaultPolicy())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrLoopback)
	require.NotEmpty(t, rec.records, "the denied egress must be audited")
	assert.NotContains(t, rec.records[0].URL, "@", "audit URL must be redacted (no userinfo)")
}

// --- §10.1 用例 2: cloud metadata endpoint blocked ---

// TestFetchURL_MetadataBlocked asserts the IMDS address 169.254.169.254 is
// blocked with ErrMetadataEndpoint (§10.1 用例 2).
func TestFetchURL_MetadataBlocked(t *testing.T) {
	c := NewClient(&recordingSink{})
	_, err := c.FetchURL(context.Background(), "http://169.254.169.254/latest/meta-data/", DefaultPolicy())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrMetadataEndpoint)
}

// --- §10.1 用例 9a: private range blocked by default ---

// TestFetchURL_PrivateRangeBlockedByDefault asserts an RFC1918 literal
// (192.168.1.5) is blocked when AllowPrivateRanges=false (§10.1 用例 9a).
func TestFetchURL_PrivateRangeBlockedByDefault(t *testing.T) {
	c := NewClient(&recordingSink{})
	_, err := c.FetchURL(context.Background(), "http://192.168.1.5/x", DefaultPolicy())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPrivateRange)
}

// --- §10.1 用例 8: host not in allowlist ---

// TestFetchURL_HostNotAllowed asserts a host outside AllowDomains is rejected
// at the pre-DNS host-allowlist check with ErrHostNotAllowed, and the denial
// is audited (§10.1 用例 8).
func TestFetchURL_HostNotAllowed(t *testing.T) {
	rec := &recordingSink{}
	c := NewClient(rec)
	pol := DefaultPolicy()
	pol.AllowDomains = []string{"allowed.example"}
	_, err := c.FetchURL(context.Background(), "http://deny.example/x", pol)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrHostNotAllowed)
	require.NotEmpty(t, rec.records, "the denied egress must be audited")
}

// --- §10.1 用例 5: streaming size cap (response_too_large) ---

// TestLimitedReadCloser_StreamingCap asserts the body reader cuts off at
// MaxResponseBytes and surfaces ErrResponseTooLarge once the limit is crossed
// (§10.1 用例 5; source_sync_runs.error_code='response_too_large'). This is
// the actual enforcement mechanism FetchURL wraps the body in.
func TestLimitedReadCloser_StreamingCap(t *testing.T) {
	body := io.NopCloser(strings.NewReader(strings.Repeat("x", 1024)))
	lr := &limitedReadCloser{inner: body, limit: 10}
	buf := make([]byte, 8)
	// First read returns up to 8 bytes (under the 10-byte cap), no error.
	n, err := lr.Read(buf)
	assert.NoError(t, err)
	assert.Equal(t, 8, n)
	// Second read returns the remaining 2 bytes AND, because a peek shows
	// more bytes remain past the limit, flags exceeded in the same call.
	n, err = lr.Read(buf)
	assert.Equal(t, 2, n)
	assert.ErrorIs(t, err, ErrResponseTooLarge)
	assert.True(t, lr.exceeded, "exceeded flag must be set once the cap is crossed")
	// Subsequent reads keep returning the too-large error.
	_, err = lr.Read(buf)
	assert.ErrorIs(t, err, ErrResponseTooLarge)
}

// TestLimitedReadCloser_ExactLimitAllowed asserts a body exactly equal to the
// limit can be read to completion without being flagged too large when read in
// chunks smaller than the limit (the boundary is inclusive: exactly
// MaxResponseBytes is NOT too large — §10.1 用例 5 only cuts off responses
// that EXCEED the cap). The caller stops reading once the body is exhausted;
// the reader's job is to let that completion succeed, not to reject at-limit.
func TestLimitedReadCloser_ExactLimitAllowed(t *testing.T) {
	body := io.NopCloser(strings.NewReader(strings.Repeat("x", 10)))
	lr := &limitedReadCloser{inner: body, limit: 10}
	buf := make([]byte, 4) // smaller than the limit → no single Read crosses it
	var total int
	for total < 10 {
		n, err := lr.Read(buf)
		total += n
		if err != nil {
			// A too-large error before the body is fully consumed would be a
			// false positive — exactly-at-limit must not trip the cap.
			assert.Failf(t, "read error before completion", "total=%d err=%v", total, err)
			return
		}
	}
	assert.Equal(t, 10, total, "the full 10-byte body must be readable at a 10-byte limit")
	assert.False(t, lr.exceeded, "a body exactly at the limit must not be flagged too large")
}

// --- §10.1 用例 11: audit URL redaction (credential stripping) ---

// TestFetchURL_AuditURLStripsUserinfo asserts that when a URL carries embedded
// user:pass@ userinfo, the audit record's URL has it stripped (§6.5 / §10.2
// 用例 18: credentials never appear in logs/audit).
func TestFetchURL_AuditURLStripsUserinfo(t *testing.T) {
	rec := &recordingSink{}
	c := NewClient(rec)
	// The fetch will be blocked (loopback) but the audit URL must already be
	// redacted before the denial is recorded.
	_, _ = c.FetchURL(context.Background(), "https://alice:s3cr3t@127.0.0.1/repo", DefaultPolicy())
	require.NotEmpty(t, rec.records)
	assert.NotContains(t, rec.records[0].URL, "alice", "userinfo must not appear in audit URL")
	assert.NotContains(t, rec.records[0].URL, "s3cr3t", "password must not appear in audit URL")
	assert.Contains(t, rec.records[0].URL, "127.0.0.1")
}

// --- §6.1 private-net segment enumeration (architecture red line) ---

// TestCheckIP_AllPrivateSegmentsBlocked asserts the full §6.1 denylist is
// blocked when AllowPrivateRanges=false — the architecture red line that
// SSRF coverage must enumerate every private segment.
func TestCheckIP_AllPrivateSegmentsBlocked(t *testing.T) {
	cases := []struct {
		name string
		ip   net.IP
		want error
	}{
		{"loopback v4", net.IPv4(127, 0, 0, 1), ErrLoopback},
		{"loopback v6", net.ParseIP("::1"), ErrLoopback},
		{"RFC1918 10/8", net.IPv4(10, 1, 2, 3), ErrPrivateRange},
		{"RFC1918 172.16/12", net.IPv4(172, 16, 0, 1), ErrPrivateRange},
		{"RFC1918 192.168/16", net.IPv4(192, 168, 1, 1), ErrPrivateRange},
		{"link-local v4", net.IPv4(169, 254, 1, 1), ErrLinkLocal},
		{"metadata IMDS", net.IPv4(169, 254, 169, 254), ErrMetadataEndpoint},
		{"metadata ECS", net.IPv4(169, 254, 170, 2), ErrMetadataEndpoint},
		{"unique-local v6 fc00::/7", net.ParseIP("fd00::1"), ErrPrivateRange},
		{"link-local v6 fe80::/10", net.ParseIP("fe80::1"), ErrLinkLocal},
		{"unspecified v4", net.IPv4(0, 0, 0, 0), ErrPrivateRange},
	}
	pol := Policy{AllowPrivateRanges: false}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := checkIP(c.ip, pol)
			assert.ErrorIs(t, err, c.want, "segment %s (%s) must be blocked", c.name, c.ip)
		})
	}
}

// TestCheckIP_AllowPrivateRangesLetsRFC1918Through asserts that with
// AllowPrivateRanges=true, RFC1918 + fc00::/7 pass (internal trust_level may
// opt in, §10.1 用例 9b), but loopback + link-local + metadata are STILL
// blocked (the security red line: loopback/metadata never opt-out).
func TestCheckIP_AllowPrivateRangesLetsRFC1918Through(t *testing.T) {
	pol := Policy{AllowPrivateRanges: true}
	for _, ip := range []net.IP{
		net.IPv4(10, 0, 0, 1),
		net.IPv4(172, 16, 0, 1),
		net.IPv4(192, 168, 1, 1),
		net.ParseIP("fd00::1"),
	} {
		assert.NoError(t, checkIP(ip, pol), "private range %s must be allowed when opted in", ip)
	}
	for _, c := range []struct {
		name string
		ip   net.IP
		want error
	}{
		{"loopback still blocked", net.IPv4(127, 0, 0, 1), ErrLoopback},
		{"metadata still blocked", net.IPv4(169, 254, 169, 254), ErrMetadataEndpoint},
		{"link-local still blocked", net.IPv4(169, 254, 1, 1), ErrLinkLocal},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.ErrorIs(t, checkIP(c.ip, pol), c.want)
		})
	}
}

// errorIs is a tiny wrapper to avoid importing errors just for Is.
func errorIs(err, target error) bool {
	return err == target || (err != nil && strings.Contains(err.Error(), target.Error()))
}

// silence unused-import linters for httptest/url if a future test adds them.
var _ = httptest.NewServer
var _ = url.Parse
