#!/usr/bin/env bash
set -euo pipefail

# BOID_INSTRUCTIONS を state ファイルに記録して artifact を emit
printf '%s' "${BOID_INSTRUCTIONS:-}" > "$E2E_STATE_DIR/agent-a-instructions.json"

cat <<'EOF'
{"payload_patch":{"artifact":{"source":"agent-a"}}}
EOF
