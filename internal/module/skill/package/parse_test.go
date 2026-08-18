package skillpkg

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skillMDWithUnknownFields builds a SKILL.md with YAML frontmatter that
// carries known fields (name/description/version/format) AND an unknown legal
// field ("custom_runtime_config") that Mora must preserve verbatim (§2.3).
func skillMDWithUnknownFields() []byte {
	return []byte("---\n" +
		"name: echo-skill\n" +
		"description: echoes input\n" +
		"version: \"1.0\"\n" +
		"format: agentskills.io/v1.0\n" +
		"schema_version: \"1.0\"\n" +
		"custom_runtime_config:\n" +
		"  endpoint: https://internal.invalid\n" +
		"  retries: 3\n" +
		"runtime: claude-code\n" +
		"capabilities:\n" +
		"  tools:\n" +
		"    - echo\n" +
		"  resources:\n" +
		"    - assets/guide.md\n" +
		"---\n" +
		"# Echo Skill\n\nA skill that echoes its input.\n")
}

// TestParse_Roundtrip_Lossless is the §9 往返门禁 for the package sub-domain:
// import→export content_hash consistent, file list consistent, unknown
// frontmatter fields preserved verbatim.
func TestParse_Roundtrip_Lossless(t *testing.T) {
	arch := (&archiveBuilder{}).
		File("SKILL.md", skillMDWithUnknownFields()).
		File("assets/guide.md", []byte("# Guide\nUse the echo tool.\n")).
		File("scripts/run.sh", []byte("#!/bin/sh\necho hi\n")). // script, never exec'd
		Bytes()

	res, err := Parse("test/echo.tar.gz", memReader{arch})
	require.NoError(t, err)

	// Unknown legal field preserved verbatim (§2.3).
	fm := res.Package.OriginalFrontmatter
	require.Contains(t, fm, "custom_runtime_config", "unknown legal field must be preserved verbatim")
	custom, ok := fm["custom_runtime_config"].(map[string]any)
	require.True(t, ok, "unknown field's nested map preserved")
	assert.Equal(t, "https://internal.invalid", custom["endpoint"])
	// goccy/go-yaml decodes integers as uint64; compare the numeric value
	// rather than the concrete type so the lossless-preservation assertion
	// does not bind to a YAML-decoder-specific integer kind.
	require.Equal(t, uint64(3), toUint64(custom["retries"]))

	// Declared format / runtime / capabilities extracted.
	assert.Equal(t, "agentskills.io/v1.0", res.Package.DeclaredFormatID)
	rt, _ := fm["runtime"].(string)
	assert.Equal(t, "claude-code", rt)

	// File list consistent: SKILL.md + guide + script.
	assert.Len(t, res.Manifest.Files, 3)
	paths := map[string]bool{}
	for _, f := range res.Manifest.Files {
		paths[f.Path] = true
	}
	assert.True(t, paths["SKILL.md"])
	assert.True(t, paths["assets/guide.md"])
	assert.True(t, paths["scripts/run.sh"])

	// content_hash is the roundtrip anchor — export must reproduce it.
	exportFiles := make([]ExportFile, 0, len(res.Package.Files))
	for _, f := range res.Package.Files {
		if f.Hash != "" { // skip symlink entries
			exportFiles = append(exportFiles, ExportFile{Path: f.Path, Content: f.Content, Kind: string(f.Kind)})
		}
	}
	exp, err := Export(exportFiles, res.ContentHash)
	require.NoError(t, err)
	assert.Equal(t, res.ContentHash, exp.ContentHash, "export content_hash must equal import content_hash (§9 gate)")

	// Re-parse the exported archive → content_hash stable across re-packaging.
	res2, err := Parse("exported", memReader{exp.Archive})
	require.NoError(t, err)
	assert.Equal(t, res.ContentHash, res2.ContentHash, "content_hash stable across re-packaging")
	assert.Equal(t, res.Manifest.EntryCount, res2.Manifest.EntryCount)
}

// toUint64 coerces a YAML-decoded numeric scalar to uint64 so the
// lossless-preservation test does not bind to a decoder-specific integer kind
// (goccy decodes to uint64; encoding/json would decode to float64).
func toUint64(v any) uint64 {
	switch n := v.(type) {
	case uint64:
		return n
	case int:
		return uint64(n)
	case int64:
		return uint64(n)
	case float64:
		return uint64(n)
	}
	return 0
}

// TestParse_MissingSkillMD — no SKILL.md is a structure failure (§4.2 #1).
func TestParse_MissingSkillMD(t *testing.T) {
	arch := (&archiveBuilder{}).
		File("assets/guide.md", []byte("# Guide\n")).
		Bytes()
	_, err := Parse("test/no-skill.tar.gz", memReader{arch})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrMissingSkillMD), "want ErrMissingSkillMD, got %v", err)
}

// TestParse_PathTraversal — an entry escaping the archive root is rejected
// before any byte is read (§4.4).
func TestParse_PathTraversal(t *testing.T) {
	arch := (&archiveBuilder{}).
		File("SKILL.md", []byte("---\nname: x\n---\nbody")).
		File("../escape.md", []byte("pwn")).
		Bytes()
	_, err := Parse("test/traversal.tar.gz", memReader{arch})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrArchivePathTraversal), "want ErrArchivePathTraversal, got %v", err)
}

// TestParse_ExecutableBitDetected — an executable bit is recorded as manifest
// metadata and NEVER honored for execution (§4.4).
func TestParse_ExecutableBitDetected(t *testing.T) {
	arch := (&archiveBuilder{}).
		File("SKILL.md", []byte("---\nname: x\n---\nbody")).
		ExecFile("scripts/run.sh", []byte("#!/bin/sh\necho hi\n")).
		Bytes()
	res, err := Parse("test/exec.tar.gz", memReader{arch})
	require.NoError(t, err)
	var execEntry *struct {
		path    string
		execBit bool
	}
	for _, f := range res.Package.Files {
		if f.Path == "scripts/run.sh" {
			execEntry = &struct {
				path    string
				execBit bool
			}{f.Path, f.ExecBit}
		}
	}
	require.NotNil(t, execEntry, "script entry present")
	assert.True(t, execEntry.execBit, "executable bit detected (recorded, not honored)")
}

// TestParse_CompressionBomb — an entry declaring a size over the per-file cap
// aborts the parse (§4.4 anti-compression-bomb).
func TestParse_CompressionBomb(t *testing.T) {
	arch := bombArchive(t, MaxPerFileSize+1)
	_, err := Parse("test/bomb.tar.gz", memReader{arch})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrArchiveTooLarge), "want ErrArchiveTooLarge, got %v", err)
}

// TestParse_SymlinkNotFollowed — a symlink entry is recorded without content;
// its target is never read (§4.4 never materialize an escape path).
func TestParse_SymlinkNotFollowed(t *testing.T) {
	arch := (&archiveBuilder{}).
		File("SKILL.md", []byte("---\nname: x\n---\nbody")).
		Symlink("assets/link", "/etc/passwd").
		Bytes()
	res, err := Parse("test/symlink.tar.gz", memReader{arch})
	require.NoError(t, err)
	var linkEntry ParsedFile
	for _, f := range res.Package.Files {
		if f.Path == "assets/link" {
			linkEntry = f
		}
	}
	assert.Equal(t, "", linkEntry.Hash, "symlink recorded with no content hash (target not followed)")
}
