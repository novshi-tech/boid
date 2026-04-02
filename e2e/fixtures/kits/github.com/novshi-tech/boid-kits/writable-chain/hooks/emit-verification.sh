#!/usr/bin/env bash
set -euo pipefail

while [[ ! -f ".boid/release-writable-hook-b" ]]; do
  sleep 0.05
done

[[ -f writable-ready.txt ]] || {
  printf 'missing writable-ready.txt\n' >&2
  exit 1
}
cat <<'EOF'
{"payload_patch":{"artifact":{"source":"emit-verification"}}}
EOF
