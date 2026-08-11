// Package egress is the network egress policy layer (design-docs/14 §6,
// design-docs/12 §7.3, §13.1 信任边界 5).
//
// All outbound calls to URL/API/Git sources MUST route through an EgressClient
// — it enforces DNS/redirect/private-network/size/type/timeout/allowlist/audit/
// secret-redaction checks. Provider adapters (TEI/Qdrant/MinIO) on the trusted
// internal network do NOT route through egress (they are same-network,
// separately credentialed).
//
// The layer enforces these invariants (§6.2 / §10.1):
//   - DNS is resolved and checked against loopback / link-local / metadata
//     endpoint / unapproved private ranges BEFORE the connection is opened.
//   - Each HTTP redirect is re-validated (re-resolve + re-check) to prevent
//     TOCTOU (first resolve public → redirect to internal). Redirects are also
//     re-checked by the socket-level DialHook as defense in depth.
//   - Response size and Content-Type are enforced (streaming cutoff on exceed).
//   - Redirect count is bounded (≤ MaxRedirects).
//   - Every egress is auditable (redacted URL, status, bytes, duration).
//   - URL userinfo (embedded credentials) is stripped before logging / storage.
//
// Errors are classified so callers (source_sync_run) can record a stable
// error_code and decide retry vs dead (§4.2, §6.5).
package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Policy is the per-source egress policy. A source's trust_level + sync_policy
// materialize one of these; internal trust_level may set AllowPrivateRanges.
type Policy struct {
	// AllowDomains restricts which hosts may be called. A host matches if it
	// equals an entry exactly or ends with a "*.suffix" entry. nil/empty means
	// "no host allowlist" — private/metadata ranges are STILL blocked.
	AllowDomains []string
	// AllowPrivateRanges permits RFC1918 / link-local / fc00:: /7 when true.
	// Default false; only internal trust_level sources may opt in.
	AllowPrivateRanges bool
	// MaxRedirects bounds the number of HTTP redirects followed. Each redirect
	// is re-validated. Default 5.
	MaxRedirects int
	// MaxResponseBytes caps the response body. A streaming reader cuts off at
	// the boundary and returns ErrResponseTooLarge. Default 100MB.
	MaxResponseBytes int64
	// AllowedContentTypes are the expected response types. Empty = no check.
	AllowedContentTypes []string
	// Timeout is the per-request overall deadline. Default 30s.
	Timeout time.Duration
}

// DefaultPolicy returns a Policy with the §6.2 defaults filled in.
func DefaultPolicy() Policy {
	return Policy{
		MaxRedirects:     5,
		MaxResponseBytes: 100 * 1024 * 1024,
		Timeout:          30 * time.Second,
	}
}

// Response is the result of a successful FetchURL.
type Response struct {
	URL           string // final URL after redirects (redacted — no userinfo)
	StatusCode    int
	ContentType   string
	Bytes         int64
	Body          io.ReadCloser // caller must Close; streaming-enforced
	ResolvedIPs   []string
	RedirectChain string // redacted URLs visited, "->"-joined (including the final)
}

// AuditRecord describes one egress for the audit sink. URLs are redacted
// (userinfo stripped) so logs never carry embedded credentials (§6.5).
type AuditRecord struct {
	URL       string
	StatusCode int
	Bytes     int64
	Duration  time.Duration
	ErrCode   string // empty on success
	ErrDetail string // redacted
}

// AuditSink receives one AuditRecord per egress call. Implementations append to
// the audit log; a nil sink is a no-op (tests / dev).
type AuditSink interface {
	Record(ctx context.Context, rec AuditRecord)
}

// Client is the egress enforcement point. It composes a net.Resolver (for DNS
// checks before connect) and an http.Client (with a DialHook that re-checks at
// the socket layer), plus an audit sink.
type Client struct {
	resolver *net.Resolver
	audit    AuditSink
}

// NewClient builds an EgressClient. A nil audit sink is allowed (no-op).
func NewClient(audit AuditSink) *Client {
	return &Client{
		resolver: &net.Resolver{},
		audit:    audit,
	}
}

// --- classified errors (§4.2 error_code mapping) ---

// Err is a classified egress error. Callers map Code to
// source_sync_runs.error_code (§4.2 / §6.5 retry-vs-dead).
type Err struct {
	Code    string
	Message string
	Cause   error
}

func (e *Err) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Err) Unwrap() error { return e.Cause }

// Stable error codes (§10.1 expected outcomes).
var (
	ErrLoopback         = &Err{Code: "loopback_blocked", Message: "egress: loopback address blocked"}
	ErrLinkLocal        = &Err{Code: "link_local_blocked", Message: "egress: link-local address blocked"}
	ErrMetadataEndpoint = &Err{Code: "metadata_endpoint_blocked", Message: "egress: cloud metadata endpoint blocked"}
	ErrPrivateRange     = &Err{Code: "private_range_blocked", Message: "egress: private range blocked"}
	ErrHostNotAllowed  = &Err{Code: "host_not_allowed", Message: "egress: host not in allowlist"}
	ErrTooManyRedirects = &Err{Code: "too_many_redirects", Message: "egress: redirect limit exceeded"}
	ErrResponseTooLarge = &Err{Code: "response_too_large", Message: "egress: response exceeded size limit"}
	ErrContentType      = &Err{Code: "content_type_rejected", Message: "egress: content-type not allowed"}
	ErrBadURL          = &Err{Code: "bad_url", Message: "egress: invalid URL"}
	ErrFetchFailed     = &Err{Code: "fetch_failed", Message: "egress: fetch failed"}
)

// Classify wraps an error with an egress Err if it isn't already one. Used by
// callers that need a stable error_code for source_sync_runs.
func Classify(err error) *Err {
	var e *Err
	if errors.As(err, &e) {
		return e
	}
	return &Err{Code: "fetch_failed", Message: "egress: fetch failed", Cause: err}
}

// --- URL redaction (§6.5) ---

// RedactURL strips embedded userinfo (user:pass@) from a URL string so logs /
// audit never carry plaintext credentials. Returns the input unchanged on parse
// failure (best-effort — a malformed URL is rejected earlier by FetchURL).
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.User != nil {
		u.User = nil
	}
	return u.String()
}

// --- FetchURL ---

// FetchURL fetches rawURL under policy. It resolves DNS, checks the resolved
// IPs against the private/metadata denylist + host allowlist, follows redirects
// (re-validating each), enforces size + content-type, and audits the call.
//
// The returned Body is a streaming reader capped at MaxResponseBytes; the caller
// MUST Close it. A size/timeout error mid-stream surfaces as the corresponding
// classified Err (the read is cut off, not silently truncated).
func (c *Client) FetchURL(ctx context.Context, rawURL string, pol Policy) (*Response, error) {
	start := time.Now()
	rec := AuditRecord{URL: RedactURL(rawURL)}
	defer func() {
		rec.Duration = time.Since(start)
		if c.audit != nil {
			c.audit.Record(ctx, rec)
		}
	}()

	pol = withDefaults(pol)

	visited, finalURL, finalReq, err := c.planRedirects(ctx, rawURL, pol)
	if err != nil {
		e := Classify(err)
		rec.ErrCode = e.Code
		rec.ErrDetail = e.Message
		return nil, e
	}

	httpClient := &http.Client{
		Timeout: pol.Timeout,
		// The redirect loop is already validated by planRedirects (which returns
		// a non-redirected request); force the client NOT to follow redirects
		// so the socket-level DialHook is the only path a connection takes.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: c.transport(pol),
	}

	reqCtx, cancel := context.WithTimeout(ctx, pol.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, finalReq.String(), nil)
	if err != nil {
		e := &Err{Code: "bad_url", Message: "egress: build request", Cause: err}
		rec.ErrCode = e.Code
		rec.ErrDetail = e.Message
		return nil, e
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		e := Classify(err)
		rec.ErrCode = e.Code
		rec.ErrDetail = e.Message
		return nil, e
	}

	rec.StatusCode = resp.StatusCode
	rec.URL = RedactURL(finalURL)
	ct := resp.Header.Get("Content-Type")
	if len(pol.AllowedContentTypes) > 0 && !contentTypeAllowed(ct, pol.AllowedContentTypes) {
		resp.Body.Close()
		e := &Err{Code: ErrContentType.Code, Message: "egress: content-type " + ct + " not allowed"}
		rec.ErrCode = e.Code
		rec.ErrDetail = e.Message
		return nil, e
	}

	// Wrap the body so reads enforce MaxResponseBytes and Close records bytes.
	lr := &limitedReadCloser{inner: resp.Body, limit: pol.MaxResponseBytes}
	return &Response{
		URL:           RedactURL(finalURL),
		StatusCode:    resp.StatusCode,
		ContentType:   ct,
		Bytes:         0,
		Body:          &countingCloser{lr: lr, rec: &rec},
		RedirectChain: visited,
	}, nil
}

// planRedirects walks the redirect chain up-front, resolving DNS + checking
// policy at each hop, returning the final non-redirect URL + a parsed request
// URL. This is a defense-in-depth companion to the DialHook: the dialer still
// re-checks at connect time (TOCTOU / DNS rebinding), but pre-checking lets us
// classify host/redirect errors before opening any socket (§10.1 用例 1/8/9).
func (c *Client) planRedirects(ctx context.Context, rawURL string, pol Policy) (visited, finalURL string, finalReq *url.URL, err error) {
	current := rawURL
	visited = RedactURL(current)
	for redirects := 0; redirects <= pol.MaxRedirects; redirects++ {
		u, perr := url.Parse(current)
		if perr != nil {
			return visited, current, nil, fmt.Errorf("%w: %v", ErrBadURL, perr)
		}
		host := u.Hostname()
		if host == "" {
			return visited, current, nil, ErrBadURL
		}
		// Host allowlist check (§10.1 用例 8).
		if !hostAllowed(host, pol.AllowDomains) {
			return visited, current, nil, ErrHostNotAllowed
		}
		// DNS resolve + IP policy check (§10.1 用例 1/2/9).
		ips, rerr := c.resolver.LookupIPAddr(ctx, host)
		if rerr != nil {
			return visited, current, nil, fmt.Errorf("%w: dns: %v", ErrFetchFailed, rerr)
		}
		for _, ip := range ips {
			if verr := checkIP(ip.IP, pol); verr != nil {
				return visited, current, nil, verr
			}
		}
		// Fetch the URL (single hop, no follow) to see if it redirects.
		httpClient := &http.Client{
			Timeout:       pol.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     c.transport(pol),
		}
		reqCtx, cancel := context.WithTimeout(ctx, pol.Timeout)
		req, rerr := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
		if rerr != nil {
			cancel()
			return visited, current, nil, fmt.Errorf("%w: %v", ErrBadURL, rerr)
		}
		resp, rerr := httpClient.Do(req)
		cancel()
		if rerr != nil {
			return visited, current, nil, Classify(rerr)
		}
		// Not a redirect → this is the final URL.
		if !isRedirect(resp.StatusCode) {
			resp.Body.Close()
			return visited, current, u, nil
		}
		loc := resp.Header.Get("Location")
		resp.Body.Close()
		if loc == "" {
			return visited, current, nil, fmt.Errorf("%w: redirect without Location", ErrFetchFailed)
		}
		next, lerr := url.Parse(loc)
		if lerr != nil {
			return visited, current, nil, fmt.Errorf("%w: bad Location: %v", ErrBadURL, lerr)
		}
		if !next.IsAbs() {
			next = u.ResolveReference(next)
		}
		current = next.String()
		visited = visited + " -> " + RedactURL(current)
		if redirects == pol.MaxRedirects {
			return visited, current, nil, ErrTooManyRedirects
		}
	}
	return visited, current, nil, ErrTooManyRedirects
}

// transport builds an http.Transport whose DialContext enforces the IP policy
// at the socket layer (defense in depth — §10.1 用例 4 DNS rebinding). The
// dialer re-resolves and re-checks each address right before connecting.
func (c *Client) transport(pol Policy) *http.Transport {
	dialer := &net.Dialer{
		Timeout:  pol.Timeout,
		Resolver: c.resolver,
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("%w: bad addr", ErrBadURL)
			}
			ips, err := c.resolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("%w: dns: %v", ErrFetchFailed, err)
			}
			for _, ip := range ips {
				if verr := checkIP(ip.IP, pol); verr != nil {
					return nil, verr
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
		},
		// Force HTTP/1.1 to keep redirect behavior predictable across sources.
		ForceAttemptHTTP2: false,
	}
}

// DialHook returns a net.Dialer.Control hook for non-HTTP protocols (git over
// SSH, custom API) that blocks private/metadata destinations at the socket
// layer. Defense in depth for protocols the http.Transport does not own.
func (c *Client) DialHook(pol Policy) func(network, addr string) (net.Conn, error) {
	pol = withDefaults(pol)
	return func(network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w: bad addr", ErrBadURL)
		}
		ips, err := c.resolver.LookupIPAddr(context.Background(), host)
		if err != nil {
			return nil, fmt.Errorf("%w: dns: %v", ErrFetchFailed, err)
		}
		for _, ip := range ips {
			if verr := checkIP(ip.IP, pol); verr != nil {
				return nil, verr
			}
		}
		d := &net.Dialer{Timeout: pol.Timeout}
		return d.Dial(network, net.JoinHostPort(host, port))
	}
}

// Transport returns an http.Transport whose DialContext enforces the IP policy
// at the socket layer. Exposed so non-FetchURL callers (e.g. a git-over-HTTPS
// HEAD probe) reuse the same DNS/IP enforcement without re-implementing it.
func (c *Client) Transport(pol Policy) *http.Transport {
	return c.transport(withDefaults(pol))
}

// --- IP policy ---

// checkIP returns a classified Err if ip is blocked under policy.
func checkIP(ip net.IP, pol Policy) error {
	if ip == nil {
		return ErrBadURL
	}
	// Metadata endpoint (§10.1 用例 2): 169.254.169.254 + 169.254.170.2
	if ip.IsLoopback() {
		return ErrLoopback
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		// Explicit metadata endpoint check before generic link-local pass.
		if isMetadataEndpoint(ip) {
			return ErrMetadataEndpoint
		}
		return ErrLinkLocal
	}
	if isMetadataEndpoint(ip) {
		return ErrMetadataEndpoint
	}
	if !pol.AllowPrivateRanges {
		if ip.IsPrivate() {
			return ErrPrivateRange
		}
		if ip.IsMulticast() {
			return ErrPrivateRange
		}
		if ip.IsUnspecified() {
			return ErrPrivateRange
		}
	}
	return nil
}

// isMetadataEndpoint reports whether ip is a known cloud metadata endpoint.
func isMetadataEndpoint(ip net.IP) bool {
	for _, cidr := range metadataCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

var metadataCIDRs = mustParseCIDRs([]string{
	"169.254.169.254/32", // AWS / GCP / Azure IMDS
	"169.254.170.2/32",   // AWS ECS task metadata
	"fd00:ec2::254/128",  // AWS IMDSv6
})

func mustParseCIDRs(cidrs []string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("egress: bad metadata CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}

// hostAllowed reports whether host matches the allowlist (exact or *.suffix).
// nil/empty allowlist = allow any host (private ranges still blocked).
func hostAllowed(host string, allow []string) bool {
	if len(allow) == 0 {
		return true
	}
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	for _, a := range allow {
		a = strings.ToLower(strings.TrimSpace(a))
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "*.") {
			suffix := a[1:] // ".suffix"
			if strings.HasSuffix(h, suffix) && len(h) > len(suffix) {
				return true
			}
			continue
		}
		if h == a {
			return true
		}
	}
	return false
}

// contentTypeAllowed reports whether ct (or its base type) is in the allowlist.
func contentTypeAllowed(ct string, allow []string) bool {
	if ct == "" {
		return false
	}
	base := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	for _, a := range allow {
		if strings.ToLower(strings.TrimSpace(a)) == base {
			return true
		}
	}
	return false
}

func isRedirect(code int) bool {
	return code >= 300 && code < 400 && code != 304 && code != 305
}

func withDefaults(pol Policy) Policy {
	if pol.MaxRedirects == 0 {
		pol.MaxRedirects = 5
	}
	if pol.MaxResponseBytes == 0 {
		pol.MaxResponseBytes = 100 * 1024 * 1024
	}
	if pol.Timeout == 0 {
		pol.Timeout = 30 * time.Second
	}
	return pol
}

// --- helpers: capped streaming reader ---

// limitedReadCloser wraps a body so reads beyond limit return ErrResponseTooLarge.
type limitedReadCloser struct {
	inner    io.ReadCloser
	limit    int64
	consumed int64
	exceeded bool
}

func (l *limitedReadCloser) Read(p []byte) (int, error) {
	if l.exceeded {
		return 0, ErrResponseTooLarge
	}
	remaining := l.limit - l.consumed
	if remaining <= 0 {
		l.exceeded = true
		return 0, ErrResponseTooLarge
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := l.inner.Read(p)
	l.consumed += int64(n)
	if l.consumed >= l.limit && err == nil {
		// Peek one more byte to see if there's more — if so, it's too large.
		var one [1]byte
		n2, err2 := l.inner.Read(one[:])
		if n2 > 0 || err2 == nil {
			l.exceeded = true
			return n, ErrResponseTooLarge
		}
	}
	return n, err
}

func (l *limitedReadCloser) Close() error { return l.inner.Close() }

// countingCloser records the bytes consumed into the audit record on Close.
type countingCloser struct {
	lr  *limitedReadCloser
	rec *AuditRecord
}

func (c *countingCloser) Read(p []byte) (int, error) { return c.lr.Read(p) }
func (c *countingCloser) Close() error {
	c.rec.Bytes = c.lr.consumed
	if c.lr.exceeded {
		c.rec.ErrCode = ErrResponseTooLarge.Code
		c.rec.ErrDetail = ErrResponseTooLarge.Message
	}
	return c.lr.Close()
}

// HashTargetKey derives a stable, content-free key from a workspace prefix +
// normalized path/URL, for Connector target_key generation (§4.3). It is a hash
// so the key is fixed-length and never leaks the original path's structure.
func HashTargetKey(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:])
}
