#!/usr/bin/env bash
# Assert that $1 (a file) contains the literal substring $2.
set -euo pipefail

file="$1"
needle="$2"

if grep -qF -- "$needle" "$file"; then
  echo "OK: found '${needle}' in $(basename "$file")"
else
  echo "FAIL: '${needle}' not found in $(basename "$file"). Contents:" >&2
  cat "$file" >&2
  exit 1
fi
