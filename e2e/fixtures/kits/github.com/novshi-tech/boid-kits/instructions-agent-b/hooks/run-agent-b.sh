#!/usr/bin/env bash
set -euo pipefail

# BOID_INSTRUCTIONS を state ファイルに記録して resolved verification を emit
printf '%s' "${BOID_INSTRUCTIONS:-}" > "$E2E_STATE_DIR/agent-b-instructions.json"

cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"review passed","status":"resolved"}]}}}
EOF
