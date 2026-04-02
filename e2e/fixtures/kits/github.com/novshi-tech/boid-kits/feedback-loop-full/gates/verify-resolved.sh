#!/usr/bin/env bash
set -euo pipefail

sleep 1

cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"verification ok","status":"resolved"}]}}}
EOF
