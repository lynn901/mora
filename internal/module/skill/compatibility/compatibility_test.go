package compatibility

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/lynn901/mora/internal/domain"
)

// TestClassify_ThreeTiers verifies the §2.2 three-tier profile determination.
func TestClassify_ThreeTiers(t *testing.T) {
	tests := []struct {
		format string
		want   domain.ProfileKind
	}{
		{"agentskills.io/v1.0", domain.ProfileAgentskills},
		{"hermes/claude", domain.ProfileHermes},
		{"hermes/custom-variant", domain.ProfileHermes},
		{"opaque", domain.ProfileOpaque},
		{"some/unknown", domain.ProfileOpaque},
		{"", domain.ProfileOpaque}, // no declared format → opaque (Mora does not guess)
	}
	for _, tc := range tests {
		t.Run(tc.format, func(t *testing.T) {
			assert.Equal(t, tc.want, Classify(tc.format))
		})
	}
}

// TestDetermine_Lossless — agentskills profile → lossless delivery, opaque
// fields enumerated (unknown legal fields preserved + reported, §2.3/§4.3).
func TestDetermine_Lossless(t *testing.T) {
	fm := map[string]any{
		"name":    "echo",
		"version": "1.0",
		"custom_field": "preserved-but-not-understood", // unknown legal
	}
	rep := Determine(ReportInput{
		Profile:     domain.ProfileAgentskills,
		FormatID:    AgentskillsFormat,
		Frontmatter: fm,
	})
	assert.Equal(t, domain.DeliveryLossless, rep.Delivery)
	assert.Empty(t, rep.RuntimeNeeds)
	assert.Contains(t, rep.OpaqueFields, "custom_field",
		"unknown legal field enumerated in opaque_fields (preserved + reported)")
}

// TestDetermine_RuntimeAdaptationNeeded — hermes profile → runtime needs
// reported; the consuming agent must provide the runtime (Mora does not exec).
func TestDetermine_RuntimeAdaptationNeeded(t *testing.T) {
	rep := Determine(ReportInput{
		Profile:         domain.ProfileHermes,
		FormatID:        "hermes/claude",
		Frontmatter:     map[string]any{"name": "h", "runtime": "claude-code"},
		DeclaredRuntime: "claude-code",
	})
	assert.Equal(t, domain.DeliveryRuntimeAdaptationNeeded, rep.Delivery)
	assert.Contains(t, rep.RuntimeNeeds, "runtime:claude-code")
}

// TestDetermine_HermesNoDeclaredRuntime — a hermes package without a declared
// runtime reports the variant itself as the runtime need.
func TestDetermine_HermesNoDeclaredRuntime(t *testing.T) {
	rep := Determine(ReportInput{
		Profile:     domain.ProfileHermes,
		FormatID:    "hermes/custom",
		Frontmatter: map[string]any{"name": "h"},
	})
	assert.Equal(t, domain.DeliveryRuntimeAdaptationNeeded, rep.Delivery)
	assert.Contains(t, rep.RuntimeNeeds, "runtime:custom")
}

// TestDetermine_Incompatible — opaque profile → incompatible delivery.
func TestDetermine_Incompatible(t *testing.T) {
	rep := Determine(ReportInput{
		Profile:     domain.ProfileOpaque,
		FormatID:    OpaqueFormat,
		Frontmatter: map[string]any{"name": "h"},
	})
	assert.Equal(t, domain.DeliveryIncompatible, rep.Delivery)
	assert.Nil(t, rep.RuntimeNeeds, "opaque package has no runtime needs (not delivered)")
}
