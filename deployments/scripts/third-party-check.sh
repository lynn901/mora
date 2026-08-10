#!/usr/bin/env bash
# deployments/scripts/third-party-check.sh — Mora 第三方治理门禁（决策书 §6 / D8）
#
# 校验 third-party/lock.json 完整性：
#   1. JSON 合法且符合 lock.schema.json 结构
#   2. 每个组件 required 字段齐（name/source_url/commit_sha_or_digest/license/notice_path/capability）
#   3. digest 非空（发布构建不存在漂移依赖）
#   4. license 通过白名单（allowlist），denylist 与 review_required 走 fail-closed / 警示
#   5. NOTICE 文件存在
#   6. ADR 文件存在（如声明 adr 字段）
#
# 退出码：0 通过；1 违反（fail closed）。
# 不依赖外部网络；纯本地校验。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
LOCK_FILE="$REPO_ROOT/third-party/lock.json"
NOTICES_DIR="$REPO_ROOT/third-party/NOTICES"

fail=0
errors=()

add_err() { errors+=("$1"); fail=1; }

if ! command -v jq >/dev/null 2>&1; then
  echo "✗ third-party-check: jq 未安装（必需）" >&2
  exit 1
fi

if [[ ! -f "$LOCK_FILE" ]]; then
  echo "✗ lock.json 不存在：$LOCK_FILE" >&2
  exit 1
fi

# 1. JSON 合法性
if ! jq empty "$LOCK_FILE" >/dev/null 2>&1; then
  echo "✗ lock.json 不是合法 JSON" >&2
  exit 1
fi

# 2. 顶层结构
project=$(jq -r '.project // empty' "$LOCK_FILE")
project_license=$(jq -r '.project_license // empty' "$LOCK_FILE")
if [[ -z "$project" || -z "$project_license" ]]; then
  add_err "lock.json 缺少 project / project_license 字段"
fi
if [[ "$project" != "mora" ]]; then
  add_err "lock.json project 字段应为 'mora'，实际为 '$project'"
fi

# 3. license_policy
allowlist=$(jq -r '.license_policy.allowlist[]?' "$LOCK_FILE")
denylist=$(jq -r '.license_policy.denylist[]?' "$LOCK_FILE")
review_required=$(jq -r '.license_policy.review_required[]?' "$LOCK_FILE")

if [[ -z "$allowlist" ]]; then
  add_err "license_policy.allowlist 为空（无法判定合规）"
fi

# 4. 组件逐条校验
components=$(jq -r '.components | length' "$LOCK_FILE")
if [[ "$components" -eq 0 ]]; then
  echo "ℹ lock.json 无第三方组件（空基线，仍通过结构校验）"
fi

for i in $(seq 0 $((components - 1))); do
  idx="components[$i]"
  name=$(jq -r ".$idx.name // empty" "$LOCK_FILE")
  source_url=$(jq -r ".$idx.source_url // empty" "$LOCK_FILE")
  digest=$(jq -r ".$idx.commit_sha_or_digest // empty" "$LOCK_FILE")
  license=$(jq -r ".$idx.license // empty" "$LOCK_FILE")
  notice_path=$(jq -r ".$idx.notice_path // empty" "$LOCK_FILE")
  capability=$(jq -r ".$idx.capability // empty" "$LOCK_FILE")
  adr=$(jq -r ".$idx.adr // empty" "$LOCK_FILE")
  status=$(jq -r ".$idx.status // empty" "$LOCK_FILE")

  [[ -z "$name" ]] && add_err "组件[$i]: 缺 name"
  [[ -z "$source_url" ]] && add_err "组件[$i] ($name): 缺 source_url"
  [[ -z "$digest" ]] && add_err "组件[$i] ($name): 缺 commit_sha_or_digest（漂移依赖风险）"
  [[ -z "$license" ]] && add_err "组件[$i] ($name): 缺 license"
  [[ -z "$notice_path" ]] && add_err "组件[$i] ($name): 缺 notice_path"
  [[ -z "$capability" ]] && add_err "组件[$i] ($name): 缺 capability"

  # digest 非空且非 TBD（reference_baseline_only 也必须有具体 digest）
  if [[ -n "$digest" && "$digest" == "TBD" ]]; then
    add_err "组件[$i] ($name): digest 为 TBD，必须固定 commit/digest 后才能发布"
  fi

  # license 白名单校验（fail closed）
  if [[ -n "$license" ]]; then
    if echo "$denylist" | grep -qxF "$license"; then
      add_err "组件[$i] ($name): license '$license' 在 denylist（copyleft/AGPL/商业限制），禁止引入"
    elif echo "$review_required" | grep -qxF "$license"; then
      echo "⚠ 组件[$i] ($name): license '$license' 需 ADR 评审（review_required）" >&2
      [[ -z "$adr" ]] && add_err "组件[$i] ($name): license '$license' 需 review 但未声明 adr"
    elif ! echo "$allowlist" | grep -qxF "$license"; then
      add_err "组件[$i] ($name): license '$license' 不在 allowlist，需先纳入白名单或走 ADR"
    fi
  fi

  # NOTICE 文件存在（路径相对 repo root）
  if [[ -n "$notice_path" ]]; then
    full_notice="$REPO_ROOT/$notice_path"
    if [[ ! -f "$full_notice" ]]; then
      add_err "组件[$i] ($name): NOTICE 文件不存在：$notice_path"
    fi
  fi

  # ADR 文件存在（如声明）
  if [[ -n "$adr" ]]; then
    full_adr="$REPO_ROOT/$adr"
    if [[ ! -f "$full_adr" ]]; then
      add_err "组件[$i] ($name): ADR 文件不存在：$adr"
    fi
  fi

  echo "✓ 组件[$i] $name — $license — ${digest:0:12} — ${status:-<no status>}"
done

echo "---"
if [[ $fail -eq 0 ]]; then
  echo "✓ third-party-check 通过：lock.json 完整、digest 固定、license 合规、NOTICE/ADR 齐全"
  exit 0
else
  echo "✗ third-party-check 失败（fail closed）：" >&2
  for e in "${errors[@]}"; do echo "  - $e" >&2; done
  exit 1
fi
