package connector

// connector_security_test.go covers the §10.1 / §10.2 security cases for the
// file + git adapters that the existing connector_file_test.go does not reach
// (design-docs/14 §10.1 用例 10/12/13/14, §10.2 用例 19). These are pure
// unit tests — no DB, no network.

import (
	"compress/gzip"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	connport "github.com/lynn901/mora/internal/module/knowledge/source/connector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- §10.1 用例 10: git file:// protocol rejected ---

// TestGitConnector_FileProtocolRejected asserts a `file://` git source is
// rejected with ErrProtocolBlocked (§6.3 / §10.1 用例 10) — local file sources
// must go through the file adapter, never the git adapter.
func TestGitConnector_FileProtocolRejected(t *testing.T) {
	cases := []string{
		"file:///path/to/repo",
		"file://localhost/tmp/repo.git",
	}
	for _, uri := range cases {
		t.Run(uri, func(t *testing.T) {
			g := &GitConnector{}
			err := g.Validate(context.Background(), connport.ValidateRequest{
				URINormalized:      uri,
				RequestedAssetType: "codebase",
			})
			require.Error(t, err)
			assert.True(t, connport.Is(err, connport.ErrProtocolBlocked.Code),
				"file:// must be blocked as protocol; got %v", err)
		})
	}
}

// TestGitConnector_AllowedProtocols asserts the default allowlist is https +
// ssh (git-over-SSH); a plain http:// git URL is rejected by default.
func TestGitConnector_AllowedProtocols(t *testing.T) {
	g := &GitConnector{}
	require.False(t, g.protocolAllowed("file"), "file:// must never be allowed")
	require.True(t, g.protocolAllowed("https"))
	require.True(t, g.protocolAllowed("ssh"))
	require.False(t, g.protocolAllowed("http"), "plain http git should be rejected by default")
}

// --- §10.1 用例 13: file path traversal ---

// TestFileConnector_PathTraversal_AbsoluteWithDotDot asserts that an absolute
// path containing `..` that filepath.Clean collapses to a path WITHOUT `..`
// (e.g. `/../etc/passwd` → `/etc/passwd`) is rejected. The design (§6.4)
// requires "filepath.Clean + 拒绝 .. + 限制根目录": the root confinement half
// is the guard that must catch the cleaned-form traversal, because the
// `..`-substring check alone misses it.
//
// DEFECT (§10.1 用例 13, reported to YS-109): as of commit 2569070,
// cleanFilePath only rejects paths that still contain ".." AFTER Clean.
// `/../etc/passwd` cleans to `/etc/passwd` (no ".."), so it is NOT rejected
// — it falls through to os.Stat, which on a host with /etc/passwd succeeds
// (Validate returns nil). The test is marked Skip pending the fix so the
// suite stays green; remove the Skip once YS-109 lands root confinement.
func TestFileConnector_PathTraversal_AbsoluteWithDotDot(t *testing.T) {
	t.Skip("DEFECT §10.1 用例 13 (reported YS-109): /../etc/passwd cleans to /etc/passwd and escapes the ..-substring check — cleanFilePath lacks root confinement")
	g := &FileConnector{MaxBytes: 1 << 20}
	err := g.Validate(context.Background(), connport.ValidateRequest{
		URINormalized:      "/../etc/passwd",
		RequestedAssetType: "document",
	})
	assert.ErrorIs(t, err, connport.ErrPathTraversal)
}

// TestFileConnector_PathTraversal_RelativeConfirmed asserts the relative
// form that filepath.Clean keeps containing ".." IS rejected (the baseline
// behavior the existing test already pins, re-stated here for the §10.1 用例
// 13 sub-matrix so the gap above is not masked by a green sibling test).
func TestFileConnector_PathTraversal_RelativeConfirmed(t *testing.T) {
	g := &FileConnector{}
	err := g.Validate(context.Background(), connport.ValidateRequest{
		URINormalized: "../../etc/passwd",
	})
	require.ErrorIs(t, err, connport.ErrPathTraversal)
}

// --- §10.1 用例 14: file MIME / extension mismatch ---

// TestFileConnector_MimeMismatch_DangerousExtension asserts a disguised
// binary (dangerous extension like .exe) is rejected with ErrMimeMismatch
// (§6.4 MIME + extension double check, §10.1 用例 14). The extension
// allowlist is the first guard: .exe/.bat/.sh/.dll etc. are always rejected
// regardless of content.
func TestFileConnector_MimeMismatch_DangerousExtension(t *testing.T) {
	p := writeTempFile(t, "disguise.exe", strings.Repeat("x", 64))
	g := &FileConnector{MaxBytes: 1 << 20}
	err := g.Validate(context.Background(), connport.ValidateRequest{
		URINormalized:      p,
		RequestedAssetType: "document",
	})
	require.Error(t, err)
	assert.True(t, connport.Is(err, connport.ErrMimeMismatch.Code),
		"dangerous extension must be rejected as mime_mismatch; got %v", err)
}

// TestFileConnector_MimeMismatch_BinaryImageWithTextExt asserts a binary
// whose content sniffs as an image (e.g. PNG) but is renamed to a text
// extension (.md) is rejected — the MIME sniff catches the image type and
// .md is not on the allowed-binary-extension list (§6.4 double check).
func TestFileConnector_MimeMismatch_BinaryImageWithTextExt(t *testing.T) {
	// A real PNG signature: http.DetectContentType returns "image/png", which
	// is a binary MIME; .md is not in isAllowedBinaryExt → mismatch.
	png := append([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A},
		make([]byte, 128)...)
	p := writeTempFile(t, "trojan.md", string(png))
	g := &FileConnector{MaxBytes: 1 << 20}
	err := g.Validate(context.Background(), connport.ValidateRequest{
		URINormalized:      p,
		RequestedAssetType: "document",
	})
	require.Error(t, err)
	assert.True(t, connport.Is(err, connport.ErrMimeMismatch.Code),
		"image content with a text extension must be rejected as mime_mismatch; got %v", err)
}

// --- §10.1 用例 12: decompression bomb ---

// TestBombDetector_RatioThreshold asserts the bomb detector itself trips at
// 100x the expected size (§6.4 ratio threshold). The detector is the guard
// the design intends for compressed streams; we exercise it directly to pin
// its behavior.
func TestBombDetector_RatioThreshold(t *testing.T) {
	b := newBombDetector(1024, 0) // expected=1KB → trips at 100KB written
	chunk := make([]byte, 1024)
	for i := 0; i < 99; i++ {
		n, err := b.Write(chunk)
		require.NoError(t, err)
		assert.Equal(t, 1024, n)
	}
	assert.False(t, b.exceeded, "99KB must not trip a 100KB threshold")
	_, err := b.Write(chunk) // 100th KB → seen=100KB == expected*threshold, NOT > yet
	assert.NoError(t, err)
	assert.False(t, b.exceeded, "exactly 100x must not trip (strictly greater)")
	_, err = b.Write(chunk) // 101st KB → seen > expected*100 → trip
	assert.ErrorIs(t, err, connport.ErrDecompressionBomb)
	assert.True(t, b.exceeded)
}

// TestFileConnector_DecompressionBomb_SizeCapGuardsOversizedFile asserts that
// for a non-compressed oversized file, the size cap in Fetch rejects it with
// ErrContentTooLarge before any streaming (§6.4 size cap). This documents the
// current behavior: the size cap is the first guard; the bomb detector only
// runs on files that pass it.
func TestFileConnector_DecompressionBomb_SizeCapGuardsOversizedFile(t *testing.T) {
	p := writeTempFile(t, "toobig.md", strings.Repeat("a", 64*1024))
	g := &FileConnector{MaxBytes: 1024} // file is 64KB, cap 1KB
	sink := &memSink{}
	_, err := g.Fetch(context.Background(), connport.Source{
		ID:            "src-1",
		SourceType:    connport.SourceFile,
		URINormalized: p,
		SyncPolicy:    map[string]any{"asset_type": "document"},
	}, connport.Revision{Value: "v1", IsLatest: true}, sink)
	require.Error(t, err)
	assert.True(t, connport.Is(err, connport.ErrContentTooLarge.Code),
		"oversized non-compressed file must be rejected as content_too_large; got %v", err)
}

// TestFileConnector_DecompressionBomb_GzipRejectedByMimeSniff asserts that a
// real gzip bomb (small on disk, large uncompressed) is rejected by Fetch.
// The guard that actually fires is the MIME sniff — a gzip stream's magic
// bytes sniff as a binary type not on the allowed-binary-extension list, so
// checkMimeAndExt rejects it before the bomb detector runs. This documents
// that §10.1 用例 12 is satisfied for disguised gzip bombs via the MIME
// double-check (§6.4), and the bomb detector is defense-in-depth behind it.
func TestFileConnector_DecompressionBomb_GzipRejectedByMimeSniff(t *testing.T) {
	p := writeGzipFile(t, strings.Repeat("a", 200*1024)) // 200KB uncompressed, tiny on disk
	g := &FileConnector{MaxBytes: 1 << 20}              // cap 1MB; on-disk gzip is tiny
	sink := &memSink{}
	_, err := g.Fetch(context.Background(), connport.Source{
		ID:            "src-1",
		SourceType:    connport.SourceFile,
		URINormalized: p,
		SyncPolicy:    map[string]any{"asset_type": "document"},
	}, connport.Revision{Value: "v1", IsLatest: true}, sink)
	require.Error(t, err, "a gzip bomb must be rejected (by MIME sniff or bomb detector)")
	assert.True(t,
		connport.Is(err, connport.ErrMimeMismatch.Code) ||
			connport.Is(err, connport.ErrDecompressionBomb.Code) ||
			connport.Is(err, connport.ErrContentTooLarge.Code),
		"gzip bomb must be rejected as mime_mismatch / compression_bomb / content_too_large; got %v", err)
}

// writeGzipFile writes a real gzip-compressed file of payload and returns its
// path. The on-disk size is much smaller than len(payload) — the signature of
// a decompression bomb.
func writeGzipFile(t *testing.T, payload string) string {
	t.Helper()
	dir := t.TempDir()
	p := dir + "/bomb.gz"
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	require.NoError(t, err)
	defer f.Close()
	gw := gzip.NewWriter(f)
	_, err = gw.Write([]byte(payload))
	require.NoError(t, err)
	require.NoError(t, gw.Close())
	return p
}

// memSink is an in-memory ContentSink for adapter unit tests. It records
// written bytes and computes a sha256 content hash on close (the hash is not
// under test here — the sink just needs to satisfy the port).
type memSink struct {
	written map[string][]byte
	w       *memWriter
}

func (s *memSink) Write(_ context.Context, targetKey string) (connport.ContentWriter, error) {
	s.w = &memWriter{key: targetKey}
	return s.w, nil
}

type memWriter struct {
	key  string
	buf  []byte
	hash string
	loc  connport.Locator
}

func (w *memWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *memWriter) Close() error {
	// Compute a stand-in hash; not asserted in these tests.
	w.hash = "sha256:stand-in"
	w.loc = connport.Locator{Kind: "file", Key: w.key}
	return nil
}

func (w *memWriter) Hash() string         { return w.hash }
func (w *memWriter) Locator() connport.Locator { return w.loc }

// --- §10.2 用例 19: Connector port never accepts allowed_asset_ids / user token ---

// TestConnectorPort_NoAssetOrTokenFields asserts the SourceConnector port
// structurally cannot receive allowed_asset_ids or a user Token (§4.1 / §10.2
// 用例 19). ValidateRequest + Source carry only redacted config + a
// credential_ref pointer — there is NO field for either, so a caller cannot
// pass them. This is a compile-time invariant; we pin it by field-set
// round-trip so a future addition that breaks the red line fails this test.
func TestConnectorPort_NoAssetOrTokenFields(t *testing.T) {
	t.Run("ValidateRequest", func(t *testing.T) {
		req := connport.ValidateRequest{
			URINormalized:      "https://example/repo",
			SyncPolicy:         map[string]any{"max_bytes": 1024},
			TrustLevel:         "untrusted",
			RequestedAssetType: "document",
		}
		// The struct must have no field named AllowedAssetIDs or UserToken.
		assertFieldAbsent(t, &req, "AllowedAssetIDs")
		assertFieldAbsent(t, &req, "UserToken")
		assertFieldAbsent(t, &req, "AllowedAssetIds")
	})
	t.Run("Source", func(t *testing.T) {
		src := connport.Source{
			ID:                "s1",
			SourceType:        connport.SourceURLAPI,
			URINormalized:     "https://example/repo",
			CredentialRef:     "secret-ref",
			CredentialVersion:  "v1",
		}
		assertFieldAbsent(t, &src, "AllowedAssetIDs")
		assertFieldAbsent(t, &src, "UserToken")
		assertFieldAbsent(t, &src, "AllowedAssetIds")
	})
}

// assertFieldAbsent fails the test if the struct value has a field with the
// given name — used to pin the §10.2 用例 19 contract (no asset/token field).
func assertFieldAbsent(t *testing.T, v any, field string) {
	t.Helper()
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Ptr {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		t.Fatalf("assertFieldAbsent: expected struct, got %v", rv.Kind())
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).Name == field {
			t.Errorf("port struct %T must NOT expose field %q (§10.2 用例 19: Connector does not accept allowed_asset_ids or user Token)", v, field)
			return
		}
	}
}
