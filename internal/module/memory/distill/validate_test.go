package distill

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
)

func mustTime(t *testing.T, s string) *time.Time {
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return &tt
}

func TestValidateCandidates_OK(t *testing.T) {
	t.Parallel()
	eid := uuid.New()
	cands := []MemoryCandidate{{
		MemoryType: domain.MemoryDecision,
		Statement:  "mora-api 默认监听 :8990",
		Scope:      "deploy",
		Validity: Validity{
			ValidFrom: mustTime(t, "2026-08-01T00:00:00Z"),
			ExpiresAt: mustTime(t, "2026-12-31T00:00:00Z"),
		},
		Confidence:      0.8,
		EntityKeys:       map[string]any{"service": "mora-api"},
		EvidenceLocator:  EvidenceLocator{EvidenceID: eid},
	}}
	if err := ValidateCandidates(cands, eid); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
	if err := ValidateCandidates(nil, eid); err != nil {
		t.Fatalf("nil slice should be valid, got %v", err)
	}
	if err := ValidateCandidates([]MemoryCandidate{}, eid); err != nil {
		t.Fatalf("empty slice should be valid, got %v", err)
	}
}

func TestValidateCandidates_FailClosed(t *testing.T) {
	t.Parallel()
	eid := uuid.New()
	cases := map[string]MemoryCandidate{
		"bad memory_type":      {MemoryType: "rumor", Statement: "x", EvidenceLocator: EvidenceLocator{EvidenceID: eid}},
		"empty statement":      {MemoryType: domain.MemoryFact, Statement: "   ", EvidenceLocator: EvidenceLocator{EvidenceID: eid}},
		"confidence out of range": {MemoryType: domain.MemoryFact, Statement: "x", Confidence: 1.5, EvidenceLocator: EvidenceLocator{EvidenceID: eid}},
		"negative confidence":  {MemoryType: domain.MemoryFact, Statement: "x", Confidence: -0.1, EvidenceLocator: EvidenceLocator{EvidenceID: eid}},
		"expires before valid": {
			MemoryType: domain.MemoryFact, Statement: "x",
			Validity: Validity{
				ValidFrom: mustTime(t, "2026-12-31T00:00:00Z"),
				ExpiresAt: mustTime(t, "2026-01-01T00:00:00Z"),
			},
			EvidenceLocator: EvidenceLocator{EvidenceID: eid},
		},
		"missing evidence id":  {MemoryType: domain.MemoryFact, Statement: "x"},
		"cross-evidence smuggle": {
			MemoryType: domain.MemoryFact, Statement: "x",
			EvidenceLocator: EvidenceLocator{EvidenceID: uuid.New()},
		},
		"statement too long": {
			MemoryType: domain.MemoryFact,
			Statement:  string(make([]byte, maxStatementLen+1)),
			EvidenceLocator: EvidenceLocator{EvidenceID: eid},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidateCandidates([]MemoryCandidate{c}, eid)
			if err == nil {
				t.Fatalf("expected ErrCandidateInvalid, got nil")
			}
			if !errors.Is(err, ErrCandidateInvalid) {
				t.Fatalf("expected ErrCandidateInvalid, got %v", err)
			}
		})
	}
}

func TestValidateCandidates_StatementBoundEnforced(t *testing.T) {
	t.Parallel()
	eid := uuid.New()
	c := MemoryCandidate{
		MemoryType:      domain.MemoryFact,
		Statement:       string(make([]byte, maxStatementLen)), // exactly at bound = ok
		EvidenceLocator: EvidenceLocator{EvidenceID: eid},
	}
	if err := ValidateCandidates([]MemoryCandidate{c}, eid); err != nil {
		t.Fatalf("statement at bound should be valid, got %v", err)
	}
}
