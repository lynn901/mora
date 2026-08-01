#!/usr/bin/env bash
#
# Fetch local font files for Mora's private-by-default font stack.
#
# Mora defaults to air-gapped operation: fonts are served from this directory
# (public/fonts) and no outbound requests are made at runtime. Run this script
# once during provisioning to download the woff2 files referenced by fonts.css.
#
# Coverage:
#   - Inter and JetBrains Mono are downloaded as local woff2 (Latin subsets).
#   - Noto Sans SC is NOT downloaded: the full CJK font is ~10 MB across 100+
#     unicode-range slices. In local mode CJK text falls back to the system
#     families declared in --font-sans (PingFang SC / Microsoft YaHei …). Build
#     with VITE_FONT_SOURCE=cdn for the full Noto Sans SC web font.
#
# Usage:
#   ./fetch-fonts.sh
#
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$DIR"

command -v curl >/dev/null 2>&1 || { echo "missing dependency: curl" >&2; exit 1; }

# Google Fonts css2 only returns woff2 URLs to a modern-browser User-Agent.
UA="Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

# Download the Latin unicode-range woff2 slice for a family+weight.
download_latin() {
  local family="$1" weight="$2" out="$3"
  local css url
  css="$(curl -fsSL -A "$UA" \
    "https://fonts.googleapis.com/css2?family=${family}:wght@${weight}&display=swap" \
    2>/dev/null)" || { echo "  skip $out (css2 unreachable)" >&2; return 0; }
  # css2 lists subsets in a fixed order; "latin" is the final block.
  url="$(printf '%s\n' "$css" | awk '/\/\* latin \*\//{f=1} f&&/woff2/{print; exit}' \
    | grep -oE "https://[^)]+\.woff2")"
  if [ -n "$url" ]; then
    curl -fsSL "$url" -o "$out" && echo "  fetched $out"
  else
    echo "  skip $out (no latin woff2 in css2 response)" >&2
  fi
}

echo "Inter…"
download_latin "Inter" 400 inter-latin-400.woff2
download_latin "Inter" 500 inter-latin-500.woff2
download_latin "Inter" 600 inter-latin-600.woff2
download_latin "Inter" 700 inter-latin-700.woff2

echo "JetBrains Mono…"
download_latin "JetBrains+Mono" 400 jetbrains-mono-400.woff2
download_latin "JetBrains+Mono" 500 jetbrains-mono-500.woff2
download_latin "JetBrains+Mono" 600 jetbrains-mono-600.woff2

echo ""
echo "Done. Missing files degrade gracefully to system fonts via fonts.css."
echo "For the full Noto Sans SC CJK web font, build with VITE_FONT_SOURCE=cdn."
