#!/usr/bin/env bash
set -euo pipefail

sleep 1

boid task create >/tmp/writable-chain-follow-up.log <<'YAML'
project_id: writable-chain
title: Writable Chain Follow-up
behavior: writable
YAML

cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"gate b ok","status":"resolved"}]}}}
EOF
