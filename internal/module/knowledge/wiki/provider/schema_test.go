package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func validPatch() PagePatch {
	sid := uuid.New()
	svid := uuid.New()
	return PagePatch{
		PageKey:        "entity/mora",
		Action:         "create",
		ContentHash:    strings.Repeat("a", 64),
		SourceVersions: []SourceVersionRef{{SourceAssetID: sid, SourceAssetVersionID: svid, ContributionHash: strings.Repeat("b", 64)}},
		RelationSuggestions: []RelationSuggestion{{Kind: "derived_from", ToAssetID: uuid.New()}},
	}
}

func TestValidatePatch_Valid(t *testing.T) {
	if err := ValidatePatch(validPatch()); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidatePatch_RejectsBadAction(t *testing.T) {
	p := validPatch()
	p.Action = "delete"
	if err := ValidatePatch(p); err == nil {
		t.Fatal("expected error for bad action")
	}
}

func TestValidatePatch_RequiresSourceVersions(t *testing.T) {
	p := validPatch()
	p.SourceVersions = nil
	if err := ValidatePatch(p); err == nil {
		t.Fatal("expected error for empty source_versions (minItems 1)")
	}
}

func TestValidatePatch_ContentHashMustBeSha256(t *testing.T) {
	p := validPatch()
	p.ContentHash = "not-a-hash"
	if err := ValidatePatch(p); err == nil {
		t.Fatal("expected error for bad content_hash")
	}
}

func TestValidatePatch_PageKeyTooLong(t *testing.T) {
	p := validPatch()
	p.PageKey = strings.Repeat("k", 257)
	if err := ValidatePatch(p); err == nil {
		t.Fatal("expected error for page_key maxLength")
	}
}

func TestValidatePatch_BadRelationKind(t *testing.T) {
	p := validPatch()
	p.RelationSuggestions[0].Kind = "bogus"
	if err := ValidatePatch(p); err == nil {
		t.Fatal("expected error for bad relation kind")
	}
}

func TestValidatePatchJSON_Malformed(t *testing.T) {
	if _, err := ValidatePatchJSON(json.RawMessage(`[{bad}`)); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestValidatePatchJSON_Valid(t *testing.T) {
	p := validPatch()
	raw, _ := json.Marshal([]PagePatch{p})
	out, err := ValidatePatchJSON(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(out))
	}
}

func TestValidateLintFinding_Valid(t *testing.T) {
	f := LintFinding{PageKey: "entity/x", Reason: "stale"}
	if err := ValidateLintFinding(f); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateLintFinding_BadReason(t *testing.T) {
	f := LintFinding{PageKey: "entity/x", Reason: "bogus"}
	if err := ValidateLintFinding(f); err == nil {
		t.Fatal("expected error for bad reason")
	}
}

func TestValidateLintFinding_SuggestionValidated(t *testing.T) {
	bad := validPatch()
	bad.Action = "nope"
	f := LintFinding{PageKey: "entity/x", Reason: "orphan", Suggestion: &bad}
	if err := ValidateLintFinding(f); err == nil {
		t.Fatal("expected error for invalid suggestion")
	}
}
