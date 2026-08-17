package handler

// memory_test.go pins the §4.4 / §4.1 error-mapping + input-resolution
// contract for the memory evidence capture handler (design-docs/18 §7.1).
// mapMemoryErr must route each evidence-service sentinel to its §11.4
// envelope so:
//   - a forbidden capture (no workspace write, §4.4) → 403/40300 (the caller
//     is authenticated and asked to write, so the denial is allowed to
//     surface);
//   - a rejected capture (secret detected, §4.1) → 400/40000;
//   - a mis-configured KEK/ObjectStore (§4.2) → 500/50000 (server fault, not
//     caller input);
// an unknown error must pass through unchanged so a real fault is not masked.
//
// The resolveOwnerType / resolveSourceKind / resolveVisibility helpers are
// pinned so a caller omitting optional fields gets the documented defaults
// (§2.2 private, agent caller → OwnerAgent).

import (
	"net/http"
	"testing"

	stderrors "errors"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
	"github.com/lynn901/mora/internal/pkg/errors"
	"github.com/stretchr/testify/assert"
)

func TestMapMemoryErr_RoutesSentinelsToCorrectEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		in         error
		wantStatus int
		wantCode   int
	}{
		{"forbidden", evidence.ErrCaptureForbidden, http.StatusForbidden, 40300},
		{"rejected (secret)", stderrors.Join(evidence.ErrCaptureRejected, evidence.ErrSecretDetected), http.StatusBadRequest, 40000},
		{"crypto not configured", evidence.ErrCryptoNotConfigured, http.StatusInternalServerError, 50000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mapMemoryErr(tc.in)
			perr, ok := got.(*errors.Error)
			if !ok {
				t.Fatalf("expected *errors.Error, got %T (%v)", got, got)
			}
			assert.Equal(t, tc.wantStatus, perr.Status, "status")
			assert.Equal(t, tc.wantCode, int(perr.Code), "code")
		})
	}
}

func TestMapMemoryErr_UnknownPassesThrough(t *testing.T) {
	orig := stderrors.New("boom")
	got := mapMemoryErr(orig)
	assert.Equal(t, orig, got, "unknown error must pass through unchanged")
}

func TestMapMemoryErr_NilIsNil(t *testing.T) {
	assert.Nil(t, mapMemoryErr(nil))
}

// TestResolveOwnerType pins the owner_type defaulting from the authenticated
// caller (§4.4 actor). An agent caller with an empty body field resolves to
// OwnerAgent; an explicit "service_account" wins over the caller shape.
func TestResolveOwnerType(t *testing.T) {
	cases := []struct {
		body string
		st   AuthState
		want domain.OwnerType
	}{
		{"", AuthState{SubjectType: domain.SubjectAgent}, domain.OwnerAgent},
		{"", AuthState{SubjectType: domain.SubjectUser}, domain.OwnerUser},
		{"", AuthState{SubjectType: domain.SubjectServiceAccount}, domain.OwnerServiceAccount},
		{"", AuthState{}, domain.OwnerUser}, // default subject → user
		{"agent", AuthState{SubjectType: domain.SubjectUser}, domain.OwnerAgent},
		{"service_account", AuthState{SubjectType: domain.SubjectUser}, domain.OwnerServiceAccount},
		{"user", AuthState{SubjectType: domain.SubjectAgent}, domain.OwnerUser},
		{"group", AuthState{}, domain.OwnerGroup},
		{"unknown", AuthState{SubjectType: domain.SubjectAgent}, domain.OwnerUser}, // unknown → default user
	}
	for _, tc := range cases {
		t.Run(tc.body+"/"+string(tc.st.SubjectType), func(t *testing.T) {
			assert.Equal(t, tc.want, resolveOwnerType(tc.body, tc.st))
		})
	}
}

func TestResolveSourceKind(t *testing.T) {
	cases := []struct {
		in   string
		want domain.EvidenceSourceKind
	}{
		{"session", domain.EvidenceSourceSession},
		{"message", domain.EvidenceSourceMessage},
		{"tool_call", domain.EvidenceSourceToolCall},
		{"document", domain.EvidenceSourceDocument},
		{"code", domain.EvidenceSourceCode},
		{"TOOL_CALL", domain.EvidenceSourceToolCall}, // case-insensitive
		{"", domain.EvidenceSourceSession},           // default session
		{"bogus", domain.EvidenceSourceSession},      // unknown → session
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveSourceKind(tc.in))
		})
	}
}

func TestResolveVisibility(t *testing.T) {
	assert.Equal(t, domain.EvidencePrivate, resolveVisibility(""))
	assert.Equal(t, domain.EvidencePrivate, resolveVisibility("private"))
	assert.Equal(t, domain.EvidenceRestricted, resolveVisibility("restricted"))
	assert.Equal(t, domain.EvidenceRestricted, resolveVisibility("RESTRICTED"))
	assert.Equal(t, domain.EvidencePrivate, resolveVisibility("bogus")) // default private (§2.2)
}
