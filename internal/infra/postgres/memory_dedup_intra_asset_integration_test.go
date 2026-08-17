//go:build integration

package postgres

// Integration tests for defects 2/3 (architecture Path A: 021-relaxed
// knowledge_relations CHECK + per-unit from_unit_id/to_unit_id columns).
//
// These exercise the real SQL the unit-test fakes cannot reach:
//   - intra-asset contradicts edge (two memory_units under ONE
//     knowledge_assets(memory) row) lands in knowledge_relations with the
//     per-unit ids set (defect 2 — the pre-021 CHECK rejected this with 23514).
//   - intra-asset supersede edge lands with per-unit ids + origin=human, and
//     the survivor is NOT auto-published (附录 A 不变量 9) (defect 2/3).
//   - cross-asset edge keeps *_unit_id NULL (the per-unit columns do not
//     pollute inter-asset relations).
//   - defect 3: an InsertRelation hard error (SQLSTATE 23xxx) aborts the
//     supersede disposition BEFORE any state mutation — superseded_by is not
//     written, the unit is not deprecated.
//
// The canonical dedup/publish integration baseline is owned by the test
// engineer (memory_dedup_publish_integration_test.go); this file verifies the
// Path-A fix against live SQL before handoff.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/memory/dedup"
	"github.com/lynn901/mora/internal/module/memory/evidence"
)

// seedTwoUnitsSameAsset inserts two candidate memory_units under ONE
// knowledge_assets(memory) row (the intra-asset dedup scenario). Returns the
// workspace id + shared asset id + the workspace owner (reviewer) + the two
// unit ids. The two units share AssetID — the case the pre-021 CHECK rejected.
func seedTwoUnitsSameAsset(t *testing.T, pool *pgxpool.Pool, ctx context.Context) (wsID, assetID, ownerID, unitA, unitB uuid.UUID) {
	t.Helper()
	wsID, assetID = seedMemoryAsset(t, pool)
	err := pool.QueryRow(ctx, `SELECT owner_id FROM knowledge_assets WHERE id=$1`, assetID).Scan(&ownerID)
	require.NoError(t, err, "read asset owner_id")
	unitA = seedCandidateUnitRow(t, pool, ctx, wsID, assetID, ownerID, "unit A statement")
	unitB = seedCandidateUnitRow(t, pool, ctx, wsID, assetID, ownerID, "unit B statement")
	return
}

// seedCandidateUnitRow inserts one candidate memory_unit row directly.
func seedCandidateUnitRow(t *testing.T, pool *pgxpool.Pool, ctx context.Context, wsID, assetID, ownerID uuid.UUID, stmt string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(ctx, `
		INSERT INTO memory_units
		  (workspace_id, asset_id, memory_type, statement, state, created_by_type, created_by_id)
		VALUES ($1,$2,'fact',$3,'candidate','user',$4)
		RETURNING id`, wsID, assetID, stmt, ownerID).Scan(&id)
	require.NoError(t, err, "seed candidate unit")
	return id
}

// TestDedupIntraAsset_ContradictsLandsRelation_Integration: two units under
// the SAME memory asset → a contradicts edge lands in knowledge_relations with
// from_unit_id/to_unit_id set, and from_asset_id=to_asset_id no longer errors
// (021 relaxed CHECK — defect 2).
func TestDedupIntraAsset_ContradictsLandsRelation_Integration(t *testing.T) {
	ctx := context.Background()
	pool := evidenceTestPool(t)
	resetMemoryTables(t, pool)
	wsID, assetID, ownerID, unitA, unitB := seedTwoUnitsSameAsset(t, pool, ctx)

	repo := NewMemoryRelationRepo(&DB{Pool: pool})
	aID, bID := unitA, unitB
	_, err := repo.InsertRelation(ctx, domain.KnowledgeRelation{
		WorkspaceID:   wsID,
		FromAssetID:   assetID,
		RelationType:  domain.RelationContradicts,
		ToAssetID:     assetID, // same asset — the pre-021 CHECK rejected this
		FromUnitID:    &aID,
		ToUnitID:      &bID,
		Origin:        domain.RelationOriginGenerated,
		CreatedByType: domain.SubjectUser,
		CreatedByID:   ownerID,
	})
	require.NoError(t, err, "intra-asset contradicts edge must land (021 relaxed CHECK)")

	var rt string
	var fromU, toU *uuid.UUID
	var fromA, toA uuid.UUID
	err = pool.QueryRow(ctx, `
		SELECT relation_type, from_unit_id, to_unit_id, from_asset_id, to_asset_id
		FROM knowledge_relations WHERE from_unit_id=$1`, unitA).
		Scan(&rt, &fromU, &toU, &fromA, &toA)
	require.NoError(t, err)
	assert.Equal(t, string(domain.RelationContradicts), rt)
	require.NotNil(t, fromU)
	require.NotNil(t, toU)
	assert.Equal(t, unitA, *fromU, "from_unit_id pinned")
	assert.Equal(t, unitB, *toU, "to_unit_id pinned")
	assert.Equal(t, assetID, fromA, "from_asset_id = the shared asset")
	assert.Equal(t, assetID, toA, "to_asset_id = the shared asset (intra-asset, permitted by 021)")
}

// TestInboxIntraAsset_SupersedeWritesEdge_Integration: supersede between two
// units of the SAME asset → the supersedes edge lands with per-unit ids +
// origin=human, and the survivor is NOT auto-published (附录 A 不变量 9).
// This exercises the real InboxService wired to real repos over live SQL.
func TestInboxIntraAsset_SupersedeWritesEdge_Integration(t *testing.T) {
	ctx := context.Background()
	pool := evidenceTestPool(t)
	resetMemoryTables(t, pool)
	wsID, _, ownerID, survivor, deprecated := seedTwoUnitsSameAsset(t, pool, ctx)

	units := NewMemoryUnitRepo(NewDB(pool))
	suggestions := NewDedupSuggestionRepo(NewDB(pool))
	links := NewMemoryEvidenceLinkRepo(NewDB(pool))
	sink := NewMemoryPublishSink(&DB{Pool: pool}, NewMemoryReviewGate(nil))
	rels := NewMemoryRelationRepo(&DB{Pool: pool})
	svc := dedup.NewInboxService(units, suggestions, links, sink, rels)

	auth := dedup.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: ownerID}
	sup := survivor
	res, err := svc.Review(ctx, auth, dedup.DispositionRequest{
		UnitID:      deprecated,
		WorkspaceID: wsID,
		Disposition: dedup.DispositionSupersede,
		SupersedeBy: &sup,
	})
	require.NoError(t, err)
	assert.Equal(t, domain.MemoryDeprecated, res.State)

	// Edge landed with per-unit ids + human origin (defect 2/3 fix).
	var rt, origin string
	var fromU, toU *uuid.UUID
	err = pool.QueryRow(ctx, `
		SELECT relation_type, from_unit_id, to_unit_id, origin
		FROM knowledge_relations WHERE from_unit_id=$1`, survivor).
		Scan(&rt, &fromU, &toU, &origin)
	require.NoError(t, err, "supersedes edge must land (defect 2/3 fix)")
	assert.Equal(t, string(domain.RelationSupersedes), rt)
	assert.Equal(t, string(domain.RelationOriginHuman), origin)
	require.NotNil(t, fromU)
	require.NotNil(t, toU)
	assert.Equal(t, survivor, *fromU)
	assert.Equal(t, deprecated, *toU)

	// Survivor NOT auto-published (附录 A 不变量 9): supersede only deprecates
	// the target; the survivor stays candidate until the reviewer publishes it.
	var survivorState string
	err = pool.QueryRow(ctx, `SELECT state FROM memory_units WHERE id=$1`, survivor).Scan(&survivorState)
	require.NoError(t, err)
	assert.NotEqual(t, string(domain.MemoryPublished), survivorState,
		"supersede must not auto-publish the survivor")

	// Deprecated unit flipped + superseded_by pinned.
	var depState string
	var depSup *uuid.UUID
	err = pool.QueryRow(ctx, `SELECT state, superseded_by FROM memory_units WHERE id=$1`, deprecated).
		Scan(&depState, &depSup)
	require.NoError(t, err)
	assert.Equal(t, string(domain.MemoryDeprecated), depState)
	require.NotNil(t, depSup)
	assert.Equal(t, survivor, *depSup)
}

// TestDedupCrossAsset_UnitIDsNil_Integration: a cross-asset relation (the
// 014-native case) keeps from_unit_id/to_unit_id NULL — the per-unit columns
// do not pollute inter-asset relations.
func TestDedupCrossAsset_UnitIDsNil_Integration(t *testing.T) {
	ctx := context.Background()
	pool := evidenceTestPool(t)
	resetMemoryTables(t, pool)
	wsID, assetA, ownerID, _, _ := seedTwoUnitsSameAsset(t, pool, ctx)
	// A second memory asset under the same workspace (different asset row).
	_, assetB := seedMemoryAsset(t, pool)

	repo := NewMemoryRelationRepo(&DB{Pool: pool})
	_, err := repo.InsertRelation(ctx, domain.KnowledgeRelation{
		WorkspaceID:   wsID,
		FromAssetID:   assetA,
		RelationType:  domain.RelationDerivedFrom, // a cross-asset relation_type
		ToAssetID:     assetB,
		Origin:        domain.RelationOriginGenerated,
		CreatedByType: domain.SubjectUser,
		CreatedByID:   ownerID,
	})
	require.NoError(t, err)

	var fromU, toU *uuid.UUID
	err = pool.QueryRow(ctx, `
		SELECT from_unit_id, to_unit_id FROM knowledge_relations
		WHERE from_asset_id=$1 AND to_asset_id=$2`, assetA, assetB).
		Scan(&fromU, &toU)
	require.NoError(t, err)
	assert.Nil(t, fromU, "cross-asset edge keeps from_unit_id NULL")
	assert.Nil(t, toU, "cross-asset edge keeps to_unit_id NULL")
}

// TestInboxSupersede_HardErrorAborts_Integration (defect 3): an InsertRelation
// hard error (SQLSTATE 23xxx) aborts the supersede disposition BEFORE any
// state mutation — superseded_by is not written, the unit is not deprecated.
// The relation write precedes the state write (inbox.go supersede), so a hard
// error needs no compensating rollback; the reviewer sees a real error instead
// of a silent success with a missing edge.
func TestInboxSupersede_HardErrorAborts_Integration(t *testing.T) {
	ctx := context.Background()
	pool := evidenceTestPool(t)
	resetMemoryTables(t, pool)
	wsID, _, ownerID, survivor, deprecated := seedTwoUnitsSameAsset(t, pool, ctx)

	units := NewMemoryUnitRepo(NewDB(pool))
	suggestions := NewDedupSuggestionRepo(NewDB(pool))
	links := NewMemoryEvidenceLinkRepo(NewDB(pool))
	sink := NewMemoryPublishSink(&DB{Pool: pool}, NewMemoryReviewGate(nil))
	svc := dedup.NewInboxService(units, suggestions, links, sink, &hardFailingRelationRepo{})

	auth := dedup.AuthContext{SubjectType: domain.SubjectUser, PrincipalID: ownerID}
	sup := survivor
	_, err := svc.Review(ctx, auth, dedup.DispositionRequest{
		UnitID:      deprecated,
		WorkspaceID: wsID,
		Disposition: dedup.DispositionSupersede,
		SupersedeBy: &sup,
	})
	require.Error(t, err, "hard error must abort the disposition, not swallow")

	// Defect 3: no state mutation happened. The unit stays candidate.
	var state string
	err = pool.QueryRow(ctx, `SELECT state FROM memory_units WHERE id=$1`, deprecated).Scan(&state)
	require.NoError(t, err)
	assert.Equal(t, string(domain.MemoryCandidate), state,
		"unit must NOT be deprecated (disposition aborted before state write)")

	// And superseded_by was not written.
	var supBy *uuid.UUID
	err = pool.QueryRow(ctx, `SELECT superseded_by FROM memory_units WHERE id=$1`, deprecated).Scan(&supBy)
	require.NoError(t, err)
	assert.Nil(t, supBy, "superseded_by must NOT be written (disposition aborted)")

	// And no relation edge landed.
	var n int
	err = pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_relations WHERE workspace_id=$1`, wsID).Scan(&n)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "no relation edge must land (hard error aborted before any write)")
}

// hardFailingRelationRepo is a KnowledgeRelationWriter whose InsertRelation
// always fails with a class-23 (integrity constraint violation) pgerror — the
// exact shape isHardRelationError keys on — so the inbox supersede path's
// hard-error branch fires. It writes nothing: the point of defect 3 is that a
// hard error aborts before any state mutation.
type hardFailingRelationRepo struct{}

func (*hardFailingRelationRepo) InsertRelation(context.Context, domain.KnowledgeRelation) (uuid.UUID, error) {
	return uuid.Nil, &pgconn.PgError{Code: "23514", Message: "violates_check"}
}

var _ evidence.KnowledgeRelationWriter = (*hardFailingRelationRepo)(nil)
