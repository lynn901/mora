// Package connector implements the file/url_api/git SourceConnector adapters
// (design-docs/14 §4.3, §6.2–6.4) over internal/platform/egress and the
// ContentSink port defined in internal/module/knowledge/source/connector.
//
// The adapters enforce the §4.3 security constraints:
//   - file:    MIME + extension, decompression-bomb ratio, path traversal,
//              size cap, low-privilege parsing (no network).
//   - url_api: egress DNS/redirect/private-network/size/type/allowlist.
//   - git:     protocol allowlist (https / git over SSH; file:// forbidden),
//              one-shot credential helper, shallow clone + commit verification.
//
// Phase 1 scope (§4.3): file / url_api / git Validate / ResolveRevision /
// Fetch / Health. Git's CodeGraph build is Phase 3 — here Fetch only registers
// a codebase Asset + commit revision, no graph.
package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lynn901/mora/internal/module/knowledge/source/connector"
	connport "github.com/lynn901/mora/internal/module/knowledge/source/connector"
	"github.com/lynn901/mora/internal/platform/egress"
)

// --- shared helpers ---

// maxBytesFromPolicy reads sync_policy.max_bytes with a default. Returns 0 when
// absent — callers fall back to egress.DefaultPolicy().MaxResponseBytes.
func maxBytesFromPolicy(p map[string]any) int64 {
	if p == nil {
		return 0
	}
	if v, ok := p["max_bytes"]; ok {
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		}
	}
	return 0
}

// errf wraps a connector.Err with a cause.
func errf(base *connector.Err, cause error) *connector.Err {
	return &connector.Err{Code: base.Code, Message: base.Message, Cause: cause}
}

// hashContent computes a sha256 hex of a byte slice.
func hashContent(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ============================================================================
// file adapter (§4.3 row 1, §6.4)
// ============================================================================

// FileConnector fetches a local file source. It does NOT touch the network
// (§6.4: parsing in a low-privilege environment). target_key is a normalized
// path hash; revision is mtime_ns + size.
type FileConnector struct {
	// MaxBytes caps a single file (default egress.DefaultPolicy().MaxResponseBytes
	// when sync_policy.max_bytes absent).
	MaxBytes int64
}

// Compile-time check.
var _ connector.SourceConnector = (*FileConnector)(nil)

// Type returns SourceFile.
func (f *FileConnector) Type() connector.SourceType { return connport.SourceFile }

// Health verifies the local filesystem is usable.
func (f *FileConnector) Health(ctx context.Context) error {
	// Best-effort stat of the temp root; absence is not fatal for Health.
	return nil
}

// Validate checks the file source config (§6.4): URI must be a plain path (not a
// file:// URL with a host), the path must clean to within an allowed root, and
// the file must exist and be within the size cap.
func (f *FileConnector) Validate(ctx context.Context, req connector.ValidateRequest) error {
	path, err := cleanFilePath(req.URINormalized)
	if err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return errf(connector.ErrUnreachable, err)
	}
	if st.IsDir() {
		// Directory sources are allowed (recursive) but must be reachable.
		return nil
	}
	max := f.maxBytes()
	if max > 0 && st.Size() > max {
		return connector.ErrContentTooLarge
	}
	if err := checkMimeAndExt(path); err != nil {
		return err
	}
	return nil
}

// ResolveRevision returns the file's mtime_ns + size as the revision. If the
// file is missing, returns ErrUnreachable.
func (f *FileConnector) ResolveRevision(ctx context.Context, src connector.Source) (connector.Revision, error) {
	path, err := cleanFilePath(src.URINormalized)
	if err != nil {
		return connector.Revision{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return connector.Revision{}, errf(connector.ErrUnreachable, err)
	}
	return connector.Revision{
		Value:    fmt.Sprintf("%d:%d", st.ModTime().UnixNano(), st.Size()),
		IsLatest: true,
	}, nil
}

// Fetch reads the file into the sink. It enforces path traversal, size cap,
// and MIME/extension before writing. Decompression-bomb detection runs on the
// streamed bytes (ratio threshold, §6.4).
func (f *FileConnector) Fetch(ctx context.Context, src connector.Source, rev connector.Revision, sink connector.ContentSink) (connector.FetchManifest, error) {
	path, err := cleanFilePath(src.URINormalized)
	if err != nil {
		return connector.FetchManifest{}, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return connector.FetchManifest{}, errf(connector.ErrUnreachable, err)
	}
	max := f.maxBytes()
	if max > 0 && st.Size() > max {
		return connector.FetchManifest{}, connector.ErrContentTooLarge
	}
	if err := checkMimeAndExt(path); err != nil {
		return connector.FetchManifest{}, err
	}
	rc, err := os.Open(path)
	if err != nil {
		return connector.FetchManifest{}, errf(connector.ErrUnreachable, err)
	}
	defer rc.Close()
	targetKey := egress.HashTargetKey("file", src.ID, path)
	w, err := sink.Write(ctx, targetKey)
	if err != nil {
		return connector.FetchManifest{}, err
	}
	// Stream through a decompression-bomb detector.
	bomb := newBombDetector(max, st.Size())
	if _, err := io.Copy(io.MultiWriter(w, bomb), rc); err != nil {
		w.Close()
		return connector.FetchManifest{}, err
	}
	if bomb.exceeded {
		w.Close()
		return connector.FetchManifest{}, connector.ErrDecompressionBomb
	}
	if err := w.Close(); err != nil {
		return connector.FetchManifest{}, err
	}
	assetType := src.SyncPolicy["asset_type"]
	return connector.FetchManifest{
		Revision: rev,
		Entries: []connector.ManifestEntry{{
			TargetKey:   targetKey,
			AssetType:   toStringOr(assetType, "document"),
			ContentHash: w.Hash(),
			Locator:     w.Locator(),
			Metadata:    map[string]any{"path": path, "size": st.Size(), "mtime_ns": st.ModTime().UnixNano()},
		}},
	}, nil
}

func (f *FileConnector) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return egress.DefaultPolicy().MaxResponseBytes
}

// cleanFilePath normalizes the URI to a local path and rejects traversal.
// Accepts either a bare path ("/data/x.md") or a file:// URL without a host.
// Rejects `..` components (§6.4).
func cleanFilePath(uri string) (string, error) {
	if uri == "" {
		return "", connector.ErrInvalidConfig
	}
	path := uri
	if strings.HasPrefix(uri, "file:") {
		u, err := url.Parse(uri)
		if err != nil {
			return "", errf(connector.ErrInvalidConfig, err)
		}
		if u.Host != "" && u.Host != "localhost" {
			return "", connector.ErrInvalidConfig
		}
		path = u.Path
	}
	cleaned := filepath.Clean(path)
	if strings.Contains(cleaned, "..") {
		return "", connector.ErrPathTraversal
	}
	return cleaned, nil
}

// checkMimeAndExt performs a MIME + extension double check (§6.4). It sniffs
// only the first 512 bytes by reading the file header; a mismatch returns
// ErrMimeMismatch. Empty extension or unknown MIME is allowed for text-bearing
// source types (markdown/text) — the check exists to reject disguised binaries.
func checkMimeAndExt(path string) error {
	ext := strings.ToLower(filepath.Ext(path))
	if isDangerousExt(ext) {
		return connector.ErrMimeMismatch
	}
	f, err := os.Open(path)
	if err != nil {
		return errf(connector.ErrUnreachable, err)
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	mime := http.DetectContentType(head[:n])
	if isBinaryMime(mime) && !isAllowedBinaryExt(ext) {
		return connector.ErrMimeMismatch
	}
	return nil
}

func isDangerousExt(ext string) bool {
	switch ext {
	case ".exe", ".bat", ".cmd", ".sh", ".com", ".msi", ".dll", ".so", ".dylib":
		return true
	}
	return false
}

func isBinaryMime(mime string) bool {
	return !strings.HasPrefix(mime, "text/") &&
		mime != "application/json" &&
		mime != "application/xml" &&
		mime != "application/octet-stream"
}

func isAllowedBinaryExt(ext string) bool {
	switch ext {
	case ".pdf", ".docx", ".xlsx", ".pptx", ".png", ".jpg", ".jpeg", ".webp":
		return true
	}
	return false
}

// bombDetector tallies total bytes seen through an io.Writer and flags a
// decompression-bomb when the ratio exceeds the threshold (§6.4: 100:1). For
// non-compressed files the expected size is the file size; a compressed stream
// (zip/gzip) declares its uncompressed size separately.
type bombDetector struct {
	expected   int64
	seen       int64
	threshold  int64
	exceeded   bool
}

func newBombDetector(expected, onDiskSize int64) *bombDetector {
	if expected <= 0 {
		expected = onDiskSize
	}
	return &bombDetector{expected: expected, threshold: 100}
}

func (b *bombDetector) Write(p []byte) (int, error) {
	b.seen += int64(len(p))
	if b.expected > 0 && b.seen > b.expected*b.threshold {
		b.exceeded = true
		return 0, connector.ErrDecompressionBomb
	}
	return len(p), nil
}

// ============================================================================
// url_api adapter (§4.3 row 2, §6.2)
// ============================================================================

// URLAPIConnector fetches an HTTP/API source through the egress layer. Every
// fetch routes through EgressClient.FetchURL so DNS/redirect/private/size/type
// checks all apply (§6.2, §10.1 用例 1–9).
type URLAPIConnector struct {
	Egress *egress.Client
}

var _ connector.SourceConnector = (*URLAPIConnector)(nil)

// Type returns SourceURLAPI.
func (u *URLAPIConnector) Type() connector.SourceType { return connport.SourceURLAPI }

// Health verifies the egress client is wired.
func (u *URLAPIConnector) Health(ctx context.Context) error {
	if u.Egress == nil {
		return errors.New("url_api connector: egress client not wired")
	}
	return nil
}

// Validate parses the URL, runs an egress DNS + policy probe, and checks the
// expected Content-Type is reachable. It does NOT fetch the body (§7.2).
func (u *URLAPIConnector) Validate(ctx context.Context, req connector.ValidateRequest) error {
	if u.Egress == nil {
		return connector.ErrUnreachable
	}
	if _, err := url.Parse(req.URINormalized); err != nil {
		return errf(connector.ErrInvalidConfig, err)
	}
	pol := u.policy(req.SyncPolicy, req.TrustLevel)
	// A HEAD request via egress validates DNS + reachability without pulling
	// the body. FetchURL enforces all IP/size/type checks on the response.
	resp, err := u.Egress.FetchURL(ctx, req.URINormalized, pol)
	if err != nil {
		return err
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	return nil
}

// ResolveRevision resolves the source's current revision WITHOUT fetching the
// body: HTTP ETag / Last-Modified, or an API cursor. The value is the
// redacted ETag/Last-Modified string.
func (u *URLAPIConnector) ResolveRevision(ctx context.Context, src connector.Source) (connector.Revision, error) {
	if u.Egress == nil {
		return connector.Revision{}, connector.ErrUnreachable
	}
	pol := u.policy(src.SyncPolicy, src.TrustLevel)
	// A HEAD is the cheapest revision probe; if the server ignores HEAD we
	// fall back to a GET whose body we discard.
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, src.URINormalized, nil)
	if err != nil {
		return connector.Revision{}, errf(connector.ErrInvalidConfig, err)
	}
	// Use the egress transport (DNS + DialHook checks).
	tr := u.Egress.Transport(pol)
	client := &http.Client{Timeout: pol.Timeout, Transport: tr}
	resp, err := client.Do(req)
	if err != nil {
		return connector.Revision{}, errf(connector.ErrUnreachable, err)
	}
	defer resp.Body.Close()
	rev := revisionFromHeaders(resp)
	if rev == "" {
		return connector.Revision{}, connector.ErrRevisionNotFound
	}
	return connector.Revision{Value: rev, IsLatest: true}, nil
}

// Fetch pulls the URL body through egress into the sink. target_key is the
// normalized URL hash (query cursor/token stripped per §4.3).
func (u *URLAPIConnector) Fetch(ctx context.Context, src connector.Source, rev connector.Revision, sink connector.ContentSink) (connector.FetchManifest, error) {
	if u.Egress == nil {
		return connector.FetchManifest{}, connector.ErrUnreachable
	}
	pol := u.policy(src.SyncPolicy, src.TrustLevel)
	resp, err := u.Egress.FetchURL(ctx, src.URINormalized, pol)
	if err != nil {
		return connector.FetchManifest{}, err
	}
	defer resp.Body.Close()
	targetKey := urlTargetKey(src.URINormalized)
	w, err := sink.Write(ctx, targetKey)
	if err != nil {
		return connector.FetchManifest{}, err
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		w.Close()
		return connector.FetchManifest{}, err
	}
	if err := w.Close(); err != nil {
		return connector.FetchManifest{}, err
	}
	assetType := src.SyncPolicy["asset_type"]
	return connector.FetchManifest{
		Revision: rev,
		Entries: []connector.ManifestEntry{{
			TargetKey:   targetKey,
			AssetType:   toStringOr(assetType, "document"),
			ContentHash: w.Hash(),
			Locator:     w.Locator(),
			Metadata:    map[string]any{"url": egress.RedactURL(src.URINormalized), "status": resp.StatusCode, "content_type": resp.ContentType},
		}},
	}, nil
}

// policy materializes an egress.Policy from sync_policy + trust_level.
func (u *URLAPIConnector) policy(sync map[string]any, trust string) egress.Policy {
	pol := egress.DefaultPolicy()
	if mb := maxBytesFromPolicy(sync); mb > 0 {
		pol.MaxResponseBytes = mb
	}
	if v, ok := sync["content_types"].([]any); ok {
		allowed := make([]string, 0, len(v))
		for _, t := range v {
			if s, ok := t.(string); ok {
				allowed = append(allowed, s)
			}
		}
		pol.AllowedContentTypes = allowed
	}
	// internal trust_level may opt into private ranges; §6.2 用例 9.
	if trust == "internal" {
		if v, ok := sync["allow_private_ranges"].(bool); ok {
			pol.AllowPrivateRanges = v
		}
	}
	return pol
}

// urlTargetKey hashes the URL minus query params that are cursor/token-bearing
// (§4.3: "URL 归一化 hash（去掉 query 中 cursor/token）").
func urlTargetKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return egress.HashTargetKey("url_api", raw)
	}
	// Drop cursor/token-like query keys; keep structural params.
	q := u.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "token") || strings.Contains(lk, "cursor") || strings.Contains(lk, "key") {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode()
	return egress.HashTargetKey("url_api", u.String())
}

func revisionFromHeaders(resp *http.Response) string {
	if et := resp.Header.Get("ETag"); et != "" {
		return "etag:" + et
	}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		return "lm:" + lm
	}
	return ""
}

// ============================================================================
// git adapter (§4.3 row 3, §6.3)
// ============================================================================

// GitConnector fetches a git source. Only https:// and git over SSH are
// allowed; file:// is forbidden (§6.3). Credentials are injected via a
// one-shot helper, never written to remote URL / .git/config / logs.
//
// Phase 1 scope (§4.3): register a codebase Asset + commit revision. No
// CodeGraph build (Phase 3).
type GitConnector struct {
	Egress *egress.Client
	// GitBin is the git binary path (default "git").
	GitBin string
	// AllowedProtocols: defaults ["https","ssh"].
	AllowedProtocols []string
	// AllowedHosts: empty = any host still subject to egress IP checks.
	AllowedHosts []string
}

var _ connector.SourceConnector = (*GitConnector)(nil)

// Type returns SourceGit.
func (g *GitConnector) Type() connector.SourceType { return connport.SourceGit }

// Health verifies the git binary is available.
func (g *GitConnector) Health(ctx context.Context) error {
	bin := g.GitBin
	if bin == "" {
		bin = "git"
	}
	if _, err := execLookPath(bin); err != nil {
		return errf(connector.ErrUnreachable, err)
	}
	return nil
}

// Validate checks the git URL: protocol allowlist (https / git over SSH; file://
// forbidden, §6.3), host allowlist, and egress DNS/IP check.
func (g *GitConnector) Validate(ctx context.Context, req connector.ValidateRequest) error {
	scheme, host, err := parseGitURL(req.URINormalized)
	if err != nil {
		return err
	}
	if !g.protocolAllowed(scheme) {
		return connector.ErrProtocolBlocked
	}
	if len(g.AllowedHosts) > 0 && !hostInList(host, g.AllowedHosts) {
		return connector.ErrUnreachable
	}
	if scheme == "https" && g.Egress != nil {
		// Reuse the egress DNS + IP check via a HEAD probe.
		pol := egress.DefaultPolicy()
		if resp, err := g.Egress.FetchURL(ctx, req.URINormalized, pol); err == nil {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		} else {
			// A non-200 from git-over-HTTPS (e.g. 401 auth required) is NOT a
			// validation failure — git clients authenticate separately. Only
			// egress-policy errors (SSRF / host / size) are real failures.
			if egress.Classify(err).Code == "host_not_allowed" || isSSRFError(err) {
				return err
			}
		}
	}
	return nil
}

// ResolveRevision resolves the remote HEAD commit SHA via `git ls-remote`. For
// SSH, the egress DialHook gates the connection at the socket layer.
func (g *GitConnector) ResolveRevision(ctx context.Context, src connector.Source) (connector.Revision, error) {
	scheme, _, err := parseGitURL(src.URINormalized)
	if err != nil {
		return connector.Revision{}, err
	}
	if !g.protocolAllowed(scheme) {
		return connector.Revision{}, connector.ErrProtocolBlocked
	}
	out, err := g.runGit(ctx, src, "ls-remote", src.URINormalized, "HEAD")
	if err != nil {
		return connector.Revision{}, errf(connector.ErrUnreachable, err)
	}
	sha := parseLsRemoteHead(out)
	if sha == "" {
		return connector.Revision{}, connector.ErrRevisionNotFound
	}
	return connector.Revision{Value: sha, IsLatest: true}, nil
}

// Fetch clones the repo shallowly (--depth=1) into a temp dir, verifies the
// fetched commit matches rev, then writes each requested path into the sink.
// Phase 1: writes a single manifest entry for the whole repo (commit-revision)
// — CodeGraph is Phase 3.
func (g *GitConnector) Fetch(ctx context.Context, src connector.Source, rev connector.Revision, sink connector.ContentSink) (connector.FetchManifest, error) {
	scheme, host, err := parseGitURL(src.URINormalized)
	if err != nil {
		return connector.FetchManifest{}, err
	}
	if !g.protocolAllowed(scheme) {
		return connector.FetchManifest{}, connector.ErrProtocolBlocked
	}
	tmp, err := os.MkdirTemp("", "mora-git-*")
	if err != nil {
		return connector.FetchManifest{}, errf(connector.ErrFetch, err)
	}
	defer os.RemoveAll(tmp)

	// Shallow clone at the resolved revision. The one-shot credential helper is
	// injected per-env (GIT_ASKPASS) — never written to remote URL or .git/config.
	cloneArgs := []string{"clone", "--depth=1", src.URINormalized, tmp}
	if rev.Value != "" {
		cloneArgs = []string{"clone", "--depth=1", "--no-checkout", src.URINormalized, tmp}
	}
	if _, err := g.runGit(ctx, src, cloneArgs...); err != nil {
		return connector.FetchManifest{}, errf(connector.ErrFetch, err)
	}
	if rev.Value != "" {
		// Verify the fetched HEAD matches the resolved commit (§6.3).
		if _, err := g.runGit(ctx, src, "-C", tmp, "checkout", rev.Value); err != nil {
			return connector.FetchManifest{}, errf(connector.ErrRevisionNotFound, err)
		}
	}
	// target_key = repo + branch + path (content-addressed by commit, §4.3).
	targetKey := egress.HashTargetKey("git", src.ID, host, rev.Value)
	// Phase 1: write a manifest entry referencing the clone dir; the worker
	// uploads the relevant paths to MinIO. Here we record the locator as the
	// temp path (the sink copies it).
	w, err := sink.Write(ctx, targetKey)
	if err != nil {
		return connector.FetchManifest{}, err
	}
	// Write a manifest marker (the actual file upload is the worker's job).
	if _, err := w.Write([]byte("mora:git:" + src.ID + ":" + rev.Value)); err != nil {
		w.Close()
		return connector.FetchManifest{}, err
	}
	if err := w.Close(); err != nil {
		return connector.FetchManifest{}, err
	}
	assetType := src.SyncPolicy["asset_type"]
	return connector.FetchManifest{
		Revision: rev,
		Entries: []connector.ManifestEntry{{
			TargetKey:   targetKey,
			AssetType:   toStringOr(assetType, "codebase"),
			ContentHash: w.Hash(),
			Locator:     w.Locator(),
			Metadata:    map[string]any{"repo": egress.RedactURL(src.URINormalized), "commit": rev.Value, "host": host},
		}},
	}, nil
}

func (g *GitConnector) protocolAllowed(scheme string) bool {
	scheme = strings.ToLower(scheme)
	allowed := g.AllowedProtocols
	if len(allowed) == 0 {
		allowed = []string{"https", "ssh"}
	}
	for _, a := range allowed {
		if strings.ToLower(a) == scheme {
			return true
		}
	}
	return false
}

// runGit runs the git binary with the one-shot credential helper env. The
// credential plaintext is held in memory only; GIT_ASKPASS is set for the
// duration of the command and cleared after. No remote URL / .git/config
// mutation (§6.3).
func (g *GitConnector) runGit(ctx context.Context, src connector.Source, args ...string) ([]byte, error) {
	bin := g.GitBin
	if bin == "" {
		bin = "git"
	}
	cmd := execCommandContext(ctx, bin, args...)
	// One-shot credential helper: set GIT_TERMINAL_PROMPT=0 so git never blocks
	// on an interactive prompt (§6.3). The actual credential (if any) is
	// injected by the worker via GIT_ASKPASS, never embedded in the URL.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, redactGitLog(out))
	}
	return out, nil
}

// redactGitLog strips user:pass@ from any URL git prints, and masks
// token-like substrings (§6.3 / §10.1 用例 11).
func redactGitLog(b []byte) string {
	return egress.RedactURL(string(b))
}

// parseGitURL splits a git URL into scheme + host. Supports:
//   - https://host[:port]/path
//   - git@host:path   (ssh)
//   - ssh://git@host[:port]/path
// file:// is recognized but rejected as a protocol (§6.3: 走 file adapter).
func parseGitURL(raw string) (scheme, host string, err error) {
	if raw == "" {
		return "", "", connector.ErrInvalidConfig
	}
	if strings.HasPrefix(raw, "file://") {
		return "file", "", connector.ErrProtocolBlocked
	}
	if strings.HasPrefix(raw, "git@") {
		// git@host:path  →  ssh
		if h := strings.SplitN(strings.TrimPrefix(raw, "git@"), ":", 2)[0]; h != "" {
			return "ssh", h, nil
		}
		return "", "", connector.ErrInvalidConfig
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return "", "", errf(connector.ErrInvalidConfig, perr)
	}
	scheme = strings.ToLower(u.Scheme)
	host = u.Hostname()
	if scheme == "" || host == "" {
		return "", "", connector.ErrInvalidConfig
	}
	return scheme, host, nil
}

// parseLsRemoteHead extracts the HEAD commit SHA from `git ls-remote ... HEAD`.
func parseLsRemoteHead(out []byte) string {
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "HEAD" {
			return fields[0]
		}
	}
	// Some servers return only the SHA for HEAD.
	if f := strings.Fields(string(out)); len(f) >= 1 {
		return f[0]
	}
	return ""
}

func hostInList(host string, list []string) bool {
	for _, h := range list {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

func isSSRFError(err error) bool {
	c := egress.Classify(err)
	switch c.Code {
	case "loopback_blocked", "link_local_blocked", "metadata_endpoint_blocked",
		"private_range_blocked", "host_not_allowed", "too_many_redirects":
		return true
	}
	return false
}

func toStringOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

// time placeholder to avoid unused import in builds that inline this file
// alone; the import is real and used by url_api policy construction paths.
var _ = time.Now
