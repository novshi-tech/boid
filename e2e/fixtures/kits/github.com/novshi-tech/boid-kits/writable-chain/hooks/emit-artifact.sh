#!/usr/bin/env bash
set -euo pipefail

while [[ ! -f ".boid/release-writable-hook-a" ]]; do
  sleep 0.05
done

printf 'hook-a ready\n' > writable-ready.txt

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.yaml" <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"hook a ready","status":"resolved"}]}}}
EOF
