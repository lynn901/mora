package domain

import "testing"

// TestPhase0ActionConstants verifies the Phase 0 action enum extensions exist
// with the exact wire values from design-docs/13 §4.1, and that the legacy
// read/write/admin values are unchanged.
func TestPhase0ActionConstants(t *testing.T) {
	cases := []struct {
		name string
		got  Action
		want string
	}{
		{"read", ActionRead, "read"},
		{"write", ActionWrite, "write"},
		{"admin", ActionAdmin, "admin"},
		{"use", ActionUse, "use"},
		{"assign", ActionAssign, "assign"},
		{"share", ActionShare, "share"},
		{"review", ActionReview, "review"},
		{"sync", ActionSync, "sync"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s: got %q want %q", c.name, c.got, c.want)
		}
	}
}

// TestPhase0TargetTypeConstants verifies the Phase 0 target-type extensions.
func TestPhase0TargetTypeConstants(t *testing.T) {
	cases := []struct {
		name string
		got  TargetType
		want string
	}{
		{"workspace", TargetWorkspace, "workspace"},
		{"directory", TargetDirectory, "directory"},
		{"document", TargetDocument, "document"},
		{"asset", TargetAsset, "asset"},
		{"source", TargetSource, "source"},
		{"agent", TargetAgent, "agent"},
		{"review", TargetReview, "review"},
		{"evidence", TargetEvidence, "evidence"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s: got %q want %q", c.name, c.got, c.want)
		}
	}
}

// TestPhase0SubjectTypeConstants verifies the Phase 0 subject-type extensions.
func TestPhase0SubjectTypeConstants(t *testing.T) {
	cases := []struct {
		name string
		got  SubjectType
		want string
	}{
		{"user", SubjectUser, "user"},
		{"group", SubjectGroup, "group"},
		{"agent", SubjectAgent, "agent"},
		{"service_account", SubjectServiceAccount, "service_account"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("%s: got %q want %q", c.name, c.got, c.want)
		}
	}
}
