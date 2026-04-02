#!/usr/bin/env bash
set -euo pipefail

while [[ ! -f ".boid/release-readonly-hooks" ]]; do
  sleep 0.05
done

cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"hook review ok","status":"resolved"}]}}}
EOF
