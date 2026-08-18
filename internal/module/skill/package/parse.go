package skillpkg

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/lynn901/mora/internal/domain"
)

// Parse errors. The service maps these to the API error envelope; the
// validator also surfaces some of them as findings.
var (
	// ErrArchiveTooLarge — a single entry or the total decompressed size
	// exceeded the cap (§4.4 anti-compression-bomb). 400.
	ErrArchiveTooLarge = errors.New("skill: archive exceeds size cap")
	// ErrTooManyEntries — the archive has more than MaxFileCount entries. 400.
	ErrTooManyEntries = errors.New("skill: archive has too many entries")
	// ErrArchivePathTraversal — an entry escapes the archive root (../ or
	// absolute). Aborts the parse; the entry is never materialized. §4.4.
	ErrArchivePathTraversal = errors.New("skill: archive path traversal")
	// ErrNotTarGz — the stream is not a valid gzip+tar archive. 400.
	ErrNotTarGz = errors.New("skill: not a tar.gz archive")
	// ErrDuplicateEntry — two entries normalize to the same path (tamper /
	// overlapping symlink). Aborts the parse for a deterministic manifest.
	ErrDuplicateEntry = errors.New("skill: duplicate archive entry")
	// ErrMissingSkillMD — no SKILL.md present (§4 structure check). The
	// service treats this as a validation failure.
	ErrMissingSkillMD = errors.New("skill: SKILL.md missing")
)

// FileKind classifies an archive entry by role (manifest metadata only — it
// never affects execution). Used by the manifest's per-file Kind field.
type FileKind string

const (
	KindSkillMD  FileKind = "skill_md"
	KindScript   FileKind = "script"
	KindAsset    FileKind = "asset"
	KindManifest FileKind = "manifest"
	KindOther    FileKind = "other"
)

// ParsedFile is one extracted archive entry held in memory. Content is the
// decompressed bytes (bounded by MaxPerFileSize). It is NEVER written to disk
// with an executable bit — the parse is in-memory only (§4.4/§7).
type ParsedFile struct {
	Path    string
	Content []byte
	Hash    string // sha256 hex of Content (empty for symlink entries)
	ExecBit bool   // executable bit detected (mode & 0o111)
	Kind    FileKind
}

// ParsedPackage is the result of Parse: the file inventory (in archive order
// for determinism), the SKILL.md entry (required for structure), and the raw
// frontmatter map preserved verbatim. The manifest and content_hash are
// derived from Files; the caller (service) persists them.
type ParsedPackage struct {
	Files                []ParsedFile
	SkillMD              *ParsedFile
	OriginalFrontmatter  map[string]any // unknown legal fields, lossless (§2.3)
	DeclaredFormatID     string         // from SKILL.md frontmatter, if any
	DeclaredSchemaVer    string         // from SKILL.md frontmatter, if any
	DeclaredCapabilities map[string]any // declared tools/skills/resources summary
	// FrontmatterParseErr is set when the SKILL.md frontmatter is present but
	// malformed. The package is still saveable (§4.3: malformed frontmatter is
	// a warn finding, not a block); the validator records this error.
	FrontmatterParseErr error
}

// ArchiveReader is the storage abstraction over the immutable archive
// original (§7 import path). The concrete MinIO adapter lives in
// internal/infra (sub-task D); tests supply an in-memory reader. The reader
// returns the raw archive bytes as a stream so the parse stays storage-free.
//
// SECURITY: implementations MUST return the archive as stored — Mora never
// synthesizes or re-packs an executable bit here. The locator (storage_key)
// is opaque to this package.
type ArchiveReader interface {
	// Open returns a streaming reader over the archive bytes identified by
	// storageKey. The caller MUST Close it.
	Open(storageKey string) (io.ReadCloser, error)
}

// ParseResult carries the parsed package plus the derived manifest and
// content_hash so the service has everything to persist in one call.
type ParseResult struct {
	Package     ParsedPackage
	Manifest    domain.SkillManifest
	ContentHash string
}

// Parse consumes an archive stream and produces a normalized manifest + the
// lossless parsed package (§2.1 / §2.3). It is the single entry point of the
// import path; preview/index/validate reuse the parsed result so the script-
// execution count stays 0 across all four paths (§4.4 / §9).
//
// The archive MUST be gzip-compressed tar (.tar.gz). Decompression is streamed
// (gzip.Reader over the storage stream); tar headers are walked one entry at a
// time. Each regular file is read into memory up to MaxPerFileSize; the
// running total is capped at MaxTotalDecompressedSize. Any overflow aborts
// with ErrArchiveTooLarge — a compression bomb cannot exhaust memory.
//
// Non-regular entries (directories, symlinks, devices) are recorded as
// metadata only; symlink targets are NOT followed (§4.4 — never materialize
// an escape path). Path traversal (.. / absolute) aborts the parse.
func Parse(storageKey string, r ArchiveReader) (*ParseResult, error) {
	if r == nil {
		return nil, errors.New("skill: archive reader is nil")
	}
	rc, err := r.Open(storageKey)
	if err != nil {
		return nil, fmt.Errorf("skill: open archive %q: %w", storageKey, err)
	}
	defer rc.Close()

	gz, err := gzip.NewReader(rc)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotTarGz, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	parsed := &ParsedPackage{}
	seen := make(map[string]bool)
	var totalSize int64

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrNotTarGz, err)
		}
		if len(parsed.Files) >= MaxFileCount {
			return nil, ErrTooManyEntries
		}

		clean, err := normalizeArchivePath(hdr.Name)
		if err != nil {
			return nil, err // traversal
		}
		if clean == "" {
			continue // directories and empty names are skipped
		}
		if seen[clean] {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateEntry, clean)
		}
		seen[clean] = true

		// Only regular files and symlinks are inspected. Directories and
		// other types are metadata only. Symlink targets are NOT followed.
		switch hdr.Typeflag {
		case tar.TypeSymlink:
			// Record the symlink as an asset entry without content; following
			// it would risk materializing an escape path (§4.4).
			parsed.Files = append(parsed.Files, ParsedFile{
				Path: clean,
				Hash: "",
				Kind: classifyKind(clean),
			})
			continue
		case tar.TypeReg, tar.TypeRegA:
			// fall through to read content
		default:
			// directories, devices, fifos: skip
			continue
		}

		// Regular file: bound the read by the per-file cap, then the total.
		if hdr.Size < 0 || hdr.Size > MaxPerFileSize {
			return nil, fmt.Errorf("%w: entry %s size %d", ErrArchiveTooLarge, clean, hdr.Size)
		}
		if totalSize+hdr.Size > MaxTotalDecompressedSize {
			return nil, fmt.Errorf("%w: total would exceed cap at %s", ErrArchiveTooLarge, clean)
		}
		// tar.Reader stops at the entry boundary, but defend against a
		// malformed size field with a LimitReader as depth-in-determinant.
		limited := io.LimitReader(tr, MaxPerFileSize+1)
		content, err := io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("skill: read entry %s: %w", clean, err)
		}
		if int64(len(content)) > MaxPerFileSize {
			return nil, fmt.Errorf("%w: entry %s exceeded per-file cap", ErrArchiveTooLarge, clean)
		}
		totalSize += int64(len(content))

		sum := sha256.Sum256(content)
		pf := ParsedFile{
			Path:    clean,
			Content: content,
			Hash:    hex.EncodeToString(sum[:]),
			ExecBit: isExecBit(hdr.Mode),
			Kind:    classifyKind(clean),
		}
		if pf.Kind == KindSkillMD && parsed.SkillMD == nil {
			smd := pf
			parsed.SkillMD = &smd
		}
		parsed.Files = append(parsed.Files, pf)
	}

	if parsed.SkillMD == nil {
		return nil, ErrMissingSkillMD
	}

	// Extract frontmatter + declared format/schema/capabilities from SKILL.md.
	// Unknown legal fields are preserved verbatim (§2.3).
	fm, fmtID, schemaVer, caps, fmErr := parseSkillMDFrontmatter(parsed.SkillMD.Content)
	if fmErr != nil {
		parsed.FrontmatterParseErr = fmErr
	}
	parsed.OriginalFrontmatter = fm
	parsed.DeclaredFormatID = fmtID
	parsed.DeclaredSchemaVer = schemaVer
	parsed.DeclaredCapabilities = caps

	manifest := buildManifest(parsed.Files, caps)
	return &ParseResult{
		Package:     *parsed,
		Manifest:    manifest,
		ContentHash: manifest.ContentHash,
	}, nil
}

// buildManifest derives the normalized manifest (§2.1) and the content_hash
// (§3.1 roundtrip anchor) from the parsed files. The content_hash is computed
// over a canonical, ordered representation of (path, size, hash) triples —
// NOT over the raw tar bytes — so export reproducibility does not depend on
// tar header timestamps, gid/uid, or packaging order.
func buildManifest(files []ParsedFile, caps map[string]any) domain.SkillManifest {
	entries := make([]domain.SkillFileEntry, 0, len(files))
	var total int64
	// Sort by path for a canonical, reproducible content_hash.
	ordered := make([]ParsedFile, len(files))
	copy(ordered, files)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	for _, f := range ordered {
		entries = append(entries, domain.SkillFileEntry{
			Path:    f.Path,
			Size:    int64(len(f.Content)),
			Hash:    f.Hash,
			ExecBit: f.ExecBit,
			Kind:    string(f.Kind),
		})
		total += int64(len(f.Content))
	}
	return domain.SkillManifest{
		Files:             entries,
		CapabilitySummary: caps,
		ContentHash:       computeContentHash(ordered),
		EntryCount:        len(entries),
		TotalSize:         total,
	}
}

// computeContentHash produces the roundtrip consistency anchor (§3.1). It
// hashes a canonical string of ordered (path, size, sha256) triples so two
// archives with identical contents but different tar metadata yield the same
// content_hash. This is the value that must match on import→export.
func computeContentHash(ordered []ParsedFile) string {
	h := sha256.New()
	for _, f := range ordered {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", f.Path, len(f.Content), f.Hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeArchivePath cleans an archive entry path and rejects traversal
// (absolute paths or any ".." segment that escapes the archive root, §4.4).
// Returns "" for a path that should be skipped (directories, ".").
//
// The ".." check runs on the raw segments BEFORE path.Clean, because Go's
// path.Clean("/../x") collapses to "/x" (an anchored path cannot escape
// above root), which would silently swallow a traversal. We must reject the
// traversal itself, not its post-clean residue.
func normalizeArchivePath(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	// Reject any ".." path segment in the raw name (defense before Clean).
	if isTraversalSegment(name) {
		return "", fmt.Errorf("%w: %s", ErrArchivePathTraversal, name)
	}
	cleaned := path.Clean("/" + name) // anchor at root so ".." resolves to "/"
	if cleaned == "/" || cleaned == "/." {
		return "", nil
	}
	rel := strings.TrimPrefix(cleaned, "/")
	if rel == "" || rel == "." {
		return "", nil
	}
	// After anchoring+cleaning, any residual "../" means Clean could not
	// fully collapse it (shouldn't happen post-segment-check, but defense in
	// depth — §4.4).
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return "", fmt.Errorf("%w: %s", ErrArchivePathTraversal, name)
	}
	return rel, nil
}

// isTraversalSegment reports whether the raw archive path contains a ".."
// segment or is absolute. A segment-level check (not substring) so a
// legitimate file named "..hidden" is not false-flagged.
func isTraversalSegment(name string) bool {
	if strings.HasPrefix(name, "/") || strings.HasPrefix(name, "\\") {
		return true
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// isExecBit reports whether the tar mode carries any executable bit. This is
// pattern recognition only — Mora records the bit in the manifest and never
// honors it for execution (§4.4).
func isExecBit(mode int64) bool {
	return mode&0o111 != 0
}

// classifyKind assigns a FileKind by path. SKILL.md and manifests are
// structural; scripts are identified by extension (they are stored and
// delivered as untrusted resources, never executed).
func classifyKind(p string) FileKind {
	base := strings.ToLower(path.Base(p))
	switch {
	case base == "skill.md":
		return KindSkillMD
	case base == "manifest.json" || base == "manifest.mora.json" || strings.HasSuffix(base, ".mora.json"):
		return KindManifest
	case isScriptExt(base):
		return KindScript
	case strings.HasPrefix(p, "scripts/") || strings.HasPrefix(p, "assets/"):
		return KindAsset
	default:
		return KindOther
	}
}

func isScriptExt(base string) bool {
	switch {
	case strings.HasSuffix(base, ".sh"),
		strings.HasSuffix(base, ".py"),
		strings.HasSuffix(base, ".js"),
		strings.HasSuffix(base, ".ts"),
		strings.HasSuffix(base, ".go"),
		strings.HasSuffix(base, ".rb"),
		strings.HasSuffix(base, ".bash"):
		return true
	}
	return false
}
