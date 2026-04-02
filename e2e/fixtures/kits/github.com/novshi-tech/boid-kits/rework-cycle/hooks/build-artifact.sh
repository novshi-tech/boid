#!/usr/bin/env bash
set -euo pipefail

while [[ ! -f ".boid/release-rework-hook" ]]; do
  sleep 0.05
done

cat <<'EOF'
{"payload_patch":{"artifact":{"source":"build-artifact"}}}
EOF
