#!/usr/bin/env bash
# deploy/chart/hack/generate-migrations-cm.sh
# 将 migrations/*.sql 打包为 Helm Chart 内 ConfigMap 的数据文件。
# Helm 不支持直接包含外部目录（非子路径），因此将迁移文件平铺到 chart/templates/ 下
# 作为一个 ConfigMap，供 migrate Job 挂载。
#
# 用法: bash deploy/chart/hack/generate-migrations-cm.sh
# 输出: deploy/chart/wiki/templates/migrations-cm.yaml

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
OUTPUT="$ROOT/deployments/chart/wiki/templates/migrations-cm.yaml"

mkdir -p "$(dirname "$OUTPUT")"

cat > "$OUTPUT" <<EOF
# 自动生成 -- 由 generate-migrations-cm.sh 创建，不要手动编辑。
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "wiki.fullname" . }}-migrations
  labels:
    {{- include "wiki.labels" . | nindent 4 }}
  annotations:
    helm.sh/hook: pre-install,pre-upgrade
    helm.sh/hook-weight: "-1"
data:
  run-migrations.sh: |
$(sed 's/^/    /' "$ROOT/deployments/run-migrations.sh")
EOF

for f in "$ROOT"/migrations/*.sql; do
  name=$(basename "$f")
  cat >> "$OUTPUT" <<EOF
  ${name}: |
$(sed 's/^/    /' "$f")
EOF
done

echo "Generated $OUTPUT ($(wc -l < "$OUTPUT") lines)"
