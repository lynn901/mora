package skill

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/lynn901/mora/internal/domain"
)

// --- delivery-path fakes ---

// fakeAssetResolver is a programmable AssetResolver for delivery tests. It
// returns a fixed asset (type+workspace) for asset ids it knows, and a fixed
// version for (asset, versionSpec) pairs it knows. Unknown → ErrPackageNotFound.
type fakeAssetResolver struct {
	asset   domain.KnowledgeAsset
	version domain.AssetVersion
	missAsset bool // return not-found for GetAsset
	missVer   bool // return not-found for ResolveVersion
	skills  []domain.KnowledgeAsset // ListSkillsByWorkspace backing
}

func (f fakeAssetResolver) GetAsset(_ context.Context, _ uuid.UUID) (domain.KnowledgeAsset, error) {
	if f.missAsset {
		return domain.KnowledgeAsset{}, ErrPackageNotFound
	}
	return f.asset, nil
}

func (f fakeAssetResolver) ResolveVersion(_ context.Context, _ uuid.UUID, _ string) (domain.AssetVersion, error) {
	if f.missVer {
		return domain.AssetVersion{}, ErrPackageNotFound
	}
	return f.version, nil
}

func (f fakeAssetResolver) ListSkillsByWorkspace(_ context.Context, _ uuid.UUID) ([]domain.KnowledgeAsset, error) {
	return f.skills, nil
}

// fakeBindingResolver returns a fixed candidate binding set for any
// (agent, workspace). The delivery service applies the §5.3 precedence.
type fakeBindingResolver struct {
	candidates []domain.AgentBinding
	err        error
}

func (f fakeBindingResolver) ActiveForAgent(_ context.Context, _, _ uuid.UUID) ([]domain.AgentBinding, error) {
	return f.candidates, f.err
}

// --- delivery fixtures ---

var (
	deliveryWS      = uuid.New()
	deliveryAgent   = uuid.New()
	deliveryAsset   = uuid.New()
	deliveryAssetType = domain.AssetTypeSkill
	deliveryVersion = uuid.New()
)

func deliveryPkg() domain.SkillPackage {
	return domain.SkillPackage{
		AssetVersionID: deliveryVersion,
		StorageKey:     "skill/test.tar.gz",
		FormatID:        domain.SkillFormatOpaque,
		Manifest: domain.SkillManifest{
			Files: []domain.SkillFileEntry{
				{Path: "SKILL.md", Hash: "h1", Kind: "skill_md"},
				{Path: "assets/guide.md", Hash: "h2", Kind: "asset"},
			},
			CapabilitySummary: map[string]any{"tools": []any{"echo"}},
			ContentHash:       "pkg-hash",
			EntryCount:        2,
		},
		OriginalFrontmatter: map[string]any{"name": "echo-skill", "version": "1.0"},
		ContentHash:         "pkg-hash",
		CompatibilityReport: domain.CompatibilityReport{Delivery: domain.DeliveryLossless},
	}
}

// setupDelivery wires a delivery service with the given candidate bindings +
// archive bytes. The asset/version resolvers are pre-seeded so a successful
// path resolves deliveryAsset → deliveryVersion → deliveryPkg().
func setupDelivery(t *testing.T, candidates []domain.AgentBinding, archiveBytes []byte) *DeliveryService {
	t.Helper()
	repo := newMemRepo()
	require.NoError(t, repo.Save(context.Background(), deliveryPkg()))
	assets := fakeAssetResolver{
		asset:   domain.KnowledgeAsset{ID: deliveryAsset, WorkspaceID: deliveryWS, AssetType: deliveryAssetType},
		version: domain.AssetVersion{ID: deliveryVersion, AssetID: deliveryAsset, VersionNo: 1, BuildStatus: domain.VersionBuildReady, GovernanceStatus: domain.VersionGovPublished},
	}
	bindings := fakeBindingResolver{candidates: candidates}
	opener := memOpener{data: archiveBytes}
	return NewDeliveryService(repo, assets, bindings, opener)
}

func mustBuildSampleArchive(t *testing.T) []byte {
	return buildSampleArchive(t)
}

// --- effective-binding precedence (§5.3) ---

// TestDeliver_AllowAssetScope wins for a simple allow binding on the asset.
func TestDeliver_AllowAssetScope(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{ID: uuid.New(), AgentID: deliveryAgent, WorkspaceID: deliveryWS,
			ScopeKind: domain.BindingScopeAsset, AssetID: &deliveryAsset,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryTool, Priority: 0},
	}, mustBuildSampleArchive(t))
	res, err := svc.Deliver(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest")
	require.NoError(t, err)
	assert.Equal(t, domain.BindingDeliveryTool, res.DeliveryMode)
	assert.NotNil(t, res.Manifest, "tool mode returns the full manifest")
	assert.Equal(t, "pkg-hash", res.ContentHash)
	assert.Equal(t, "echo-skill", res.Header["name"])
}

// TestDeliver_WorkspaceScopeAllow covers every asset in the workspace.
func TestDeliver_WorkspaceScopeAllow(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{ID: uuid.New(), AgentID: deliveryAgent, WorkspaceID: deliveryWS,
			ScopeKind: domain.BindingScopeWorkspace, Effect: domain.BindingAllow,
			VersionPolicy: domain.BindingFollowPublished, DeliveryMode: domain.BindingDeliverySummary},
	}, mustBuildSampleArchive(t))
	res, err := svc.Deliver(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest")
	require.NoError(t, err)
	assert.Equal(t, domain.BindingDeliverySummary, res.DeliveryMode)
	assert.Nil(t, res.Manifest, "summary mode does NOT return the raw file list")
	assert.Equal(t, []any{"echo"}, res.CapabilitySummary["tools"])
}

// TestDeliver_AssetTypeScopeAllow matches by the asset's type.
func TestDeliver_AssetTypeScopeAllow(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{ID: uuid.New(), AgentID: deliveryAgent, WorkspaceID: deliveryWS,
			ScopeKind: domain.BindingScopeAssetType, AssetType: &deliveryAssetType,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryInline},
	}, mustBuildSampleArchive(t))
	res, err := svc.Deliver(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest")
	require.NoError(t, err)
	assert.Equal(t, domain.BindingDeliveryInline, res.DeliveryMode)
	assert.NotNil(t, res.Manifest, "inline mode returns the manifest (bytes via progressive read)")
}

// TestDeliver_DenyBeatsAllow: an explicit deny on the asset wins over a
// workspace-scope allow (§5.3 / AC-7: 显式拒绝 > 显式允许). Surface not-found.
func TestDeliver_DenyBeatsAllow(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{ID: uuid.New(), AgentID: deliveryAgent, WorkspaceID: deliveryWS,
			ScopeKind: domain.BindingScopeWorkspace, Effect: domain.BindingAllow,
			VersionPolicy: domain.BindingFollowPublished, DeliveryMode: domain.BindingDeliveryTool},
		{ID: uuid.New(), AgentID: deliveryAgent, WorkspaceID: deliveryWS,
			ScopeKind: domain.BindingScopeAsset, AssetID: &deliveryAsset,
			Effect: domain.BindingDeny, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryTool},
	}, mustBuildSampleArchive(t))
	_, err := svc.Deliver(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest")
	require.ErrorIs(t, err, ErrPackageNotFound, "deny → not-found, no leak")
}

// TestDeliver_NarrowerScopeWins: when two allow bindings apply, the narrower
// scope's delivery_mode wins (asset-scope tool beats workspace-scope summary).
func TestDeliver_NarrowerScopeWins(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{ID: uuid.New(), AgentID: deliveryAgent, WorkspaceID: deliveryWS,
			ScopeKind: domain.BindingScopeWorkspace, Effect: domain.BindingAllow,
			VersionPolicy: domain.BindingFollowPublished, DeliveryMode: domain.BindingDeliverySummary},
		{ID: uuid.New(), AgentID: deliveryAgent, WorkspaceID: deliveryWS,
			ScopeKind: domain.BindingScopeAsset, AssetID: &deliveryAsset,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryTool},
	}, mustBuildSampleArchive(t))
	res, err := svc.Deliver(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest")
	require.NoError(t, err)
	assert.Equal(t, domain.BindingDeliveryTool, res.DeliveryMode, "asset-scope (narrower) wins")
	assert.NotNil(t, res.Manifest)
}

// TestDeliver_HigherPriorityWins: among same-effect same-scope-kind bindings,
// higher priority wins.
func TestDeliver_HigherPriorityWins(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{ID: uuid.New(), AgentID: deliveryAgent, WorkspaceID: deliveryWS,
			ScopeKind: domain.BindingScopeAsset, AssetID: &deliveryAsset,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliverySummary, Priority: 1},
		{ID: uuid.New(), AgentID: deliveryAgent, WorkspaceID: deliveryWS,
			ScopeKind: domain.BindingScopeAsset, AssetID: &deliveryAsset,
			Effect: domain.BindingAllow, VersionPolicy: domain.BindingFollowPublished,
			DeliveryMode: domain.BindingDeliveryTool, Priority: 10},
	}, mustBuildSampleArchive(t))
	res, err := svc.Deliver(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest")
	require.NoError(t, err)
	assert.Equal(t, domain.BindingDeliveryTool, res.DeliveryMode, "priority 10 beats 1")
}

// --- no-leak (§8.2) ---

// TestDeliver_NoBinding_NotFound: an agent with no binding for the skill gets
// not-found — it cannot tell a missing skill from one it is not bound to.
func TestDeliver_NoBinding_NotFound(t *testing.T) {
	svc := setupDelivery(t, nil, mustBuildSampleArchive(t))
	_, err := svc.Deliver(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest")
	require.ErrorIs(t, err, ErrPackageNotFound)
}

// TestDeliver_CrossWorkspace_NotFound: the asset is in a different workspace
// than the delegated scope → not-found (no leak).
func TestDeliver_CrossWorkspace_NotFound(t *testing.T) {
	repo := newMemRepo()
	require.NoError(t, repo.Save(context.Background(), deliveryPkg()))
	assets := fakeAssetResolver{asset: domain.KnowledgeAsset{
		ID: deliveryAsset, WorkspaceID: uuid.New(), AssetType: deliveryAssetType,
	}}
	bindings := fakeBindingResolver{candidates: []domain.AgentBinding{
		{AgentID: deliveryAgent, WorkspaceID: deliveryWS, ScopeKind: domain.BindingScopeWorkspace,
			Effect: domain.BindingAllow, DeliveryMode: domain.BindingDeliveryTool},
	}}
	svc := NewDeliveryService(repo, assets, bindings, memOpener{data: mustBuildSampleArchive(t)})
	_, err := svc.Deliver(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest")
	require.ErrorIs(t, err, ErrPackageNotFound, "cross-workspace → not-found, no leak")
}

// TestDeliver_NoAgentContext_NotFound: a service_account caller with no agent
// context (AgentID nil) is refused — the internal token alone never
// authorizes skill delivery (§11.2).
func TestDeliver_NoAgentContext_NotFound(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{AgentID: deliveryAgent, WorkspaceID: deliveryWS, ScopeKind: domain.BindingScopeWorkspace,
			Effect: domain.BindingAllow, DeliveryMode: domain.BindingDeliveryTool},
	}, mustBuildSampleArchive(t))
	_, err := svc.Deliver(context.Background(), uuid.Nil, deliveryWS, deliveryAsset, "latest")
	require.ErrorIs(t, err, ErrPackageNotFound)
}

// --- progressive resource read (§6.2) ---

// TestReadResource_Inline reads a declared resource file and verifies its hash.
func TestReadResource_Inline(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{AgentID: deliveryAgent, WorkspaceID: deliveryWS, ScopeKind: domain.BindingScopeAsset,
			AssetID: &deliveryAsset, Effect: domain.BindingAllow,
			VersionPolicy: domain.BindingFollowPublished, DeliveryMode: domain.BindingDeliveryInline},
	}, mustBuildSampleArchive(t))
	rc, err := svc.ReadResource(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest", "assets/guide.md")
	require.NoError(t, err)
	assert.Equal(t, "assets/guide.md", rc.Path)
	assert.Equal(t, "pkg-hash", rc.ContentHash)
	assert.Contains(t, string(rc.Content), "Guide")
}

// TestReadResource_SummaryRefused: a summary-mode binding does NOT permit raw
// resource reads — the agent received only the capability summary, not the
// file inventory. Reading a resource would leak beyond the granted surface.
func TestReadResource_SummaryRefused(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{AgentID: deliveryAgent, WorkspaceID: deliveryWS, ScopeKind: domain.BindingScopeAsset,
			AssetID: &deliveryAsset, Effect: domain.BindingAllow,
			VersionPolicy: domain.BindingFollowPublished, DeliveryMode: domain.BindingDeliverySummary},
	}, mustBuildSampleArchive(t))
	_, err := svc.ReadResource(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest", "assets/guide.md")
	require.ErrorIs(t, err, ErrPackageNotFound, "summary mode refuses raw reads — no leak")
}

// TestReadResource_PathNotInManifest: a path that is not a manifest entry is
// refused (defence-in-depth against path traversal / synthetic paths).
func TestReadResource_PathNotInManifest(t *testing.T) {
	svc := setupDelivery(t, []domain.AgentBinding{
		{AgentID: deliveryAgent, WorkspaceID: deliveryWS, ScopeKind: domain.BindingScopeAsset,
			AssetID: &deliveryAsset, Effect: domain.BindingAllow,
			VersionPolicy: domain.BindingFollowPublished, DeliveryMode: domain.BindingDeliveryInline},
	}, mustBuildSampleArchive(t))
	_, err := svc.ReadResource(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest", "secrets/../../etc/passwd")
	require.ErrorIs(t, err, ErrPackageNotFound, "non-manifest path → not-found, no leak")
}

// TestReadResource_NoBinding_NotFound: no binding → no resource read (no leak).
func TestReadResource_NoBinding_NotFound(t *testing.T) {
	svc := setupDelivery(t, nil, mustBuildSampleArchive(t))
	_, err := svc.ReadResource(context.Background(), deliveryAgent, deliveryWS, deliveryAsset, "latest", "SKILL.md")
	require.ErrorIs(t, err, ErrPackageNotFound)
}
