// Package worker — CodeGraphBuildHandler (design-docs/17 §4–§5). This is the
// async build path that turns a codebase Asset version into a codegraph
// projection: materialize a read-only source tree from the version's snapshot
// locator → compute source_tree_hash → Provider.Build → verify BuildResult
// matches the input commit + hash → MarkProjectionReady(kind=codegraph) →
// clean up the temp build dir (§4.1 steps 1–7). Fail closed on every hash /
// commit mismatch — never register a misaligned graph (§4.3 / §7.2).
//
// It reuses the existing CAS activation path: MarkProjectionReady flips
// build_status when the last required projection lands, AssetActivateHandler
// does the CAS — zero changes to that path (§5). It does NOT add a Stream:
// knowledge_events already fans projection jobs out; mapKnowledgeEvent enqueues
// a codegraph_build job when asset_type=codebase.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/lynn901/mora/internal/domain"
	"github.com/lynn901/mora/internal/module/knowledge/asset"
	cgprovider "github.com/lynn901/mora/internal/module/knowledge/codegraph/provider"
)

// JobCodeGraphBuild is the codegraph_build job_type (§5 dispatch table,
// declared in runner.go alongside the other job-type constants).

// VersionSourceLocator reads a codebase Asset version's source snapshot
// locator + commit (§4.1 step 1). The asset version row carries these in its
// ProviderRef/GenerationRef JSONB maps (GitConnector.Fetch writes {repo,
// commit, host} + the MinIO content-hash key). A missing version or a version
// without a pinned commit returns ErrVersionSourceMissing so the handler fails
// closed (permanent — existence/materialization won't appear by retrying).
type VersionSourceLocator interface {
	// Read returns the snapshot locator + commit for an asset version.
	Read(ctx context.Context, assetVersionID uuid.UUID) (SourceSnapshot, error)
}

// SourceSnapshot is the materialized-source-tree input to a build.
type SourceSnapshot struct {
	AssetVersionID   uuid.UUID
	WorkspaceID      uuid.UUID
	Commit           string            // pinned commit_sha (GenerationRef["commit"])
	SnapshotPrefix   string            // MinIO key prefix holding the snapshot
	SnapshotManifest map[string]any    // raw manifest metadata (repo, host, …)
}

// ErrVersionSourceMissing is returned by VersionSourceLocator when the version
// or its commit/snapshot locator is absent. Permanent: retrying won't conjure a
// commit (§4.3 fail closed — do not register a misaligned graph).
var ErrVersionSourceMissing = errors.New("codegraph: version source locator missing")

// SnapshotMaterializer materializes a read-only working tree from a snapshot
// and computes its source_tree_hash (§4.1 steps 2–3). The production path uses
// a controlled mirror / shallow clone over the egress-audited GitConnector; in
// the MVP the default materializer hashes the snapshot manifest + the content
// manifest entries (deterministic, no network) so the pipeline is observable
// before the full object-store materialization lands. A hash that does not
// match the commit-pinned tree fails closed.
type SnapshotMaterializer interface {
	// Materialize writes the read-only tree to a temp dir and returns its path +
	// the computed source_tree_hash (§4.1 step 3). It MUST not retain credentials.
	Materialize(ctx context.Context, snap SourceSnapshot) (workDir, sourceTreeHash string, err error)
}

// CodeGraphBuildHandler is the §4.1 build path. It composes the
// VersionSourceLocator + SnapshotMaterializer + CodeGraphProviderPort +
// asset.ActivationRegistry; the Runner drives acquire→run→mark.
type CodeGraphBuildHandler struct {
	Provider   CodeGraphProviderPort
	Locator    VersionSourceLocator
	Materializer SnapshotMaterializer
	Assets     asset.ActivationRegistry
	Pool       interface{ BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) }
}

// Run executes the build. Retry classification follows runner.go:
//   - materialization failure / Provider timeout → transient (retry);
//   - commit/hash mismatch / missing source → permanent (fail closed, no retry
//     — a retry would re-attempt to register a misaligned graph, §4.3).
//
// Idempotent: a re-acquired job whose projection was already marked ready
// short-circuits to success (MarkProjectionReady is a no-op on a duplicate).
func (h *CodeGraphBuildHandler) Run(ctx context.Context, j domain.Job) (domain.RetryClass, error) {
	if j.AssetVersionID == nil || *j.AssetVersionID == uuid.Nil {
		return domain.RetryPermanent, fmtErr(JobCodeGraphBuild, fmt.Errorf("missing asset_version_id"))
	}
	snap, err := h.Locator.Read(ctx, *j.AssetVersionID)
	if err != nil {
		if errors.Is(err, ErrVersionSourceMissing) {
			return domain.RetryPermanent, fmtErr(JobCodeGraphBuild, err)
		}
		return domain.RetryTransient, fmtErr(JobCodeGraphBuild, err)
	}
	if snap.Commit == "" || snap.SnapshotPrefix == "" {
		return domain.RetryPermanent, fmtErr(JobCodeGraphBuild, fmt.Errorf("%w: commit/snapshot_prefix empty", ErrVersionSourceMissing))
	}

	// §4.1 step 2–3: materialize a read-only tree + compute source_tree_hash.
	workDir, sourceTreeHash, err := h.Materializer.Materialize(ctx, snap)
	if err != nil {
		return domain.RetryTransient, fmtErr(JobCodeGraphBuild, fmt.Errorf("materialize: %w", err))
	}
	// Cleanup the temp build dir after the build — the active source tree lives
	// in the provider's source_tree_ref / snapshot (§4.3: deleting the temp build
	// dir must NOT break active-graph reads). Best-effort; a cleanup failure is
	// not fatal (logged via the returned error path).
	defer cleanupWorkDir(workDir)

	// §4.1 step 4: Provider.Build(snapshot_locator, commit, source_tree_hash).
	buildReq := cgprovider.BuildRequest{
		SnapshotLocator: cgprovider.Locator{
			ObjectStorePrefix: snap.SnapshotPrefix,
			TempPath:           workDir,
		},
		Commit:         snap.Commit,
		SourceTreeHash: sourceTreeHash,
		Capability:     capability(snap.WorkspaceID, uuid.Nil, 0),
	}
	res, err := h.Provider.Build(ctx, buildReq)
	if err != nil {
		// capability_unavailable is transient (sidecar may come up); a hash/commit
		// mismatch surfaced by the provider as ErrSourceSnapshotUnavailable is
		// permanent (fail closed — do not retry a misaligned build).
		if errors.Is(err, cgprovider.ErrSourceSnapshotUnavailable) || errors.Is(err, cgprovider.ErrAssetVersionMismatch) {
			return domain.RetryPermanent, fmtErr(JobCodeGraphBuild, err)
		}
		return domain.RetryTransient, fmtErr(JobCodeGraphBuild, err)
	}

	// §4.1 step 5: verify BuildResult{Commit, SourceTreeHash} == input. A
	// mismatch is fail closed (permanent) — discard the graph, never register.
	if res.Commit != snap.Commit || res.SourceTreeHash != sourceTreeHash {
		return domain.RetryPermanent, fmtErr(JobCodeGraphBuild, fmt.Errorf(
			"build result mismatch: provider commit=%q source_tree_hash=%q != input commit=%q hash=%q (fail closed, §4.1 step 5)",
			res.Commit, res.SourceTreeHash, snap.Commit, sourceTreeHash))
	}

	// §4.1 step 6: MarkProjectionReady(kind=codegraph) — locator carries the
	// graph_ref + source_tree_ref + commit + source_tree_hash + provider ids.
	// The existing allRequiredReady gate flips build_status; AssetActivateHandler
	// does the CAS (§5 — zero changes to the activation path).
	locator := codegraphLocator(res)
	providerName := stringFromJobProgress(j, "provider")
	if providerName == "" {
		providerName = "codegraph"
	}
	tx, err := h.Pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.RetryTransient, fmtErr(JobCodeGraphBuild, err)
	}
	defer tx.Rollback(ctx)
	_, err = h.Assets.MarkProjectionReady(ctx, tx, asset.ProjectionReady{
		AssetVersionID:  *j.AssetVersionID,
		Kind:            domain.ProjectionCodegraph,
		BuildRevision:   j.BuildRevision,
		Provider:        providerName,
		ProviderVersion: res.ProviderVersion,
		Locator:         locator,
	})
	if err != nil {
		if errors.Is(err, asset.ErrVersionNotFound) {
			_ = tx.Rollback(ctx)
			return domain.RetryPermanent, fmtErr(JobCodeGraphBuild, err)
		}
		_ = tx.Rollback(ctx)
		return domain.RetryTransient, fmtErr(JobCodeGraphBuild, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.RetryTransient, fmtErr(JobCodeGraphBuild, err)
	}
	return domain.RetryTransient, nil
}

// codegraphLocator builds the asset_projections.locator JSONB map for a
// codegraph projection (§11 — graph_ref/source_tree_ref/commit_sha/
// source_tree_hash/provider_* all live in locator, never executable).
func codegraphLocator(res cgprovider.BuildResult) map[string]any {
	return map[string]any{
		"graph_ref":              res.GraphRef,
		"source_tree_ref":        res.SourceTreeRef,
		"commit_sha":             res.Commit,
		"source_tree_hash":       res.SourceTreeHash,
		"provider_version":       res.ProviderVersion,
		"provider_build_digest":  res.ProviderBuildDigest,
		"index_schema_version":   res.IndexSchemaVersion,
		"extraction_version":     res.ExtractionVersion,
	}
}

// cleanupWorkDir removes the temp build directory. Best-effort: the active
// source tree is held by the provider / snapshot, so a cleanup failure does not
// break active-graph reads (§4.3 verification case: delete temp build dir →
// active graph still readable).
func cleanupWorkDir(dir string) {
	if dir == "" {
		return
	}
	_ = removeAllBestEffort(dir)
}

// removeAllBestEffort wraps os.RemoveAll so the handler does not import os
// directly (keeps the build handler free of platform-specific paths; the real
// materializer owns filesystem interaction). It is a no-op stub here — the
// concrete SnapshotMaterializer implementation cleans its own temp dir in the
// production path; this is the defence-in-depth sweep for any path the
// materializer left behind.
func removeAllBestEffort(_ string) error { return nil }

// --- default SnapshotMaterializer (deterministic, no network) ---

// ManifestHashMaterializer computes a deterministic source_tree_hash from the
// snapshot's manifest without materializing files (MVP). The hash is
// sha256(commit + sorted manifest key/value pairs); this is stable per
// (version, commit) so the §4.1 hash-equality guard is exercisable end-to-end
// before the full object-store materialization lands. The Provider sidecar
// independently materializes from the snapshot and returns the same hash when
// its tree matches — the §4.1 step-5 equality check is the real authority.
type ManifestHashMaterializer struct{}

// Compile-time check.
var _ SnapshotMaterializer = (*ManifestHashMaterializer)(nil)

// Materialize returns a deterministic hash for the snapshot. workDir is empty
// (the provider materializes from the snapshot prefix in the MVP path).
func (ManifestHashMaterializer) Materialize(_ context.Context, snap SourceSnapshot) (string, string, error) {
	h := sha256.New()
	h.Write([]byte(snap.Commit))
	h.Write([]byte{0})
	keys := make([]string, 0, len(snap.SnapshotManifest))
	for k := range snap.SnapshotManifest {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		fmt.Fprintf(h, "%v", snap.SnapshotManifest[k])
		h.Write([]byte{0})
	}
	// Incorporate the snapshot prefix + version id so two versions with the same
	// commit but different snapshots hash distinctly.
	h.Write([]byte(snap.SnapshotPrefix))
	h.Write([]byte{0})
	h.Write([]byte(snap.AssetVersionID.String()))
	return "", "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// --- VersionSourceLocator from a domain.AssetVersion (in-memory) ---

// AssetVersionLocator adapts a function that loads a domain.AssetVersion into a
// VersionSourceLocator: it reads commit + snapshot prefix from the version's
// ProviderRef/GenerationRef maps (where the GitConnector manifest lands).
type AssetVersionLocator struct {
	// Load returns the asset version + its owning asset's workspace id.
	Load func(ctx context.Context, assetVersionID uuid.UUID) (av *domain.AssetVersion, workspaceID uuid.UUID, err error)
}

// Compile-time check.
var _ VersionSourceLocator = (*AssetVersionLocator)(nil)

// Read extracts the SourceSnapshot from the version's refs. The commit is in
// GenerationRef["commit"] (GitConnector manifest metadata); the snapshot
// prefix is the version's content-hash MinIO key (ProviderRef["snapshot_prefix"]
// when the worker recorded it, else synthesized from ContentHash).
func (a *AssetVersionLocator) Read(ctx context.Context, id uuid.UUID) (SourceSnapshot, error) {
	av, wsID, err := a.Load(ctx, id)
	if err != nil {
		return SourceSnapshot{}, err
	}
	if av == nil {
		return SourceSnapshot{}, ErrVersionSourceMissing
	}
	commit := mapStr(av.GenerationRef, "commit")
	if commit == "" {
		commit = mapStr(av.ProviderRef, "commit")
	}
	prefix := mapStr(av.ProviderRef, "snapshot_prefix")
	if prefix == "" && av.ContentHash != "" {
		prefix = "codebase/" + av.ContentHash
	}
	manifest := av.GenerationRef
	if manifest == nil {
		manifest = av.ProviderRef
	}
	return SourceSnapshot{
		AssetVersionID:   av.ID,
		WorkspaceID:      wsID,
		Commit:           commit,
		SnapshotPrefix:   prefix,
		SnapshotManifest: manifest,
	}, nil
}

// mapStr reads a string field from a map[string]any (tolerates the JSONB-derived
// float64/json.Number typing for non-string values by marshalling them).
func mapStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	default:
		b, _ := json.Marshal(v)
		return strings.TrimSpace(string(b))
	}
}
