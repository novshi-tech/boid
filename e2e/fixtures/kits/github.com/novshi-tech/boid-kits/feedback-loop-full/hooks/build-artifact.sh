#!/usr/bin/env bash
set -euo pipefail

while [[ ! -f ".boid/release-feedback-hook" ]]; do
  sleep 0.05
done

cat <<'EOF'
{"payload_patch":{"artifact":{"source":"build-artifact"}}}
EOF
