#!/usr/bin/env bash
# third-party/check.sh — the third-party governance gate (design-docs/13 §6.4, D8).
#
# Fail-closed: exits non-zero on ANY violation so CI can block publish. What it checks:
#   1. lock.json is valid JSON and structurally well-formed (required keys present).
#   2. Drift: every Go component's digest matches go.sum; every npm component's
#      digest matches web/package-lock.json. A mismatch = a dep was bumped without
#      re-running sync-lock.sh (and thus without an ADR review) → FAIL.
#   3. License policy: every runtime component's license is in the whitelist and
#      NOT in the denylist. Denylist (copyleft / no-commercial / commons-clause) = FAIL.
#   4. NOTICE presence: every component with a non-null notice_path must have that
#      file committed under third-party/NOTICES/.
#   5. Reference baselines: CodeGraph must stay "TBD" (no runtime dep may silently
#      appear); Agent Skills spec must be "accepted" with a NOTICE file.
#
# Requires: jq, and the repo's go.sum / web/package-lock.json. Does NOT require go,
# node, or syft — those are for `make sbom`, not for this gate.
set -uo pipefail

ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
cd "$ROOT"

LOCK="third-party/lock.json"
GOSUM="go.sum"
NPMLOCK="web/package-lock.json"
PASS=0
FAIL=0

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
yel()   { printf '\033[33m%s\033[0m\n' "$*"; }
plain() { printf '%s\n' "$*"; }

ok()   { green "  ✓ $*"; PASS=$((PASS+1)); }
warn() { yel "  ! $*"; }
fail() { red "  ✗ $*"; FAIL=$((FAIL+1)); }

if ! command -v jq >/dev/null 2>&1; then
  red "third-party-check: jq is required"
  exit 2
fi

for f in "$LOCK" "$GOSUM" "$NPMLOCK"; do
  if [ ! -f "$f" ]; then
    red "third-party-check: missing $f"
    exit 2
  fi
done

plain "third-party-check: validating $LOCK"

# ---- 1. Structural validation -------------------------------------------------
if ! jq -e '
  (.components | type == "array") and
  (.license_policy.whitelist | type == "array") and
  (.license_policy.denylist | type == "array") and
  (.components | length > 0) and
  all(.components[]; has("name") and has("ecosystem") and has("digest") and has("license"))
' "$LOCK" >/dev/null 2>&1; then
  red "third-party-check: $LOCK is not structurally valid (missing required keys / empty components)"
  exit 1
fi
ok "lock.json structure: $(jq '.components|length' "$LOCK") components"

# Load policy arrays.
WHITELIST=$(jq -c '.license_policy.whitelist' "$LOCK")
DENYLIST=$(jq -c '.license_policy.denylist' "$LOCK")

# ---- 2. Drift: Go digests vs go.sum ------------------------------------------
NCOMP=$(jq '.components|length' "$LOCK")
i=0
while [ "$i" -lt "$NCOMP" ]; do
  eco=$(jq -r ".components[$i].ecosystem" "$LOCK")
  name=$(jq -r ".components[$i].name" "$LOCK")
  ver=$(jq -r ".components[$i].version" "$LOCK")
  digest=$(jq -r ".components[$i].digest" "$LOCK")
  lic=$(jq -r ".components[$i].license" "$LOCK")
  notice=$(jq -r ".components[$i].notice_path // empty" "$LOCK")

  case "$eco" in
    go)
      actual=$(grep "^${name} ${ver} h1:" "$GOSUM" 2>/dev/null | awk '{print $3}' | head -1)
      if [ -z "$actual" ]; then
        fail "go $name $ver: not found in $GOSUM (was it removed from go.mod?)"
      elif [ "$actual" != "$digest" ]; then
        fail "go $name $ver: digest drift (lock=$digest, go.sum=$actual) — run third-party/sync-lock.sh"
      else
        ok "go $name $ver: digest matches go.sum"
      fi
      ;;
    npm)
      actual=$(jq -r --arg n "$name" '.packages["node_modules/"+$n].integrity // empty' "$NPMLOCK" 2>/dev/null)
      if [ -z "$actual" ]; then
        fail "npm $name: not found in $NPMLOCK (was it removed from package.json?)"
      elif [ "$actual" != "$digest" ]; then
        fail "npm $name $ver: digest drift (lock=$digest, package-lock=$actual) — run third-party/sync-lock.sh"
      else
        ok "npm $name $ver: digest matches package-lock.json"
      fi
      ;;
    reference)
      # Reference baselines are not validated against a runtime digest source.
      # CodeGraph selection was completed in Phase 3 (ADR-0001 accepted); the
      # baseline is locked to a pinned commit. Agent Skills spec must be accepted.
      status=$(jq -r ".components[$i].status // empty" "$LOCK")
      adr=$(jq -r ".components[$i].adr // empty" "$LOCK")
      case "$name" in
        codegraph|CodeGraph)
          # Selection done: status must reflect the accepted baseline, and ADR-0001
          # must be marked accepted on disk. Digest is the npm tarball shasum,
          # filled when the sidecar lands — warn (not fail) until then, but FAIL
          # if someone regressed the status or the ADR.
          if [ "$status" != "selected-baseline-phase3" ]; then
            fail "codegraph: status regressed to '$status' (expected selected-baseline-phase3) — was ADR-0001 reverted?"
          elif [ "$digest" = "TBD" ] || [[ "$digest" == TBD-* ]]; then
            warn "codegraph: digest still TBD ($digest) — ADR-0001 accepted, fill npm tarball dist.shasum at sidecar landing"
          else
            ok "reference codegraph: baseline locked (v$ver, commit pinned, ADR-0001 accepted)"
          fi
          ;;
        "Agent Skills spec")
          if [ "$status" = "accepted" ]; then
            ok "reference Agent Skills spec: accepted (v$ver)"
          else
            fail "reference Agent Skills spec: status not accepted ($status)"
          fi
          ;;
        *)
          ok "reference $name: $ver"
          ;;
      esac
      ;;
  esac

  # ---- 3. License policy (runtime components only) ---------------------------
  # Denylist uses shell glob so CC-BY-NC-* etc. work without jq gymnastics.
  # The whitelist lookup uses --null-input (-n): without it jq-1.7 (ubuntu-latest
  # CI runners) ignores the --argjson $wl binding and runs the filter against
  # empty stdin → no output → `jq -e` exits 4 → every license spuriously "not in
  # whitelist". jq-1.6 worked without -n (passed locally, failed in CI). -n makes
  # $wl bind as the input on both versions.
  if [ "$eco" = "go" ] || [ "$eco" = "npm" ]; then
    case "$lic" in
      AGPL-*|GPL-2.*|GPL-3.*|LGPL-*|SSPL|BUSL-*|CC-BY-NC-*|commons-clause)
        fail "$eco $name: DENIED license '$lic' (matches denylist)"
        ;;
      *)
        if ! jq -en --arg lic "$lic" --argjson wl "$WHITELIST" '$wl | index($lic)' >/dev/null 2>&1; then
          fail "$eco $name: license '$lic' not in whitelist"
        else
          ok "$eco $name: license '$lic' allowed"
        fi
        ;;
    esac
  fi

  # ---- 4. NOTICE presence ----------------------------------------------------
  if [ -n "$notice" ]; then
    if [ -f "$notice" ]; then
      ok "$eco $name: NOTICE present ($notice)"
    else
      fail "$eco $name: notice_path '$notice' missing on disk"
    fi
  fi

  i=$((i+1))
done

# ---- 5. Undeclared runtime deps drift (lock missing entries present in source) ----
plain "third-party-check: scanning for undeclared deps (in source, missing from lock)"
# Go: every direct require in go.mod must have a lock entry.
declare -a undeclared_go=()
while read -r mod ver; do
  [ -z "${mod:-}" ] && continue
  if ! jq -e --arg n "$mod" '[.components[]|select(.ecosystem=="go")|.name] | index($n)' "$LOCK" >/dev/null 2>&1; then
    undeclared_go+=("$mod")
  fi
done < <(awk '/^require \(/,/^)/' go.mod | grep -vE 'indirect|^require \(|^\)' | sed 's/^[[:space:]]*//' | awk 'NF>=2 {print $1, $2}')
for m in "${undeclared_go[@]:-}"; do
  [ -z "$m" ] && continue
  fail "go $m: in go.mod but missing from lock.json — run third-party/sync-lock.sh"
done
if [ "${#undeclared_go[@]}" -eq 0 ]; then ok "all go.mod direct deps present in lock"; fi

# npm: every direct dep in package.json must have a lock entry.
declare -a undeclared_npm=()
while read -r name; do
  [ -z "${name:-}" ] && continue
  if ! jq -e --arg n "$name" '[.components[]|select(.ecosystem=="npm")|.name] | index($n)' "$LOCK" >/dev/null 2>&1; then
    undeclared_npm+=("$name")
  fi
done < <(jq -r '((.dependencies // {}) + (.devDependencies // {})) | keys[]' web/package.json)
for m in "${undeclared_npm[@]:-}"; do
  [ -z "$m" ] && continue
  fail "npm $m: in package.json but missing from lock.json — run third-party/sync-lock.sh"
done
if [ "${#undeclared_npm[@]}" -eq 0 ]; then ok "all package.json direct deps present in lock"; fi

# ---- Summary ------------------------------------------------------------------
plain ""
plain "third-party-check: PASS=$PASS FAIL=$FAIL"
if [ "$FAIL" -gt 0 ]; then
  red "third-party-check: FAILED ($FAIL violation(s)) — publish blocked"
  exit 1
fi
green "third-party-check: passed — no drift, licenses compliant, NOTICES present"
exit 0
