#!/usr/bin/env bash
set -euo pipefail

while [[ ! -f "release-rework-hook" ]]; do
  sleep 0.05
done
rm -f "release-rework-hook"

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.yaml" <<'EOF'
{"payload_patch":{"artifact":{"source":"build-artifact"}}}
EOF
