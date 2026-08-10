# Third-Party Governance Gate

Implements the third-party governance gate for Mora — `design-docs/13-phase0-contract-safety-baseline.md` §6 (decision D8).

The gate ensures **no published build ships with drifted, unreviewed, or license-incompatible third-party components**. Every direct runtime/dev dependency is pinned to an immutable digest in `lock.json`; the gate validates that pin against the live `go.sum` / `web/package-lock.json` on every PR and publish.

## Layout

```
third-party/
  lock.json              # immutable baseline: every direct dep's name/version/digest/license
  licenses-override.json # SPDX licenses for Go direct deps (go.sum carries none)
  check.sh               # the gate — run by `make third-party-check`
  sync-lock.sh           # regenerates lock.json from go.sum + package-lock.json
  generate-notices.sh    # aggregates THIRD_PARTY_NOTICES.md — run by `make notices`
  NOTICES/               # per-component NOTICE/LICENSE file copies
    CodeGraph.NOTICE
    agent-skills-spec.NOTICE
  adr/                   # third-party introduction ADRs
    0000-template.md
    0001-codegraph-selection.md   # CodeGraph — Phase 3 selection framework (TBD)
    0002-skill-spec-baseline.md   # Agent Skills spec — accepted reference baseline
```

## Commands (Makefile)

| Command | What it does | Fail-closed? |
|---|---|---|
| `make third-party-check` | Validate lock.json drift vs go.sum/package-lock.json, license whitelist, NOTICE presence | ✅ yes (non-zero = publish blocked) |
| `make sbom` | Generate a CycloneDX SBOM via the pinned `anchore/syft` image (no local install) | ✅ yes |
| `make notices` | Aggregate `THIRD_PARTY_NOTICES.md` from lock.json + NOTICES/ | no (advisory) |
| `make third-party-sync` | **Maintainer only** — regenerate lock.json after a dep bump | no |

## Workflow for adding / bumping a direct dependency

1. Update `go.mod` / `web/package.json` and run `go mod tidy` / `npm install`.
2. Run `make third-party-sync` to regenerate `lock.json` from the new `go.sum` / `package-lock.json`.
3. If the component is **new** (not a version bump of an existing one), write an ADR under `third-party/adr/` using `0000-template.md`, recording source_url / commit_or_digest / license / capability / NOTICE.
4. If the component is a runtime dependency, ensure its license is in the `license_policy.whitelist` (and **not** in the denylist) in `lock.json`. For Go modules, curate the SPDX identifier in `licenses-override.json`.
5. Drop the component's LICENSE/NOTICE text under `third-party/NOTICES/<name>.NOTICE`.
6. Run `make third-party-check sbom notices` — all must pass.
7. Commit `lock.json` + the ADR + the NOTICE file.

## License policy

- **Whitelist** (no review): Apache-2.0, MIT, ISC, BSD-2-Clause, BSD-3-Clause, MPL-2.0, PostgreSQL.
- **Review required** (manual sign-off): MPL-2.0 (file-level copyleft — record in ADR).
- **Denylist** (publish blocked): AGPL-3.0*, GPL-2.0/3.0*, LGPL-*, SSPL, BUSL-1.1, CC-BY-NC-*, commons-clause.

> **Go licenses are manually curated.** `go.sum` carries no license metadata, so the SPDX identifier for each Go direct dependency is hand-set in `licenses-override.json` and copied into `lock.json` by `sync-lock.sh`. After bumping/adding a Go module in `go.mod`, you **must** manually verify the upstream license and update `licenses-override.json` if it changed — the gate does not auto-detect Go licenses. npm licenses come from `package-lock.json` and are automated.

## Reference baselines (non-runtime)

Two reference (spec / selection-framework) baselines are pinned, not runtime code:

- **CodeGraph** (ADR-0001): selection is deferred to Phase 3 (YS-97). Pinned as `TBD` — the gate reports it as "selection pending" and will **fail closed** if a real CodeGraph runtime dependency appears in `go.mod` / `package-lock.json` without a matching, non-TBD lock entry + accepted ADR.
- **Agent Skills spec** (ADR-0002): reference specification for the Skill package format profile, version 1.0, status `accepted`.

## What the gate checks (`check.sh`)

1. `lock.json` is structurally valid (required keys, non-empty components).
2. **Drift**: every Go component's `digest` matches the `h1:` hash in `go.sum`; every npm component's `digest` matches the `integrity` in `web/package-lock.json`. Mismatch = a bump without ADR review.
3. **Undeclared deps**: every direct dep in `go.mod` / `web/package.json` has a lock entry (catches additions that skipped `sync-lock.sh`).
4. **License policy**: every runtime component's license is in the whitelist and not in the denylist.
5. **NOTICE presence**: every component with a `notice_path` has that file on disk.
6. **Reference integrity**: CodeGraph stays `TBD` until Phase 3; Agent Skills spec stays `accepted`.

Any violation → non-zero exit → CI blocks publish (see `.github/workflows/third-party-gate.yml`).
