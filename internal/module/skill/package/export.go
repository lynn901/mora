package skillpkg

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/lynn901/mora/internal/domain"
)

// Export errors.
var (
	// ErrExportMismatch — the exported content_hash did not equal the import
	// content_hash (§9 往返门禁). The exporter aborts and the caller surfaces
	// this as a hard validation failure: a non-reproducible package is not
	// deliverable.
	ErrExportMismatch = errors.New("skill: export content_hash mismatch")
)

// ExportFile is one entry to (re)package on export. Content is the
// decompressed bytes; the exporter does not honor an executable bit on
// writeback (it writes a plain 0o644 header — §4.4: never materialize an
// executable bit). The Kind is metadata carried from the manifest.
type ExportFile struct {
	Path    string
	Content []byte
	Kind    string
}

// ExportResult is the exported archive plus the recomputed content_hash, so
// the caller can assert import→export equality before persisting delivery.
type ExportResult struct {
	Archive    []byte // gzip+tar bytes
	ContentHash string
	Hashes     map[string]string // path → sha256, for the validation_report
}

// ExportToWriter reconstructs a gzip+tar archive from the given files and
// writes it to w, returning the recomputed content_hash. The archive is
// canonical: entries are written in path-sorted order with normalized tar
// headers (fixed mode 0o644, zero uid/gid, deterministic mtime) so the
// content_hash is reproducible independent of the original tar packaging
// (§3.1 roundtrip anchor). No executable bit is ever written (§4.4).
//
// The content_hash returned here MUST equal the import content_hash for the
// roundtrip gate (§9) to pass; the service asserts this equality.
func ExportToWriter(w io.Writer, files []ExportFile) (string, map[string]string, error) {
	if len(files) == 0 {
		return "", nil, errors.New("skill: cannot export empty package")
	}
	// Canonical order: path-sorted.
	ordered := make([]ExportFile, len(files))
	copy(ordered, files)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })

	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	hashes := make(map[string]string, len(ordered))
	h := sha256.New()
	for _, f := range ordered {
		sum := sha256.Sum256(f.Content)
		sh := hex.EncodeToString(sum[:])
		hashes[f.Path] = sh
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", f.Path, len(f.Content), sh)

		hdr := &tar.Header{
			Name: f.Path,
			Mode: 0o644, // no executable bit, ever (§4.4)
			Size: int64(len(f.Content)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", nil, fmt.Errorf("skill: write tar header %s: %w", f.Path, err)
		}
		if _, err := tw.Write(f.Content); err != nil {
			return "", nil, fmt.Errorf("skill: write tar body %s: %w", f.Path, err)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), hashes, nil
}

// Export reconstructs a gzip+tar archive in memory and verifies the
// recomputed content_hash equals the expected import content_hash (§9 往返
// 门禁). On mismatch it returns ErrExportMismatch. This is the export-side
// roundtrip gate paired with Parse's import-side content_hash.
func Export(files []ExportFile, expectedContentHash string) (*ExportResult, error) {
	var buf bytes.Buffer
	ch, hashes, err := ExportToWriter(&buf, files)
	if err != nil {
		return nil, err
	}
	if expectedContentHash != "" && ch != expectedContentHash {
		return nil, fmt.Errorf("%w: got %s want %s", ErrExportMismatch, ch, expectedContentHash)
	}
	return &ExportResult{
		Archive:     buf.Bytes(),
		ContentHash: ch,
		Hashes:      hashes,
	}, nil
}

// ExportFromManifest rebuilds the export file list from a stored manifest +
// a content provider (the MinIO-backed original reader, supplied by the
// infra layer). It is the export path used by the GET :export route: pull the
// immutable original, re-derive the manifest, assert content_hash equality.
//
// The content provider yields the decompressed bytes for a manifest entry's
// path. In tests it is an in-memory map; in production it streams from MinIO
// via the same ArchiveReader the import path uses.
func ExportFromManifest(manifest domain.SkillManifest, provider ContentProvider) (*ExportResult, error) {
	if provider == nil {
		return nil, errors.New("skill: content provider is nil")
	}
	files := make([]ExportFile, 0, len(manifest.Files))
	for _, e := range manifest.Files {
		content, err := provider.Content(e.Path)
		if err != nil {
			return nil, fmt.Errorf("skill: export content for %s: %w", e.Path, err)
		}
		files = append(files, ExportFile{Path: e.Path, Content: content, Kind: e.Kind})
	}
	return Export(files, manifest.ContentHash)
}

// ContentProvider yields the decompressed content of a manifest entry by
// archive-relative path. The concrete MinIO-backed provider is wired by the
// infra layer (sub-task D); tests supply an in-memory provider.
type ContentProvider interface {
	Content(path string) ([]byte, error)
}
