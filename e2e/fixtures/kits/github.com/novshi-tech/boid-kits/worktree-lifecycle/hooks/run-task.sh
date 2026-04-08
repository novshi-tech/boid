#!/usr/bin/env bash
set -euo pipefail

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.yaml" <<'EOF'
{"payload_patch":{"artifact":{"status":"completed"}}}
EOF
