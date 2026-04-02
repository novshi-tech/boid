#!/usr/bin/env bash
set -euo pipefail

cat <<'EOF'
{"payload_patch":{"artifact":{"branch":"boid/e2e","commit":"deadbeef"}}}
EOF
