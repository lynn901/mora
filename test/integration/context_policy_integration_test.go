//go:build integration

// context_policy_integration_test.go — Phase 6-S1 (YS-202) integration tests for
// the context_authority_policies repo against a live PostgreSQL
// (mora-postgres-1). DATABASE_URL-gated; skipped when unset. Run with:
//
//	DATABASE_URL=postgres://mora:mora@mora-postgres-1:5432/mora?sslmode=disable \
//	  go test -tags=integration ./test/integration/... -run ContextPolicySuite -count=1 -v
//
// These cover the acceptance gate "策略 repo 单测：加载 current 策略、版本递增、
// is_current 排他" that the fake-repo unit tests cannot: they exercise the real
// pgx transaction, the EXCLUDE/partial-unique constraint on is_current, and the
// policy_version increment SQL.
package integration

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/suite"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/infra/postgres"
	ctxmod "github.com/lynn901/mora/internal/module/knowledge/context"
)

type ContextPolicySuite struct {
	suite.Suite
	pool *pgxpool.Pool
	db   *postgres.DB
	repo *postgres.ContextPolicyRepo
}

func TestContextPolicySuite(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	suite.Run(t, new(ContextPolicySuite))
}

func (s *ContextPolicySuite) SetupSuite() {
	pool, err := pgxpool.New(context.Background(), os.Getenv("DATABASE_URL"))
	s.Require().NoError(err)
	s.pool = pool
	s.db = postgres.NewDB(pool)
	s.repo = postgres.NewContextPolicyRepo(s.db)
}

func (s *ContextPolicySuite) TearDownSuite() { s.pool.Close() }

func (s *ContextPolicySuite) SetupTest() {
	ctx := context.Background()
	for _, t := range []string{"context_authority_policies", "context_eval_runs"} {
		_, _ = s.pool.Exec(ctx, "DELETE FROM "+t)
	}
	_, _ = s.pool.Exec(ctx, `DELETE FROM workspaces WHERE slug LIKE 'ctxpol-%'`)
	_, _ = s.pool.Exec(ctx, `DELETE FROM users WHERE email LIKE 'ctxpol-%'`)
}

func (s *ContextPolicySuite) seedWorkspace(owner domain.UUID, slug string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	s.Require().NoError(s.pool.QueryRow(ctx,
		`INSERT INTO workspaces (name, slug, owner_id) VALUES ($1,$2,$3) RETURNING id`,
		"WS "+slug, slug, owner).Scan(&id))
	return id
}

func (s *ContextPolicySuite) seedUser(email string) domain.UUID {
	ctx := context.Background()
	var id domain.UUID
	s.Require().NoError(s.pool.QueryRow(ctx,
		`INSERT INTO users (email, name, status) VALUES ($1,$2,'active') RETURNING id`,
		email, "ctxpol owner").Scan(&id))
	return id
}

// TestLoadCurrent_Missing verifies a missing policy yields ErrPolicyNotFound
// (caller falls back to built-in defaults, §5.3).
func (s *ContextPolicySuite) TestLoadCurrent_Missing() {
	ctx := context.Background()
	ws := s.seedWorkspace(s.seedUser("ctxpol-missing@x"), "ctxpol-missing")
	_, err := s.repo.LoadCurrent(ctx, ws, ctxmod.IntentSpec)
	s.ErrorIs(err, ctxmod.ErrPolicyNotFound)
}

// TestUpsert_VersionIncrement verifies each upsert increments policy_version
// and supersedes the prior current row (§5.3).
func (s *ContextPolicySuite) TestUpsert_VersionIncrement() {
	ctx := context.Background()
	ws := s.seedWorkspace(s.seedUser("ctxpol-incr@x"), "ctxpol-incr")

	r1, err := s.repo.Upsert(ctx, ctxmod.AuthorityPolicyRecord{
		WorkspaceID: ws,
		Intent:      ctxmod.IntentSpec,
		Config: ctxmod.PolicyConfig{
			PrimaryBasis:        []domain.AssetType{domain.AssetTypeDocument},
			MustSurfaceConflicts: []string{"old_spec"},
			Weights:             map[domain.AssetType]float64{domain.AssetTypeDocument: 0.9},
		},
	})
	s.Require().NoError(err)
	s.Equal(1, r1.PolicyVersion)
	s.True(r1.IsCurrent)

	// Second upsert: version 2, prior row superseded.
	r2, err := s.repo.Upsert(ctx, ctxmod.AuthorityPolicyRecord{
		WorkspaceID: ws,
		Intent:      ctxmod.IntentSpec,
		Config: ctxmod.PolicyConfig{
			PrimaryBasis:        []domain.AssetType{domain.AssetTypeDocument},
			MustSurfaceConflicts: []string{"old_spec", "impl_drift"},
			Weights:             map[domain.AssetType]float64{domain.AssetTypeDocument: 0.8},
		},
	})
	s.Require().NoError(err)
	s.Equal(2, r2.PolicyVersion)

	// LoadCurrent returns version 2.
	cur, err := s.repo.LoadCurrent(ctx, ws, ctxmod.IntentSpec)
	s.Require().NoError(err)
	s.Equal(2, cur.PolicyVersion)
	s.True(cur.IsCurrent)
	s.Equal([]string{"old_spec", "impl_drift"}, cur.Config.MustSurfaceConflicts)

	// The prior row is superseded (is_current=false, superseded_at set).
	var priorCurrent bool
	s.Require().NoError(s.pool.QueryRow(ctx,
		`SELECT is_current FROM context_authority_policies WHERE id = $1`, r1.ID).Scan(&priorCurrent))
	s.False(priorCurrent, "prior row must be superseded (is_current=false)")

	// CurrentVersion reports 2 (cache key, §5.3).
	v, err := s.repo.CurrentVersion(ctx, ws, ctxmod.IntentSpec)
	s.Require().NoError(err)
	s.Equal(2, v)
}

// TestIsCurrent_Exclusive verifies only one is_current row per (workspace,
// intent) — the EXCLUDE + partial unique constraint (migration 024). A second
// is_current insert at the SQL level MUST fail.
func (s *ContextPolicySuite) TestIsCurrent_Exclusive() {
	ctx := context.Background()
	ws := s.seedWorkspace(s.seedUser("ctxpol-exc@x"), "ctxpol-exc")

	_, err := s.repo.Upsert(ctx, ctxmod.AuthorityPolicyRecord{
		WorkspaceID: ws, Intent: ctxmod.IntentRationale,
		Config: ctxmod.PolicyConfig{Weights: map[domain.AssetType]float64{domain.AssetTypeMemory: 0.9}},
	})
	s.Require().NoError(err)

	// Raw second is_current insert (bypassing the repo's supersede logic) must
	// be rejected by the DB constraint — proves the constraint is the gate,
	// not application logic.
	_, err = s.pool.Exec(ctx, `
		INSERT INTO context_authority_policies (workspace_id, intent, policy_version, is_current, config)
		VALUES ($1, 'rationale', 99, TRUE, '{}'::jsonb)`, ws)
	s.Error(err, "second is_current must violate exclusion constraint")
}

// TestListByWorkspace verifies all four intent policies return for a workspace.
func (s *ContextPolicySuite) TestListByWorkspace() {
	ctx := context.Background()
	ws := s.seedWorkspace(s.seedUser("ctxpol-list@x"), "ctxpol-list")
	for _, it := range []ctxmod.Intent{ctxmod.IntentSpec, ctxmod.IntentRevision, ctxmod.IntentRationale, ctxmod.IntentProcedure} {
		_, err := s.repo.Upsert(ctx, ctxmod.AuthorityPolicyRecord{
			WorkspaceID: ws, Intent: it,
			Config: ctxmod.PolicyConfig{Weights: map[domain.AssetType]float64{domain.AssetTypeDocument: 0.5}},
		})
		s.Require().NoError(err)
	}
	out, err := s.repo.ListByWorkspace(ctx, ws)
	s.Require().NoError(err)
	s.Len(out, 4)
}

// TestCurrentVersion_None verifies 0 when no row exists (cache key cold-start).
func (s *ContextPolicySuite) TestCurrentVersion_None() {
	ctx := context.Background()
	ws := s.seedWorkspace(s.seedUser("ctxpol-none@x"), "ctxpol-none")
	v, err := s.repo.CurrentVersion(ctx, ws, ctxmod.IntentProcedure)
	s.Require().NoError(err)
	s.Equal(0, v)
}

// TestConfigRoundTrip verifies the PolicyConfig JSONB survives a store+load
// round-trip (primary_basis, weights, must_surface_conflicts, exclude_when).
func (s *ContextPolicySuite) TestConfigRoundTrip() {
	ctx := context.Background()
	ws := s.seedWorkspace(s.seedUser("ctxpol-rt@x"), "ctxpol-rt")
	in := ctxmod.PolicyConfig{
		PrimaryBasis:         []domain.AssetType{domain.AssetTypeDocument, domain.AssetTypeMemory},
		MustSurfaceConflicts: []string{"old_spec", "impl_drift"},
		Weights: map[domain.AssetType]float64{
			domain.AssetTypeDocument: 0.9,
			domain.AssetTypeCodebase: 0.5,
			domain.AssetTypeMemory:   0.4,
			domain.AssetTypeSkill:    0.3,
		},
		ExcludeWhen: []string{"deprecated", "version_mismatch"},
		Raw:         map[string]any{"custom_key": "custom_val"}, // forward-compat unknown key
	}
	_, err := s.repo.Upsert(ctx, ctxmod.AuthorityPolicyRecord{
		WorkspaceID: ws, Intent: ctxmod.IntentSpec, Config: in,
	})
	s.Require().NoError(err)

	cur, err := s.repo.LoadCurrent(ctx, ws, ctxmod.IntentSpec)
	s.Require().NoError(err)
	s.Equal(in.PrimaryBasis, cur.Config.PrimaryBasis)
	s.Equal(in.MustSurfaceConflicts, cur.Config.MustSurfaceConflicts)
	s.Equal(in.ExcludeWhen, cur.Config.ExcludeWhen)
	s.Equal(in.Weights, cur.Config.Weights)
	// Unknown key preserved in Raw (forward-compat).
	s.Equal("custom_val", cur.Config.Raw["custom_key"])
}

// TestLoadCurrent_IsolatesByWorkspace verifies workspace isolation (AC-4):
// workspace B cannot read workspace A's policy (LoadCurrent returns NotFound).
func (s *ContextPolicySuite) TestLoadCurrent_IsolatesByWorkspace() {
	ctx := context.Background()
	wsA := s.seedWorkspace(s.seedUser("ctxpol-a@x"), "ctxpol-a")
	wsB := s.seedWorkspace(s.seedUser("ctxpol-b@x"), "ctxpol-b")

	_, err := s.repo.Upsert(ctx, ctxmod.AuthorityPolicyRecord{
		WorkspaceID: wsA, Intent: ctxmod.IntentSpec,
		Config: ctxmod.PolicyConfig{Weights: map[domain.AssetType]float64{domain.AssetTypeDocument: 0.9}},
	})
	s.Require().NoError(err)

	// Workspace B sees nothing for the same intent.
	_, err = s.repo.LoadCurrent(ctx, wsB, ctxmod.IntentSpec)
	s.ErrorIs(err, ctxmod.ErrPolicyNotFound)
}
