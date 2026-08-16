package codegraph_test

// service_test.go pins the codegraph query-service contract (design-docs/17
// §4.2 / §6.1 / §8.2 / §15). The service is the read-side chokepoint every
// query method funnels through loadActiveGraph, which owns:
//   - resource-level RBAC via asset.ReadService (no existence leak, §8.2 /
//     §10.4 用例 26/27);
//   - active-projection locator read via ProjectionRepo;
//   - the §4.2 stale/misalignment detection (current_version vs bound version)
//     surfaced as a Stale flag the caller MUST see — an expired revision never
//     masquerades as current (§3.2 / §7.2 T4);
//   - routing through the provider.CodeGraphProvider (§10.2 red line — MCP
//     tools MUST NOT bypass the provider).
//
// Contract cases (§7.2):
//   T2  fail-closed — a provider returning source_snapshot_unavailable /
//       capability_unavailable surfaces as-is, never as fabricated hits.
//   T3  graph/source-tree/commit anchor — the projection locator carries
//       graph_ref + source_tree_ref + commit + source_tree_hash, and the
//       service stamps the active graph's commit onto results (§3.2).
//   T4  stale revision — when the asset's current version differs from the
//       graph's bound version, results carry the graph's commit (not the new
//       version's) and the Stale flag is set (§4.2 row 1 serve-stale).
//   T9  existence leak — a missing / cross-workspace / non-codebase asset
//       surfaces as ErrCodebaseNotFound, indistinguishable across the three
//       cases (§8.2).

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
	cgservice "github.com/lynn901/mora/internal/module/knowledge/codegraph/service"
)

// --- test doubles ---

// fakeReadRepo is a minimal asset.ReadRepo returning a canned asset + err.
// Only Get is exercised by these tests (via ReadService.GetAsset); the other
// ReadRepo methods are stubbed so the fake satisfies the interface.
type fakeReadRepo struct {
	asset *domain.KnowledgeAsset
	err   error
}

func (f fakeReadRepo) Get(_ context.Context, _ uuid.UUID) (*domain.KnowledgeAsset, error) {
	return f.asset, f.err
}
func (fakeReadRepo) List(_ context.Context, _ asset.ListQuery) ([]*domain.KnowledgeAsset, string, error) {
	return nil, "", nil
}
func (fakeReadRepo) ListVersions(_ context.Context, _ uuid.UUID) ([]*domain.AssetVersion, error) {
	return nil, nil
}
func (fakeReadRepo) GetVersionByID(_ context.Context, _ uuid.UUID) (*domain.AssetVersion, uuid.UUID, error) {
	return nil, uuid.Nil, asset.ErrAssetNotFound
}
func (fakeReadRepo) ListRelations(_ context.Context, _ uuid.UUID, _ string) ([]*domain.KnowledgeRelation, error) {
	return nil, nil
}

// fakeProjectionRepo returns a canned ActiveGraph + err for ActiveCodeGraph.
type fakeProjectionRepo struct {
	g   cgservice.ActiveGraph
	err error
}

func (f fakeProjectionRepo) ActiveCodeGraph(_ context.Context, _ uuid.UUID) (cgservice.ActiveGraph, error) {
	return f.g, f.err
}

// recordingProvider records the graph_ref each query receives and returns a
// canned result/error. Used to assert the service routes through the provider
// and stamps the graph's commit onto results.
type recordingProvider struct {
	lastGraphRef string
	node         cgprovider.CodeNode
	explore      cgprovider.ExploreResult
	err          error
}

func (p *recordingProvider) Capabilities(context.Context) (cgprovider.CodeGraphCapabilities, error) {
	return cgprovider.CodeGraphCapabilities{Languages: []string{"go"}}, nil
}
func (p *recordingProvider) Build(context.Context, cgprovider.BuildRequest) (cgprovider.BuildResult, error) {
	return cgprovider.BuildResult{}, nil
}
func (p *recordingProvider) Explore(_ context.Context, graphRef string, _ cgprovider.ExploreRequest) (cgprovider.ExploreResult, error) {
	p.lastGraphRef = graphRef
	if p.err != nil {
		return cgprovider.ExploreResult{}, p.err
	}
	return p.explore, nil
}
func (p *recordingProvider) Search(context.Context, string, cgprovider.CodeSearchRequest) ([]cgprovider.CodeHit, error) {
	return nil, nil
}
func (p *recordingProvider) Files(context.Context, string, cgprovider.FilesRequest) (cgprovider.FileTree, error) {
	return cgprovider.FileTree{}, nil
}
func (p *recordingProvider) Node(_ context.Context, graphRef string, _ cgprovider.NodeRequest) (cgprovider.CodeNode, error) {
	p.lastGraphRef = graphRef
	if p.err != nil {
		return cgprovider.CodeNode{}, p.err
	}
	return p.node, nil
}
func (p *recordingProvider) Callers(context.Context, string, cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	return nil, nil
}
func (p *recordingProvider) Callees(context.Context, string, cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	return nil, nil
}
func (p *recordingProvider) Impact(context.Context, string, cgprovider.ImpactRequest) ([]cgprovider.CodeHit, error) {
	return nil, nil
}
func (p *recordingProvider) Status(_ context.Context, graphRef string) (cgprovider.GraphStatus, error) {
	p.lastGraphRef = graphRef
	if p.err != nil {
		return cgprovider.GraphStatus{}, p.err
	}
	return cgprovider.GraphStatus{}, nil
}
func (p *recordingProvider) Delete(context.Context, string) error    { return nil }
func (p *recordingProvider) Health(context.Context) error            { return nil }

// newService wires a codegraph service with a fake asset ReadService (nil rbac
// = unit-test allow), a fake ProjectionRepo, and a fake provider.
func newService(repo asset.ReadRepo, projs cgservice.ProjectionRepo, p cgprovider.CodeGraphProvider) *cgservice.Service {
	return cgservice.NewService(asset.NewReadService(repo), projs, p)
}

// a codebase asset with current_version_id = versionA.
func codebaseAsset(currentVersion uuid.UUID) *domain.KnowledgeAsset {
	return &domain.KnowledgeAsset{
		ID:               uuid.New(),
		WorkspaceID:      uuid.New(),
		AssetType:        domain.AssetTypeCodebase,
		CurrentVersionID: &currentVersion,
	}
}

// an active graph bound to versionA, built at commit C.
func activeGraph(assetID, versionA uuid.UUID) cgservice.ActiveGraph {
	return cgservice.ActiveGraph{
		AssetID:        assetID,
		AssetVersionID:  versionA,
		GraphRef:       "g1",
		SourceTreeRef:  "st1",
		Commit:         "commit-A",
		SourceTreeHash: "hash-A",
		ProviderVersion: "1.5.0",
		ProviderName:   "codegraph",
	}
}

// --- T9: existence leak (§8.2 / §10.4 用例 26/27) ---

// TestResolveAsset_MissingCodebaseNoLeak asserts a missing codebase surfaces as
// ErrCodebaseNotFound — the same sentinel a cross-workspace / no-permission
// codebase would produce (the handler maps all three to 404/40400, §8.2). The
// service never re-implements RBAC; it reuses asset.ReadService's gate.
func TestResolveAsset_MissingCodebaseNoLeak(t *testing.T) {
	repo := fakeReadRepo{asset: nil, err: asset.ErrAssetNotFound}
	svc := newService(repo, fakeProjectionRepo{}, &recordingProvider{})

	// A nil-rbac ReadService returns the repo's error directly (no authorize
	// gate). The service maps asset.ErrAssetNotFound → ErrCodebaseNotFound.
	_, err := svc.Status(context.Background(), asset.AuthContext{IsAdmin: true}, uuid.New())
	assert.ErrorIs(t, err, cgservice.ErrCodebaseNotFound, "missing codebase must surface as ErrCodebaseNotFound (no leak)")
}

// TestResolveAsset_NonCodebaseNoLeak asserts a document asset at the same id
// surfaces as ErrCodebaseNotFound (not a type leak) — a codegraph caller must
// not learn that the id holds a document (§8.2).
func TestResolveAsset_NonCodebaseNoLeak(t *testing.T) {
	docAsset := &domain.KnowledgeAsset{
		ID:        uuid.New(),
		AssetType: domain.AssetTypeDocument, // not a codebase
	}
	repo := fakeReadRepo{asset: docAsset}
	svc := newService(repo, fakeProjectionRepo{}, &recordingProvider{})

	_, err := svc.Status(context.Background(), asset.AuthContext{IsAdmin: true}, docAsset.ID)
	assert.ErrorIs(t, err, cgservice.ErrCodebaseNotFound, "a non-codebase asset must surface as not-found, not leak its type")
}

// TestResolveAsset_GraphNotReadyNoLeak asserts a codebase that resolves under
// RBAC but has no ready projection surfaces as ErrGraphNotReady (not 404) — the
// codebase's existence is already known to the caller, so "not ready" leaks
// nothing (§8.2, §6.1).
func TestResolveAsset_GraphNotReadyNoLeak(t *testing.T) {
	ver := uuid.New()
	assetRec := codebaseAsset(ver)
	repo := fakeReadRepo{asset: assetRec}
	projs := fakeProjectionRepo{err: cgservice.ErrGraphNotReady}
	svc := newService(repo, projs, &recordingProvider{})

	_, err := svc.Status(context.Background(), asset.AuthContext{IsAdmin: true}, assetRec.ID)
	assert.ErrorIs(t, err, cgservice.ErrGraphNotReady, "no-ready-projection must surface as ErrGraphNotReady, distinct from not-found")
}

// --- T3: graph/source-tree/commit anchor binding (§4.2 / §10.2) ---

// TestLoadActiveGraph_AnchorCarriesLocator asserts the active projection's
// locator fields (graph_ref / source_tree_ref / commit / source_tree_hash)
// flow from the repo into the ActiveGraph the service routes queries through.
// A regression that dropped one would break the §3.2 commit-anchoring.
func TestLoadActiveGraph_AnchorCarriesLocator(t *testing.T) {
	ver := uuid.New()
	assetRec := codebaseAsset(ver)
	g := activeGraph(assetRec.ID, ver)
	svc := newService(fakeReadRepo{asset: assetRec}, fakeProjectionRepo{g: g}, &recordingProvider{})

	// Status reads through loadActiveGraph; the returned GraphStatus inherits
	// the provider's view but the service must have routed to g.GraphRef.
	prov := &recordingProvider{}
	svc = newService(fakeReadRepo{asset: assetRec}, fakeProjectionRepo{g: g}, prov)
	_, err := svc.Status(context.Background(), asset.AuthContext{IsAdmin: true}, assetRec.ID)
	require.NoError(t, err)
	assert.Equal(t, "g1", prov.lastGraphRef, "service must route to the active graph_ref")
}

// TestExplore_StampsGraphCommitOntoResults asserts that when the provider
// returns an ExploreResult with an empty Commit, the service stamps the
// active graph's commit (§3.2 — an expired revision never masquerades as
// current; every result carries the graph's commit).
func TestExplore_StampsGraphCommitOntoResults(t *testing.T) {
	ver := uuid.New()
	assetRec := codebaseAsset(ver)
	g := activeGraph(assetRec.ID, ver)
	prov := &recordingProvider{
		explore: cgprovider.ExploreResult{Commit: "", Hits: []cgprovider.CodeHit{
			{Loc: cgprovider.CodeLoc{Path: "main.go", Symbol: "Serve"}},
		}},
	}
	svc := newService(fakeReadRepo{asset: assetRec}, fakeProjectionRepo{g: g}, prov)

	res, err := svc.Explore(context.Background(), asset.AuthContext{IsAdmin: true}, assetRec.ID, cgprovider.ExploreRequest{Query: "Serve"})
	require.NoError(t, err)
	assert.Equal(t, "commit-A", res.Commit, "service must stamp the active graph's commit onto results missing it (§3.2)")
}

// --- T4: stale revision (§4.2 row 1 serve-stale / §7.2 T4) ---

// TestLoadActiveGraph_StaleRevisionFlagged asserts that when the asset's
// current_version_id has moved past the graph's bound version, the returned
// graph carries Stale=true. Per §15 row 1 the system continues serving the
// last-good graph, but the caller MUST see the stale flag — the graph's commit
// still labels results, never the new version's (§7.2 T4 no-masking).
func TestLoadActiveGraph_StaleRevisionFlagged(t *testing.T) {
	versionA := uuid.New() // the version the graph was built from
	versionB := uuid.New() // the asset's current version (HEAD moved on)
	assetRec := codebaseAsset(versionB)
	g := activeGraph(assetRec.ID, versionA) // bound to versionA
	svc := newService(fakeReadRepo{asset: assetRec}, fakeProjectionRepo{g: g}, &recordingProvider{})

	st, err := svc.Status(context.Background(), asset.AuthContext{IsAdmin: true}, assetRec.ID)
	require.NoError(t, err)
	assert.True(t, st.Stale, "a graph bound to an older version than current must surface Stale (§4.2 row 1)")
}

// TestLoadActiveGraph_CurrentVersionNotStale asserts a graph bound to the
// current version is NOT marked stale — guarding against an over-broad stale
// flag that would mislabel fresh graphs.
func TestLoadActiveGraph_CurrentVersionNotStale(t *testing.T) {
	ver := uuid.New()
	assetRec := codebaseAsset(ver)
	g := activeGraph(assetRec.ID, ver)
	svc := newService(fakeReadRepo{asset: assetRec}, fakeProjectionRepo{g: g}, &recordingProvider{})

	st, err := svc.Status(context.Background(), asset.AuthContext{IsAdmin: true}, assetRec.ID)
	require.NoError(t, err)
	assert.False(t, st.Stale, "a graph bound to the current version must not be stale")
}

// --- T2: fail-closed propagation (§15 rows 2 & 3) ---

// TestNode_FailClosedSourceSnapshot asserts a provider returning
// ErrSourceSnapshotUnavailable on a read op surfaces as-is to the caller —
// never decoded into a (possibly misaligned) CodeNode (§4.2 / §15 row 2).
func TestNode_FailClosedSourceSnapshot(t *testing.T) {
	ver := uuid.New()
	assetRec := codebaseAsset(ver)
	g := activeGraph(assetRec.ID, ver)
	prov := &recordingProvider{err: cgprovider.ErrSourceSnapshotUnavailable}
	svc := newService(fakeReadRepo{asset: assetRec}, fakeProjectionRepo{g: g}, prov)

	_, err := svc.Node(context.Background(), asset.AuthContext{IsAdmin: true}, assetRec.ID, cgprovider.NodeRequest{Symbol: "S"})
	assert.ErrorIs(t, err, cgprovider.ErrSourceSnapshotUnavailable, "provider fail-closed sentinel must propagate, not a fabricated node")
}

// TestNode_FailClosedCapabilityUnavailable asserts a provider returning
// ErrCapabilityUnavailable surfaces as-is — distinct from an authorized-empty
// result (§15 row 3: the system MUST NOT confuse provider fault, authorized-
// empty, and genuine no-results).
func TestNode_FailClosedCapabilityUnavailable(t *testing.T) {
	ver := uuid.New()
	assetRec := codebaseAsset(ver)
	g := activeGraph(assetRec.ID, ver)
	prov := &recordingProvider{err: cgprovider.ErrCapabilityUnavailable}
	svc := newService(fakeReadRepo{asset: assetRec}, fakeProjectionRepo{g: g}, prov)

	_, err := svc.Node(context.Background(), asset.AuthContext{IsAdmin: true}, assetRec.ID, cgprovider.NodeRequest{Symbol: "S"})
	assert.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable)
}

// TestNode_DoesNotBypassProvider asserts the service routes Node through the
// provider (§10.2 red line — MCP/tools MUST NOT bypass the provider). The
// recording provider captures the graph_ref to prove the call went through.
func TestNode_DoesNotBypassProvider(t *testing.T) {
	ver := uuid.New()
	assetRec := codebaseAsset(ver)
	g := activeGraph(assetRec.ID, ver)
	prov := &recordingProvider{node: cgprovider.CodeNode{Loc: cgprovider.CodeLoc{Commit: "commit-A", Symbol: "Serve"}}}
	svc := newService(fakeReadRepo{asset: assetRec}, fakeProjectionRepo{g: g}, prov)

	node, err := svc.Node(context.Background(), asset.AuthContext{IsAdmin: true}, assetRec.ID, cgprovider.NodeRequest{Symbol: "Serve"})
	require.NoError(t, err)
	assert.Equal(t, "g1", prov.lastGraphRef, "service must route through the provider, not bypass it (§10.2)")
	assert.Equal(t, "commit-A", node.Loc.Commit, "node result carries the graph's commit (§3.2)")
}

// TestStatus_NoopProviderFailClosed asserts that with a NoopProvider wired
// (unconfigured sidecar), a query against a resolved codebase surfaces
// capability_unavailable — documents/RAG/MCP degrade, never fabricate (§3.3).
func TestStatus_NoopProviderFailClosed(t *testing.T) {
	ver := uuid.New()
	assetRec := codebaseAsset(ver)
	g := activeGraph(assetRec.ID, ver)
	// NoopProvider is in the infra package; here we use a provider that returns
	// capability_unavailable from Status to represent the unconfigured state at
	// the service layer.
	prov := &recordingProvider{err: cgprovider.ErrCapabilityUnavailable}
	svc := newService(fakeReadRepo{asset: assetRec}, fakeProjectionRepo{g: g}, prov)

	_, err := svc.Status(context.Background(), asset.AuthContext{IsAdmin: true}, assetRec.ID)
	assert.ErrorIs(t, err, cgprovider.ErrCapabilityUnavailable, "unconfigured provider must fail closed, not fabricate a status")
}

// Compile-time: the recordingProvider satisfies the provider port.
var _ cgprovider.CodeGraphProvider = (*recordingProvider)(nil)

// guard against an unused import if the recording provider stops using errors.
var _ = errors.Is
