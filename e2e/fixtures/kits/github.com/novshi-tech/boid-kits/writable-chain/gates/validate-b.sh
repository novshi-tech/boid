#!/usr/bin/env bash
set -euo pipefail

sleep 1

boid task create --title "Writable Chain Follow-up" --project writable-chain --behavior writable >/tmp/writable-chain-follow-up.log

cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"gate b ok","status":"resolved"}]}}}
EOF
