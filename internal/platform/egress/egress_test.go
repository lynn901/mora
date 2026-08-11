package egress

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestRedactURL_StripsUserinfo asserts embedded user:pass@ is stripped so logs
// / audit never carry plaintext credentials (§6.5). The scheme + host + path +
// query survive (minus the userinfo).
func TestRedactURL_StripsUserinfo(t *testing.T) {
	got := RedactURL("https://alice:s3cr3t@git.acme.internal/repo.git?token=x")
	assert.Equal(t, "https://git.acme.internal/repo.git?token=x", got)
}

// TestRedactURL_NoUserinfoUnchanged asserts a URL without embedded creds is
// returned unchanged.
func TestRedactURL_NoUserinfoUnchanged(t *testing.T) {
	in := "https://github.com/acme/repo.git"
	assert.Equal(t, in, RedactURL(in))
}

// TestRedactURL_MalformedUnchanged asserts a malformed URL is returned as-is
// (best-effort; a malformed URL is rejected earlier by FetchURL/Validate).
func TestRedactURL_MalformedUnchanged(t *testing.T) {
	in := "://not-a-url"
	assert.Equal(t, in, RedactURL(in))
}

// TestHostAllowed asserts the allowlist matches exact hosts and *.suffix
// patterns, and is case-insensitive + trims a trailing dot.
func TestHostAllowed(t *testing.T) {
	allow := []string{"github.com", "*.acme.internal"}
	cases := []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"GITHUB.COM", true},
		{"github.com.", true},       // trailing dot trimmed
		{"api.acme.internal", true}, // *.suffix match
		{"ACME.INTERNAL", false},    // *.suffix needs a leading label
		{"evilacme.internal", false}, // not a subdomain boundary
		{"gitlab.com", false},       // not in allowlist
		{"", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, hostAllowed(c.host, allow), "host=%q", c.host)
	}
}

// TestHostAllowed_EmptyAllowlistAllowsAnyHost asserts nil/empty allowlist =
// allow any host (private/metadata ranges are STILL blocked at the IP layer).
func TestHostAllowed_EmptyAllowlistAllowsAnyHost(t *testing.T) {
	assert.True(t, hostAllowed("anything.example", nil))
}

// TestCheckIP_LoopbackBlocked asserts 127.0.0.1 is blocked regardless of the
// AllowPrivateRanges flag (loopback is always forbidden, §6.2).
func TestCheckIP_LoopbackBlocked(t *testing.T) {
	for _, allow := range []bool{false, true} {
		err := checkIP(net.IPv4(127, 0, 0, 1), Policy{AllowPrivateRanges: allow})
		assert.ErrorIs(t, err, ErrLoopback, "allow=%v", allow)
	}
}

// TestCheckIP_MetadataEndpointBlocked asserts the AWS/GCP/Azure IMDS address
// 169.254.169.254 is blocked as ErrMetadataEndpoint (§10.1 用例 2).
func TestCheckIP_MetadataEndpointBlocked(t *testing.T) {
	err := checkIP(net.IPv4(169, 254, 169, 254), Policy{AllowPrivateRanges: true})
	assert.ErrorIs(t, err, ErrMetadataEndpoint)
}

// TestCheckIP_PrivateRangeBlockedByDefault asserts RFC1918 is blocked when
// AllowPrivateRanges is false (the default — §6.2), and allowed when true
// (internal trust_level sources may opt in).
func TestCheckIP_PrivateRangeBlockedByDefault(t *testing.T) {
	rfc1918 := []net.IP{
		net.IPv4(10, 0, 0, 1),
		net.IPv4(172, 16, 0, 1),
		net.IPv4(192, 168, 1, 1),
	}
	for _, ip := range rfc1918 {
		assert.ErrorIs(t, checkIP(ip, Policy{AllowPrivateRanges: false}), ErrPrivateRange, "ip=%s", ip)
		assert.NoError(t, checkIP(ip, Policy{AllowPrivateRanges: true}), "ip=%s", ip)
	}
}

// TestCheckIP_LinkLocalBlocked asserts 169.254.x.x (non-metadata) is blocked
// as ErrLinkLocal by default.
func TestCheckIP_LinkLocalBlocked(t *testing.T) {
	err := checkIP(net.IPv4(169, 254, 1, 1), Policy{})
	assert.ErrorIs(t, err, ErrLinkLocal)
}

// TestCheckIP_PublicAllowed asserts a public IP passes the IP policy.
func TestCheckIP_PublicAllowed(t *testing.T) {
	// 8.8.8.8 is a well-known public address.
	assert.NoError(t, checkIP(net.IPv4(8, 8, 8, 8), Policy{}))
}

// TestContentTypeAllowed asserts the base type (before `;params`) is matched
// case-insensitively, and an empty Content-Type is rejected.
func TestContentTypeAllowed(t *testing.T) {
	allow := []string{"application/json", "text/markdown"}
	assert.True(t, contentTypeAllowed("application/json", allow))
	assert.True(t, contentTypeAllowed("application/json; charset=utf-8", allow))
	assert.True(t, contentTypeAllowed("text/markdown", allow))
	assert.False(t, contentTypeAllowed("application/xml", allow))
	assert.False(t, contentTypeAllowed("", allow))
}

// TestHashTargetKey_StableAndFixedLength asserts the target_key is a stable
// sha256 hex (same inputs → same key; different inputs → different key) and is
// fixed-length so it never leaks the original path's structure (§4.3).
func TestHashTargetKey_StableAndFixedLength(t *testing.T) {
	a := HashTargetKey("file", "src-1", "/data/x.md")
	b := HashTargetKey("file", "src-1", "/data/x.md")
	c := HashTargetKey("file", "src-1", "/data/y.md")
	assert.Equal(t, a, b, "same inputs → same key")
	assert.NotEqual(t, a, c, "different inputs → different key")
	assert.Equal(t, 64, len(a), "sha256 hex = 64 chars")
}
