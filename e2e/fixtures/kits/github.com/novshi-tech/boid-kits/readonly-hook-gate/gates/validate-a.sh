#!/usr/bin/env bash
set -euo pipefail

sleep 1

cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"gate a ok","status":"resolved"}]}}}
EOF
