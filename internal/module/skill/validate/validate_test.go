package validate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/lynn901/mora/internal/domain"
)

// shaHex is a test helper to compute a file's sha256 (matches the validator's
// recomputation) so a manifest hash can be seeded truthfully.
func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// goodManifest builds a structurally-sound manifest for a tiny agentskills
// package with a declared capability + resource that exists in the archive.
func goodManifest() (domain.SkillManifest, map[string][]byte) {
	body := []byte("# Guide")
	guideHash := shaHex(body)
	files := map[string][]byte{
		"SKILL.md":      []byte("---\nname: x\n---\nbody"),
		"assets/guide.md": body,
	}
	m := domain.SkillManifest{
		Files: []domain.SkillFileEntry{
			{Path: "SKILL.md", Size: int64(len(files["SKILL.md"])), Hash: shaHex(files["SKILL.md"]), Kind: "skill_md"},
			{Path: "assets/guide.md", Size: int64(len(body)), Hash: guideHash, Kind: "asset"},
		},
		CapabilitySummary: map[string]any{
			"resources": []any{"assets/guide.md"},
		},
		EntryCount: 2,
		ContentHash: "deadbeef",
	}
	return m, files
}

type memHashProvider struct{ files map[string][]byte }

func (m memHashProvider) Content(p string) ([]byte, error) {
	b, ok := m.files[p]
	if !ok {
		return nil, errors.New("not found")
	}
	return b, nil
}

// TestRun_Passed — clean agentskills package → passed (saveable, NOT exec).
func TestRun_Passed(t *testing.T) {
	m, files := goodManifest()
	report, status := Run(Input{
		Manifest:       m,
		Profile:        domain.ProfileAgentskills,
		ContentProvider: memHashProvider{files},
	})
	assert.Equal(t, domain.SkillValidationPassed, status)
	assert.False(t, report.HasBlockFinding(), "clean package has no block findings")
}

// TestRun_BlockFinding_ForcesFailed — a block finding rolls up to failed.
func TestRun_BlockFinding_ForcesFailed(t *testing.T) {
	m, files := goodManifest()
	m.ContentHash = "" // manifest incomplete → block finding
	report, status := Run(Input{
		Manifest:       m,
		Profile:        domain.ProfileAgentskills,
		ContentProvider: memHashProvider{files},
	})
	assert.Equal(t, domain.SkillValidationFailed, status)
	assert.True(t, report.HasBlockFinding())
}

// TestRun_OpaqueProfile_ForcesOpaque — an opaque profile forces status=opaque
// even when structure passes (archived-only, not "passed").
func TestRun_OpaqueProfile_ForcesOpaque(t *testing.T) {
	m, files := goodManifest()
	_, status := Run(Input{
		Manifest:       m,
		Profile:        domain.ProfileOpaque,
		ContentProvider: memHashProvider{files},
	})
	assert.Equal(t, domain.SkillValidationOpaque, status)
}

// TestRun_HashMismatch_Block — a tampered file content recomputed hash !=
// manifest hash → block finding → failed.
func TestRun_HashMismatch_Block(t *testing.T) {
	m, files := goodManifest()
	// Tamper: provider returns different bytes than the manifest hash claims.
	tampered := map[string][]byte{
		"SKILL.md":        files["SKILL.md"],
		"assets/guide.md": []byte("# Tampered content"),
	}
	report, status := Run(Input{
		Manifest:       m,
		Profile:        domain.ProfileAgentskills,
		ContentProvider: memHashProvider{tampered},
	})
	assert.Equal(t, domain.SkillValidationFailed, status)
	var hashFinding *domain.ValidationFinding
	for i := range report.Findings {
		if report.Findings[i].Code == CodeHashMismatch {
			hashFinding = &report.Findings[i]
		}
	}
	require.NotNil(t, hashFinding, "expected a hash mismatch finding")
	assert.Equal(t, domain.SeverityBlock, hashFinding.Severity)
}

// TestRun_MissingResource_Block — a frontmatter-declared resource not present
// in the archive → block finding.
func TestRun_MissingResource_Block(t *testing.T) {
	m, files := goodManifest()
	// Declare a resource that does not exist in the archive.
	m.CapabilitySummary = map[string]any{
		"resources": []any{"assets/ghost.md"},
	}
	report, status := Run(Input{
		Manifest:       m,
		Profile:        domain.ProfileAgentskills,
		ContentProvider: memHashProvider{files},
	})
	assert.Equal(t, domain.SkillValidationFailed, status)
	assert.True(t, report.HasBlockFinding())
}

// TestRun_ExecutableBit_Info — an executable bit is recorded as info, never
// block (the bit is metadata; Mora never honors it for execution).
func TestRun_ExecutableBit_Info(t *testing.T) {
	m, files := goodManifest()
	m.Files[1].ExecBit = true // guide has exec bit
	report, status := Run(Input{
		Manifest:       m,
		Profile:        domain.ProfileAgentskills,
		ContentProvider: memHashProvider{files},
	})
	// info findings do not force failed.
	assert.Equal(t, domain.SkillValidationPassed, status)
	var execFound bool
	for _, f := range report.Findings {
		if f.Code == CodeExecutableBit {
			assert.Equal(t, domain.SeverityInfo, f.Severity)
			execFound = true
		}
	}
	assert.True(t, execFound, "executable bit recorded as info finding")
}

// TestRun_FrontmatterMalformed_Block — malformed frontmatter → block.
func TestRun_FrontmatterMalformed_Block(t *testing.T) {
	m, files := goodManifest()
	_, status := Run(Input{
		Manifest:          m,
		Profile:           domain.ProfileAgentskills,
		ContentProvider:   memHashProvider{files},
		FrontmatterParseErr: errors.New("bad yaml: tab character"),
	})
	assert.Equal(t, domain.SkillValidationFailed, status)
}

// TestRun_NoCapabilities_Warn — missing capabilities is a warn (not block);
// the package is saveable but delivery is a bare manifest.
func TestRun_NoCapabilities_Warn(t *testing.T) {
	m, files := goodManifest()
	m.CapabilitySummary = map[string]any{} // empty
	report, status := Run(Input{
		Manifest:       m,
		Profile:        domain.ProfileAgentskills,
		ContentProvider: memHashProvider{files},
	})
	assert.Equal(t, domain.SkillValidationPassed, status, "warn does not force failed")
	var capsFound bool
	for _, f := range report.Findings {
		if f.Code == CodeCapabilitiesMissing {
			assert.Equal(t, domain.SeverityWarn, f.Severity)
			capsFound = true
		}
	}
	assert.True(t, capsFound, "missing capabilities recorded as warn")
}

// TestRun_NoProvider_SkipsHashes — without a content provider the hash
// recompute is skipped (info), not failed.
func TestRun_NoProvider_SkipsHashes(t *testing.T) {
	m, _ := goodManifest()
	report, status := Run(Input{
		Manifest:       m,
		Profile:        domain.ProfileAgentskills,
		ContentProvider: nil,
	})
	assert.Equal(t, domain.SkillValidationPassed, status)
	var skipFound bool
	for _, f := range report.Findings {
		if f.Code == CodeHashMismatch {
			skipFound = true
		}
	}
	assert.True(t, skipFound, "hash recompute skipped (info finding)")
}
