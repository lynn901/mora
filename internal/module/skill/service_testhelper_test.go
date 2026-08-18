package skill

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"

	"github.com/stretchr/testify/require"
)

// These archive builders live in the skill (service) test package so the
// service-level §9 roundtrip test is self-contained — it does not import the
// package sub-domain's test helpers (which are in package skillpkg's test
// build). Each builder returns gzip+tar bytes.

// archiveEntry is one file in a test archive.
type archiveEntry struct {
	path    string
	content []byte
	mode    int64
}

func buildArchive(t testing.TB, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: e.path, Mode: e.mode, Size: int64(len(e.content)),
		}))
		_, _ = tw.Write(e.content)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func fileEntry(path string, content []byte) archiveEntry  { return archiveEntry{path, content, 0o644} }
func execEntry(path string, content []byte) archiveEntry  { return archiveEntry{path, content, 0o755} }

// sampleSKILLMD is a standard agentskills.io/v1.0 SKILL.md with known fields
// AND an unknown legal field ("custom_runtime_config") that Mora must
// preserve verbatim (§2.3).
const sampleSKILLMD = "---\n" +
	"name: echo-skill\n" +
	"description: echoes input\n" +
	"version: \"1.0\"\n" +
	"format: agentskills.io/v1.0\n" +
	"schema_version: \"1.0\"\n" +
	"runtime: claude-code\n" +
	"custom_runtime_config:\n" +
	"  endpoint: https://internal.invalid\n" +
	"  retries: 3\n" +
	"capabilities:\n" +
	"  tools:\n" +
	"    - echo\n" +
	"  resources:\n" +
	"    - assets/guide.md\n" +
	"---\n" +
	"# Echo Skill\n\nA skill that echoes its input.\n"

// buildSampleArchive is the standard sample package (§9 gate fixture).
func buildSampleArchive(t testing.TB) []byte {
	return buildArchive(t,
		fileEntry("SKILL.md", []byte(sampleSKILLMD)),
		fileEntry("assets/guide.md", []byte("# Guide\nUse the echo tool.\n")),
		execEntry("scripts/run.sh", []byte("#!/bin/sh\necho hi\n")),
	)
}

// buildOpaqueArchive — SKILL.md with no format field → opaque profile.
func buildOpaqueArchive(t testing.TB) []byte {
	return buildArchive(t,
		fileEntry("SKILL.md", []byte("---\nname: opaque-pkg\n---\nbody")),
		fileEntry("assets/data.bin", []byte("opaque bytes")),
	)
}

// buildNoSkillMDArchive — no SKILL.md → ErrMissingSkillMD.
func buildNoSkillMDArchive(t testing.TB) []byte {
	return buildArchive(t,
		fileEntry("assets/guide.md", []byte("# Guide\n")),
	)
}

// buildBombArchive — an entry whose tar header declares a size over the
// per-file cap (anti-compression-bomb, §4.4). The header is written via
// tar.Writer but NO content bytes are written for the oversized entry; on
// read, tar.Reader yields the header (size > cap) and Parse aborts with
// ErrArchiveTooLarge before attempting to read content. tar.Writer's
// missed-bytes check fires only on Close, which we swallow — the bytes are
// already flushed to gzip and the reader sees the header fine.
func buildBombArchive(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "SKILL.md", Mode: 0o644, Size: int64(len("---\nname: x\n---\nbody")),
	}))
	_, _ = tw.Write([]byte("---\nname: x\n---\nbody"))
	// Oversized header, no content written (tar.Writer Close will object,
	// but the header is already in the gzip stream and readable on read-back).
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "assets/huge.bin", Mode: 0o644, Size: 1 << 30,
	}))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}
