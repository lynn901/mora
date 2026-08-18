// Package skillpkg implements the Skill package import & parse domain
// (design-docs/19 §2.1 / §7 — Phase 5-2, YS-161). It is the first of three
// sub-domains under internal/module/skill (package / compatibility /
// validate). The directory is named "package" to match the issue's three-
// sub-domain split; the Go package name is skillpkg because "package" is a
// reserved word.
//
// What this package owns:
//   - Streaming archive parse (tar/gzip) with a decompressed-size cap that
//     defeats compression bombs (§4.4 / §7): the archive is consumed as a
//     stream and never materialized with an executable bit on disk. Files are
//     read into memory only up to the per-file and total caps; entries that
//     exceed the cap abort the parse with ErrArchiveTooLarge.
//   - manifest.mora.json generation (§2.1): a normalized, ordered file
//     inventory with per-file sha256 hashes and a derived content_hash that
//     is reproducible on export (independent of tar headers / timestamps).
//   - Lossless preservation of unknown legal frontmatter (§2.3): the raw,
//     unparsed frontmatter map is returned verbatim so the service can write
//     it to skill_packages.original_frontmatter without dropping fields Mora
//     does not semantically understand.
//
// What this package does NOT do (hard invariants, §4.4 / §7):
//   - It never executes anything. script / ELF / shebang detection is
//     pattern-recognition only — exec_bit, ELF magic, shebang are recorded
//     as manifest metadata and never invoke exec(2). Script-execution count
//     across import/preview/index/validate is 0 (§9 门禁).
//   - It never writes an executable bit to disk. The archive is consumed
//     in-memory; the only persisted artifact is the MinIO immutable original
//     referenced by storage_key (owned by the REST/infra layer, sub-task D).
//
// Layering (modular monolith): the archive bytes come from an ArchiveReader
// port (the MinIO/original-object locator abstraction). The concrete MinIO
// adapter is out of scope here (sub-task D wires it); tests supply an
// in-memory reader. This package stays storage-free and pgx-free.
package skillpkg

// MaxTotalDecompressedSize is the ceiling on the sum of decompressed file
// sizes across the whole archive (§4.4 anti-compression-bomb). A zip/q2-bomb
// that decompresses past this aborts the parse before exhausting memory.
const MaxTotalDecompressedSize int64 = 256 * 1024 * 1024 // 256 MiB

// MaxPerFileSize is the ceiling on a single decompressed entry. An entry
// whose declared size exceeds this aborts the parse (do not trust a 4 GiB
// SKILL.md).
const MaxPerFileSize int64 = 64 * 1024 * 1024 // 64 MiB

// MaxFileCount caps the number of entries in one archive. A pathologically
// deep or wide archive must not exhaust memory building the manifest.
const MaxFileCount = 10_000
