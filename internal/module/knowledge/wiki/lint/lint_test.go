package lint

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeRepo struct {
	pages []PageView
	next  string
}

func (f *fakeRepo) LintView(_ context.Context, _ uuid.UUID, _ string, _ int) ([]PageView, string, error) {
	return f.pages, f.next, nil
}

func sid() uuid.UUID { return uuid.New() }

func TestRun_DetectsMissingSource(t *testing.T) {
	repo := &fakeRepo{pages: []PageView{{
		PageKey: "entity/x", PageKind: "entity", AutomationState: "managed",
		SourceVersions: []SourceVersionView{{SourceAssetID: sid(), SourceAssetVersionID: nil}},
	}}}
	out, _, err := Run(context.Background(), repo, uuid.New(), "", []CheckKind{CheckMissingSource}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Reason != CheckMissingSource {
		t.Fatalf("expected one missing_source finding, got %+v", out)
	}
}

func TestRun_DetectsStale_Window(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	repo := &fakeRepo{pages: []PageView{{
		PageKey: "entity/x", PageKind: "entity", AutomationState: "managed",
		LastMaintainedAt: &old,
	}}}
	out, _, err := Run(context.Background(), repo, uuid.New(), "", []CheckKind{CheckStale}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Reason != CheckStale {
		t.Fatalf("expected stale finding, got %+v", out)
	}
}

func TestRun_StaleSkipsLockedManual(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, state := range []string{"locked", "manual"} {
		repo := &fakeRepo{pages: []PageView{{
			PageKey: "entity/x", PageKind: "entity", AutomationState: state,
			LastMaintainedAt: &old,
		}}}
		out, _, _ := Run(context.Background(), repo, uuid.New(), "", []CheckKind{CheckStale}, 0, 10)
		if len(out) != 0 {
			t.Fatalf("state=%s: expected no stale finding, got %+v", state, out)
		}
	}
}

func TestRun_DetectsConflict(t *testing.T) {
	s := sid()
	repo := &fakeRepo{pages: []PageView{{
		PageKey: "entity/x", PageKind: "entity", AutomationState: "managed",
		SourceVersions: []SourceVersionView{
			{SourceAssetID: s, ContributionHash: "aaa"},
			{SourceAssetID: s, ContributionHash: "bbb"},
		},
	}}}
	out, _, _ := Run(context.Background(), repo, uuid.New(), "", []CheckKind{CheckConflict}, 0, 10)
	if len(out) != 1 {
		t.Fatalf("expected one conflict finding, got %+v", out)
	}
}

func TestRun_DetectsOrphan(t *testing.T) {
	repo := &fakeRepo{pages: []PageView{{
		PageKey: "entity/x", PageKind: "entity", AutomationState: "managed",
		CurrentVersionID: nil,
	}}}
	out, _, _ := Run(context.Background(), repo, uuid.New(), "", []CheckKind{CheckOrphan}, 0, 10)
	if len(out) != 1 || out[0].Reason != CheckOrphan {
		t.Fatalf("expected orphan, got %+v", out)
	}
}

func TestRun_DetectsSchemaDrift(t *testing.T) {
	repo := &fakeRepo{pages: []PageView{{
		PageKey: "entity/x", PageKind: "bogus_kind", AutomationState: "managed",
		CurrentVersionID: ptrUUID(uuid.New()),
	}}}
	out, _, _ := Run(context.Background(), repo, uuid.New(), "", []CheckKind{CheckSchemaDrift}, 0, 10)
	if len(out) != 1 || out[0].Reason != CheckSchemaDrift {
		t.Fatalf("expected schema_drift, got %+v", out)
	}
}

func TestRun_AllFiveFindingsForOnePage(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	s := sid()
	repo := &fakeRepo{pages: []PageView{{
		PageKey: "entity/x", PageKind: "bogus", AutomationState: "managed",
		LastMaintainedAt: &old,
		SourceVersions: []SourceVersionView{
			{SourceAssetID: s, ContributionHash: "aaa"},
			{SourceAssetID: s, ContributionHash: "bbb", SourceAssetVersionID: nil},
		},
	}}}
	out, _, _ := Run(context.Background(), repo, uuid.New(), "", nil, 0, 10)
	// Expect: missing_source, stale, conflict, orphan (nil current? no—nil), schema_drift.
	// current_version nil → orphan; page_kind bogus → schema_drift.
	got := map[CheckKind]bool{}
	for _, f := range out {
		got[f.Reason] = true
	}
	for _, k := range AllChecks {
		if !got[k] {
			t.Errorf("expected finding for %s", k)
		}
	}
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }
