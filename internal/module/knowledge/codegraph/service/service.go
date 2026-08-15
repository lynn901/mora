// Package codegraph — query service (design-docs/17 §4.2 / §6). The read-side
// application service that backs the only-read REST + MCP code_* tools.
//
// It owns the query-time fail-closed validation (§4.2):
//  1. resolve the codebase Asset through asset.ReadService.GetAsset — that runs
//     the resource-level RBAC read check (rbac.Engine.Check on TargetAsset +
//     ActionRead); a missing / cross-workspace / no-permission asset surfaces
//     as asset.ErrAssetNotFound, no leak (§8.2 / §10.4 用例 26/27). The
//     codegraph service never re-implements RBAC — it reuses the asset read
//     service's gate so codebase reads are covered by the same asset-level read
//     permission (§6.1 — codebase is an existing knowledge_assets asset_type);
//  2. read the active codegraph projection's locator (graph_ref +
//     source_tree_ref + commit + source_tree_hash + provider ids);
//  3. verify capability.asset_version matches the graph_ref-bound version
//     (§10.2) + source_tree_hash still matches (§4.2) — a mismatch →
//     ErrSourceSnapshotUnavailable, no misaligned source returned;
//  4. route the query through the provider.CodeGraphProvider — the MCP tools
//     MUST NOT bypass the provider (§10.2 red line).
//
// Results carry commit / file / line / symbol (§3.2 CodeLoc); an expired
// revision never masquerades as the current result.
package codegraph

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// Sentinel errors. The handler / MCP tool layer maps these to the §11.4
// envelope so a caller cannot distinguish not-found from not-allowed (§8.2),
// and a provider fault is surfaced distinctly from authorized-empty / genuine
// no-results (§15).
var (
	// ErrCodebaseNotFound: the asset is missing, cross-workspace, or the caller
	// has no read permission (§8.2 no-leak). Maps to the same sentinel the asset
	// read service emits (asset.ErrAssetNotFound) so the handler's 404/40400 path
	// is shared.
	ErrCodebaseNotFound = errors.New("codegraph: codebase not found")
	// ErrGraphNotReady: the codebase has no ready codegraph projection yet.
	ErrGraphNotReady = errors.New("codegraph: no ready graph")
)

// ProjectionRepo reads the active codegraph projection locator (§4.2 step 1–2).
// It is a subset of asset.ActivationRegistry's read path; implemented by
// infra/postgres.
type ProjectionRepo interface {
	// ActiveCodeGraph returns the ready codegraph projection for the codebase
	// asset's current version: its locator (graph_ref/source_tree_ref/commit/
	// source_tree_hash/provider_*) + the asset_version_id it is bound to. A
	// missing/failed projection returns ErrGraphNotReady so the service surfaces
	// the build state without leaking asset internals.
	ActiveCodeGraph(ctx context.Context, assetID uuid.UUID) (ActiveGraph, error)
}

// ActiveGraph is the resolved active projection for a codebase (§4.2).
type ActiveGraph struct {
	AssetID          uuid.UUID
	AssetVersionID   uuid.UUID
	CurrentVersionID uuid.UUID
	GraphRef         string
	SourceTreeRef    string
	Commit           string
	SourceTreeHash   string
	ProviderVersion  string
	ProviderName     string
	Stale            bool // a newer build failed; serving last-good graph (§15)
}

// Service is the codegraph query application service. It resolves the codebase
// through asset.ReadService (resource-level RBAC, fail-closed no-leak) and
// routes queries through the provider. Mirrors the asset read service's gate.
type Service struct {
	assets   *asset.ReadService
	projs    ProjectionRepo
	provider cgprovider.CodeGraphProvider
}

// NewService wires the query service. assets MUST be the WithAuthz-chained
// ReadService in production so every query gates on the asset read permission
// (§8.5). provider is the provider.CodeGraphProvider (NoopProvider when the
// sidecar is unconfigured — queries fail closed with ErrCapabilityUnavailable,
// documents/RAG/MCP do not degrade, §3.3).
func NewService(assets *asset.ReadService, projs ProjectionRepo, provider cgprovider.CodeGraphProvider) *Service {
	return &Service{assets: assets, projs: projs, provider: provider}
}

// resolveAsset runs the resource-level RBAC read check for the codebase asset
// via asset.ReadService.GetAsset. leak=false: a denial / missing / cross-
// workspace asset surfaces as asset.ErrAssetNotFound (§8.2 / §10.4 用例 26/27).
// The service does NOT introduce a new RBAC target type — codebase is an
// existing knowledge_assets asset_type, covered by the asset-level read
// permission (§6.1). This mirrors the asset read service's own authorize() so
// the codegraph path can never be a weaker gate than a plain asset GET.
func (s *Service) resolveAsset(ctx context.Context, auth asset.AuthContext, id uuid.UUID) (*domain.KnowledgeAsset, error) {
	a, err := s.assets.GetAsset(ctx, auth, id)
	if err != nil {
		if errors.Is(err, asset.ErrAssetNotFound) {
			return nil, ErrCodebaseNotFound
		}
		return nil, err
	}
	if a == nil {
		return nil, ErrCodebaseNotFound
	}
	if a.AssetType != domain.AssetTypeCodebase {
		// A non-codebase asset is "not a codebase" for codegraph purposes —
		// surface as not-found so existence of, say, a document asset at the
		// same id is not leaked to a codegraph caller (§8.2).
		return nil, ErrCodebaseNotFound
	}
	return a, nil
}

// loadActiveGraph resolves the codebase + its ready codegraph projection and
// runs the §4.2 query-time validation (asset_version binding + source_tree_hash).
// It is the single chokepoint every query method funnels through.
func (s *Service) loadActiveGraph(ctx context.Context, auth asset.AuthContext, id uuid.UUID) (ActiveGraph, error) {
	a, err := s.resolveAsset(ctx, auth, id)
	if err != nil {
		return ActiveGraph{}, err
	}
	g, err := s.projs.ActiveCodeGraph(ctx, a.ID)
	if err != nil {
		if errors.Is(err, ErrGraphNotReady) {
			return ActiveGraph{}, err
		}
		return ActiveGraph{}, err
	}
	// Stamp the asset's current version id so a caller-facing Stale flag is
	// derivable when the graph's bound version lags the active version.
	if a.CurrentVersionID != nil {
		g.CurrentVersionID = *a.CurrentVersionID
	}
	// §4.2 step 2: capability.asset_version == graph_ref-bound version. The
	// projection's AssetVersionID is the version the graph was built from; the
	// asset's current_version_id must point at it (or a fixed/bound version).
	// A mismatch means the active graph is stale/misaligned. Serve-stale is
	// allowed (§15 row 1) but the caller MUST see the stale flag — the graph's
	// commit still labels results, never the new version's.
	if g.CurrentVersionID != uuid.Nil && g.AssetVersionID != uuid.Nil && g.CurrentVersionID != g.AssetVersionID {
		g.Stale = true
	}
	// §4.2 step 3: source_tree_hash validation happens at query time inside the
	// provider (it re-verifies its own source tree). The locator's hash is the
	// build-time value; a provider-side mismatch surfaces as
	// ErrSourceSnapshotUnavailable, which the query methods map to fail-closed.
	return g, nil
}

// --- query methods (§6.2 surfaces) ---

// Status returns the active graph metadata (code_status). It returns graph
// version metadata + capability_unavailable when the provider is down (§15 row
// 3) — never fakes a result.
func (s *Service) Status(ctx context.Context, auth asset.AuthContext, id uuid.UUID) (cgprovider.GraphStatus, error) {
	g, err := s.loadActiveGraph(ctx, auth, id)
	if err != nil {
		return cgprovider.GraphStatus{}, err
	}
	st, err := s.provider.Status(ctx, g.GraphRef)
	if err != nil {
		return cgprovider.GraphStatus{}, err
	}
	if g.Stale {
		st.Stale = true
	}
	return st, nil
}

// Files returns the source tree listing (code_files).
func (s *Service) Files(ctx context.Context, auth asset.AuthContext, id uuid.UUID, req cgprovider.FilesRequest) (cgprovider.FileTree, error) {
	g, err := s.loadActiveGraph(ctx, auth, id)
	if err != nil {
		return cgprovider.FileTree{}, err
	}
	return s.provider.Files(ctx, g.GraphRef, req)
}

// Search runs a code search (code_search).
func (s *Service) Search(ctx context.Context, auth asset.AuthContext, id uuid.UUID, req cgprovider.CodeSearchRequest) ([]cgprovider.CodeHit, error) {
	g, err := s.loadActiveGraph(ctx, auth, id)
	if err != nil {
		return nil, err
	}
	return s.provider.Search(ctx, g.GraphRef, req)
}

// Explore runs the combined query (code_explore).
func (s *Service) Explore(ctx context.Context, auth asset.AuthContext, id uuid.UUID, req cgprovider.ExploreRequest) (cgprovider.ExploreResult, error) {
	g, err := s.loadActiveGraph(ctx, auth, id)
	if err != nil {
		return cgprovider.ExploreResult{}, err
	}
	res, err := s.provider.Explore(ctx, g.GraphRef, req)
	if err != nil {
		return cgprovider.ExploreResult{}, err
	}
	// §4.2 / §3.2: every result carries the graph's commit. The provider fills
	// it, but we defend: if a hit is missing its commit, stamp the active graph's
	// commit so an expired revision never masquerades as current.
	if res.Commit == "" {
		res.Commit = g.Commit
	}
	return res, nil
}

// Node resolves one symbol (code_node).
func (s *Service) Node(ctx context.Context, auth asset.AuthContext, id uuid.UUID, req cgprovider.NodeRequest) (cgprovider.CodeNode, error) {
	g, err := s.loadActiveGraph(ctx, auth, id)
	if err != nil {
		return cgprovider.CodeNode{}, err
	}
	return s.provider.Node(ctx, g.GraphRef, req)
}

// Callers returns the incoming call edges (code_callers).
func (s *Service) Callers(ctx context.Context, auth asset.AuthContext, id uuid.UUID, req cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	g, err := s.loadActiveGraph(ctx, auth, id)
	if err != nil {
		return nil, err
	}
	return s.provider.Callers(ctx, g.GraphRef, req)
}

// Callees returns the outgoing call edges (code_callees).
func (s *Service) Callees(ctx context.Context, auth asset.AuthContext, id uuid.UUID, req cgprovider.NodeRequest) ([]cgprovider.CodeEdge, error) {
	g, err := s.loadActiveGraph(ctx, auth, id)
	if err != nil {
		return nil, err
	}
	return s.provider.Callees(ctx, g.GraphRef, req)
}

// Impact computes the change-impact set (code_impact).
func (s *Service) Impact(ctx context.Context, auth asset.AuthContext, id uuid.UUID, req cgprovider.ImpactRequest) ([]cgprovider.CodeHit, error) {
	g, err := s.loadActiveGraph(ctx, auth, id)
	if err != nil {
		return nil, err
	}
	return s.provider.Impact(ctx, g.GraphRef, req)
}

// Capabilities exposes the provider's advertised surface (for the MCP tools'
// "only expose contract-tested capabilities" gate, §6.2). It does not touch a
// codebase — no RBAC needed.
func (s *Service) Capabilities(ctx context.Context) (cgprovider.CodeGraphCapabilities, error) {
	return s.provider.Capabilities(ctx)
}
