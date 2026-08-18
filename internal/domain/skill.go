package domain

import "time"

// Phase 5 Skill package value objects (design-docs/12 §4.5, migration 022
// skill_packages; design-docs/19 §2/§3.1/§4.3 — the authoritative field
// contract for skill_packages). These are the in-memory data shapes the
// module/skill service and the infra/postgres adapter read/write. No business
// logic here — just the data shapes (same precedent as AgentBinding in this
// file and AssetVersion above).
//
// Mora 不执行 Skill: validation_status=passed means "saveable / deliverable",
// NOT "executable" (§4). Scripts and binaries may be stored and delivered as
// untrusted resources, but Mora's parse/preview/index/validate paths never
// execute them (script-execution count = 0 across all four paths, §4.4/§7).

// --- format_id / profile (§2.2) ---

// SkillFormatID is the stored format_id of a skill package. It is the
// concrete profile string written to skill_packages.format_id. The Profile
// classifier (module/skill/compatibility) maps a format_id to one of three
// ProfileKind tiers.
//
// Three concrete families (§2.2):
//   - SkillFormatAgentskills — "agentskills.io/v1.0": fully understood by Mora,
//     lossless delivery.
//   - SkillFormatHermesPrefix — "hermes/": extension profile. Unknown legal
//     frontmatter is preserved verbatim in original_frontmatter; only runtime
//     needs are reported.
//   - SkillFormatOpaque — "opaque": archive only, no capability discovery.
type SkillFormatID string

const (
	SkillFormatAgentskills SkillFormatID = "agentskills.io/v1.0"
	SkillFormatOpaque      SkillFormatID = "opaque"
	// SkillFormatHermesPrefix is a prefix, not a complete format_id: a hermes
	// package's format_id is "hermes/<variant>" (e.g. "hermes/claude"). The
	// classifier accepts any format_id with this prefix as the hermes tier.
	SkillFormatHermesPrefix SkillFormatID = "hermes/"
)

// ProfileKind is the three-tier compatibility classification (§2.2). It is the
// verdict of the profile determinator, distinct from the stored format_id:
// the format_id is the package's declared family, the ProfileKind is Mora's
// understanding of it.
type ProfileKind string

const (
	ProfileAgentskills ProfileKind = "agentskills" // full understanding, lossless
	ProfileHermes      ProfileKind = "hermes"      // extension fields preserved, runtime needs reported
	ProfileOpaque      ProfileKind = "opaque"       // archive only, no discovery
)

// --- validation_status / severity (§4.3, migration 022 CHECK) ---

// SkillValidationStatus mirrors the skill_packages.validation_status CHECK
// constraint. pending on import; the validator sets passed/failed/opaque.
//
// passed ONLY means the package is structurally saveable/deliverable — it is
// NOT an executability assertion (Mora does not execute Skills).
type SkillValidationStatus string

const (
	SkillValidationPending SkillValidationStatus = "pending"
	SkillValidationPassed   SkillValidationStatus = "passed"
	SkillValidationFailed   SkillValidationStatus = "failed" // a severity=block finding
	SkillValidationOpaque   SkillValidationStatus = "opaque" // opaque profile
)

// ValidationSeverity is the severity of a ValidationFinding (§4.3 findings).
// A single severity=block finding forces validation_status=failed.
type ValidationSeverity string

const (
	SeverityBlock ValidationSeverity = "block" // forces failed
	SeverityWarn  ValidationSeverity = "warn"
	SeverityInfo   ValidationSeverity = "info"
)

// --- compatibility_report.delivery (§4.3) ---

// DeliveryVerdict is skill_packages.compatibility_report.delivery.
type DeliveryVerdict string

const (
	DeliveryLossless                DeliveryVerdict = "lossless"                  // agentskills.io, fully understood
	DeliveryRuntimeAdaptationNeeded DeliveryVerdict = "runtime_adaptation_needed" // hermes: preserved, runtime must adapt
	DeliveryIncompatible            DeliveryVerdict = "incompatible"             // cannot be delivered
)

// --- manifest / report structures (§3.1 / §4.3) ---

// SkillFileEntry is one file in manifest.mora.json's file list (§2.1). The
// manifest is the normalized, ordered file inventory; per-file hashes anchor
// the roundtrip identity independently of archive packaging (tar headers,
// timestamps) so content_hash is reproducible on export.
type SkillFileEntry struct {
	Path    string `json:"path"`         // archive-relative, normalized (no leading /)
	Size    int64  `json:"size"`          // decompressed size in bytes
	Hash    string `json:"hash"`          // sha256 of decompressed content (hex)
	ExecBit bool   `json:"exec_bit"`     // executable bit detected — untrusted, NEVER exec'd
	Kind    string `json:"kind"`          // skill_md | script | asset | manifest | other
}

// SkillManifest is the normalized manifest (skill_packages.manifest, §3.1 /
// §2.1). It is a derived projection over the archive: the file inventory, a
// summary of the declared capabilities, and the roundtrip content_hash. The
// raw, unparsed original frontmatter lives separately in OriginalFrontmatter
// to guarantee lossless roundtrip of unknown legal fields (§2.3).
type SkillManifest struct {
	Files             []SkillFileEntry   `json:"files"`
	CapabilitySummary map[string]any     `json:"capability_summary,omitempty"` // declared tools/skills/resources summary
	ContentHash       string             `json:"content_hash"`                 // roundtrip anchor (§3.1)
	EntryCount        int                `json:"entry_count"`
	TotalSize         int64              `json:"total_size"`
}

// ValidationFinding is one static-check result (§4.3 validation_report.findings).
type ValidationFinding struct {
	Check    string             `json:"check"`     // check id, e.g. "structure.skill_md"
	Severity ValidationSeverity `json:"severity"`
	Code     string             `json:"code"`      // stable machine code, e.g. "SKILL_MD_MISSING"
	Message  string             `json:"message"`
	Path     string             `json:"path,omitempty"` // archive path, if relevant
}

// ValidationReport is skill_packages.validation_report (§4.3): findings +
// per-file hashes + a signature echo (no secret values — Mora only records
// signature presence/shape, it does not verify against a key store).
type ValidationReport struct {
	Findings  []ValidationFinding `json:"findings"`
	Hashes    map[string]string   `json:"hashes"`    // path → sha256 (duplicates manifest hashes for tamper detection)
	Signature map[string]any      `json:"signature,omitempty"`
}

// CompatibilityReport is skill_packages.compatibility_report (§4.3): the
// delivery verdict + the runtime needs Mora cannot satisfy + the frontmatter
// fields Mora did not understand (preserved verbatim, not dropped).
type CompatibilityReport struct {
	Delivery     DeliveryVerdict `json:"delivery"`
	RuntimeNeeds []string        `json:"runtime_needs,omitempty"` // e.g. "runtime:claude-code"
	OpaqueFields []string        `json:"opaque_fields,omitempty"` // unknown frontmatter field paths
}

// SkillPackage is the skill_packages row value object (§4.5 / migration 022).
// It mounts 1:1 on a knowledge_asset_version (asset_version_id is both PK and
// FK → knowledge_asset_versions(id) ON DELETE CASCADE). The MinIO immutable
// archive original is referenced by StorageKey; Mora never materializes an
// executable bit from it onto disk (§4.4/§7).
type SkillPackage struct {
	AssetVersionID      UUID
	StorageKey          string             // MinIO immutable archive original locator (non-executable selector)
	FormatID            SkillFormatID      // agentskills.io/v1.0 | hermes/* | opaque
	SchemaVersion       string             // spec version snapshot (e.g. "1.0")
	Manifest            SkillManifest      // normalized manifest
	OriginalFrontmatter map[string]any     // unknown legal fields preserved verbatim (§2.3, roundtrip anchor)
	ContentHash         string             // roundtrip consistency anchor (§3.1)
	Signature           map[string]any     // signature info (no secret values)
	ProvenanceRef       map[string]any     // provenance reference (no plaintext credentials)
	ValidationStatus    SkillValidationStatus
	ValidationReport    ValidationReport
	CompatibilityReport CompatibilityReport
	ScannerVersion       string             // scanner version (result reproducibility / reconciliation)
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// HasBlockFinding reports whether the validation report contains a
// severity=block finding — the rule that forces validation_status=failed
// (§4.3). Kept as a value method so the service and tests share one definition.
func (r ValidationReport) HasBlockFinding() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityBlock {
			return true
		}
	}
	return false
}
