// Package compatibility implements the Skill package profile determination
// and compatibility report domain (design-docs/19 §2.2 / §4.3 — Phase 5-2,
// YS-161). It is the second of three sub-domains under internal/module/skill.
//
// What this package owns:
//   - Three-tier profile classification (§2.2): a package's declared
//     format_id is mapped to one of ProfileAgentskills / ProfileHermes /
//     ProfileOpaque. The verdict is Mora's understanding of the package,
//     distinct from the stored format_id string.
//   - compatibility_report (§4.3): the delivery verdict (lossless /
//     runtime_adaptation_needed / incompatible) + the runtime needs Mora
//     cannot satisfy + the opaque frontmatter fields Mora preserves but does
//     not semantically understand.
//
// The compatibility report is the runtime's view of a package: even a
// fully-structured package can be runtime_adaptation_needed (hermes profile:
// preserved but Mora cannot honor the runtime) or incompatible (declares a
// runtime Mora refuses to satisfy). It is distinct from the validation
// report (structural soundness) — a package can be structurally valid but
// runtime-incompatible.
package compatibility

import (
	"strings"

	"github.com/lynn901/mora/internal/domain"
)

// SpecBaseline is the pinned agentskills.io spec version Mora understands
// (ADR-0002, third-party/lock.json: name "Agent Skills spec", version "1.0",
// digest "spec-v1.0"). A package whose format_id matches
// "agentskills.io/v1.0" is the lossless tier.
const (
	SpecBaseline      = "v1.0"
	AgentskillsFormat = "agentskills.io/v1.0"
	HermesPrefix      = "hermes/"
	OpaqueFormat      = "opaque"
)

// Classify maps a declared format_id to a ProfileKind (§2.2). The mapping:
//   - "agentskills.io/v1.0" (exactly the pinned spec) → ProfileAgentskills.
//   - "hermes/<variant>" → ProfileHermes. The variant is opaque to Mora;
//     unknown legal frontmatter is preserved verbatim, runtime needs reported.
//   - "opaque" or any unrecognized format → ProfileOpaque. The package is
//     archived only; no capability discovery, no delivery.
//
// An empty format_id (package without a declared format) is treated as
// opaque: Mora will not guess a profile it cannot attest.
func Classify(formatID string) domain.ProfileKind {
	switch {
	case formatID == AgentskillsFormat:
		return domain.ProfileAgentskills
	case strings.HasPrefix(formatID, HermesPrefix):
		return domain.ProfileHermes
	default:
		return domain.ProfileOpaque
	}
}

// ReportInput is what the determinator needs to produce a compatibility
// report: the classified profile, the preserved frontmatter (so opaque
// fields can be enumerated), and the declared runtime (if any) so the
// runtime_needs list can be populated.
type ReportInput struct {
	Profile        domain.ProfileKind
	FormatID       string
	Frontmatter    map[string]any // the lossless preserved frontmatter
	DeclaredRuntime string         // e.g. "claude-code", if declared
}

// knownFrontmatterKeys mirrors package/knownFrontmatterKeys. Kept local so
// the compatibility package has no cross-sub-domain dependency (the three
// sub-domains are intentionally independent; the service composes them).
var knownFrontmatterKeys = map[string]bool{
	"name": true, "description": true, "version": true, "format": true,
	"schema_version": true, "license": true, "author": true, "capabilities": true,
}

// Determine produces the compatibility_report (§4.3) for a classified package.
//
//   - ProfileAgentskills → delivery=lossless. Mora fully understands the
//     package; opaque_fields is still populated (unknown-but-legal fields are
//     preserved and reported, never silently dropped — §2.3).
//   - ProfileHermes → delivery=runtime_adaptation_needed. Unknown fields are
//     preserved; runtime_needs lists the runtime the consuming agent must
//     provide (Mora only stores and delivers, it does not execute).
//   - ProfileOpaque → delivery=incompatible. Mora cannot discover or deliver
//     capabilities; the package is archived only.
//
// opaque_fields enumerates every frontmatter key outside the known set, so
// the report is honest about what Mora did not understand.
func Determine(in ReportInput) domain.CompatibilityReport {
	opaque := opaqueFields(in.Frontmatter)
	switch in.Profile {
	case domain.ProfileAgentskills:
		return domain.CompatibilityReport{
			Delivery:     domain.DeliveryLossless,
			OpaqueFields: opaque,
		}
	case domain.ProfileHermes:
		needs := []string{}
		if in.DeclaredRuntime != "" {
			needs = append(needs, "runtime:"+in.DeclaredRuntime)
		} else {
			// A hermes package with no declared runtime still needs *some*
			// runtime to honor its variant — report the variant itself.
			needs = append(needs, "runtime:"+strings.TrimPrefix(in.FormatID, HermesPrefix))
		}
		return domain.CompatibilityReport{
			Delivery:     domain.DeliveryRuntimeAdaptationNeeded,
			RuntimeNeeds: needs,
			OpaqueFields: opaque,
		}
	default: // ProfileOpaque
		return domain.CompatibilityReport{
			Delivery:     domain.DeliveryIncompatible,
			RuntimeNeeds: nil,
			OpaqueFields: opaque,
		}
	}
}

// opaqueFields enumerates frontmatter keys Mora does not semantically
// understand (the values are still preserved verbatim in original_frontmatter;
// this list is the honesty surface of the compatibility report, §4.3).
// Sorted for deterministic output.
func opaqueFields(fm map[string]any) []string {
	if len(fm) == 0 {
		return nil
	}
	out := make([]string, 0, len(fm))
	for k := range fm {
		if !knownFrontmatterKeys[strings.ToLower(k)] {
			out = append(out, k)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
