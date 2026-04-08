#!/usr/bin/env bash
set -euo pipefail

while [[ ! -f ".boid/release-worktree-hook" ]]; do
  sleep 0.05
done

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.yaml" <<'EOF'
{"payload_patch":{"artifact":{"done":true}}}
EOF
