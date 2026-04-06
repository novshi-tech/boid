#!/usr/bin/env bash
set -euo pipefail

while [[ ! -f ".boid/release-readonly-hooks" ]]; do
  sleep 0.05
done

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.yaml" <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"hook review ok","status":"resolved"}]}}}
EOF
