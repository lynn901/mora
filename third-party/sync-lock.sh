#!/usr/bin/env bash
# third-party/sync-lock.sh — regenerate third-party/lock.json from go.sum + web/package-lock.json.
#
# The lock.json is the immutable third-party baseline (design-docs/13 §6.1, D8).
# It pins every direct runtime/dev dependency to its module/package digest so that
# `make third-party-check` can detect drift (a changed digest = a bumped dep that
# has not been ADR-reviewed).
#
# Why a generator, not hand-maintenance: digests must match go.sum / package-lock.json
# byte-for-byte. Transcribing them by hand invites drift on every bump. This script
# reads them straight from the source files, so lock.json is always a faithful mirror.
# Licenses for Go modules (go.sum carries no license metadata) are curated in
# licenses-override.json; npm licenses are authoritative from package-lock.json.
#
# Idempotent: identical inputs → identical output. Requires jq.
# Re-run after bumping any direct dep, review the diff, and commit the result.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$ROOT"

LOCK="$ROOT/third-party/lock.json"
OVR="$ROOT/third-party/licenses-override.json"

if ! command -v jq >/dev/null 2>&1; then
  echo "third-party/sync-lock.sh: jq is required (install jq or run inside the gate image)" >&2
  exit 1
fi

# Validate source files exist.
for f in go.sum go.mod web/package-lock.json web/package.json third-party/licenses-override.json; do
  if [ ! -f "$ROOT/$f" ]; then
    echo "third-party/sync-lock.sh: missing $f" >&2
    exit 1
  fi
done

# Build a newline-delimited stream of component JSON objects (one per line),
# then assemble with jq. Using NDJSON avoids shell state across subshells
# (the bug where `,` separators get dropped at pipeline boundaries).

COMPONENTS="$(mktemp)"
trap 'rm -f "$COMPONENTS"' EXIT
: > "$COMPONENTS"

# ---- Go direct deps: digest = go.sum h1 hash (module zip SHA-256). ----
awk '/^require \(/,/^)/' "$ROOT/go.mod" \
  | grep -vE 'indirect|^require \(|^\)' \
  | sed 's/^[[:space:]]*//' \
  | awk 'NF>=2 {print $1, $2}' \
  | while read -r mod ver; do
      [ -z "${mod:-}" ] && continue
      digest=$(grep "^${mod} ${ver} h1:" "$ROOT/go.sum" 2>/dev/null | awk '{print $3}' || true)
      if [ -z "${digest:-}" ]; then
        echo "third-party/sync-lock.sh: no go.sum h1 digest for ${mod} ${ver}" >&2
        exit 1
      fi
      license=$(jq -r --arg m "$mod" '.[$m] // "UNKNOWN"' "$OVR")
      source_url="https://$(printf '%s' "$mod" | awk -F/ '{print $1"/"$2"/"$3}')"
      jq -nc \
        --arg name "$mod" --arg version "$ver" --arg digest "$digest" \
        --arg license "$license" --arg url "$source_url" \
        '{ecosystem:"go", name:$name, version:$version, digest:$digest, digest_type:"go-h1", source_url:$url, license:$license, notice_path:null, capability:"backend-runtime"}'
    done >> "$COMPONENTS"

# ---- npm direct deps: digest = package-lock.json integrity (sha512-...). ----
( cd "$ROOT/web" && jq -r --slurpfile lock package-lock.json '
    ((.dependencies // {}) + (.devDependencies // {})) as $direct
    | $direct | keys[] as $name
    | ($lock[0].packages["node_modules/" + $name] // {}) as $p
    | select($p.version != null)
    | [$name, $p.version, ($p.integrity // ""), ($p.license // "UNKNOWN")]
    | @tsv
  ' package.json
) | while IFS=$'\t' read -r name ver integrity license; do
      [ -z "${license:-}" ] && license="UNKNOWN"
      jq -nc \
        --arg name "$name" --arg version "$ver" --arg digest "$integrity" \
        --arg license "$license" \
        '{ecosystem:"npm", name:$name, version:$version, digest:$digest, digest_type:"npm-integrity", source_url:("https://www.npmjs.com/package/"+$name), license:$license, notice_path:null, capability:"frontend-runtime"}'
    done >> "$COMPONENTS"

# ---- Reference baselines (spec / selection framework, non-runtime). ----
# CodeGraph: ADR-0001 accepted (2026-08-15), baseline locked to
# @colbymchenry/codegraph v1.5.0, commit c6aaa20358cd6adcd04b87bdef8e5803ad146f3a
# (MIT, PGP-verified). The npm tarball dist.shasum is a supplemental SBOM-traceability
# field filled at sidecar landing; until then digest stays TBD (warn, not fail) per
# check.sh. The status must read selected-baseline-phase3 — regressing it fails the gate.
echo '{"ecosystem":"reference","name":"CodeGraph","version":"1.5.0","digest":"TBD","digest_type":"tbd","source_url":"https://github.com/colbymchenry/codegraph","license":"MIT","notice_path":"third-party/NOTICES/CodeGraph.NOTICE","capability":"code-symbol/call/impact-query","adr":"third-party/adr/0001-codegraph-selection.md","status":"selected-baseline-phase3"}' >> "$COMPONENTS"
echo '{"ecosystem":"reference","name":"Agent Skills spec","version":"1.0","digest":"spec-v1.0","digest_type":"spec-version","source_url":"https://agentskills.io/","license":"spec-declared","notice_path":"third-party/NOTICES/agent-skills-spec.NOTICE","capability":"skill-package-format-profile","adr":"third-party/adr/0002-skill-spec-baseline.md","status":"accepted"}' >> "$COMPONENTS"

if [ ! -s "$COMPONENTS" ]; then
  echo "third-party/sync-lock.sh: no components collected (source files empty?)" >&2
  exit 1
fi

# Assemble the final document. `-n` (null input) so jq does not block on stdin;
# `--slurpfile` reads the NDJSON stream into `$comps` as an array of objects.
jq -nS --tab --slurpfile comps "$COMPONENTS" '{
  "$schema": "https://mora.local/schemas/third-party-lock.json",
  version: 1,
  generated_from: {
    go_sum: "go.sum",
    npm_lock: "web/package-lock.json",
    note: "Regenerate with third-party/sync-lock.sh after any direct-dependency bump."
  },
  license_policy: {
    whitelist: ["Apache-2.0","MIT","ISC","BSD-2-Clause","BSD-3-Clause","MPL-2.0","PostgreSQL"],
    review_required: ["MPL-2.0"],
    denylist: ["AGPL-3.0","AGPL-3.0-only","AGPL-3.0-or-later","GPL-2.0","GPL-2.0-only","GPL-2.0-or-later","GPL-3.0","GPL-3.0-only","GPL-3.0-or-later","LGPL-2.1","LGPL-3.0","SSPL","BUSL-1.1","CC-BY-NC-*","commons-clause"]
  },
  components: ($comps | sort_by(.ecosystem, .name))
}' > "$LOCK"

echo "wrote $LOCK"
echo "  go:         $(jq '[.components[]|select(.ecosystem=="go")]|length' "$LOCK")"
echo "  npm:        $(jq '[.components[]|select(.ecosystem=="npm")]|length' "$LOCK")"
echo "  reference:  $(jq '[.components[]|select(.ecosystem=="reference")]|length' "$LOCK")"
echo "  total:      $(jq '.components|length' "$LOCK")"
echo "verify with: make third-party-check"
