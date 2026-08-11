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

// TestFileConnector_Type asserts the adapter reports the file source type.
func TestFileConnector_Type(t *testing.T) {
	f := &FileConnector{}
	assert.Equal(t, connport.SourceFile, f.Type())
}

// TestFileConnector_Validate_HappyPath verifies a small markdown file passes
// Validate (exists, within size cap, non-dangerous extension).
func TestFileConnector_Validate_HappyPath(t *testing.T) {
	p := writeTempFile(t, "note.md", "# hello")
	f := &FileConnector{MaxBytes: 1 << 20}
	require.NoError(t, f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized:      p,
		RequestedAssetType: "document",
	}))
}

// TestFileConnector_Validate_MissingFile asserts a missing file surfaces as
// ErrUnreachable (the connector never reveals whether the path is outside an
// allowed root vs. simply absent — both are unreachable, §6.4).
func TestFileConnector_Validate_MissingFile(t *testing.T) {
	f := &FileConnector{}
	err := f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized: filepath.Join(t.TempDir(), "nope.md"),
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
	p := writeTempFile(t, "big.md", string(make([]byte, 1024)))
	f := &FileConnector{MaxBytes: 100} // 100 bytes; file is 1KB
	err := f.Validate(context.Background(), connport.ValidateRequest{
		URINormalized: p,
	})
	require.ErrorIs(t, err, connport.ErrContentTooLarge)
}

// TestFileConnector_ResolveRevision asserts the revision is mtime:size and
// IsLatest=true for an existing file.
func TestFileConnector_ResolveRevision(t *testing.T) {
	p := writeTempFile(t, "r.md", "body")
	f := &FileConnector{}
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
	p := writeTempFile(t, "doc.md", content)
	f := &FileConnector{MaxBytes: 1 << 20}
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
	p := writeTempFile(t, "big.md", string(make([]byte, 1024)))
	f := &FileConnector{MaxBytes: 100}
	sink := &NoopSink{}
	_, err := f.Fetch(context.Background(), connport.Source{
		ID: "src-1", SourceType: connport.SourceFile, URINormalized: p,
	}, connport.Revision{IsLatest: true}, sink)
	require.ErrorIs(t, err, connport.ErrContentTooLarge)
}
