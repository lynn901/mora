package postgres

// codegraph_projection_repo.go implements codegraph.ProjectionRepo — the
// read-side lookup that resolves a codebase asset's ACTIVE codegraph projection
// (design-docs/17 §4.2 step 1–2). The query service calls ActiveCodeGraph to
// fetch the ready projection's locator (graph_ref / source_tree_ref / commit /
// source_tree_hash / provider_*) bound to the asset's current_version_id, then
// runs the query-time validation + provider call.
//
// A codebase with no ready codegraph projection returns codegraph.ErrGraphNotReady
// so the service surfaces the build state without leaking asset internals (the
// caller already passed resource-level RBAC at resolveAsset — they see "not
// ready", never a 404 for the asset itself). All SQL is parameterized
// (07-security §10).

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	cgservice "github.com/lynn901/mora/internal/module/knowledge/codegraph/service"
)

// CodeGraphProjectionRepo is the postgres implementation of
// codegraph.ProjectionRepo.
type CodeGraphProjectionRepo struct{ db *DB }

// NewCodeGraphProjectionRepo builds a codegraph.ProjectionRepo over the mora
// database.
func NewCodeGraphProjectionRepo(db *DB) *CodeGraphProjectionRepo {
	return &CodeGraphProjectionRepo{db: db}
}

// Compile-time check.
var _ cgservice.ProjectionRepo = (*CodeGraphProjectionRepo)(nil)

// ActiveCodeGraph resolves the ready codegraph projection bound to the codebase
// asset's current_version_id (§4.2). It joins knowledge_assets →
// knowledge_asset_versions → asset_projections for projection_kind='codegraph'
// AND status='ready'. A missing asset / missing current version / no ready
// codegraph projection → codegraph.ErrGraphNotReady (the caller already passed
// asset RBAC; surfacing "not ready" leaks nothing, §8.2).
//
// The locator JSONB carries graph_ref / source_tree_ref / commit_sha /
// source_tree_hash / provider_version / provider_build_digest /
// index_schema_version / extraction_version (written by
// codegraphLocator in the build handler, §11).
func (r *CodeGraphProjectionRepo) ActiveCodeGraph(ctx context.Context, assetID uuid.UUID) (cgservice.ActiveGraph, error) {
	var (
		locatorBytes   []byte
		versionID      uuid.UUID
		currentVersion uuid.UUID
		providerName   string
	)
	// Resolve the asset's current_version_id, then its ready codegraph
	// projection. A single joined query avoids a TOCTOU window between reading
	// current_version_id and reading the projection (§4.2 — the binding must be
	// read atomically). LEFT JOIN asset_projections so a codebase with no ready
	// projection yet surfaces as ErrGraphNotReady rather than ErrNoRows-into-error.
	row := r.db.Pool.QueryRow(ctx, `
		SELECT a.current_version_id,
		       v.id,
		       p.provider,
		       p.locator
		  FROM knowledge_assets a
		  JOIN knowledge_asset_versions v ON v.id = a.current_version_id
		  LEFT JOIN asset_projections p
		    ON p.asset_version_id = v.id
		   AND p.projection_kind = 'codegraph'
		   AND p.status = 'ready'
		 WHERE a.id = $1
		 ORDER BY p.built_at DESC NULLS LAST
		 LIMIT 1`, assetID)
	if err := row.Scan(&currentVersion, &versionID, &providerName, &locatorBytes); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return cgservice.ActiveGraph{}, cgservice.ErrGraphNotReady
		}
		return cgservice.ActiveGraph{}, err
	}
	// current_version_id NULL → no active version yet (a brand-new codebase).
	if currentVersion == uuid.Nil {
		return cgservice.ActiveGraph{}, cgservice.ErrGraphNotReady
	}
	// No ready codegraph projection row (LEFT JOIN yielded NULL locator).
	if len(locatorBytes) == 0 {
		return cgservice.ActiveGraph{}, cgservice.ErrGraphNotReady
	}
	var loc struct {
		GraphRef            string `json:"graph_ref"`
		SourceTreeRef       string `json:"source_tree_ref"`
		Commit              string `json:"commit_sha"`
		SourceTreeHash      string `json:"source_tree_hash"`
		ProviderVersion     string `json:"provider_version"`
		ProviderBuildDigest string `json:"provider_build_digest"`
		IndexSchemaVersion  string `json:"index_schema_version"`
		ExtractionVersion   string `json:"extraction_version"`
	}
	if err := json.Unmarshal(locatorBytes, &loc); err != nil {
		// A malformed locator is treated as not-ready — never serve a graph we
		// cannot validate (§4.2 fail closed).
		return cgservice.ActiveGraph{}, cgservice.ErrGraphNotReady
	}
	if loc.GraphRef == "" {
		return cgservice.ActiveGraph{}, cgservice.ErrGraphNotReady
	}
	return cgservice.ActiveGraph{
		AssetID:          assetID,
		AssetVersionID:   versionID,
		CurrentVersionID: currentVersion,
		GraphRef:         loc.GraphRef,
		SourceTreeRef:    loc.SourceTreeRef,
		Commit:           loc.Commit,
		SourceTreeHash:   loc.SourceTreeHash,
		ProviderVersion:  loc.ProviderVersion,
		ProviderName:     providerName,
	}, nil
}
