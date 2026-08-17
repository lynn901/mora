//go:build integration

package postgres

// Integration probe for defect 1 (publish-tx ordering +
// review_requests.asset_version_id NOT NULL). Verifies the PublishUnit tx
// commits against real SQL after the step reorder (create asset version
// first, then review_request carrying that version id).
//
// The canonical dedup/publish integration baseline is owned by the test
// engineer (memory_dedup_publish_integration_test.go). This probe exists only
// to confirm the defect-1 fix against live SQL before handoff; it exercises the
// same PublishUnit path the canonical tests do.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

func TestPublishUnit_Defect1_PublishTxCommits_Integration(t *testing.T) {
	ctx := context.Background()
	pool := evidenceTestPool(t)
	resetMemoryTables(t, pool)
	wsID, assetID := seedMemoryAsset(t, pool)

	// PublishUnit requires a governance_profile_id (review_requests.governance_
	// profile_id NOT NULL). Seed one for the workspace's memory asset_type.
	profileID := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO governance_profiles
		  (id, workspace_id, name, asset_type, transition_rules, review_roles,
		   auto_publish, evidence_required, required_projections, is_system)
		VALUES ($1,$2,$3,'memory','{}','{}','{"memory":false}'::jsonb,false,'[]'::jsonb,false)`,
		profileID, wsID, "memory-profile-"+profileID.String()[:8])
	require.NoError(t, err, "seed governance profile")

	// A reviewer (the seeded workspace owner) publishes a candidate unit.
	var reviewerID uuid.UUID
	err = pool.QueryRow(ctx, `SELECT owner_id FROM knowledge_assets WHERE id=$1`, assetID).Scan(&reviewerID)
	require.NoError(t, err)

	var unitID uuid.UUID
	err = pool.QueryRow(ctx, `
		INSERT INTO memory_units
		  (workspace_id, asset_id, memory_type, statement, state, created_by_type, created_by_id)
		VALUES ($1,$2,'fact','go vet before merge','candidate','user',$3)
		RETURNING id`, wsID, assetID, reviewerID).Scan(&unitID)
	require.NoError(t, err, "seed candidate unit")

	sink := NewMemoryPublishSink(&DB{Pool: pool}, NewMemoryReviewGate(nil))
	verID, err := sink.PublishUnit(ctx, evidence.PublishUnitRequest{
		UnitID:              unitID,
		WorkspaceID:         wsID,
		AssetID:             assetID,
		GovernanceProfileID: profileID,
		ReviewerType:        domain.SubjectUser,
		ReviewerID:          reviewerID,
		PolicyVersion:       "probe-v1",
	})
	require.NoError(t, err, "PublishUnit tx must commit (defect 1: review_requests.asset_version_id NOT NULL)")
	assert.NotEqual(t, uuid.Nil, verID)

	// The review_request MUST carry the version id (NOT NULL + FK satisfied).
	var rrAV uuid.UUID
	err = pool.QueryRow(ctx, `SELECT asset_version_id FROM review_requests WHERE asset_id=$1`, assetID).Scan(&rrAV)
	require.NoError(t, err)
	assert.Equal(t, verID, rrAV, "review_request.asset_version_id must reference the created version")

	// The unit is published + pinned to the version.
	var state string
	var unitAV *uuid.UUID
	err = pool.QueryRow(ctx, `SELECT state, asset_version_id FROM memory_units WHERE id=$1`, unitID).Scan(&state, &unitAV)
	require.NoError(t, err)
	assert.Equal(t, string(domain.MemoryPublished), state)
	require.NotNil(t, unitAV)
	assert.Equal(t, verID, *unitAV)
}
