package connector

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	connport "github.com/lynn901/mora/internal/module/knowledge/source/connector"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTempFile creates a temp file with the given content + extension and
// returns its absolute path. The caller's t.Cleanup removes it.
func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

// writeTempFileRooted is writeTempFile that also returns the temp dir as the
// connector's AllowedRoot, so the path is provably confined (§6.4 "限制根目录").
func writeTempFileRooted(t *testing.T, name, content string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	path = filepath.Join(root, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return root, path
}

// TestFileConnector_Type asserts the adapter reports the file source type.
func TestFileConnector_Type(t *testing.T) {
	f := &FileConnector{}
	assert.Equal(t, connport.SourceFile, f.Type())
}

// TestFileConnector_Validate_HappyPath verifies a small markdown file passes
// Validate (exists, within size cap, non-dangerous extension).
func TestFileConnector_Validate_HappyPath(t *testing.T) {
	root, p := writeTempFileRooted(t, "note.md", "# hello")
	f := &FileConnector{MaxBytes: 1 << 20, AllowedRoot: root}
	require.NoError(t, f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized:      p,
		RequestedAssetType: "document",
	}))
}

// TestFileConnector_Validate_MissingFile asserts a missing file surfaces as
// ErrUnreachable (the connector never reveals whether the path is outside an
// allowed root vs. simply absent — both are unreachable, §6.4).
func TestFileConnector_Validate_MissingFile(t *testing.T) {
	root := t.TempDir()
	f := &FileConnector{AllowedRoot: root}
	err := f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized: filepath.Join(root, "nope.md"),
	})
	require.Error(t, err)
	var cerr *connport.Err
	require.ErrorAs(t, err, &cerr)
	assert.Equal(t, connport.ErrUnreachable.Code, cerr.Code)
}

// TestFileConnector_Validate_PathTraversal asserts a `..` traversal is
// rejected with ErrPathTraversal before any stat (§6.4 traversal guard).
func TestFileConnector_Validate_PathTraversal(t *testing.T) {
	f := &FileConnector{}
	err := f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized: "../../etc/passwd",
	})
	require.ErrorIs(t, err, connport.ErrPathTraversal)
}

// TestFileConnector_Validate_SizeCap asserts a file larger than MaxBytes is
// rejected with ErrContentTooLarge (§6.4 size cap).
func TestFileConnector_Validate_SizeCap(t *testing.T) {
	root, p := writeTempFileRooted(t, "big.md", string(make([]byte, 1024)))
	f := &FileConnector{MaxBytes: 100, AllowedRoot: root} // 100 bytes; file is 1KB
	err := f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized: p,
	})
	require.ErrorIs(t, err, connport.ErrContentTooLarge)
}

// TestFileConnector_ResolveRevision asserts the revision is mtime:size and
// IsLatest=true for an existing file.
func TestFileConnector_ResolveRevision(t *testing.T) {
	root, p := writeTempFileRooted(t, "r.md", "body")
	f := &FileConnector{AllowedRoot: root}
	rev, err := f.ResolveRevision(context.Background(), connport.Source{
		ID:            "src-1",
		SourceType:    connport.SourceFile,
		URINormalized: p,
	})
	require.NoError(t, err)
	assert.True(t, rev.IsLatest)
	assert.Contains(t, rev.Value, ":")
}

// TestFileConnector_Fetch_HappyPath verifies Fetch streams the file into the
// sink and returns a manifest entry whose content_hash is the sha256 of the
// file bytes. Uses NoopSink so no bytes are retained.
func TestFileConnector_Fetch_HappyPath(t *testing.T) {
	content := "# title\nbody"
	root, p := writeTempFileRooted(t, "doc.md", content)
	f := &FileConnector{MaxBytes: 1 << 20, AllowedRoot: root}
	sink := &NoopSink{}
	rev := connport.Revision{Value: "1:10", IsLatest: true}
	manifest, err := f.Fetch(context.Background(), connport.Source{
		ID:            "src-1",
		SourceType:    connport.SourceFile,
		URINormalized: p,
		SyncPolicy:    map[string]any{"asset_type": "document"},
	}, rev, sink)
	require.NoError(t, err)
	require.Len(t, manifest.Entries, 1)
	e := manifest.Entries[0]
	assert.Equal(t, "document", e.AssetType)
	assert.NotEmpty(t, e.ContentHash)
	assert.Equal(t, hashContent([]byte(content)), e.ContentHash)
	assert.Equal(t, rev, manifest.Revision)
}

// TestFileConnector_Fetch_PathTraversal asserts Fetch also enforces the
// traversal guard (not just Validate) — a crafted URI is rejected before any
// file is opened.
func TestFileConnector_Fetch_PathTraversal(t *testing.T) {
	f := &FileConnector{}
	sink := &NoopSink{}
	_, err := f.Fetch(context.Background(), connport.Source{
		ID:            "src-1",
		URINormalized: "../escape.md",
	}, connport.Revision{}, sink)
	require.ErrorIs(t, err, connport.ErrPathTraversal)
}

// TestFileConnector_Fetch_SizeCap asserts Fetch rejects an oversized file
// before opening it (no partial write to the sink).
func TestFileConnector_Fetch_SizeCap(t *testing.T) {
	root, p := writeTempFileRooted(t, "big.md", string(make([]byte, 1024)))
	f := &FileConnector{MaxBytes: 100, AllowedRoot: root}
	sink := &NoopSink{}
	_, err := f.Fetch(context.Background(), connport.Source{
		ID: "src-1", SourceType: connport.SourceFile, URINormalized: p,
	}, connport.Revision{IsLatest: true}, sink)
	require.ErrorIs(t, err, connport.ErrContentTooLarge)
}

// --- §10.1 用例 13: absolute-path traversal escapes the root after Clean ---

// TestFileConnector_PathTraversal_AbsoluteWithDotDot asserts that an absolute
// path containing ".." which filepath.Clean collapses to a path WITHOUT ".."
// (e.g. "/../etc/passwd" → "/etc/passwd") is rejected as ErrPathTraversal. The
// ".."-substring check alone misses this; the §6.4 root confinement
// (AllowedRoot prefix) is what catches it. Reported as a defect in §10.1
// 用例 13; this is the regression guard.
func TestFileConnector_PathTraversal_AbsoluteWithDotDot(t *testing.T) {
	root := t.TempDir()
	f := &FileConnector{MaxBytes: 1 << 20, AllowedRoot: root}
	err := f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized:      "/../etc/passwd",
		RequestedAssetType: "document",
	})
	require.ErrorIs(t, err, connport.ErrPathTraversal)
}

// TestFileConnector_PathTraversal_NoRootRejectsAll asserts a file source with
// no AllowedRoot configured rejects every path — a rootless file source cannot
// be safely confined (§6.4 "限制根目录"). This is the fail-closed default.
func TestFileConnector_PathTraversal_NoRootRejectsAll(t *testing.T) {
	f := &FileConnector{MaxBytes: 1 << 20} // no AllowedRoot
	err := f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized:      "/etc/passwd",
		RequestedAssetType: "document",
	})
	require.ErrorIs(t, err, connport.ErrPathTraversal)
}

// TestFileConnector_PathTraversal_OutsideRoot asserts a clean absolute path
// that is simply not under AllowedRoot (no ".." at all) is still rejected —
// root confinement, not just ".." detection.
func TestFileConnector_PathTraversal_OutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md") // different temp dir
	require.NoError(t, os.WriteFile(outside, []byte("x"), 0o600))
	f := &FileConnector{MaxBytes: 1 << 20, AllowedRoot: root}
	err := f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized:      outside,
		RequestedAssetType: "document",
	})
	require.ErrorIs(t, err, connport.ErrPathTraversal)
}

// TestFileConnector_PathTraversal_BoundarySafe asserts the root-prefix check
// is path-boundary aware: a path whose cleaned form merely shares a string
// prefix with AllowedRoot (e.g. "/dataevil" vs root "/data") is rejected,
// not a false positive for containment.
func TestFileConnector_PathTraversal_BoundarySafe(t *testing.T) {
	root := t.TempDir() // e.g. /tmp/TestXxx123
	// Sibling dir whose name extends root's last segment — must NOT count as
	// inside root. Build it as a literal string-suffix sibling.
	sibling := root + "evil"
	require.NoError(t, os.MkdirAll(sibling, 0o700))
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })
	f := &FileConnector{AllowedRoot: root}
	err := f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized:      sibling,
		RequestedAssetType: "document",
	})
	require.ErrorIs(t, err, connport.ErrPathTraversal)
}
